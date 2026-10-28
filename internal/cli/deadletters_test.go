package cli

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// deadletters lists a session's dead letters, without bodies, and requeues
// or dismisses one.
func TestCLIDeadLetters(t *testing.T) {
	dir := isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "a-one")
	invoke(t, "register", "b", "--instance", "b-one")
	sent := invoke(t, "--session", "a", "--instance", "a-one", "send", "b", "a secret plan", "--operation-id", "one")
	id := sent["id"].(string)
	db, err := sql.Open("sqlite", filepath.Join(dir, "state", "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE deliveries SET dead_at='2026-10-28T00:00:00Z',attempts=5 WHERE message_id=?`, id); err != nil {
		t.Fatal(err)
	}
	list := invoke(t, "--session", "b", "--instance", "b-one", "deadletters", "list")
	rows := list["dead_letters"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["message_id"] != id || rows[0].(map[string]any)["body"] != nil {
		t.Fatal(list)
	}
	// The project owner may read another session's, by name.
	if owned := invoke(t, "--session", "a", "--instance", "a-one", "deadletters", "list", "--target-session", "b"); len(owned["dead_letters"].([]any)) != 1 {
		t.Fatal(owned)
	}
	invoke(t, "--session", "b", "--instance", "b-one", "deadletters", "requeue", id)
	if list = invoke(t, "--session", "b", "--instance", "b-one", "deadletters", "list", "--limit", "5"); len(list["dead_letters"].([]any)) != 0 {
		t.Fatal(list)
	}
	rejectCLI(t, "--session", "b", "--instance", "b-one", "deadletters", "dismiss")
	rejectCLI(t, "--session", "b", "--instance", "b-one", "deadletters", "sideways")
}
