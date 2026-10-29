package tui

import (
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

func queueView(t *testing.T, role string, mode Mode) (Model, *fakeAuthority) {
	t.Helper()
	a := &fakeAuthority{results: map[string]bus.Result{
		"project_overview": {Source: "server", Data: map[string]any{"project": map[string]any{"slug": "core", "role": role}}},
		"linear_app_status": {Source: "server", Data: map[string]any{"configured": true,
			"outbox": map[string]any{"queued": float64(2), "sending": float64(1), "oldest_queued_age_s": float64(20 * 60), "waiting": "project_hourly_cap", "dead_24h": float64(0), "expired_24h": float64(0), "uncertain_24h": float64(0)},
			"budget": map[string]any{"used": float64(143), "limit": float64(600), "source": "default", "window_resets_at": "2026-10-27T11:00:00Z"}}},
		"linear_write_status": {Source: "server", Data: map[string]any{"writes": []any{
			map[string]any{"operation_id": "op-1", "operation": "comment", "issue": "SYN-6", "state": "queued", "reason": "project_hourly_cap", "queued_at": "2026-10-27T09:40:00Z", "next_attempt_at": "2026-10-27T11:00:00Z", "position": float64(1)},
			map[string]any{"operation_id": "op-0", "operation": "update", "issue": "SYN-5", "state": "done", "queued_at": "2026-10-27T09:00:00Z", "settled_at": "2026-10-27T09:05:00Z", "result": "issue SYN-5"},
		}}},
		"comment_watch_budget_set": {Source: "server", Data: map[string]any{}},
	}}
	source := &testLinearSource{binding: bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}}
	cfg := testConfig(a)
	cfg.Mode = mode
	cfg.Linear = source
	cfg.Now = func() time.Time { return time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) }
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(120, 40)
	m, cmd := m.Show(ScreenLinear)
	return quickSettle(m, cmd), a
}

func (f *fakeAuthority) asked(op string) []bus.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bus.Request
	for _, c := range f.calls {
		if c.request.Operation == op {
			out = append(out, c.request)
		}
	}
	return out
}

// A hosted project's Linear screen shows the hour's budget and the queue
// in its header, coloured by doctor's thresholds; o lists the queue with
// typed fields only.
func TestLinearShowsTheBudgetAndQueue(t *testing.T) {
	m, a := queueView(t, "member", ModeCloud)
	if view := Render(m); !strings.Contains(view, "Linear: 143/600 this hour · 3 queued (oldest 20 min)") {
		t.Fatalf("no header:\n%s", view)
	}
	if !strings.Contains(footerText(m), "o queued writes") || strings.Contains(footerText(m), "set the budget") {
		t.Fatal("a member's footer:", footerText(m))
	}
	m = do(m, "o")
	view := Render(m)
	for _, want := range []string{"Queued Linear writes", "comment · SYN-6 · queued (project_hourly_cap) · queued 20 min ago · next try", "update · SYN-5 · done · queued 60 min ago · issue SYN-5"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if got := a.asked("linear_write_status"); len(got) != 1 || got[0].Args["scope"] != "project" || got[0].Session != "" {
		t.Fatal("the queue was not read project-wide as the person:", got)
	}
	m = do(m, "b")
	if m.inputMode != inputNone {
		t.Fatal("a member could set the budget")
	}
	if m = do(m, "esc"); m.linear.step != "profiles" {
		t.Fatal("esc did not go back:", m.linear.step)
	}
}

// The owner sets the project's budget with b, a number and y; any other
// key keeps it, and empty goes back to the server's.
func TestLinearOwnerSetsTheBudget(t *testing.T) {
	m, a := queueView(t, "owner", ModeCloud)
	if !strings.Contains(footerText(m), "b set the budget") {
		t.Fatal("no b for the owner:", footerText(m))
	}
	m = do(m, "b", "9", "0", "0", "enter")
	if !strings.Contains(m.status.text, "Set this project's Linear budget to 900 calls an hour?") {
		t.Fatal(m.status.text)
	}
	m = do(m, "n")
	if len(a.asked("comment_watch_budget_set")) != 0 {
		t.Fatal("n set the budget")
	}
	m = do(m, "b", "x", "enter")
	if len(a.asked("comment_watch_budget_set")) != 0 || !strings.Contains(m.status.text, "whole number") {
		t.Fatal("a non-number:", m.status.text)
	}
	m = do(m, "b", "9", "0", "0", "enter", "y")
	m = do(m, "b", "enter", "y")
	got := a.asked("comment_watch_budget_set")
	if len(got) != 2 || got[0].Args["watch_budget"] != 900 || got[1].Args["watch_budget"] != nil || got[0].Session != "" {
		t.Fatal("set:", got)
	}
}

// A local project has no queue: no header, no o.
func TestLinearLocalHasNoQueue(t *testing.T) {
	m, a := queueView(t, "owner", ModeLocal)
	if strings.Contains(Render(m), "this hour") || strings.Contains(footerText(m), "queued writes") || len(a.asked("linear_app_status")) != 0 {
		t.Fatal("a local project shows a queue:", Render(m))
	}
}

// The header splits the hour when people or agent sessions spent any of
// it through their own profiles (FR8).
func TestLinearQueueLine(t *testing.T) {
	for name, c := range map[string]struct {
		q    linearQueue
		text string
	}{
		"quiet":     {linearQueue{used: 10, limit: 600}, "Linear: 10/600 this hour (o: queue)"},
		"sending":   {linearQueue{used: 10, limit: 600, sending: 1}, "· 1 queued (o: queue)"},
		"given up":  {linearQueue{used: 10, limit: 600, dead: 1, expired: 1}, "· 2 given up today"},
		"uncertain": {linearQueue{used: 10, limit: 600, uncertain: 1}, "· 1 uncertain"},
		"split":     {linearQueue{used: 40, limit: 600, people: 10, agents: 20}, "Linear: 40/600 this hour (people 10, agents 20) (o: queue)"},
		"no cap":    {linearQueue{used: 40, people: 10, agents: 20}, "Linear: 40 this hour (no cap) (people 10, agents 20) (o: queue)"},
	} {
		if got := linearQueueLine(&c.q); !strings.Contains(got, c.text) {
			t.Errorf("%s: %q", name, got)
		}
	}
}
