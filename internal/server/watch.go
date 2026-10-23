package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"clawbridge/internal/bus"
)

// The server's own watcher (live/V03-OPS.md §3). On the hosted VM it ticks
// in process every CLAWBRIDGE_WATCH_INTERVAL (option A); the scheduler tick
// endpoint (option B) is for a deployment without a long-lived process.

// NewWatcher builds the server's watcher over the Linear app. The app
// wiring (v0.2.7) sets it; while it is nil the watcher stays off.
var NewWatcher func(ctx context.Context, store *bus.Store, ledgerDirectory string) (Ticker, error)

// watchSettings is the watcher's environment.
type watchSettings struct {
	interval                  time.Duration
	ledgerDirectory           string
	tickAudience, tickAccount string
}

// readWatchSettings reads CLAWBRIDGE_WATCH_INTERVAL (1m–30m, empty for no
// in-process clock), CLAWBRIDGE_WATCH_LEDGER_DIR (a writable directory for
// the app's marker writes) and the two tick variables (both or neither).
func readWatchSettings(getenv func(string) string) (watchSettings, error) {
	w := watchSettings{
		ledgerDirectory: strings.TrimSpace(getenv("CLAWBRIDGE_WATCH_LEDGER_DIR")),
		tickAudience:    strings.TrimSpace(getenv("CLAWBRIDGE_TICK_AUDIENCE")),
		tickAccount:     strings.TrimSpace(getenv("CLAWBRIDGE_TICK_SERVICE_ACCOUNT")),
	}
	if raw := strings.TrimSpace(getenv("CLAWBRIDGE_WATCH_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Minute || d > 30*time.Minute {
			return w, fmt.Errorf("CLAWBRIDGE_WATCH_INTERVAL must be a duration from 1m to 30m")
		}
		w.interval = d
	}
	if (w.tickAudience == "") != (w.tickAccount == "") {
		return w, fmt.Errorf("set both CLAWBRIDGE_TICK_AUDIENCE and CLAWBRIDGE_TICK_SERVICE_ACCOUNT, or neither")
	}
	if (w.interval > 0 || w.tickAudience != "") && w.ledgerDirectory == "" {
		return w, fmt.Errorf("the watcher needs CLAWBRIDGE_WATCH_LEDGER_DIR, a writable directory")
	}
	return w, nil
}

func (w watchSettings) wanted() bool { return w.interval > 0 || w.tickAudience != "" }

// startWatcher builds the watcher when it is wanted and the app is wired,
// starts the in-process clock, and returns the handler options for the
// tick endpoint. A watcher that cannot start leaves the server serving.
func startWatcher(ctx context.Context, store *bus.Store, w watchSettings, opts HandlerOptions) HandlerOptions {
	if !w.wanted() {
		return opts
	}
	if NewWatcher == nil {
		log.Print("watcher: off (it needs the Linear app, v0.2.7)")
		return opts
	}
	if err := os.MkdirAll(w.ledgerDirectory, 0o700); err != nil {
		log.Print("watcher: off (its ledger directory is not writable)")
		return opts
	}
	ticker, err := NewWatcher(ctx, store, w.ledgerDirectory)
	if err != nil || ticker == nil {
		log.Print("watcher: off (the Linear app is not configured)")
		return opts
	}
	if w.tickAudience != "" {
		verifier, err := NewGoogleTickVerifier(ctx, w.tickAudience, w.tickAccount)
		if err != nil {
			log.Print("watcher: tick endpoint off (invalid tick configuration)")
		} else {
			opts.TickVerifier, opts.Ticker = verifier, ticker
		}
	}
	if w.interval > 0 {
		go runWatchLoop(ctx, ticker, w.interval)
		log.Printf("watcher: ticking every %s", w.interval)
	}
	return opts
}

// runWatchLoop ticks every interval until ctx ends, each tick within
// tickDeadline. Ticks never overlap; a slow tick delays the next.
func runWatchLoop(ctx context.Context, ticker Ticker, interval time.Duration) {
	clock := time.NewTicker(interval)
	defer clock.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.C:
			tick, cancel := context.WithTimeout(ctx, tickDeadline)
			ticker.Tick(tick)
			cancel()
		}
	}
}
