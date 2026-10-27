package bus

import (
	"strings"
	"testing"
	"time"
)

// Only the project owner sets context watching; every member reads it; a
// session reports its own context, and the figures are stored.
func TestPostgresContextWatch(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("own")
	people := map[string]string{}
	for _, role := range []string{"reader", "lead"} {
		people[role] = newID(role) + "@identity-test.example"
		f.member(people[role])
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people[role], "role": role}})
	}
	set := Request{Operation: "context_watch_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "context_watch": map[string]any{
		"enabled": true, "levels": []any{map[string]any{"role": "*", "warn_tokens": 100, "handover_tokens": 200}}}}}
	if _, err := f.as(people["lead"], "sub-lead", set); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("a lead set context watching:", err)
	}
	f.must(f.owner, "sub-owner", set)
	got := f.must(people["reader"], "sub-reader", Request{Operation: "context_watch_get", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	if got["revision"] != int64(1) || !got["context_watch"].(ContextWatch).Enabled {
		t.Fatal(got)
	}
	r, err := f.in("report_context", "own", map[string]any{"tokens": 150, "window": 1000, "level": "ok", "compactions": 2, "observed_at": time.Now().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if out := r.Data.(map[string]any); out["level"] != "warn" || out["compactions"] != int64(1) {
		t.Fatal(out)
	}
	// Read through the fixture's own workspace and project: the test
	// database is shared, and other fixtures name sessions "own" too.
	sessions := f.must(f.owner, "sub-owner", Request{Operation: "list_sessions", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)["sessions"].([]map[string]any)
	var own map[string]any
	for _, s := range sessions {
		if s["name"] == "own" {
			own = s
		}
	}
	if own == nil {
		t.Fatalf("no session own in %d sessions", len(sessions))
	}
	if own["context_tokens"] != int64(150) || own["context_window"] != int64(1000) || own["context_level"] != "warn" || own["context_compactions"] != int64(1) {
		t.Fatalf("%v %v %v %v", own["context_tokens"], own["context_window"], own["context_level"], own["context_compactions"])
	}
}
