package linearboard

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// WatchInterval is how often the comment watcher wakes; the bus decides
// which issues are due, so most cycles read nothing.
const WatchInterval = time.Minute

// WatchProviderTTL is how long the watcher keeps one native connection: its
// connects dominate a quiet watch's Linear calls.
const WatchProviderTTL = 30 * time.Minute

// watchPollTimeout bounds one issue's poll, as a CLI read is bounded.
const watchPollTimeout = 45 * time.Second

// watchRenewEvery renews the lease within a long cycle, well inside its
// 3-minute TTL.
const watchRenewEvery = time.Minute

// watchMaxReported is the most comments one report carries (the bus limit).
const watchMaxReported = 100

// WatchOptions configure one comment watcher process.
type WatchOptions struct {
	// Budget is the Linear calls per hour the lease allows; 0 takes the
	// bus default.
	Budget int
	// Once runs one cycle and releases the lease.
	Once bool
	// Interval is the time between cycles; WatchInterval when zero.
	Interval time.Duration
	// Cycle receives each cycle's outcome.
	Cycle func(WatchCycle) error
	// Within runs after each cycle while the lease is still held, with
	// its fence, so other sources (intake) share the lease and budget.
	Within func(ctx context.Context, fence map[string]any) error
	// MarkersOnly applies the due claim markers and nothing else: no
	// comment sweep or poll (a nudge, §8.3).
	MarkersOnly bool
	// WithinOnly runs Within alone under the lease: no comment sweep,
	// poll or markers (an operator's forced health run).
	WithinOnly bool
	// StepsRoom is the time Within needs between the comment poll and the
	// markers: a pass with a deadline stops polling comments with
	// CommentStop(StepsRoom) left.
	StepsRoom time.Duration
	// NoMarkers runs the comment sweep and poll, then Within, but applies
	// no claim markers (a webhook pass, FR14): the ticks and nudges do.
	NoMarkers bool
}

// WatchCycle is what one watch cycle did. Held names the issues whose poll
// could not decide, and why; their cursors stay, so nothing is skipped.
type WatchCycle struct {
	Polled          int               `json:"polled"`
	Notified        []string          `json:"notified"`
	Held            map[string]string `json:"held,omitempty"`
	Stopped         []string          `json:"stopped,omitempty"`
	BudgetUsed      int               `json:"budget_used"`
	BudgetLimit     int               `json:"budget_limit"`
	BudgetExhausted bool              `json:"budget_exhausted,omitempty"`
	// CommentsStopped is "deadline" when the poll stopped to leave the
	// pass's later steps their time; the issues not polled stay due, so the
	// next pass goes on with them.
	CommentsStopped string `json:"comments_stopped,omitempty"`
	// Deferred counts the due issues the hour's budget left for later
	// (0 from a server before v0.3.12).
	Deferred int `json:"deferred,omitempty"`
	// Markers are the claim markers this cycle applied, first.
	Markers []MarkerApply `json:"markers,omitempty"`
	// MarkersReadOnly is set when the watcher's Linear profile cannot
	// write, so claim markers wait for a client that can.
	MarkersReadOnly bool `json:"markers_read_only,omitempty"`
	// MarkersLeft counts the due rows this pass didn't apply, and
	// MarkersStopped says why: limit, budget, time, deadline or error
	// (empty when it applied every due row). MarkersCharged is the pass's
	// net charge, after reconciling and refunding.
	MarkersLeft    int    `json:"markers_left,omitempty"`
	MarkersStopped string `json:"markers_stopped,omitempty"`
	MarkersCharged int    `json:"markers_charged,omitempty"`
}

// watchLease is the fence every watch operation carries.
type watchLease struct {
	token      string
	generation any
	renewed    time.Time
	// hour and spent are this process's own count of Linear calls in the
	// current hour: a second ceiling, should a report be lost.
	hour  string
	spent int
}

// overBudget charges calls to the process's hour and reports whether the
// hour's limit is spent.
func (l *watchLease) overBudget(now time.Time, calls, limit int) bool {
	if hour := now.UTC().Format("2006-01-02T15"); hour != l.hour {
		l.hour, l.spent = hour, 0
	}
	l.spent += calls
	return limit > 0 && l.spent >= limit
}

func (l watchLease) fence(args map[string]any) map[string]any {
	args["lease_token"], args["generation"] = l.token, l.generation
	return args
}

// outOfScopeError marks an issue the binding does not cover.
type outOfScopeError string

func (e outOfScopeError) Error() string { return string(e) }

// IsOutOfScope reports whether err refused an issue the binding does not
// cover.
func IsOutOfScope(err error) bool {
	var outside outOfScopeError
	return errors.As(err, &outside)
}

// Watch runs the project's comment watcher as the caller's session: it holds
// the bus lease, polls the issues the bus says are due, and reports each
// poll; the bus composes and delivers the notices. It returns when ctx ends,
// after one cycle with Once, or when the lease is lost.
func (s *Service) Watch(ctx context.Context, scope bus.Request, opts WatchOptions) error {
	if opts.Interval <= 0 {
		opts.Interval = WatchInterval
	}
	args := map[string]any{"action": "acquire"}
	if opts.Budget > 0 {
		args["budget"] = opts.Budget
	}
	got, err := s.commentLedger(ctx, scope, "comment_watch_lease", args)
	if err != nil {
		return err
	}
	token, _ := got["lease_token"].(string)
	lease := &watchLease{token: token, generation: got["generation"], renewed: s.cfg.Now()}
	defer func() {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = s.commentLedger(release, scope, "comment_watch_lease", lease.fence(map[string]any{"action": "release"}))
	}()
	if !opts.MarkersOnly && !opts.WithinOnly {
		s.SweepComments(ctx, scope)
	}
	calls := s.countCalls()
	for {
		if err = s.renewWatch(ctx, scope, lease, true); err != nil {
			return err
		}
		var cycle WatchCycle
		credit := watchConnectCalls
		if !opts.WithinOnly {
			if cycle, err = s.watchCycle(ctx, scope, lease, calls, opts.MarkersOnly, opts.StepsRoom, &credit); err != nil {
				return err
			}
		}
		if opts.Within != nil {
			if err = s.renewWatch(ctx, scope, lease, false); err != nil {
				return err
			}
			if err = opts.Within(ctx, lease.fence(map[string]any{})); err != nil {
				return err
			}
		}
		if !opts.MarkersOnly && !opts.WithinOnly && !opts.NoMarkers {
			// The hotfix (live/MARKER-PASS-HOTFIX.md): a tick's markers come
			// last, in the time the other steps leave, so they can never
			// starve the comment watch; nudge passes drain the rest in
			// their own budget.
			if err = s.applyDueMarkers(ctx, scope, lease, &cycle, calls, &credit); err != nil {
				return err
			}
		}
		if opts.Cycle != nil {
			if err = opts.Cycle(cycle); err != nil {
				return err
			}
		}
		if opts.Once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.Interval):
		}
	}
}

// renewWatch renews the lease, every cycle or when a long cycle has held it
// for watchRenewEvery. A lost lease ends the watcher.
func (s *Service) renewWatch(ctx context.Context, scope bus.Request, lease *watchLease, always bool) error {
	now := s.cfg.Now()
	if !always && now.Sub(lease.renewed) < watchRenewEvery {
		return nil
	}
	if _, err := s.commentLedger(ctx, scope, "comment_watch_lease", lease.fence(map[string]any{"action": "renew"})); err != nil {
		return err
	}
	lease.renewed = now
	return nil
}

// watchConnectCalls is what opening a native connection costs: initialize
// and tools/list. The bus charges it once per cycle up front.
const watchConnectCalls = 2

// linearCalls counts what this process has sent to Linear: tool calls, and
// connections opened at watchConnectCalls each. known is false when a
// provider cannot count its calls.
type linearCalls struct {
	tools, opens uint64
	known        bool
}

// countCalls makes every provider this Service opens from now on count
// toward the returned totals.
func (s *Service) countCalls() func() linearCalls {
	open := s.cfg.Open
	opened := []Provider{}
	s.cfg.Open = func(ctx context.Context, h *http.Client, readonly bool) (Provider, error) {
		p, err := open(ctx, h, readonly)
		if err == nil {
			opened = append(opened, p)
		}
		return p, err
	}
	return func() linearCalls {
		n := linearCalls{opens: uint64(len(opened)), known: true}
		for _, p := range opened {
			sent, ok := sentCount(p)
			n.tools += sent
			n.known = n.known && ok
		}
		return n
	}
}

// pollCost is what one poll spent, for its report: every tool call and
// connection between before and after. The cycle's up-front connection
// charge (credit) pays for the first connection it opened.
func pollCost(before, after linearCalls, credit *int) int {
	if !after.known {
		return 1
	}
	spent := int(after.tools-before.tools) + watchConnectCalls*int(after.opens-before.opens)
	if after.opens > before.opens {
		paid := min(*credit, spent)
		spent -= paid
		*credit -= paid
	}
	return spent
}

func (s *Service) watchCycle(ctx context.Context, scope bus.Request, lease *watchLease, calls func() linearCalls, markersOnly bool, stepsRoom time.Duration, credit *int) (WatchCycle, error) {
	cycle := WatchCycle{Notified: []string{}}
	if markersOnly {
		return cycle, s.applyDueMarkers(ctx, scope, lease, &cycle, calls, credit)
	}
	due, err := s.commentLedger(ctx, scope, "comment_watch_due", lease.fence(map[string]any{}))
	if err != nil {
		return cycle, err
	}
	cycle.BudgetUsed, cycle.BudgetLimit = number(due["budget_used"]), number(due["budget_limit"])
	cycle.BudgetExhausted, _ = due["budget_exhausted"].(bool)
	cycle.Deferred = number(due["deferred"])
	issues, _ := due["issues"].([]any)
	for _, raw := range issues {
		if lease.overBudget(s.cfg.Now(), 0, cycle.BudgetLimit) {
			cycle.BudgetExhausted = true
			break
		}
		// A pass with a deadline (a server tick's per-project share) keeps
		// room for intake, health and the markers after it: a large budget
		// must not let the comment poll take the whole share.
		if tooLate(ctx, CommentStop(stepsRoom)) {
			cycle.CommentsStopped = "deadline"
			break
		}
		item, _ := raw.(map[string]any)
		issue, _ := item["issue_uuid"].(string)
		since, _ := item["since"].(string)
		if err = s.renewWatch(ctx, scope, lease, false); err != nil {
			return cycle, err
		}
		before := calls()
		report := s.pollIssue(ctx, scope, lease, issue, since)
		spent := pollCost(before, calls(), credit)
		report["calls"] = min(spent, 1000)
		lease.overBudget(s.cfg.Now(), spent, cycle.BudgetLimit)
		// A poll the pass's deadline cut still reports, on a short live
		// context: its calls are charged and the issue stays due, so the
		// next pass doesn't find the same budget and redo the same work.
		rctx, cancel := reportContext(ctx)
		out, err := s.commentLedger(rctx, scope, "comment_watch_report", lease.fence(report))
		if err != nil && report["team_id"] != nil && strings.Contains(err.Error(), `unsupported argument "team_id"`) {
			// A server older than webhook misses takes no team.
			delete(report, "team_id")
			out, err = s.commentLedger(rctx, scope, "comment_watch_report", lease.fence(report))
		}
		cancel()
		if err != nil {
			var coded interface{ Code() string }
			if errors.As(err, &coded) && coded.Code() == "conflict" {
				return cycle, err
			}
			cycle.hold(issue, "report_failed")
			continue
		}
		cycle.Polled++
		if used, ok := out["budget_used"]; ok {
			cycle.BudgetUsed = number(used)
		}
		if notified, ok := out["notified"].([]any); ok {
			for _, id := range notified {
				if s, ok := id.(string); ok {
					cycle.Notified = append(cycle.Notified, s)
				}
			}
		} else if notified, ok := out["notified"].([]string); ok {
			cycle.Notified = append(cycle.Notified, notified...)
		}
		if stopped, _ := out["stopped"].(bool); stopped {
			cycle.Stopped = append(cycle.Stopped, issue)
		} else if failure, _ := report["error"].(string); failure != "" {
			cycle.hold(issue, failure)
		} else if failure, _ := out["error"].(string); failure != "" {
			cycle.hold(issue, failure)
		}
	}
	return cycle, nil
}

// applyDueMarkers applies the claim markers the bus says are due, under
// the lease; markers come before comment polls.
// The marker pass's bounds (the hotfix, live/MARKER-PASS-HOTFIX.md). It
// runs last in a tick, and alone in a nudge. It takes at most
// markerPassRows rows; it starts none after markerPassTime, nor with less
// than markerRowRoom left; and it runs under a context that ends
// markerReserve before its caller's deadline, so a row in flight is cut
// there and the lease's release and the tick's record keep that time. In a
// nudge's own 20 s: 8 s + one slow row (7 s seen live, 9 s at most) + 3 s
// = 20 s. A row cut short settles uncertain and stays due.
const markerPassRows = 3

// MarkerPassRoom is the time a marker pass needs left to apply a row: its
// own reserve, and a row's room. A caller that runs work before the markers
// leaves them at least this much.
func MarkerPassRoom() time.Duration { return markerReserve + markerRowRoom }

// CommentStop is the time a pass with a deadline keeps after its comment
// poll. With stepsRoom (the server's intake and the steps after it) it is
// the markers' room plus stepsRoom, and never less than commentPollRoom;
// without, commentPollRoom.
func CommentStop(stepsRoom time.Duration) time.Duration {
	if stepsRoom <= 0 {
		return commentPollRoom
	}
	return max(commentPollRoom, MarkerPassRoom()+stepsRoom)
}

// The pass's clock bounds are variables only so tests can shrink them.
var (
	markerPassTime      = 8 * time.Second
	markerRowRoom       = 6 * time.Second
	markerReserve       = 3 * time.Second
	markerSettleTimeout = 2 * time.Second
	// commentPollRoom is the time a pass with a deadline keeps after its
	// comment poll: no new issue is polled with less left.
	commentPollRoom = 10 * time.Second
)

// reportContext is ctx, or, once ctx has ended, a short live context for
// recording what was already spent.
func reportContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), markerSettleTimeout)
}

// tooLate reports whether ctx ends within room: no time to start a row.
func tooLate(ctx context.Context, room time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < room
}

func isConflict(err error) bool { return hasCode(err, "conflict") }

func hasCode(err error, code string) bool {
	var coded interface{ Code() string }
	return errors.As(err, &coded) && coded.Code() == code
}

// applyDueMarkers is the marker pass: a few due rows, under one held
// operation (one connection, one binding proof), each reconciled with what
// it spent. Only a lost lease fails the cycle: a pass that runs out of time
// or meets an error stops, refunds the rows it didn't start, and says why.
func (s *Service) applyDueMarkers(ctx context.Context, scope bus.Request, lease *watchLease, cycle *WatchCycle, calls func() linearCalls, credit *int) error {
	if s.cfg.Connections == nil {
		return nil
	}
	if profile, err := s.cfg.Connections.Status(ctx, s.cfg.ProfileID); err != nil || profile.Access != linearconn.Write {
		cycle.MarkersReadOnly = true
		return nil
	}
	due, err := s.commentLedger(ctx, scope, "marker_due", lease.fence(map[string]any{"limit": markerPassRows}))
	if hasCode(err, "invalid") {
		// A server older than limit (v0.3.4) refuses the arg: ask without
		// it, and give back below the rows past a pass's.
		due, err = s.commentLedger(ctx, scope, "marker_due", lease.fence(map[string]any{}))
	}
	if err != nil {
		if isConflict(err) {
			return err
		}
		cycle.MarkersStopped = "error"
		return nil
	}
	markers, _ := due["markers"].([]any)
	cycle.MarkersLeft = number(due["left"])
	cycle.MarkersStopped, _ = due["stopped"].(string)
	if len(markers) == 0 {
		return nil
	}
	pass := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		pass, cancel = context.WithDeadline(ctx, deadline.Add(-markerReserve))
		defer cancel()
	}
	held, release := s.hold()
	defer release()
	start := s.cfg.Now()
	// A budget stop stands: another pass this hour would find no budget,
	// so it must not read as rows a nudge can take now.
	stopped := func(why string) {
		if cycle.MarkersStopped != "budget" {
			cycle.MarkersStopped = why
		}
	}
	stop := func(i int, why string) {
		// The rows not started give their charge back now.
		for _, raw := range markers[i:] {
			m, _ := raw.(map[string]any)
			refund := map[string]any{"assignment_id": m["assignment_id"], "phase": m["phase"], "calls": 0, "charged": m["charged"]}
			_, _ = s.commentLedger(ctx, scope, "marker_spent", lease.fence(refund))
		}
		cycle.MarkersLeft += len(markers) - i
		stopped(why)
	}
	if len(markers) > markerPassRows {
		stop(markerPassRows, "limit")
		markers = markers[:markerPassRows]
	}
	for i, raw := range markers {
		switch {
		case i > 0 && s.cfg.Now().Sub(start) >= markerPassTime:
			stop(i, "time")
			return nil
		case pass.Err() != nil || tooLate(pass, markerRowRoom):
			stop(i, "deadline")
			return nil
		}
		m, _ := raw.(map[string]any)
		assignment, _ := m["assignment_id"].(string)
		phase, _ := m["phase"].(string)
		if err = s.renewWatch(ctx, scope, lease, false); err != nil {
			if isConflict(err) {
				return err
			}
			stop(i, "error")
			return nil
		}
		before, reads := calls(), s.stateReads.Load()
		cycle.Markers = append(cycle.Markers, held.ApplyMarker(pass, scope, assignment, phase, lease.fence(map[string]any{})))
		spent, err := s.markerSpent(ctx, scope, lease, m, before, calls(), s.stateReads.Load()-reads, credit)
		cycle.MarkersCharged += spent
		if err != nil {
			return err
		}
	}
	if pass.Err() != nil {
		// The last row was cut.
		stopped("deadline")
	}
	return nil
}

// markerSpent reconciles a row's up-front charge with what it spent: every
// counted tool call and connection, and the authoritative state reads. A
// provider that cannot count reports nothing, so its charge stands. Only a
// lost lease stops the cycle; an authority without marker_spent (older,
// or a member's client watcher) keeps the charge.
// It returns the row's net charge: what it spent once reconciled, or its
// up-front charge when it can't be.
func (s *Service) markerSpent(ctx context.Context, scope bus.Request, lease *watchLease, row map[string]any, before, after linearCalls, reads int64, credit *int) (int, error) {
	charged, ok := row["charged"]
	if !ok || !after.known {
		return number(charged), nil
	}
	spent := min(pollCost(before, after, credit)+int(reads), 1000)
	report := map[string]any{"assignment_id": row["assignment_id"], "phase": row["phase"], "calls": spent, "charged": charged}
	if _, err := s.commentLedger(ctx, scope, "marker_spent", lease.fence(report)); err != nil {
		if isConflict(err) {
			return 0, err
		}
		return number(charged), nil
	}
	return spent, nil
}

func (c *WatchCycle) hold(issue, reason string) {
	if c.Held == nil {
		c.Held = map[string]string{}
	}
	c.Held[issue] = reason
}

// pollIssue reads one issue's comments since the bus's cursor and returns
// the report: the comments that may be news, or why the poll cannot decide.
// Comments ClawBridge posted and comments by agents are never reported.
func (s *Service) pollIssue(ctx context.Context, scope bus.Request, lease *watchLease, issue, since string) map[string]any {
	report := map[string]any{"issue_uuid": issue}
	ctx, cancel := context.WithTimeout(ctx, watchPollTimeout)
	defer cancel()
	read, err := s.IssueV2WithComments(ctx, scope, issue, CommentsInputV2{Since: since})
	switch {
	case err != nil:
		report["error"] = watchFailure(err)
	case read.CommentsOwnership != "available":
		report["error"] = "ownership_unavailable"
	case read.CommentsYoungPending:
		report["error"] = "young_pending"
	case read.Truncated && !read.StoppedEarly:
		report["error"] = "read_failed"
	}
	if report["error"] != nil {
		return report
	}
	// An old reservation whose body matched is settled once, so the
	// comment reads as ours from now on.
	for _, p := range read.own.pending {
		if p.matched == nil {
			continue
		}
		args := lease.fence(map[string]any{"operation_digest": p.digest, "issue_uuid": issue, "comment_id": p.matched.ID, "created_at": p.matched.CreatedAt})
		if _, err = s.commentLedger(ctx, scope, "linear_comment_settle_match", args); err != nil {
			report["error"] = "ownership_unavailable"
			return report
		}
	}
	fresh := []CommentV2{}
	for _, c := range read.Items {
		if *c.PostedByClawBridge || c.AuthorKind != "unknown" {
			continue
		}
		fresh = append(fresh, c)
	}
	// Newest first, so a burst beyond the bus limit keeps the latest.
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].CreatedAt > fresh[j].CreatedAt })
	comments := []any{}
	for _, c := range fresh[:min(len(fresh), watchMaxReported)] {
		item := map[string]any{"comment_id": c.ID, "created_at": c.CreatedAt, "author_kind": c.AuthorKind}
		if c.ParentID != nil {
			item["parent_id"] = *c.ParentID
		}
		comments = append(comments, item)
	}
	report["comments"] = comments
	// The issue's team, from this same read: the bus counts a notified
	// comment's webhook miss against it (FR14 §6 P5). No extra call.
	if team := strings.ToLower(read.TeamID); linearprovider.ValidID(team) {
		report["team_id"] = team
	}
	return report
}

// watchFailure is the bus's name for a poll that failed.
func watchFailure(err error) string {
	var provider *linearprovider.Error
	var outside outOfScopeError
	switch {
	case errors.As(err, &outside):
		return "out_of_binding"
	case errors.As(err, &provider) && provider.Kind == "rate_limited":
		return "rate_limited"
	case errors.As(err, &provider) && provider.Kind == "unauthorized":
		return "auth"
	}
	return "read_failed"
}

// SweepComments settles this session's comments whose bus settle was
// missed. It needs the session's write access, as a comment does; any
// failure leaves them for the next sweep.
func (s *Service) SweepComments(ctx context.Context, scope bus.Request) {
	b, err := s.BindingV2(ctx, scope)
	if err != nil || !b.Configured || s.cfg.Authority == "" {
		return
	}
	access, err := s.writeAccessV2(ctx, scope, b)
	if err != nil {
		return
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	s.sweepComments(ctx, scope, s.operations(ctx), namespace)
}

// number reads a bus integer, local or decoded from JSON.
func number(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case interface{ Int64() (int64, error) }:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
