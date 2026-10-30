package notify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
)

// channelState writes the session's channel record as the channel server
// would, live and fresh, then changed by mutate.
func channelState(t *testing.T, dir string, mutate func(*ctxwatch.ChannelState)) {
	t.Helper()
	now := time.Now().UTC()
	st := ctxwatch.ChannelState{PID: os.Getpid(), State: ctxwatch.ChannelActive, LastPushSeq: 9,
		ConfirmedAt: now.Add(-time.Minute).Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}
	if mutate != nil {
		mutate(&st)
	}
	if err := ctxwatch.SaveChannelState(dir, "session-name", st); err != nil {
		t.Fatal(err)
	}
}

// deadPID is a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 4_000_000; pid > 3_000_000; pid -= 7919 {
		if !ctxwatch.ProcessAlive(pid) {
			return pid
		}
	}
	t.Skip("no free process id found")
	return 0
}

func commandConfig(t *testing.T, hints *int) Config {
	t.Helper()
	c := config(t)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c.Binding.Adapter, c.Binding.Command = "command", []string{program, "{text}"}
	c.CommandRunner = func(context.Context, []string, []string, []byte) error {
		*hints++
		return nil
	}
	c.ContextDir = t.TempDir()
	c.Executor = executorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation != "notification_status" {
			return bus.Result{Data: map[string]any{}}, nil
		}
		after, _ := r.Args["after"].(int64)
		if after >= 9 {
			return result(2, 9, 0), nil
		}
		return result(2, 9, 2), nil
	})
	return c
}

// FR26: with a fresh, live channel whose push was confirmed, the command
// adapter leaves the wake to it, says why, and doesn't advance its
// checkpoint, so the batch is still hinted if the channel falls back.
func TestNotifySkipsForAConfirmedChannel(t *testing.T) {
	hints := 0
	c := commandConfig(t, &hints)
	channelState(t, c.ContextDir, nil)
	var diag bytes.Buffer
	c.Diagnostics = &diag
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 0 || load(t, c.StatePath).After != 0 {
		t.Fatal("typed over a confirmed channel, or advanced", hints, load(t, c.StatePath).After)
	}
	if !strings.Contains(diag.String(), `"type":"hint_skipped"`) || !strings.Contains(diag.String(), `"reason":"channel"`) || strings.Contains(diag.String(), "hint_accepted") {
		t.Fatal(diag.String())
	}
	for name, mutate := range map[string]func(*ctxwatch.ChannelState){
		"stale": func(s *ctxwatch.ChannelState) {
			s.UpdatedAt = time.Now().Add(-91 * time.Second).UTC().Format(time.RFC3339)
		},
		"dead pid":    func(s *ctxwatch.ChannelState) { s.PID = deadPID(t) },
		"unconfirmed": func(s *ctxwatch.ChannelState) { s.State, s.ConfirmedAt = ctxwatch.ChannelUnconfirmed, "" },
		"fallback":    func(s *ctxwatch.ChannelState) { s.State = ctxwatch.ChannelFallback },
		"inactive":    func(s *ctxwatch.ChannelState) { s.State = ctxwatch.ChannelInactive },
		"another":     nil,
		"no record":   nil,
	} {
		hints = 0
		c := commandConfig(t, &hints)
		switch name {
		case "another":
			// Another session's confirmed channel doesn't count.
			c.Binding.Session = "other-session"
			channelState(t, c.ContextDir, nil)
		case "no record":
		default:
			channelState(t, c.ContextDir, mutate)
		}
		if err := Run(context.Background(), c); err != nil {
			t.Fatal(name, err)
		}
		if hints != 1 || load(t, c.StatePath).After != 9 {
			t.Fatal(name, hints, load(t, c.StatePath).After)
		}
	}
}

// The watcher keeps skipping the batch, saying so once, while the channel
// holds; when it falls back, the same batch is typed.
func TestNotifyTypesTheBatchAfterAFallback(t *testing.T) {
	hints := 0
	c := commandConfig(t, &hints)
	c.Once = false
	channelState(t, c.ContextDir, nil)
	var diag bytes.Buffer
	c.Diagnostics = &diag
	waits := 0
	c.Wait = func(context.Context, time.Duration) error {
		waits++
		switch waits {
		case 3:
			if hints != 0 || load(t, c.StatePath).After != 0 {
				t.Fatal("hinted while the channel held", hints)
			}
			channelState(t, c.ContextDir, func(s *ctxwatch.ChannelState) { s.State = ctxwatch.ChannelFallback })
		case 5:
			return context.Canceled
		}
		return nil
	}
	if err := Run(context.Background(), c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if hints != 1 || load(t, c.StatePath).After != 9 || strings.Count(diag.String(), "hint_skipped") != 1 {
		t.Fatal(hints, load(t, c.StatePath).After, diag.String())
	}
}

// The Codex adapter skips the same way; the JSON adapter, which wakes
// nobody, never does.
func TestNotifyCodexAndJSONWithAChannel(t *testing.T) {
	c := config(t)
	c.Binding.Adapter = "codex"
	c.Binding.Thread = "00000000-0000-4000-8000-0000000000a1"
	c.CodexCommand = fakeCodex(t)
	c.ContextDir = t.TempDir()
	channelState(t, c.ContextDir, nil)
	var queued [][]string
	c.Runner = RunnerFunc(func(_ context.Context, _ string, args []string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"queue", "--help"}) {
			return []byte("queue --thread --message"), nil
		}
		queued = append(queued, args)
		return nil, nil
	})
	c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) { return result(2, 9, 2), nil })
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 || load(t, c.StatePath).After != 0 {
		t.Fatal(queued)
	}
	j := config(t)
	j.ContextDir = c.ContextDir
	var out bytes.Buffer
	j.Output = &out
	j.Executor = c.Executor
	if err := Run(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "inbox_changed") || load(t, j.StatePath).After != 9 {
		t.Fatal(out.String())
	}
}
