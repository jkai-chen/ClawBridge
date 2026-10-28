package bus

import (
	"path/filepath"
	"testing"
)

// Schema 41 (Q1): a board linked before it starts off, so the upgrade
// begins no writes; a project linked after has no row and takes the
// default, on.
func TestIssueSubscribersSchema40StartsLinkedBoardsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t, s}
	h.call("workspace_init", "", map[string]any{"mode": "local"})
	h.call("project_create", "", map[string]any{"slug": "p"})
	h.call("project_create", "", map[string]any{"slug": "later"})
	h.link(0, []string{cpOne})
	if _, err = s.db.Exec(rolesDowngradeSQLite + kindDowngradeSQLite + `PRAGMA user_version=38;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h = &harness{t, s}
	got := h.call("issue_subscribers_get", "", map[string]any{}).(map[string]any)
	if got["enabled"] != false || got["default"] != false || got["revision"] != 1 || got["on"] != false {
		t.Fatalf("the linked board: %+v", got)
	}
	var n int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM issue_subscribers_policy p JOIN projects j ON j.id=p.project_id WHERE j.slug='later'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("an unlinked project got a row:", n, err)
	}
}
