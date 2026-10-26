package bus

import (
	"context"
	"path/filepath"
	"testing"
)

// A nudge resolves the request's workspace and project, by name or ID, to
// a project bound to a Linear board; anything else resolves to nothing.
func TestWatchProjectIDResolvesOnlyABoundProject(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	team := "11111111-1111-4111-8111-111111111111"
	for _, r := range []Request{{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}, {Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}}, {Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "unbound"}}, {Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "team_board", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{}}}} {
		if _, err = s.Execute(context.Background(), r); err != nil {
			t.Fatal(r.Operation, err)
		}
	}
	var pid, wid string
	if err = s.db.QueryRow(`SELECT p.id,p.workspace_id FROM projects p WHERE p.slug='p'`).Scan(&pid, &wid); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ w, p, want string }{{"w", "p", pid}, {wid, pid, pid}, {"w", "unbound", ""}, {"other", "p", ""}, {"w", "missing", ""}} {
		if got, err := s.WatchProjectID(context.Background(), c.w, c.p); err != nil || got != c.want {
			t.Fatal(c, got, err)
		}
	}
}
