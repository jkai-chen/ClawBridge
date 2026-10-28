package server

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"clawbridge/internal/bus"
)

const maintenanceUsage = "usage: clawbridge operator maintenance start --reason TEXT [--ttl 15m] (--wait DURATION | --no-wait) --confirm | maintenance end --confirm | maintenance show"

// maintenanceStore is the operator's view of the drain.
type maintenanceStore interface {
	MaintenanceState(ctx context.Context) (bus.Maintenance, error)
	StartMaintenance(ctx context.Context, reason, by string, ttl time.Duration) (bus.Maintenance, error)
	EndMaintenance(ctx context.Context) (bool, error)
	MaintenanceInstances(ctx context.Context) ([]bus.MaintenanceInstance, error)
}

// drainPoll is how often maintenance start --wait reads the instances.
var drainPoll = time.Second

// openMaintenanceStore opens the database for the maintenance command;
// tests replace it.
var openMaintenanceStore = func(ctx context.Context, dsn string) (maintenanceStore, func(), error) {
	store, err := bus.OpenPostgresMaintenance(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open Postgres: %w", err)
	}
	return store, func() { store.Close() }, nil
}

// runMaintenance is operator maintenance start|end|show
// (live/ZERO-LOSS-DEPLOY-DESIGN.md §2.1): the deploy's drain before the
// swap.
func runMaintenance(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "start" && args[0] != "end" && args[0] != "show") {
		return errors.New(maintenanceUsage)
	}
	command := args[0]
	f := flag.NewFlagSet("operator maintenance "+command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	confirm := f.Bool("confirm", false, "confirm the change")
	reason := f.String("reason", "", "what the maintenance is for, shown to operators only, e.g. \"deploy v0.3.9\"")
	ttl := f.Duration("ttl", bus.MaintenanceDefaultTTL, "how long until maintenance ends on its own, at most 60m")
	wait := f.Duration("wait", 0, "wait up to this long for every live server to drain, then print drained")
	noWait := f.Bool("no-wait", false, "start without waiting, for emergencies only: in-flight work may be cut")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	if err := f.Parse(args[1:]); err != nil {
		return errors.New(maintenanceUsage)
	}
	if f.NArg() > 0 {
		return errors.New(maintenanceUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	switch command {
	case "start":
		if strings.TrimSpace(*reason) == "" {
			return errors.New("maintenance start needs --reason")
		}
		if *ttl <= 0 || *ttl > bus.MaintenanceMaxTTL {
			return errors.New("--ttl is more than 0 and at most 60m")
		}
		if set["wait"] == *noWait {
			return errors.New("maintenance start needs --wait DURATION, or --no-wait for an emergency (in-flight work may be cut)")
		}
		if set["wait"] && (*wait <= 0 || *wait > *ttl) {
			return errors.New("--wait is more than 0 and at most --ttl")
		}
		if !*confirm {
			return errors.New("maintenance start makes every server refuse writes with 503 maintenance, close its streams and pause its workers until maintenance end, the --ttl or the next server start; repeat with --confirm")
		}
	case "end":
		if set["reason"] || set["ttl"] || set["wait"] || *noWait {
			return errors.New("maintenance end takes only --confirm")
		}
		if !*confirm {
			return errors.New("maintenance end lets every server take writes again; repeat with --confirm")
		}
	case "show":
		if set["reason"] || set["ttl"] || set["wait"] || *noWait || *confirm {
			return errors.New("maintenance show takes no flags but --database-url-env")
		}
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	store, closeStore, err := openMaintenanceStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer closeStore()
	switch command {
	case "end":
		ended, err := store.EndMaintenance(ctx)
		if err != nil {
			return err
		}
		if !ended {
			_, err = fmt.Fprintln(stdout, "no maintenance was on")
			return err
		}
		_, err = fmt.Fprintln(stdout, "maintenance ended; servers take writes again and their workers resume after a 90 s grace")
		return err
	case "show":
		m, err := store.MaintenanceState(ctx)
		if err != nil {
			return err
		}
		instances, err := store.MaintenanceInstances(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"maintenance": m, "instances": instances})
	}
	m, err := store.StartMaintenance(ctx, *reason, "operator", *ttl)
	if errors.Is(err, bus.ErrMaintenanceInput) {
		return errors.New("--reason is 1-200 characters with no control character")
	}
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(stdout, "maintenance on until %s\n", m.EndsAt.UTC().Format("15:04:05Z")); err != nil {
		return err
	}
	if *noWait {
		_, err = fmt.Fprintln(stdout, "not waiting: requests and worker runs already in flight may be cut by a swap now")
		return err
	}
	return waitDrained(ctx, store, *wait, stdout)
}

// waitDrained waits until every live server has drained for this
// maintenance, and names those that haven't when the wait runs out (D7):
// don't swap on a partial drain.
func waitDrained(ctx context.Context, store maintenanceStore, wait time.Duration, stdout io.Writer) error {
	deadline := time.Now().Add(wait)
	for {
		instances, err := store.MaintenanceInstances(ctx)
		if err != nil {
			return err
		}
		live, pending := 0, []string{}
		for _, i := range instances {
			if !i.Live {
				continue
			}
			live++
			if !i.Drained {
				pending = append(pending, i.Instance)
			}
		}
		if len(pending) == 0 {
			_, err = fmt.Fprintf(stdout, "drained: %d of %d live servers\n", live, live)
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("not drained after %s: %s; maintenance stays on: decide before the swap, or run maintenance end", wait, strings.Join(pending, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPoll):
		}
	}
}
