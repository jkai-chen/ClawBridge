package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
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

func (a watcherApp) SubscribeIssue(ctx context.Context, issue, email, user string) (string, error) {
	return a.app.SubscribeIssue(ctx, issue, email, user)
}
func (a watcherApp) HasScope(ctx context.Context, scope string) (bool, error) {
	return a.app.HasScope(ctx, scope)
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

// Fallback is a fallback nudge's pass: markers, then the dispatcher.
func (t watcherTicker) Fallback(ctx context.Context, pid string) any { return t.w.Fallback(ctx, pid) }

// watchSettings is the watcher's environment.
type watchSettings struct {
	interval                  time.Duration
	tickAudience, tickAccount string
	// budget is CLAWBRIDGE_LINEAR_BUDGET; 0 when unset (the bus's 600).
	budget int
	// budgetMax is CLAWBRIDGE_LINEAR_BUDGET_MAX, the owner's ceiling.
	budgetMax int
}

// readWatchSettings reads CLAWBRIDGE_WATCH_INTERVAL (1m–30m, empty for no
// in-process clock), the two tick variables (both or neither) and
// CLAWBRIDGE_WATCH_BUDGET. The app's marker writes use the shared
// operation ledger in Postgres.
func readWatchSettings(getenv func(string) string) (watchSettings, error) {
	w := watchSettings{
		tickAudience: strings.TrimSpace(getenv("CLAWBRIDGE_TICK_AUDIENCE")),
		tickAccount:  strings.TrimSpace(getenv("CLAWBRIDGE_TICK_SERVICE_ACCOUNT")),
	}
	budget, err := readWatchBudget(getenv)
	if err != nil {
		return w, err
	}
	w.budget = budget
	if w.budgetMax, err = readLinearBudgetMax(getenv, budget); err != nil {
		return w, err
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

// readWatchBudget reads CLAWBRIDGE_LINEAR_BUDGET (or its old name
// CLAWBRIDGE_WATCH_BUDGET): the Linear calls per hour every project may
// make by default, agent writes included, 1–600; empty is 0, the bus's
// default of 600.
func readWatchBudget(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv("CLAWBRIDGE_LINEAR_BUDGET"))
	if old := strings.TrimSpace(getenv("CLAWBRIDGE_WATCH_BUDGET")); raw == "" {
		raw = old
	} else if old != "" && old != raw {
		return 0, fmt.Errorf("CLAWBRIDGE_LINEAR_BUDGET and its old name CLAWBRIDGE_WATCH_BUDGET differ; set only CLAWBRIDGE_LINEAR_BUDGET")
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 600 {
		return 0, fmt.Errorf("CLAWBRIDGE_LINEAR_BUDGET must be a whole number from 1 to 600 (Linear calls per hour)")
	}
	return n, nil
}

// Linear's hourly request limit for an OAuth app user, which bounds the
// operator's ceiling.
const linearAppHourly = 5000

// readLinearBudgetMax reads CLAWBRIDGE_LINEAR_BUDGET_MAX: how far a project
// owner may raise the project's cap (R2), default 2000, at most Linear's
// 5000 an hour for the app. It may not be below the default cap, so owners
// always have room to raise.
func readLinearBudgetMax(getenv func(string) string, budget int) (int, error) {
	if budget == 0 {
		budget = bus.LinearBudgetDefault
	}
	n := bus.LinearBudgetCeiling
	if raw := strings.TrimSpace(getenv("CLAWBRIDGE_LINEAR_BUDGET_MAX")); raw != "" {
		var err error
		if n, err = strconv.Atoi(raw); err != nil || n < 1 || n > linearAppHourly {
			return 0, fmt.Errorf("CLAWBRIDGE_LINEAR_BUDGET_MAX must be a whole number from 1 to %d (Linear calls per hour)", linearAppHourly)
		}
	}
	if n < budget {
		return 0, fmt.Errorf("CLAWBRIDGE_LINEAR_BUDGET_MAX (%d) is below the default cap CLAWBRIDGE_LINEAR_BUDGET (%d); raise it or lower the cap", n, budget)
	}
	return n, nil
}

// budgeted is a watcher whose lease budget the server sets.
type budgeted interface{ SetBudget(int) }

var _ budgeted = watcherTicker{}

// SetBudget sets the Linear calls per hour of every tick and nudge.
func (t watcherTicker) SetBudget(n int) { t.w.Budget = n }

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
	if b, ok := built.(budgeted); ok {
		b.SetBudget(w.budget)
	}
	// The clock and the endpoint share one flight, so their ticks never
	// overlap.
	ticker := oneAtATime(built)
	ticker.drain = opts.Drain
	if w.tickAudience != "" {
		verifier, err := NewGoogleTickVerifier(ctx, w.tickAudience, w.tickAccount)
		if err != nil {
			log.Print("watcher: tick endpoint off (invalid tick configuration)")
		} else {
			opts.TickVerifier, opts.Ticker = verifier, ticker
		}
	}
	if w.interval > 0 {
		// Nudges ride beside the in-process clock only: with the scheduler's
		// endpoint alone there is no process to run them. A tick whose
		// marker pass left rows hands them to the nudges.
		if markers, ok := built.(markerPasser); ok && store != nil {
			n := newNudger(ticker, markers, store.WatchProjectID)
			ticker.after = n.Ticked
			go n.run(ctx)
			opts.Nudge, opts.NudgeProject = n.Nudge, n.NudgeProject
		}
		go runWatchLoop(ctx, ticker, w.interval)
		log.Printf("watcher: ticking every %s", w.interval)
	}
	return opts
}

// watchRetry and watchRetries bound how a clock tick that finds the
// flight held (a nudge's pass) waits for it: a few seconds at a time, so
// the interval's comments, intake, health and documents aren't skipped.
var (
	watchRetry   = 5 * time.Second
	watchRetries = 12
)

// runWatchLoop ticks every interval until ctx ends, each tick within
// tickDeadline. Ticks never overlap; a slow tick delays the next. A tick
// that finds the flight held tries again shortly, up to watchRetries.
func runWatchLoop(ctx context.Context, ticker Ticker, interval time.Duration) {
	clock := time.NewTicker(interval)
	defer clock.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.C:
			for try := 0; ; try++ {
				tick, cancel := context.WithTimeout(ctx, tickDeadline)
				got := ticker.Tick(tick)
				cancel()
				if !heldFlight(got) || try >= watchRetries {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(watchRetry):
				}
			}
		}
	}
}

// heldFlight reports a tick the single flight didn't start.
func heldFlight(got any) bool {
	m, ok := got.(map[string]any)
	return ok && m["running"] == true
}
