package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/watcher"
)

// The server's own watcher (live/V03-OPS.md §3). On the hosted VM it ticks
// in process every CLAWBRIDGE_WATCH_INTERVAL (option A); the scheduler tick
// endpoint (option B) is for a deployment without a long-lived process.

// NewWatcher builds the server's watcher over the Linear app; tests
// replace it.
var NewWatcher = func(_ context.Context, store *bus.Store, app *linearapp.App) (Ticker, error) {
	return watcherTicker{&watcher.Watcher{Store: store, App: watcherApp{app}, Ledger: linearboard.SharedLedger(store, nil)}}, nil
}

// watcherApp is the server's Linear app as the watcher uses it.
type watcherApp struct{ app *linearapp.App }

var (
	_ watcher.App = watcherApp{}
	_ Ticker      = watcherTicker{}
)

func (a watcherApp) Open(ctx context.Context, readonly bool) (linearboard.Provider, error) {
	p, err := a.app.Provider(ctx, readonly)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (a watcherApp) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	return a.app.GraphQL(ctx, query, variables, out)
}

func (a watcherApp) Identity(ctx context.Context) (watcher.AppIdentity, error) {
	id, err := a.app.Identity(ctx)
	return watcher.AppIdentity{AppUserID: id.AppUserID, LinearOrgID: id.LinearOrgID}, err
}

// watcherTicker serves the watcher's ticks to the loop and the endpoint.
type watcherTicker struct{ w *watcher.Watcher }

func (t watcherTicker) Tick(ctx context.Context) any { return t.w.Tick(ctx) }

// Markers is a nudge's marker pass of one project.
func (t watcherTicker) Markers(ctx context.Context, pid string) any { return t.w.Markers(ctx, pid) }

// watchSettings is the watcher's environment.
type watchSettings struct {
	interval                  time.Duration
	tickAudience, tickAccount string
}

// readWatchSettings reads CLAWBRIDGE_WATCH_INTERVAL (1m–30m, empty for no
// in-process clock) and the two tick variables (both or neither). The
// app's marker writes use the shared operation ledger in Postgres.
func readWatchSettings(getenv func(string) string) (watchSettings, error) {
	w := watchSettings{
		tickAudience: strings.TrimSpace(getenv("CLAWBRIDGE_TICK_AUDIENCE")),
		tickAccount:  strings.TrimSpace(getenv("CLAWBRIDGE_TICK_SERVICE_ACCOUNT")),
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
	if opts.Writer == nil || opts.Writer.App == nil {
		log.Print("watcher: off (the Linear app is not configured)")
		return opts
	}
	built, err := NewWatcher(ctx, store, opts.Writer.App)
	if err != nil || built == nil {
		log.Print("watcher: off (the Linear app is not configured)")
		return opts
	}
	// The clock and the endpoint share one flight, so their ticks never
	// overlap.
	ticker := oneAtATime(built)
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
		// Nudges ride beside the in-process clock only: with the scheduler's
		// endpoint alone there is no process to run them.
		if markers, ok := built.(markerPasser); ok && store != nil {
			n := newNudger(ticker, markers, store.WatchProjectID)
			go n.run(ctx)
			opts.Nudge = n.Nudge
		}
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
