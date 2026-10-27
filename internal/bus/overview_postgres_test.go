package bus

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// M2: every project role reads the overview, and it shows nothing that
// role can't already read through list_sessions, taskboard_get_v2 and
// status_policy_get; someone outside the project is refused.
func TestPostgresProjectOverviewPerRole(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner, lead, member, reader, outsider := "owner@overview-test.example", "lead@overview-test.example", "member@overview-test.example",
		"reader@overview-test.example", "outsider@overview-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(principal, op, session string, a map[string]any) (map[string]any, error) {
		if a == nil {
			a = map[string]any{}
		}
		r, e := s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a})
		m, _ := r.Data.(map[string]any)
		return m, e
	}
	must := func(principal, op, session string, a map[string]any) map[string]any {
		t.Helper()
		m, e := exec(principal, op, session, a)
		if e != nil {
			t.Fatalf("%s as %s: %v", op, principal, e)
		}
		return m
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "workspace_init", Workspace: w, Args: map[string]any{"mode": "server"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_create", Workspace: w, Args: map[string]any{"slug": "p", "git_remote": "https://user:token-sentinel@example.test/org/repo.git"}}); e != nil {
		t.Fatal(e)
	}
	for p, role := range map[string]string{lead: "lead", member: "member", reader: "reader"} {
		must(owner, "project_role_set", "", map[string]any{"principal": p, "role": role})
	}
	must(owner, "register_session", "owner-s", map[string]any{"instance": "owner-instance-sentinel", "role": "worker"})
	must(member, "register_session", "member-s", map[string]any{"instance": "member-instance-sentinel", "role": "worker"})
	must(owner, "taskboard_set_v2", "", map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne, cpTwo}})
	must(owner, "status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	// Names: a lead refreshes them like a link; a member or reader can't.
	must(lead, "taskboard_names_set", "", map[string]any{"expected_revision": 1, "linear_project_names": map[string]any{cpOne: "Payments"}})
	for _, p := range []string{member, reader} {
		if _, e := exec(p, "taskboard_names_set", "", map[string]any{"expected_revision": 1, "linear_project_names": map[string]any{cpOne: "Mine"}}); e == nil {
			t.Fatalf("%s set names", p)
		}
	}

	for _, principal := range []string{owner, lead, member, reader} {
		got := must(principal, "project_overview", "", nil)
		raw, _ := json.Marshal(got)
		for _, never := range []string{"instance-sentinel", `"instance"`, "token-sentinel", "transport", "notification_policy"} {
			if strings.Contains(string(raw), never) {
				t.Fatalf("%s sees %q: %s", principal, never, raw)
			}
		}
		// The rows are list_sessions' own, for the same reader.
		listed := must(principal, "list_sessions", "", map[string]any{"limit": 100})
		var want, have []string
		for _, r := range listed["sessions"].([]map[string]any) {
			want = append(want, r["id"].(string))
		}
		for _, r := range got["sessions"].([]map[string]any) {
			have = append(have, r["id"].(string))
		}
		sort.Strings(want)
		sort.Strings(have)
		if !reflect.DeepEqual(want, have) {
			t.Fatalf("%s: overview rows %v, list_sessions %v", principal, have, want)
		}
		// The board is taskboard_get_v2's, for the same reader.
		board := must(principal, "taskboard_get_v2", "", nil)["binding"].(map[string]any)
		linear := got["linear"].(map[string]any)
		var ids []string
		for _, p := range linear["projects"].([]map[string]any) {
			ids = append(ids, p["id"].(string))
		}
		if !reflect.DeepEqual(ids, stringsOf(board["linear_project_ids"])) {
			t.Fatalf("%s: projects %v, binding %v", principal, ids, board["linear_project_ids"])
		}
		if p := linear["projects"].([]map[string]any)[0]; p["name"] != "Payments" || linear["names_updated_at"] == nil {
			t.Fatalf("%s: names %v", principal, linear)
		}
		if role := got["project"].(map[string]any)["role"]; role == "" {
			t.Fatalf("%s: no role", principal)
		}
	}
	if _, e := exec(outsider, "project_overview", "", nil); e == nil || !strings.Contains(e.Error(), "member") && !strings.Contains(e.Error(), "forbidden") {
		t.Fatalf("an outsider read the overview: %v", e)
	}
}

func stringsOf(v any) []string {
	out := []string{}
	switch l := v.(type) {
	case []string:
		out = append(out, l...)
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
