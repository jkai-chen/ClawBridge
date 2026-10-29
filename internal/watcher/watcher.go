// Package watcher is the server's own watcher (live/INTAKE-DESIGN.md §2):
// on each scheduler tick it runs every due project's comment watch, claim
// markers, issue intake, project health and document notices as the
// project's system session, reading and writing Linear as the ClawBridge
// Linear app.
package watcher

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
)

// TickProjects bounds the projects one tick serves, and ProjectDeadline
// each project's share of a tick, so one slow project cannot starve the
// rest (IN-M1).
const (
	TickProjects    = 10
	ProjectDeadline = 20 * time.Second
)

// App is the Linear app connection the watcher uses. The server's app
// connection (internal/linearapp) is adapted to it.
type App interface {
	// Open returns a native provider over the app token; readonly selects
	// Linear's read-only MCP endpoint.
	Open(ctx context.Context, readonly bool) (linearboard.Provider, error)
	// GraphQL runs one read-only GraphQL query with the app token.
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
	// Identity is the app's Linear user.
	Identity(ctx context.Context) (AppIdentity, error)
	// SubscribeIssue adds one subscriber to one issue as the app, the owner
	// by email or anyone else by ID; it answers a class, never Linear's text
	// (linearapp.SubscribeIssue).
	SubscribeIssue(ctx context.Context, issue, email, user string) (string, error)
	// HasScope is whether the app's token in use was granted scope.
	HasScope(ctx context.Context, scope string) (bool, error)
}

// AppIdentity names the app's Linear user and organisation.
type AppIdentity struct{ AppUserID, LinearOrgID string }

// Store is what the watcher needs of the bus.
type Store interface {
	ExecuteSystem(ctx context.Context, projectID string, r bus.Request) (bus.Result, error)
	WatchProjects(ctx context.Context, limit int) ([]string, error)
	WatchTicked(ctx context.Context, projectID string, ok bool, failure string) error
	IntakeSystemState(ctx context.Context, projectID string) (bus.IntakeState, error)
	IntakeCursorSet(ctx context.Context, projectID, source, subject, polled string, signal bool) error
	IntakeEnqueueSystem(ctx context.Context, projectID string, ev bus.IntakeEvent) (string, error)
	IntakeAttribution(ctx context.Context, projectID, issue, kind, target string, from, to time.Time) (bus.AttributionCredit, error)
	ExecuteDispatcher(ctx context.Context, projectID, session string, r bus.Request) (bus.Result, error)
}

// Watcher serves ticks. One Watcher lives for its server process: it keeps
// one board Service per project, so connections and binding proofs are
// reused across ticks.
type Watcher struct {
	Store Store
	App   App
	// Ledger is the operation ledger of the app's marker writes: the hosted
	// server's shared ledger, so an uncertain write survives a restart and
	// is seen by every instance. Without it (a local test store) the
	// private ledger in LedgerDirectory is used.
	Ledger          func(context.Context) linearboard.OperationLedger
	LedgerDirectory string
	Now             func() time.Time
	// Budget is the Linear calls per hour each tick's and nudge's lease
	// allows (CLAWBRIDGE_WATCH_BUDGET); 0 takes the bus default, 200.
	Budget int

	mu       sync.Mutex
	services map[string]*linearboard.Service
	labels   map[string]labelCache
	// users caches each Linear user's eligibility for role trust (T2'
	// (b)): a read no older than userCheckTTL.
	users map[string]userCheck
}

type userCheck struct {
	eligible bool
	checked  time.Time
}

type labelCache struct {
	ids     []string
	checked time.Time
}

// ProjectTick is what one tick did for one project.
type ProjectTick struct {
	ProjectID string                  `json:"project_id"`
	Comments  *linearboard.WatchCycle `json:"comments,omitempty"`
	Intake    *IntakePoll             `json:"intake,omitempty"`
	Health    []HealthRun             `json:"health,omitempty"`
	Docs      *DocPoll                `json:"docs,omitempty"`
	Dispatch  *DispatchRun            `json:"dispatch,omitempty"`
	// Subscribers is the issue subscribers pass (ISSUE-SUBSCRIBERS-PLAN).
	Subscribers *SubscribePass `json:"subscribers,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// Tick serves up to TickProjects projects, the longest since their last
// tick first, each within ProjectDeadline.
func (w *Watcher) Tick(ctx context.Context) []ProjectTick {
	projects, err := w.Store.WatchProjects(ctx, TickProjects)
	if err != nil {
		return []ProjectTick{{Error: "projects_unavailable"}}
	}
	out := []ProjectTick{}
	// Health runs are capped per tick across all projects (HP-M3).
	healthLeft := HealthRunsPerTick
	for _, pid := range projects {
		pctx, cancel := context.WithTimeout(ctx, ProjectDeadline)
		tick := w.project(pctx, pid, healthLeft)
		cancel()
		logTick(pid, "tick", tick)
		// Nudged runs have their own allowance in the bus (R101-L2).
		for _, run := range tick.Health {
			if run.Nudge == "" {
				healthLeft--
			}
		}
		_ = w.Store.WatchTicked(ctx, pid, tick.Error == "", tick.Error)
		out = append(out, tick)
	}
	return out
}

func (w *Watcher) project(ctx context.Context, pid string, healthLeft int) ProjectTick {
	tick := ProjectTick{ProjectID: pid}
	svc := w.service(pid)
	scope := bus.Request{Session: bus.SystemSessionName, Args: map[string]any{}}
	err := svc.Watch(ctx, scope, linearboard.WatchOptions{
		Budget: w.Budget,
		Once:   true,
		Cycle: func(c linearboard.WatchCycle) error {
			tick.Comments = &c
			return nil
		},
		// Intake, then health, then documents run after the comment
		// watch and markers, within what the hour's budget leaves.
		Within: func(ctx context.Context, fence map[string]any) error {
			poll, err := w.intake(ctx, svc, scope, pid, fence)
			tick.Intake = poll
			if err != nil {
				return err
			}
			if tick.Subscribers, err = w.subscribers(ctx, svc, scope, pid, fence); err != nil {
				return err
			}
			if tick.Health, err = w.health(ctx, svc, scope, pid, fence, healthLeft, false); err != nil {
				return err
			}
			if tick.Docs, err = w.docs(ctx, svc, pid, fence); err != nil {
				return err
			}
			tick.Dispatch, err = w.dispatcher(ctx, pid, fence)
			return err
		},
	})
	if err != nil {
		tick.Error = failureCode(err)
	}
	return tick
}

// Markers applies one project's due claim markers under the project's
// lease and budget like a tick's: a nudge after a confirm or a release
// (live/STATUS-FLOW-DESIGN.md §8.3). The project flow checks those moves
// asked for run after them (§11.2), and nothing else: never the
// dispatcher (R106-M1). It doesn't count as the project's tick.
func (w *Watcher) Markers(ctx context.Context, pid string) ProjectTick {
	return w.nudged(ctx, pid, false)
}

// Fallback is Markers followed by the project's dispatcher: a nudge after
// a model case ended without a decision, so its rules fallback runs within
// about a minute (DISPATCHER-MODEL-API d5, D5-M3). Only this nudge runs the
// dispatcher, so the rules' fact-waits still count ticks, not marker
// nudges.
func (w *Watcher) Fallback(ctx context.Context, pid string) ProjectTick {
	return w.nudged(ctx, pid, true)
}

func (w *Watcher) nudged(ctx context.Context, pid string, dispatch bool) ProjectTick {
	ctx, cancel := context.WithTimeout(ctx, nudgeDeadline)
	defer cancel()
	tick := ProjectTick{ProjectID: pid}
	scope := bus.Request{Session: bus.SystemSessionName, Args: map[string]any{}}
	svc := w.service(pid)
	err := svc.Watch(ctx, scope, linearboard.WatchOptions{
		Budget:      w.Budget,
		Once:        true,
		MarkersOnly: true,
		// A failed nudge check never costs the pass its marker output
		// (R101-L3): the markers are done, and the daily run catches up.
		Within: func(ctx context.Context, fence map[string]any) error {
			var err error
			if tick.Health, err = w.health(ctx, svc, scope, pid, fence, 0, true); err != nil {
				tick.Error = failureCode(err)
			}
			if !dispatch {
				return nil
			}
			tick.Dispatch, err = w.dispatcher(ctx, pid, fence)
			return err
		},
		Cycle: func(c linearboard.WatchCycle) error {
			tick.Comments = &c
			return nil
		},
	})
	if err != nil {
		tick.Error = failureCode(err)
	}
	kind := "nudge"
	if dispatch {
		kind = "fallback"
	}
	logTick(pid, kind, tick)
	return tick
}

// nudgeDeadline is a nudge pass's deadline: a tick's per-project share,
// not the whole tick's. A variable only so tests can shrink it.
var nudgeDeadline = ProjectDeadline

// MarkersPending reports whether the marker pass stopped with rows left
// that another pass would take now: stopped by its row limit or its clock,
// not by the hour's budget or an error.
func (t ProjectTick) MarkersPending() bool {
	c := t.Comments
	if c == nil || c.MarkersLeft == 0 {
		return false
	}
	switch c.MarkersStopped {
	case "limit", "time", "deadline":
		return true
	}
	return false
}

// logf is where the watcher's outcome lines go; tests read them.
var logf = log.Printf

// logTick logs a tick's or nudge's marker pass, and a failure, as codes
// and counts only: never Linear or bus text, issue IDs or titles.
func logTick(pid, kind string, tick ProjectTick) {
	if c := tick.Comments; c != nil && (len(c.Markers) > 0 || c.MarkersLeft > 0 || c.MarkersStopped != "") {
		stopped := c.MarkersStopped
		if stopped == "" {
			stopped = "done"
		}
		logf("watcher: markers: project %s (%s): applied %d, left %d, stopped %s, charged %d", pid, kind, len(c.Markers), c.MarkersLeft, stopped, c.MarkersCharged)
	}
	if p := tick.Subscribers; p != nil && (p.Done+p.Skipped+p.Retry > 0 || p.Left > 0) {
		logf("watcher: subscribers: project %s (%s): done %d, skipped %d, retry %d, left %d", pid, kind, p.Done, p.Skipped, p.Retry, p.Left)
	}
	if tick.Error != "" {
		logf("watcher: %s: project %s failed: %s", kind, pid, tick.Error)
	}
}

// service is the project's board Service: the system session's executor,
// the app's connection, and the server's binding proof cache.
func (w *Watcher) service(pid string) *linearboard.Service {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.services == nil {
		w.services = map[string]*linearboard.Service{}
	}
	if svc, ok := w.services[pid]; ok {
		return svc
	}
	svc := linearboard.New(linearboard.Config{
		Authority:       "server-watcher",
		Executor:        systemExecutor{w.Store, pid},
		Connections:     appConnections{},
		ProfileID:       appProfile,
		LedgerDirectory: w.LedgerDirectory,
		Ledger:          w.Ledger,
		Open: func(ctx context.Context, _ *http.Client, readonly bool) (linearboard.Provider, error) {
			return w.App.Open(ctx, readonly)
		},
		Now:        w.Now,
		IssueState: linearboard.GraphQLIssueState(w.App.GraphQL),
	})
	svc.KeepProviderOpen(linearboard.WatchProviderTTL)
	svc.CacheReadBindingProof(linearboard.BindingProofTTL)
	w.services[pid] = svc
	return svc
}

// appProfile is the profile ID the board Service sees for the app.
const appProfile = "linear-app"

// systemExecutor runs a board Service's bus calls as the project's system
// session.
type systemExecutor struct {
	store Store
	pid   string
}

func (e systemExecutor) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	return e.store.ExecuteSystem(ctx, e.pid, r)
}

// appConnections presents the app as a write-capable profile; the app
// connection itself is opened through the Service's Open.
type appConnections struct{}

func (appConnections) Status(_ context.Context, id string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: id, Name: "ClawBridge Linear app", Access: linearconn.Write}, nil
}
func (appConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	return &http.Client{}, nil
}

// failureCode is a tick failure's code: never Linear or bus text.
func failureCode(err error) string {
	var coded interface{ Code() string }
	switch {
	case errors.As(err, &coded):
		return coded.Code()
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return "failed"
}
