// Package watcher is the server's own watcher (live/INTAKE-DESIGN.md §2):
// on each scheduler tick it runs every due project's comment watch, claim
// markers, issue intake, project health and document notices as the
// project's system session, reading and writing Linear as the ClawBridge
// Linear app.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
)

// TickProjects bounds the projects one tick serves, and ProjectDeadline
// each project's share of a tick, so one slow project cannot starve the
// rest (IN-M1). A larger binding gets a larger share (projectShare).
const (
	TickProjects    = 10
	ProjectDeadline = 20 * time.Second
)

// A project's share grows by shareStep for each bound team or Linear
// project past shareBaseUnits, up to maxProjectShare: a binding of many
// teams and projects reads more each pass (its issues, comments, intake
// and health per team and project) than a small one, and a flat share
// cut its later steps every tick. A project the tick has less than
// minProjectShare left for waits for the next tick, first in line, rather
// than starting a pass the tick's own deadline would cut.
var (
	shareBaseUnits  = 4
	shareStep       = 2 * time.Second
	maxProjectShare = 40 * time.Second
	minProjectShare = ProjectDeadline
	// markerRoom is the part of a project's share the steps after the
	// comment watch leave for the claim markers: what a marker pass needs
	// to apply a row (linearboard.MarkerPassRoom, 9 s).
	markerRoom = linearboard.MarkerPassRoom()
	// stepsRoom is what the comment poll leaves the steps on top of the
	// markers' room: enough to start intake, and a second of slack. So in
	// any share a pass starts with (at least ProjectDeadline, 20 s): the
	// comment poll stops with markerRoom+stepsRoom (15 s) left, so it
	// polls for at least 5 s; the steps run until markerRoom is left, so
	// they get at least stepsRoom (6 s, intake needs intakeRoom, 5 s); the
	// markers get at least markerRoom (9 s).
	stepsRoom = intakeRoom + time.Second
)

// watcherProofTTL is how long the watcher's read paths reuse a verified
// binding: across ticks (at most 30 minutes apart), not only within one.
// Verifying reads every team and its projects, a dozen calls or more on a
// large binding, which a 60 s reuse paid again every tick. The proof is
// keyed by the binding's revision and contents, so a relink misses; any
// failed verify drops it; writes never use it.
const watcherProofTTL = 20 * time.Minute

// slowPassMS is a pass's time from which its steps' times are logged.
const slowPassMS = 5000

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
	IntakeCursorSoon(ctx context.Context, projectID, source, subject string) error
	IntakeEnqueueSystem(ctx context.Context, projectID string, ev bus.IntakeEvent) (string, error)
	IntakeEnqueueNew(ctx context.Context, projectID string, ev bus.IntakeEvent) (string, error)
	IntakeAttribution(ctx context.Context, projectID, issue, kind, target string, from, to time.Time) (bus.AttributionCredit, error)
	ExecuteDispatcher(ctx context.Context, projectID, session string, r bus.Request) (bus.Result, error)
	// WebhookMiss counts an event a poll found that the team's webhook
	// should have rung for (FR14 §6 P5).
	WebhookMiss(ctx context.Context, team string, linearAt, foundAt time.Time) (bool, error)
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
	// Budget is an opt-in cap on the Linear calls per hour each tick's and
	// nudge's lease allows (CLAWBRIDGE_LINEAR_BUDGET); 0 is no cap, the
	// default: Linear's own rate limit, through the pacer, is the brake.
	Budget int

	mu       sync.Mutex
	services map[string]*linearboard.Service
	labels   map[string]labelCache
	// users caches each Linear user's eligibility for role trust (T2'
	// (b)): a read no older than userCheckTTL.
	users map[string]userCheck
	// budgets are each project's hour of Linear calls, as this process's
	// passes saw it (noteBudget).
	budgets map[string]*budgetHour
	// docsByProvider: Linear refused the app's document list once, so the
	// document watch lists through the provider for the rest of the process.
	docsByProvider bool
	// docsFirst: a cut-off page showed the app's document list is oldest
	// first, so its oldest end is read with first, not last.
	docsFirst bool
	// skipLogged is when each intake skip was last logged, so one stuck
	// issue is logged once an hour (see skipped).
	skipLogged map[string]time.Time
	// noLabelLogged is each project's teams with no needs-agent label as
	// the last summary line named them.
	noLabelLogged map[string]string
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
	// ShareMS is the time the tick gave this project, in milliseconds
	// (projectShare, or what the tick had left); 0 for a nudge.
	ShareMS int64  `json:"share_ms,omitempty"`
	Error   string `json:"error,omitempty"`
	// TookMS is each step's time in milliseconds (comments, then the
	// steps after it, then markers), so a slow project shows where its
	// share went.
	TookMS map[string]int64 `json:"took_ms,omitempty"`
}

// took records step's time since start, and returns now.
func (t *ProjectTick) took(step string, start time.Time) time.Time {
	now := time.Now()
	if t.TookMS == nil {
		t.TookMS = map[string]int64{}
	}
	t.TookMS[step] = now.Sub(start).Milliseconds()
	return now
}

// Tick serves up to TickProjects projects, the longest since their last
// tick first, each within its share (projectShare) of what the tick has
// left. The projects it has too little left for are not served: they stay
// first for the next tick.
func (w *Watcher) Tick(ctx context.Context) []ProjectTick {
	projects, err := w.Store.WatchProjects(ctx, TickProjects)
	if err != nil {
		return []ProjectTick{{Error: "projects_unavailable"}}
	}
	out := []ProjectTick{}
	// Health runs are capped per tick across all projects (HP-M3).
	healthLeft := HealthRunsPerTick
	for i, pid := range projects {
		share := w.projectShare(ctx, pid)
		if deadline, ok := ctx.Deadline(); ok {
			left := time.Until(deadline)
			if left < minProjectShare {
				logf("watcher: tick: %d project(s) wait for the next tick: %dms left, under %dms", len(projects)-i, left.Milliseconds(), minProjectShare.Milliseconds())
				break
			}
			share = min(share, left)
		}
		pctx, cancel := context.WithTimeout(ctx, share)
		tick := w.project(pctx, pid, healthLeft)
		cancel()
		tick.ShareMS = share.Milliseconds()
		logTick(pid, "tick", tick)
		w.noteBudget(pid, tick)
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

// projectShare is pid's share of a tick: ProjectDeadline, plus shareStep
// for each bound team or Linear project past shareBaseUnits, up to
// maxProjectShare. The binding is the stored one, read from the bus (no
// Linear call); a project whose binding can't be read gets ProjectDeadline.
func (w *Watcher) projectShare(ctx context.Context, pid string) time.Duration {
	scope := bus.Request{Session: bus.SystemSessionName, Args: map[string]any{}}
	b, err := w.service(pid).BindingV2(ctx, scope)
	if err != nil {
		return ProjectDeadline
	}
	return shareFor(len(b.LinearTeamIDs) + len(b.LinearProjectIDs))
}

// shareFor is the share of a binding of units teams and projects.
func shareFor(units int) time.Duration {
	share := ProjectDeadline
	if units > shareBaseUnits {
		share += time.Duration(units-shareBaseUnits) * shareStep
	}
	return min(share, maxProjectShare)
}

func (w *Watcher) project(ctx context.Context, pid string, healthLeft int) ProjectTick {
	tick := ProjectTick{ProjectID: pid}
	svc := w.service(pid)
	scope := bus.Request{Session: bus.SystemSessionName, Args: map[string]any{}}
	mark := time.Now()
	err := svc.Watch(ctx, scope, linearboard.WatchOptions{
		Budget:    w.Budget,
		Once:      true,
		StepsRoom: stepsRoom,
		Cycle: func(c linearboard.WatchCycle) error {
			tick.Comments = &c
			return nil
		},
		// Intake, then health, then documents run after the comment
		// watch and markers, within what the hour's budget leaves.
		// Each step stands alone: one that fails is recorded and the next
		// still runs, while the share lasts. Only a lost lease ends the
		// pass, since every later step would be refused.
		Within: func(ctx context.Context, fence map[string]any) error {
			mark = tick.took("comments", mark)
			// The steps leave markerRoom of the share to the markers that
			// follow them.
			if deadline, ok := ctx.Deadline(); ok {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline.Add(-markerRoom))
				defer cancel()
			}
			steps := []struct {
				name string
				run  func() error
			}{
				{"intake", func() (err error) { tick.Intake, err = w.intake(ctx, svc, scope, pid, fence); return err }},
				{"subscribers", func() (err error) { tick.Subscribers, err = w.subscribers(ctx, svc, scope, pid, fence); return err }},
				{"health", func() (err error) {
					tick.Health, err = w.health(ctx, svc, scope, pid, fence, healthLeft, false)
					return err
				}},
				{"docs", func() (err error) { tick.Docs, err = w.docs(ctx, svc, pid, fence); return err }},
				{"dispatcher", func() (err error) { tick.Dispatch, err = w.dispatcher(ctx, pid, fence); return err }},
			}
			for _, step := range steps {
				if ctx.Err() != nil {
					tick.fail(ctx, ctx.Err())
					return nil
				}
				err := step.run()
				mark = tick.took(step.name, mark)
				if err != nil {
					if lostLease(err) {
						return err
					}
					tick.fail(ctx, err)
				}
			}
			return nil
		},
	})
	tick.took("markers", mark)
	if err != nil {
		tick.fail(ctx, err)
	}
	return tick
}

// fail records a step's failure: the first one stands as the tick's error.
// A failure once the share has run out is "deadline", however the step
// reported it.
func (t *ProjectTick) fail(ctx context.Context, err error) {
	if t.Error != "" {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Error = "deadline"
		return
	}
	t.Error = failureCode(err)
}

// lostLease: the watch lease is gone (a conflict), so the pass must stop.
func lostLease(err error) bool {
	var coded interface{ Code() string }
	return errors.As(err, &coded) && coded.Code() == "conflict"
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
				tick.fail(ctx, err)
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
		tick.fail(ctx, err)
	}
	kind := "nudge"
	if dispatch {
		kind = "fallback"
	}
	logTick(pid, kind, tick)
	w.noteBudget(pid, tick)
	return tick
}

// Webhook is a webhook's pass of one project (FR14): its comment watch,
// then its intake, under the project's lease and budget like a tick's,
// soon after a verified delivery marked its cursors due. It runs no
// markers, subscribers, health, documents or dispatcher, and doesn't count
// as the project's tick. Only the cursors the bus says are due are read.
func (w *Watcher) Webhook(ctx context.Context, pid string) error {
	tick := w.webhook(ctx, pid)
	if tick.Error != "" {
		return errors.New(tick.Error)
	}
	return nil
}

func (w *Watcher) webhook(ctx context.Context, pid string) ProjectTick {
	ctx, cancel := context.WithTimeout(ctx, nudgeDeadline)
	defer cancel()
	tick := ProjectTick{ProjectID: pid}
	scope := bus.Request{Session: bus.SystemSessionName, Args: map[string]any{}}
	svc := w.service(pid)
	mark := time.Now()
	err := svc.Watch(ctx, scope, linearboard.WatchOptions{
		Budget: w.Budget,
		Once:   true,
		// The comment poll leaves intake its room, as a tick's does.
		StepsRoom: stepsRoom,
		NoMarkers: true,
		Within: func(ctx context.Context, fence map[string]any) error {
			mark = tick.took("comments", mark)
			var err error
			tick.Intake, err = w.intake(ctx, svc, scope, pid, fence)
			tick.took("intake", mark)
			if err != nil {
				if lostLease(err) {
					return err
				}
				tick.fail(ctx, err)
			}
			return nil
		},
		Cycle: func(c linearboard.WatchCycle) error {
			tick.Comments = &c
			return nil
		},
	})
	if err != nil {
		tick.fail(ctx, err)
	}
	logTick(pid, "webhook", tick)
	w.noteBudget(pid, tick)
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
	// A slow pass says where its share went, by step.
	parts, total := []string{}, int64(0)
	for _, step := range []string{"comments", "intake", "subscribers", "health", "docs", "dispatcher", "markers"} {
		if ms, ok := tick.TookMS[step]; ok {
			part := fmt.Sprintf("%s %dms", step, ms)
			if step == "intake" && tick.Intake != nil && len(tick.Intake.TookMS) > 0 {
				split := []string{}
				for _, sig := range []string{"S1", "S2", "S3"} {
					if ms, ok := tick.Intake.TookMS[sig]; ok {
						split = append(split, fmt.Sprintf("%s %dms", sig, ms))
					}
				}
				part += " (" + strings.Join(split, ", ") + ")"
			}
			parts = append(parts, part)
			total += ms
		}
	}
	if total >= slowPassMS {
		of := ""
		if tick.ShareMS > 0 {
			of = fmt.Sprintf(" of its %dms share", tick.ShareMS)
		}
		logf("watcher: %s: project %s took %dms%s: %s", kind, pid, total, of, strings.Join(parts, ", "))
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
		ServerCharged:   true,
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
	svc.CacheReadBindingProof(watcherProofTTL)
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

// budgetHour is one project's comment-watch budget in one UTC hour, as the
// passes of this process saw it.
type budgetHour struct {
	hour                           string // 2006-01-02T15
	used, limit, refused, deferred int
}

// noteBudget records a pass's use of the hour's budget, after logging each
// project whose hour has ended: a log line per project and hour that needs
// no owner sign-in to read, and names no issue.
func (w *Watcher) noteBudget(pid string, tick ProjectTick) {
	hour := w.now().UTC().Format("2006-01-02T15")
	w.mu.Lock()
	ended := w.rollBudgets(hour)
	defer func() {
		// The ended hours' top kinds are read from the store, so they
		// are logged without the lock.
		w.mu.Unlock()
		w.logBudgets(ended)
	}()
	c := tick.Comments
	if c == nil {
		return
	}
	if w.budgets == nil {
		w.budgets = map[string]*budgetHour{}
	}
	b := w.budgets[pid]
	if b == nil {
		b = &budgetHour{hour: hour}
		w.budgets[pid] = b
	}
	// The hour's count only grows, and a markers-only nudge reads none, so
	// the most any pass read holds; the cap is the last one read (0: none).
	if c.BudgetUsed > b.used {
		b.used = c.BudgetUsed
	}
	if c.BudgetUsed > 0 || c.BudgetLimit > 0 {
		b.limit = c.BudgetLimit
	}
	// A pass the budget stopped, in its comment watch or its markers.
	if c.BudgetExhausted || c.MarkersStopped == "budget" {
		b.refused++
	}
	// What one pass left for later; the same issue left by several passes
	// counts once, so the hour keeps the most any pass left.
	deferred := c.Deferred
	if c.MarkersStopped == "budget" {
		deferred += c.MarkersLeft
	}
	b.deferred = max(b.deferred, deferred)
}

// endedBudget is a project's hour that has ended, to be logged.
type endedBudget struct {
	pid string
	budgetHour
}

// rollBudgets forgets every project's hour before hour and returns them,
// in project order. The caller holds w.mu.
func (w *Watcher) rollBudgets(hour string) []endedBudget {
	var ended []endedBudget
	for pid, b := range w.budgets {
		if b.hour != hour {
			ended = append(ended, endedBudget{pid: pid, budgetHour: *b})
			delete(w.budgets, pid)
		}
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].pid < ended[j].pid })
	return ended
}

// budgetKindsStore is the store's calls by kind in an hour, when it keeps
// them (the bus's, schema 52).
type budgetKindsStore interface {
	LinearBudgetKindsAt(ctx context.Context, pid, hour string) ([]bus.LinearBudgetKind, error)
}

var _ budgetKindsStore = (*bus.Store)(nil)

// budgetKindsTimeout bounds the reads of the ended hours' kinds, all of
// them together.
const budgetKindsTimeout = 5 * time.Second

// logBudgets logs each ended hour, with its top three kinds when the store
// can say; codes and counts only. The caller doesn't hold w.mu.
func (w *Watcher) logBudgets(ended []endedBudget) {
	kinds, _ := w.Store.(budgetKindsStore)
	if len(ended) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budgetKindsTimeout)
	defer cancel()
	for _, b := range ended {
		limit := "none"
		if b.limit > 0 {
			limit = strconv.Itoa(b.limit)
		}
		top := ""
		if kinds != nil {
			if got, err := kinds.LinearBudgetKindsAt(ctx, b.pid, b.hour); err == nil {
				top = topBudgetKinds(got, 3)
			}
		}
		logf("watcher: budget: project %s hour %s: used %d of %s, refused %d passes, deferred %d%s", b.pid, b.hour[len(b.hour)-2:], b.used, limit, b.refused, b.deferred, top)
	}
}

// topBudgetKinds is ", top: KIND N, KIND N, …" for the first n kinds with
// calls (they come the most first), or "" when there are none.
func topBudgetKinds(kinds []bus.LinearBudgetKind, n int) string {
	var parts []string
	for _, k := range kinds {
		if len(parts) == n {
			break
		}
		if k.Calls > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k.Kind, k.Calls))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return ", top: " + strings.Join(parts, ", ")
}
