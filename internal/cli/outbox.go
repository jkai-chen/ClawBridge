package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"clawbridge/internal/remote"
)

// outboxUsage is the outbox command's usage.
const outboxUsage = "usage: clawbridge outbox list [--dead] | flush | retry ID | drop ID --confirm drop"

// commandDrain bounds the drain a CLI command runs before its own call
// (ZERO-LOSS-DEPLOY §1.4).
const commandDrain = 2 * time.Second

// runOutbox shows and handles the writes waiting in this machine's outbox
// (§1.5): what couldn't reach the server, kept to be sent when it's back.
func runOutbox(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(outboxUsage)
	}
	sub, args := args[0], args[1:]
	f := flag.NewFlagSet("outbox "+sub, flag.ContinueOnError)
	f.SetOutput(stderr)
	dead := f.Bool("dead", false, "list the dead letters instead of the waiting writes")
	confirm := f.String("confirm", "", "drop: the word drop, to delete the write")
	ordered, err := interspersedFlags(f, args)
	if err != nil {
		return err
	}
	if err = f.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	pos := f.Args()
	var id int64
	switch sub {
	case "list", "flush":
		if len(pos) != 0 {
			return errors.New(outboxUsage)
		}
	case "retry", "drop":
		if len(pos) != 1 {
			return errors.New(outboxUsage)
		}
		if id, err = strconv.ParseInt(pos[0], 10, 64); err != nil || id <= 0 {
			return errors.New("ID is the number outbox list shows")
		}
	default:
		return errors.New(outboxUsage)
	}
	if (*dead && sub != "list") || (*confirm != "" && sub != "drop") {
		return errors.New(outboxUsage)
	}
	c, _, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	if c.Mode != "cloud" {
		return errors.New("the outbox is a hosted server's client's: this profile uses a local database, which never queues")
	}
	client, err := remote.New(remoteConfig(c))
	if err != nil {
		return err
	}
	ob := client.Outbox()
	if ob == nil {
		return errors.New("the client outbox is off for this profile (CLAWBRIDGE_CLIENT_OUTBOX=off, or no local cache); nothing is queued")
	}
	defer ob.Close()
	switch sub {
	case "list":
		items, err := ob.List(ctx, *dead)
		if err != nil {
			return err
		}
		return emit(ctx, stdout, "outbox list", map[string]any{"source": "client_outbox", "data": map[string]any{"dead": *dead, "items": items}})
	case "flush":
		report, err := client.DrainOutbox(ctx, time.Minute)
		if err != nil {
			return err
		}
		data := map[string]any{"sent": report.Sent, "dead": report.Dead, "waiting": report.Waiting}
		if report.SignIn {
			data["note"] = "sign in again to send the waiting writes"
		}
		return emit(ctx, stdout, "outbox flush", map[string]any{"source": "client_outbox", "data": data})
	case "retry":
		if err = ob.Retry(ctx, id); err != nil {
			return err
		}
		return emit(ctx, stdout, "outbox retry", map[string]any{"source": "client_outbox", "data": map[string]any{"id": id, "state": "queued", "note": "back at the head of its queue; it is sent at the next drain (outbox flush sends it now)"}})
	default:
		if *confirm != "drop" {
			return fmt.Errorf("dropping deletes write %d for good: it is never sent; pass --confirm drop", id)
		}
		if err = ob.Drop(ctx, id); err != nil {
			return err
		}
		return emit(ctx, stdout, "outbox drop", map[string]any{"source": "client_outbox", "data": map[string]any{"id": id, "state": "dropped"}})
	}
}

// tuiOutboxEvery is how often the running TUI looks at a non-empty outbox,
// as an agent's MCP server does; a successful call wakes it sooner. Tests
// shorten it.
var tuiOutboxEvery = 5 * time.Second

// tuiOutbox sends what waits before the TUI opens, keeps sending it while
// the TUI runs (FR11), and counts what waits, for its status line. All of
// it stops with ctx; stopped waits for the sending to end, so nothing of
// it outlives the TUI.
func tuiOutbox(ctx context.Context, client *remote.Client) (count func() int, stopped func()) {
	drainBeforeCommand(ctx, client)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.DrainOutboxWhile(ctx, tuiOutboxEvery)
	}()
	return func() int { return outboxCount(ctx, client) }, func() { <-done }
}

// drainBeforeCommand sends what waits in the outbox, briefly, before a
// command's own call, so a command never overtakes them.
func drainBeforeCommand(ctx context.Context, client *remote.Client) {
	if client != nil {
		client.DrainOutbox(ctx, commandDrain)
	}
}

// outboxLines prints the outbox's notices on stderr.
func outboxLines(stderr io.Writer) func(string) {
	return func(line string) { fmt.Fprintln(stderr, line) }
}

// outboxCount is what waits in the client's outbox, for the TUI.
func outboxCount(ctx context.Context, client *remote.Client) int {
	ob := client.Outbox()
	if ob == nil {
		return 0
	}
	st, err := ob.Status(ctx)
	if err != nil {
		return 0
	}
	return st.Waiting
}
