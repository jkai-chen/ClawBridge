package bus

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// appCharge is ChargeLinearBudget's charge in a local store's transaction
// (ChargeLinearBudget itself needs the hosted ledger).
func (h *harness) appCharge(calls int, kind string) {
	h.t.Helper()
	ctx := context.Background()
	// The project's ID first: a local store has one connection.
	pid := h.projectID()
	raw, err := h.s.db.BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer raw.Rollback()
	if err = h.s.chargeLinearBudget(ctx, &transaction{Tx: raw}, pid, calls, kind); err != nil {
		h.t.Fatal(err)
	}
	if err = raw.Commit(); err != nil {
		h.t.Fatal(err)
	}
}

// appChargeBy is ChargeLinearBudgetBy's charge in a local store's
// transaction, as appCharge is ChargeLinearBudget's.
func (h *harness) appChargeBy(calls int, by, kind string) {
	h.t.Helper()
	ctx := context.Background()
	pid := h.projectID()
	raw, err := h.s.db.BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer raw.Rollback()
	if err = h.s.chargeLinearBudgetBy(ctx, &transaction{Tx: raw}, pid, calls, by, kind); err != nil {
		h.t.Fatal(err)
	}
	if err = raw.Commit(); err != nil {
		h.t.Fatal(err)
	}
}

// kindRows is every row of the table, as "hour kind calls/events".
func (h *harness) kindRows() []string {
	h.t.Helper()
	rows, err := h.s.db.Query(`SELECT hour,kind,calls,events FROM linear_budget_kinds ORDER BY hour,kind`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var hour, kind string
		var calls, events int
		if err = rows.Scan(&hour, &kind, &calls, &events); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, hour+" "+kind+" "+strconv.Itoa(calls)+"/"+strconv.Itoa(events))
	}
	return out
}

// One hour with a watch pass, two app-route writes and a personal write
// has three kinds; their calls sum to the hour's used, their events count
// the charges, and the view has them the most calls first.
func TestLinearBudgetKindsSplitTheHour(t *testing.T) {
	w := newWatchHarness(t)
	due := w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any)
	watched := due["budget_used"].(int)
	if watched <= 2 {
		t.Fatal("the watch pass charged nothing:", due)
	}
	w.appCharge(16, "app_write.update")
	w.appCharge(16, "app_write.update")
	w.call("linear_write_charge", "", charge("p-1", "create", 2))
	b := w.overview("")["linear_budget"].(LinearBudget)
	want := []LinearBudgetKind{{Kind: "app_write.update", Calls: 32, Events: 2}, {Kind: "watch.comments", Calls: watched, Events: 1}, {Kind: "profile.create", Calls: 2, Events: 1}}
	if !reflect.DeepEqual(b.Kinds, want) || b.LastHour != nil {
		t.Fatalf("%+v %+v", b.Kinds, b.LastHour)
	}
	sum := 0
	for _, k := range b.Kinds {
		sum += k.Calls
	}
	if sum != b.Used || b.Used != w.budgetUsed() {
		t.Fatal(sum, b.Used, w.budgetUsed())
	}
	// project linear-budget show carries the same.
	status := w.call("comment_watch_status", "", nil).(map[string]any)
	if !reflect.DeepEqual(status["kinds"], want) || status["last_hour"] != nil {
		t.Fatal(status)
	}
}

// An issue history read is charged by the server as the app's read,
// app_read.history, and the overview names it; never as a profile's:
// profile.read is no kind at all.
func TestIssueHistoryReadIsChargedAsAnAppRead(t *testing.T) {
	w := newWatchHarness(t)
	w.appChargeBy(3, "agents", "app_read.history")
	b := w.overview("")["linear_budget"].(LinearBudget)
	want := []LinearBudgetKind{{Kind: "app_read.history", Calls: 3, Events: 1}}
	if b.By.Agents != 3 || b.By.People != 0 || b.By.App != 0 {
		t.Fatal(b.By)
	}
	if !reflect.DeepEqual(b.Kinds, want) {
		t.Fatalf("%+v", b.Kinds)
	}
	if KnownLinearCallKind("profile.read") || !KnownLinearCallKind("app_read.history") {
		t.Fatal("the read's kind")
	}
}

// A read (0) records nothing; a refund lowers a kind's calls, never below
// zero, and is no event; an unknown kind is counted as watch.other.
func TestLinearBudgetKindsReadsAndRefunds(t *testing.T) {
	h := newHarness(t)
	pid := h.projectID()
	hour := layout(time.Now())[:13]
	record := func(kind string, calls int) {
		t.Helper()
		ctx := context.Background()
		raw, err := h.s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = recordCallKind(ctx, &transaction{Tx: raw}, pid, hour, kind, calls); err != nil {
			t.Fatal(err)
		}
		if err = raw.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	record("watch.health", 0)
	if got := h.kindRows(); len(got) != 0 {
		t.Fatal("a read recorded:", got)
	}
	record("watch.health", 8)
	record("watch.health", 0)
	record("watch.health", -3)
	if got := h.kindRows(); !reflect.DeepEqual(got, []string{hour + " watch.health 5/1"}) {
		t.Fatal(got)
	}
	record("watch.health", -9)
	record("watch.flow", -2)
	record("watch.helth", 2)
	if got := h.kindRows(); !reflect.DeepEqual(got, []string{hour + " watch.health 0/1", hour + " watch.other 2/1"}) {
		t.Fatal(got)
	}
}

// The hour rolls: at H+2 the view shows H+1 as last_hour, and H's rows are
// gone.
func TestLinearBudgetKindsRollWithTheHour(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 30, 9, 30, 0, 0, time.UTC)
	hostClock = func() time.Time { return now }
	t.Cleanup(func() { hostClock = time.Now })
	h.call("linear_write_charge", "", charge("p-1", "create", 4))
	now = now.Add(time.Hour)
	h.call("linear_write_charge", "", charge("p-2", "update", 5))
	h.call("linear_write_charge", "", charge("p-3", "update", 2))
	if got := h.kindRows(); !reflect.DeepEqual(got, []string{"2026-10-30T09 profile.create 4/1", "2026-10-30T10 profile.update 7/2"}) {
		t.Fatal("the previous hour is kept:", got)
	}
	b := h.overview("")["linear_budget"].(LinearBudget)
	if b.LastHour == nil || b.LastHour.Hour != "2026-10-30T09" || len(b.Kinds) != 1 || b.Kinds[0] != (LinearBudgetKind{Kind: "profile.update", Calls: 7, Events: 2}) {
		t.Fatalf("%+v %+v", b.Kinds, b.LastHour)
	}
	now = now.Add(time.Hour)
	h.call("linear_write_charge", "", charge("p-4", "view", 1))
	if got := h.kindRows(); !reflect.DeepEqual(got, []string{"2026-10-30T10 profile.update 7/2", "2026-10-30T11 profile.view 1/1"}) {
		t.Fatal("hour H outlived H+2's charge:", got)
	}
	b = h.overview("")["linear_budget"].(LinearBudget)
	want := &LinearBudgetHour{Hour: "2026-10-30T10", Kinds: []LinearBudgetKind{{Kind: "profile.update", Calls: 7, Events: 2}}}
	if !reflect.DeepEqual(b.LastHour, want) || b.Used != 1 || len(b.Kinds) != 1 {
		t.Fatalf("%+v %+v", b.Kinds, b.LastHour)
	}
	if got, err := h.s.LinearBudgetKindsAt(context.Background(), h.projectID(), "2026-10-30T10"); err != nil || !reflect.DeepEqual(got, want.Kinds) {
		t.Fatal(got, err)
	}
}

// watch_charge records its kind when it is one of the set's watch kinds;
// any other, or none (an older watcher), is watch.other.
func TestWatchChargeNamesItsKind(t *testing.T) {
	h := newHealthHarness(t)
	h.sys("watch_charge", map[string]any{"calls": 3, "kind": "watch.intake"})
	h.sys("watch_charge", map[string]any{"calls": 1, "kind": "watch.dispatcher"})
	h.sys("watch_charge", map[string]any{"calls": 2, "kind": "project_status"})
	h.sys("watch_charge", map[string]any{"calls": 2, "kind": "watch.made_up"})
	h.sys("watch_charge", map[string]any{"calls": 1})
	hour := layout(time.Now())[:13]
	if got := h.kindRows(); !reflect.DeepEqual(got, []string{hour + " watch.dispatcher 1/1", hour + " watch.intake 3/1", hour + " watch.other 5/3"}) {
		t.Fatal(got)
	}
}

// Every kind a charge names in the bus and the server is in the set: a
// literal kind, or a prefix (app_write., profile.) some kind has.
func TestEveryChargeKindIsInTheSet(t *testing.T) {
	call := regexp.MustCompile(`\.(charge|chargeBy|ChargeLinearBudget|ChargeLinearBudgetBy)\(ctx|recordCallKind\(ctx`)
	literal := regexp.MustCompile(`^"([a-z_.]+)"(\s*\+\s*\w+)?$`)
	allowed := map[string]bool{"kind": true, `watchChargeKind(c.a["kind"])`: true}
	found := 0
	for _, dir := range []string{".", "../server"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			src := string(raw)
			for _, loc := range call.FindAllStringIndex(src, -1) {
				if line := src[strings.LastIndex(src[:loc[0]], "\n")+1 : loc[0]]; strings.HasPrefix(strings.TrimSpace(line)+" ", "func ") {
					continue
				}
				args := callArgs(t, src, strings.Index(src[loc[0]:], "(")+loc[0])
				// recordCallKind's kind is its fifth argument; the others'
				// is their last.
				last := args[len(args)-1]
				if strings.HasPrefix(src[loc[0]:], "recordCallKind") {
					last = args[4]
				}
				found++
				if allowed[last] {
					continue
				}
				m := literal.FindStringSubmatch(last)
				if m == nil {
					t.Errorf("%s: a charge names its kind as %s", file, last)
					continue
				}
				if m[2] == "" && !KnownLinearCallKind(m[1]) {
					t.Errorf("%s: %q is not in the set", file, m[1])
				}
				if m[2] != "" && !hasKindPrefix(m[1]) {
					t.Errorf("%s: no kind starts with %q", file, m[1])
				}
			}
		}
	}
	if found < 25 {
		t.Fatal("too few charges found:", found)
	}
}

// callArgs is the arguments of the call whose "(" is at open.
func callArgs(t *testing.T, src string, open int) []string {
	t.Helper()
	var args []string
	depth, start := 0, open+1
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
			if depth == 0 {
				return append(args, strings.TrimSpace(src[start:i]))
			}
		case ',':
			if depth == 1 {
				args = append(args, strings.TrimSpace(src[start:i]))
				start = i + 1
			}
		}
	}
	t.Fatal("unclosed call")
	return nil
}

func hasKindPrefix(prefix string) bool {
	for kind := range linearCallKinds {
		if strings.HasPrefix(kind, prefix) && strings.HasSuffix(prefix, ".") {
			return true
		}
	}
	return false
}

// kindCalls is the calls of kind in the table, any hour; -1 for no row.
func (h *harness) kindCalls(kind string) (calls, events int) {
	h.t.Helper()
	calls, events = -1, -1
	rows, err := h.s.db.Query(`SELECT calls,events FROM linear_budget_kinds WHERE kind=?`, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err = rows.Scan(&calls, &events); err != nil {
			h.t.Fatal(err)
		}
	}
	return calls, events
}

// One health_due pass charges a health run to watch.health and a
// flow-only run to watch.flow; the flow run's report refunds what it
// didn't read from watch.flow only.
func TestHealthDueSplitsHealthAndFlow(t *testing.T) {
	h := newFlowHarness(t, "")
	h.enable(projectTwo, 0, nil)
	h.due()
	if _, e := h.s.db.Exec(`UPDATE project_flow_state SET next_flow_at=NULL`); e != nil {
		t.Fatal(e)
	}
	runs := h.sys("health_due", map[string]any{"limit": 2})["runs"].([]any)
	kinds := map[string]bool{}
	for _, r := range runs {
		m := r.(map[string]any)
		kinds[m["linear_project"].(string)+" "+map[bool]string{true: "health", false: "flow"}[m["health"] == true]] = true
	}
	if !kinds[projectTwo+" health"] || !kinds[projectOne+" flow"] || len(runs) != 2 {
		t.Fatal("not one health run and one flow run:", runs)
	}
	if calls, events := h.kindCalls("watch.health"); calls != HealthMaxCalls || events != 1 {
		t.Fatal("watch.health:", calls, events)
	}
	if calls, events := h.kindCalls("watch.flow"); calls != FlowReadCalls || events != 1 {
		t.Fatal("watch.flow:", calls, events)
	}
	h.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 2, "flow": flowFacts(psBacklog, issues{total: 3, started: 1})})
	if calls, events := h.kindCalls("watch.flow"); calls != 2 || events != 1 {
		t.Fatal("the flow run's refund:", calls, events)
	}
	if calls, _ := h.kindCalls("watch.health"); calls != HealthMaxCalls {
		t.Fatal("a flow refund moved watch.health:", calls)
	}
}

// forcedPostOnLinear makes a forced health post Linear accepted, ready for
// the operator to record as deleted.
func (h *healthHarness) forcedPostOnLinear() HealthPostRef {
	h.t.Helper()
	h.enable(projectOne, 0, map[string]any{"enabled": true, "post": true})
	h.farSlot()
	post, _ := h.force(projectOne, true, withUpdates(blockedFacts(5)))["post"].(map[string]any)
	if post == nil {
		h.t.Fatal("no forced post")
	}
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": post["day"], "outcome": "accepted", "update_id": updateOne})
	ref, e := h.s.OperatorForcedHealthPost(context.Background(), projectOne, updateOne)
	if e != nil {
		h.t.Fatal(e)
	}
	return ref
}

// The operator's record of a deleted forced post charges operator.health,
// in the same transaction; a project with no watch_lease row is charged
// nothing, so records nothing.
func TestOperatorHealthDeleteNamesItsKind(t *testing.T) {
	h := newHealthHarness(t)
	ref := h.forcedPostOnLinear()
	if e := h.s.OperatorForcedHealthPostDeleted(context.Background(), ref); e != nil {
		t.Fatal(e)
	}
	if calls, events := h.kindCalls("operator.health"); calls != HealthPostDeleteCalls || events != 1 {
		t.Fatal("operator.health:", calls, events)
	}
	h = newHealthHarness(t)
	ref = h.forcedPostOnLinear()
	if _, e := h.s.db.Exec(`DELETE FROM watch_lease`); e != nil {
		t.Fatal(e)
	}
	if e := h.s.OperatorForcedHealthPostDeleted(context.Background(), ref); e != nil {
		t.Fatal(e)
	}
	if calls, _ := h.kindCalls("operator.health"); calls != -1 {
		t.Fatal("recorded without a lease row:", calls)
	}
}
