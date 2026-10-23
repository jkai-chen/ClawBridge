package linearboard

import (
	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"context"
	"errors"
	"net/http"
	"sort"
	"time"
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
	// Markers are the claim markers this cycle applied, first.
	Markers []MarkerApply `json:"markers,omitempty"`
	// MarkersReadOnly is set when the watcher's Linear profile cannot
	// write, so claim markers wait for a client that can.
	MarkersReadOnly bool `json:"markers_read_only,omitempty"`
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
	s.SweepComments(ctx, scope)
	calls := s.countCalls()
	for {
		if err = s.renewWatch(ctx, scope, lease, true); err != nil {
			return err
		}
		cycle, err := s.watchCycle(ctx, scope, lease, calls)
		if err != nil {
			return err
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

func (s *Service) watchCycle(ctx context.Context, scope bus.Request, lease *watchLease, calls func() linearCalls) (WatchCycle, error) {
	cycle := WatchCycle{Notified: []string{}}
	credit := watchConnectCalls
	if err := s.applyDueMarkers(ctx, scope, lease, &cycle); err != nil {
		return cycle, err
	}
	due, err := s.commentLedger(ctx, scope, "comment_watch_due", lease.fence(map[string]any{}))
	if err != nil {
		return cycle, err
	}
	cycle.BudgetUsed, cycle.BudgetLimit = number(due["budget_used"]), number(due["budget_limit"])
	cycle.BudgetExhausted, _ = due["budget_exhausted"].(bool)
	issues, _ := due["issues"].([]any)
	for _, raw := range issues {
		if lease.overBudget(s.cfg.Now(), 0, cycle.BudgetLimit) {
			cycle.BudgetExhausted = true
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
		spent := pollCost(before, calls(), &credit)
		report["calls"] = min(spent, 1000)
		lease.overBudget(s.cfg.Now(), spent, cycle.BudgetLimit)
		out, err := s.commentLedger(ctx, scope, "comment_watch_report", lease.fence(report))
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
func (s *Service) applyDueMarkers(ctx context.Context, scope bus.Request, lease *watchLease, cycle *WatchCycle) error {
	if s.cfg.Connections == nil {
		return nil
	}
	if profile, err := s.cfg.Connections.Status(ctx, s.cfg.ProfileID); err != nil || profile.Access != linearconn.Write {
		cycle.MarkersReadOnly = true
		return nil
	}
	due, err := s.commentLedger(ctx, scope, "marker_due", lease.fence(map[string]any{}))
	if err != nil {
		var coded interface{ Code() string }
		if errors.As(err, &coded) && coded.Code() == "conflict" {
			return err
		}
		return nil
	}
	markers, _ := due["markers"].([]any)
	for _, raw := range markers {
		m, _ := raw.(map[string]any)
		assignment, _ := m["assignment_id"].(string)
		phase, _ := m["phase"].(string)
		if err = s.renewWatch(ctx, scope, lease, false); err != nil {
			return err
		}
		cycle.Markers = append(cycle.Markers, s.ApplyMarker(ctx, scope, assignment, phase, lease.fence(map[string]any{})))
	}
	return nil
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
	s.sweepComments(ctx, scope, ledger{s.cfg.LedgerDirectory, s.cfg.Now}, namespace)
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
