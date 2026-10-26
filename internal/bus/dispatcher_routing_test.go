package bus

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// With the dispatcher as the project's coordinator, a coordinator notice
// goes by its subject (§6): a bound Linear project's domain, else the one
// domain its labels match, else the fallback. Without it, to the
// coordinator session as before.
func TestCoordinatorTargets(t *testing.T) {
	h := dispatcherHarness(t)
	ctx := context.Background()
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.ExecuteDispatcher(ctx, pid, DispatcherSessionName, Request{Operation: "dispatcher_get"}); err != nil {
		t.Fatal(err)
	}
	me := h.principal()
	platform := paymentsDomain(me)
	platform["id"], platform["name"] = "platform", "Platform"
	platform["match"] = map[string]any{"linear_projects": []any{projectTwo}, "labels": []any{"platform"}}
	platform["coordinator_session"] = map[string]any{"name": "b"}
	h.call("dispatcher_domains_set", "a", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{paymentsDomain(me), platform}}})
	sql := func(q string) {
		if _, err := h.s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sql(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='clawbridge-dispatcher')`)
	targets := func(project string, labels ...string) []string {
		t.Helper()
		tx, err := h.s.beginIntake(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		rows, err := h.s.coordinatorTargets(ctx, tx, pid, project, labels)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, r := range rows {
			names = append(names, str(r, "name"))
		}
		sort.Strings(names)
		return names
	}
	fallback := []string{"a", "b", "c"} // every session is the owner's here
	for name, c := range map[string]struct {
		project string
		labels  []string
		want    []string
	}{
		"payments project":           {projectOne, nil, []string{"c"}},
		"platform project":           {projectTwo, nil, []string{"b"}},
		"project beats labels":       {projectTwo, []string{"payments"}, []string{"b"}},
		"one label domain":           {"", []string{"payments"}, []string{"c"}},
		"two labels, one domain":     {"", []string{"Payments", "payments"}, []string{"c"}},
		"labels in two domains":      {"", []string{"payments", "platform"}, fallback},
		"unbound project, no labels": {"33333333-3333-4333-8333-333333333333", nil, fallback},
		"nothing known":              {"", nil, fallback},
		"unmatched label":            {"", []string{"other"}, fallback},
	} {
		if got := targets(c.project, c.labels...); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: %v, want %v", name, got, c.want)
		}
	}
	// A coordinator gone quiet: the domain owner's other sessions.
	sql(`UPDATE sessions SET last_seen='2000-01-01T00:00:00Z' WHERE name='c'`)
	if got := targets(projectOne); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("stale coordinator: %v", got)
	}
	// Not the dispatcher: the coordinator session, whatever the subject.
	sql(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='a')`)
	if got := targets(projectTwo, "platform"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("plain coordinator: %v", got)
	}
}

// An intake notice routes by the app's read of the issue: its project's
// domain coordinator hears it, not the project's former coordinator.
func TestIntakeNoticeRoutesByTheAppsRead(t *testing.T) {
	h := newIntakeHarness(t)
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	h.call("label_policy_set", "a", map[string]any{"expected_revision": 0, "label_policy": map[string]any{"labels": []any{"payments", "platform"}}})
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.ExecuteDispatcher(context.Background(), pid, DispatcherSessionName, Request{Operation: "dispatcher_get"}); err != nil {
		t.Fatal(err)
	}
	me := h.principal()
	platform := paymentsDomain(me)
	platform["id"], platform["name"] = "platform", "Platform"
	platform["match"] = map[string]any{"linear_projects": []any{projectTwo}, "labels": []any{"platform"}}
	platform["coordinator_session"] = map[string]any{"name": "b"}
	h.call("dispatcher_domains_set", "a", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{paymentsDomain(me), platform}}})
	if _, err := h.s.db.Exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='clawbridge-dispatcher')`); err != nil {
		t.Fatal(err)
	}
	prove := h.prove
	h.prove = func(ctx context.Context, wid, pid, issue string) (IntakeIssue, error) {
		x, err := prove(ctx, wid, pid, issue)
		x.ProjectID = projectTwo
		return x, err
	}
	if out := h.request(intakeIssue); out["result"] != "queued" {
		t.Fatal(out)
	}
	if b, c := h.intakeInbox("b"), h.intakeInbox("c"); len(b) != 1 || len(c) != 0 {
		t.Fatalf("routed to b %d times, c %d times", len(b), len(c))
	}
}

// A health notice routes by its Linear project's domain.
func TestHealthNoticeRoutesByProject(t *testing.T) {
	h := newHealthHarness(t)
	h.call("label_policy_set", "a", map[string]any{"expected_revision": 0, "label_policy": map[string]any{"labels": []any{"payments", "platform"}}})
	if _, err := h.s.ExecuteDispatcher(context.Background(), h.pid, DispatcherSessionName, Request{Operation: "dispatcher_get"}); err != nil {
		t.Fatal(err)
	}
	me := h.principal()
	platform := paymentsDomain(me)
	platform["id"], platform["name"] = "platform", "Platform"
	platform["match"] = map[string]any{"linear_projects": []any{projectTwo}, "labels": []any{"platform"}}
	platform["coordinator_session"] = map[string]any{"name": "b"}
	h.call("dispatcher_domains_set", "a", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{paymentsDomain(me), platform}}})
	if _, err := h.s.db.Exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='clawbridge-dispatcher')`); err != nil {
		t.Fatal(err)
	}
	h.enable(projectTwo, 0, nil)
	h.run(projectTwo, blockedFacts(5))
	h.run(projectTwo, blockedFacts(5))
	rows, err := h.s.db.Query(`SELECT address FROM messages WHERE kind='project-health'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var to []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		to = append(to, a)
	}
	if !reflect.DeepEqual(to, []string{"b"}) {
		t.Fatalf("health notice went to %v, want [b]", to)
	}
}
