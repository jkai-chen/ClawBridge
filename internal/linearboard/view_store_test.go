package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// H1: an agent's taskboard_issues_v2 {view_id} goes through a real store
// as a registered session; every bus read keeps the session's instance.
func TestViewIssuesAsASessionOnSQLite(t *testing.T) {
	store, e := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	filter := `{"priority":{"eq":1}}`
	sum := sha256.Sum256([]byte(filter))
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "ws", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "ws", Args: map[string]any{"slug": "prj"}},
		{Operation: "register_session", Workspace: "ws", Project: "prj", Session: "agent", Args: map[string]any{"instance": "agent-i"}},
		{Operation: "taskboard_set_v2", Workspace: "ws", Project: "prj", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{teamID}, "linear_default_team_id": teamID, "linear_project_ids": []string{projectA}}},
		{Operation: "linear_view_record", Workspace: "ws", Project: "prj", Args: map[string]any{"action": "create", "view_id": vwView, "team_id": teamID, "name": "Urgent bugs", "filter": filter, "filter_sha256": hex.EncodeToString(sum[:]), "filter_as_of": "2026-10-27T21:00:00.000Z", "agent_filter": "available", "expected_binding_revision": 1}},
	} {
		if _, e := store.Execute(ctx, r); e != nil {
			t.Fatal(r.Operation, e)
		}
	}
	s, _, p, _ := viewWriteTest(t)
	s.cfg.Executor = store
	one, three := 1, 3
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{viewRow(viIssue1, &one), viewRow(viIssue2, &three)}, Complete: true}}
	scope := bus.Request{Workspace: "ws", Project: "prj", Session: "agent", Args: map[string]any{"instance": "agent-i"}}
	out, e := s.ExecuteV2(ctx, scope, "issues", map[string]any{"limit": float64(10), "view_id": vwView})
	if page, ok := out.(ViewIssuesPage); e != nil || !ok || len(page.Items) != 1 || page.Items[0].ID != viIssue1 {
		t.Fatal(out, e)
	}
	// Another instance of the session is refused by the bus.
	scope.Args = map[string]any{"instance": "someone-else"}
	if _, e = s.ExecuteV2(ctx, scope, "issues", map[string]any{"limit": float64(10), "view_id": vwView}); e == nil {
		t.Fatal("a wrong instance read the view")
	}
}
