package linearboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// liveOnly is the bus as a server runs it: a call on a context that is
// already done fails, as a cancelled SQL transaction does.
type liveOnly struct{ Executor }

func (e liveOnly) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if err := ctx.Err(); err != nil {
		return bus.Result{}, err
	}
	return e.Executor.Execute(ctx, r)
}

// cutCommenter lands a comment on Linear's side, then never answers: the
// write is applied, and the caller sees only its deadline.
type cutCommenter struct{ commentingV2 }

func (c cutCommenter) Comment(ctx context.Context, issue, body string) (linearprovider.Receipt, error) {
	if _, err := c.commentingV2.Comment(ctx, issue, body); err != nil {
		return linearprovider.Receipt{}, err
	}
	<-ctx.Done()
	return linearprovider.Receipt{}, ctx.Err()
}

func dueRows(n int) []any {
	rows := []any{}
	for i := 0; i < n; i++ {
		charged := 13
		if i == 0 {
			charged = 34
		}
		rows = append(rows, map[string]any{"assignment_id": fmt.Sprintf("asg_%d", i+1), "phase": "claim", "issue_uuid": issueID, "charged": charged})
	}
	return rows
}

// refunds are the marker_spent reports that gave a row's charge back.
func refunds(a *authorityV2) []string {
	out := []string{}
	for _, r := range a.comments.markerSpents {
		if r["calls"] == 0 {
			out = append(out, fmt.Sprint(r["assignment_id"], "=", r["charged"]))
		}
	}
	return out
}

// passTest is a watcher Service on a fake app, with a clock the test
// moves, the bus refusing calls on a done context, and the issue due for
// a comment poll after the markers.
func passTest(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request, *time.Time, *linearprovider.Comments) {
	t.Helper()
	s, a, p, scope := markerTest(t)
	s.cfg.Executor = liveOnly{s.cfg.Executor}
	clock := time.Now()
	s.cfg.Now = func() time.Time { return clock }
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	result := &linearprovider.Comments{Complete: true}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: result}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	a.comments.due = []any{map[string]any{"issue_uuid": issueID, "since": "2026-10-22T07:00:00.000000000Z"}}
	return s, a, p, scope, &clock, result
}

func watchPass(t *testing.T, s *Service, scope bus.Request, ctx context.Context) (WatchCycle, error) {
	t.Helper()
	var got WatchCycle
	err := s.Watch(ctx, scope, WatchOptions{Budget: 200, Once: true, Cycle: func(c WatchCycle) error { got = c; return nil }})
	return got, err
}

// (a) A pass asks for at most markerPassRows rows, starts none once it has
// run markerPassTime, refunds the rows it didn't start, and the comment
// watch still runs after it.
func TestMarkerPassStopsAtItsTimeBox(t *testing.T) {
	s, a, _, scope, clock, _ := passTest(t)
	a.comments.markersDue, a.comments.markersLeft, a.comments.markersStopped = dueRows(3), 2, "limit"
	a.comments.onGo = func(args map[string]any) {
		if args["part"] == "comment" {
			*clock = clock.Add(5 * time.Second)
		}
	}
	cycle, err := watchPass(t, s, scope, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.comments.markerDueArgs[0]["limit"] != markerPassRows {
		t.Fatal("the pass asked for more rows than it takes:", a.comments.markerDueArgs[0])
	}
	if len(cycle.Markers) != 2 || cycle.MarkersStopped != "time" || cycle.MarkersLeft != 3 {
		t.Fatalf("%d rows, stopped %q, left %d", len(cycle.Markers), cycle.MarkersStopped, cycle.MarkersLeft)
	}
	if got := refunds(a); len(got) != 1 || got[0] != "asg_3=13" {
		t.Fatal("the unstarted row was not refunded its charge:", got)
	}
	// The net charge is what the started rows spent, as reconciled, or
	// their charge where it couldn't be.
	net := 0
	for _, r := range a.comments.markerSpents {
		if r["calls"] != 0 {
			net += r["calls"].(int)
		}
	}
	if len(a.comments.markerSpents) == 1 {
		net = 34 + 13
	}
	if cycle.MarkersCharged != net || net == 0 {
		t.Fatal("net charge:", cycle.MarkersCharged, "want", net, a.comments.markerSpents)
	}
	if len(a.comments.reports) != 1 {
		t.Fatal("the comment watch did not run after the pass")
	}
}

// MH-M1, MH-M2: a row still in flight at the pass's deadline (its caller's
// minus markerReserve) is cut, not the tick. Its comment had landed, so it
// settles uncertain, on a live context; the rows after it are refunded;
// the comment watch ran before the pass; and the next attempt finds the
// comment instead of posting it again.
func TestMarkerPassCutsARowInFlightAndKeepsTheTick(t *testing.T) {
	s, a, p, scope, _, result := passTest(t)
	open := s.cfg.Open
	s.cfg.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) {
		pv, err := open(ctx, h, ro)
		return cutCommenter{pv.(commentingV2)}, err
	}
	a.comments.markersDue = dueRows(3)
	room, reserve := markerRowRoom, markerReserve
	markerRowRoom, markerReserve = 100*time.Millisecond, 300*time.Millisecond
	defer func() { markerRowRoom, markerReserve = room, reserve }()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	started := time.Now()
	cycle, err := watchPass(t, s, scope, ctx)
	if err != nil {
		t.Fatal("the cut row failed the tick:", err)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatal("the pass ran past its deadline:", took)
	}
	if len(cycle.Markers) != 1 || cycle.Markers[0].Comment != "uncertain" || cycle.MarkersStopped != "deadline" || cycle.MarkersLeft != 2 {
		t.Fatalf("%+v", cycle)
	}
	if c := settled(a, "comment"); c == nil || c["outcome"] != "uncertain" {
		t.Fatal("the cut comment was not settled uncertain on a live context:", a.comments.markerSettles)
	}
	if got := refunds(a); len(got) != 2 {
		t.Fatal("the rows after the cut were not refunded:", got)
	}
	if len(a.comments.reports) != 1 {
		t.Fatal("the comment watch did not run")
	}
	if len(p.bodies) != 1 {
		t.Fatal(p.bodies)
	}
	// The next attempt checks for the landed comment and posts nothing.
	a.comments.gos["comment"] = map[string]any{"action": "check", "issue_uuid": issueID, "body": claimBody, "check_from": started.Add(-time.Minute).UTC().Format(time.RFC3339Nano), "check_to": started.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)}
	a.comments.gos["label"] = map[string]any{"action": "none"}
	result.Items = []linearprovider.Comment{commentRow(ourComment, claimBody, started)}
	s.cfg.Open = open
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil); out.Comment != "done" || len(p.bodies) != 1 {
		t.Fatal("the retry posted the comment again:", out, p.bodies)
	}
}

// (b) One pass holds one operation: one connection and one binding proof
// for all its rows, where each row used to open and prove its own.
func TestMarkerPassSharesOneConnection(t *testing.T) {
	s, a, _, scope, _, _ := passTest(t)
	opens := 0
	open := s.cfg.Open
	s.cfg.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) {
		opens++
		return open(ctx, h, ro)
	}
	a.comments.markersDue = dueRows(3)
	a.comments.due = nil
	cycle, err := watchPass(t, s, scope, context.Background())
	if err != nil || len(cycle.Markers) != 3 {
		t.Fatal(err, cycle)
	}
	for _, m := range cycle.Markers {
		if m.Comment != "done" || m.Label != "done" {
			t.Fatal(m)
		}
	}
	if opens != 1 {
		t.Fatal("the rows did not share the pass's connection and proof:", opens)
	}
}

// A pass whose bus answers with an error stops, and the cycle goes on;
// only a lost lease ends it.
func TestMarkerPassErrorsStopThePassNotTheCycle(t *testing.T) {
	s, a, _, scope, _, _ := passTest(t)
	a.comments.markersDue = nil
	s.cfg.Executor = failing{s.cfg.Executor, "marker_due"}
	cycle, err := watchPass(t, s, scope, context.Background())
	if err != nil || cycle.MarkersStopped != "error" || len(a.comments.reports) != 1 {
		t.Fatal(err, cycle.MarkersStopped, len(a.comments.reports))
	}
}

// failing fails one operation, as an unavailable bus does.
type failing struct {
	Executor
	op string
}

func (f failing) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == f.op {
		return bus.Result{}, fmt.Errorf("%s unavailable", f.op)
	}
	return f.Executor.Execute(ctx, r)
}

// A tick runs its comment watch and Within steps before the marker pass,
// so markers can never take their time.
func TestTickRunsMarkersLast(t *testing.T) {
	s, a, _, scope, _, _ := passTest(t)
	a.comments.markersDue = dueRows(1)
	order := []string{}
	a.comments.onGo = func(map[string]any) {
		if len(order) == 0 || order[len(order)-1] != "markers" {
			order = append(order, "markers")
		}
	}
	err := s.Watch(context.Background(), scope, WatchOptions{Budget: 200, Once: true,
		Within: func(context.Context, map[string]any) error {
			order = append(order, fmt.Sprintf("within after %d polls", len(a.comments.reports)))
			return nil
		}})
	if err != nil || strings.Join(order, ",") != "within after 1 polls,markers" {
		t.Fatal(err, order)
	}
}

// With too little time left to start a row, a pass starts none, refunds
// every row it was handed, and says why.
func TestMarkerPassStartsNoRowWithoutRoom(t *testing.T) {
	s, a, _, scope, _, _ := passTest(t)
	a.comments.markersDue = dueRows(2)
	ctx, cancel := context.WithTimeout(context.Background(), markerReserve+markerRowRoom-time.Second)
	defer cancel()
	cycle, err := watchPass(t, s, scope, ctx)
	if err != nil || len(cycle.Markers) != 0 || cycle.MarkersStopped != "deadline" || len(refunds(a)) != 2 {
		t.Fatal(err, cycle, refunds(a))
	}
}

// R116-L3: a cut on the pass's last row reports deadline, not the bus's
// "" or limit.
func TestMarkerPassCutOnItsLastRowSaysDeadline(t *testing.T) {
	s, a, _, scope, _, _ := passTest(t)
	open := s.cfg.Open
	s.cfg.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) {
		pv, err := open(ctx, h, ro)
		return cutCommenter{pv.(commentingV2)}, err
	}
	a.comments.markersDue = dueRows(1)
	room, reserve := markerRowRoom, markerReserve
	markerRowRoom, markerReserve = 100*time.Millisecond, 300*time.Millisecond
	defer func() { markerRowRoom, markerReserve = room, reserve }()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	cycle, err := watchPass(t, s, scope, ctx)
	if err != nil || len(cycle.Markers) != 1 || cycle.MarkersStopped != "deadline" {
		t.Fatalf("%v %+v", err, cycle)
	}
}

// R116-L3: a budget stop stands when the pass also stops on its clock, so
// a nudge doesn't come back every minute to an empty budget.
func TestMarkerPassKeepsABudgetStop(t *testing.T) {
	s, a, _, scope, clock, _ := passTest(t)
	a.comments.markersDue, a.comments.markersLeft, a.comments.markersStopped = dueRows(3), 2, "budget"
	a.comments.onGo = func(args map[string]any) {
		if args["part"] == "comment" {
			*clock = clock.Add(5 * time.Second)
		}
	}
	cycle, err := watchPass(t, s, scope, context.Background())
	if err != nil || len(cycle.Markers) != 2 || cycle.MarkersStopped != "budget" || cycle.MarkersLeft != 3 {
		t.Fatalf("%v %d rows, stopped %q, left %d", err, len(cycle.Markers), cycle.MarkersStopped, cycle.MarkersLeft)
	}
}
