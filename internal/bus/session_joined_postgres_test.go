package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// FR23 on a hosted store: the notice names the member by their recorded
// display name, never an email; the limit is each member's own; the
// overview's newest joins name no principal.
func TestPostgresSessionJoined(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	f.worker("coord")
	f.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE project_id=? AND name='coord') WHERE id=?`, pid, pid)
	notices := func() []string {
		t.Helper()
		rows, err := f.s.db.QueryContext(ctx, `SELECT m.body FROM messages m JOIN deliveries d ON d.message_id=m.id JOIN sessions r ON r.id=d.recipient_id WHERE m.project_id=$1 AND m.kind='session-joined' AND r.name='coord' ORDER BY m.sequence`, pid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var body string
			_ = rows.Scan(&body)
			out = append(out, strings.SplitN(body, "\n", 2)[0])
		}
		return out
	}
	member := newID("m") + "@identity-test.example"
	f.member(member)
	register := func(principal, name, kind string) {
		t.Helper()
		subject := "sub-" + principal
		if principal == f.owner {
			subject = "sub-owner"
		}
		f.must(principal, subject, Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: name, Args: map[string]any{"instance": name + "-i", "role": "worker", "agent_kind": kind}})
	}
	f.must(member, "sub-"+member, Request{Operation: "member_display_name_set", Workspace: f.workspace, Args: map[string]any{"display_name": "Grace Hopper"}})
	register(member, "joiner", "claude-code")
	register(f.owner, "owner-s", "codex")
	register(member, "joiner", "claude-code") // the same instance again: nothing
	f.exec(`UPDATE members SET display_name='grace@example.test' WHERE principal=?`, member)
	register(member, "joiner-2", "codex")
	want := []string{
		"Session joiner joined: Grace Hopper's claude-code agent, no lanes.",
		"Session owner-s joined: a member's codex agent, no lanes.",
		"Session joiner-2 joined: a member's codex agent, no lanes.",
	}
	if got := notices(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("notices:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The member's sixth registration of the hour is held back; the
	// owner's is not.
	for _, name := range []string{"joiner-3", "joiner-4", "joiner-5", "joiner-6"} {
		register(member, name, "other")
	}
	register(f.owner, "owner-2", "other")
	got := notices()
	if len(got) != 7 || !strings.HasPrefix(got[5], "Session joiner-5 joined") || !strings.HasPrefix(got[6], "Session owner-2 joined") {
		t.Fatalf("rate limit: %v", got)
	}
	if f.audited("join_notice_suppressed", f.sessionIDOf(pid, "joiner-6")) != 1 {
		t.Fatal("no history row for the held-back notice")
	}
	for _, body := range got {
		if strings.Contains(body, "@") || strings.Contains(body, member) || strings.Contains(body, f.owner) {
			t.Fatal("a notice names a principal:", body)
		}
	}
	out := f.must(f.owner, "sub-owner", Request{Operation: "project_overview", Workspace: f.workspace, Project: "p", Args: map[string]any{}})
	joins := out.Data.(map[string]any)["newest_joins"].([]map[string]any)
	raw, _ := json.Marshal(joins)
	if len(joins) != 3 || joins[0]["name"] != "owner-2" || strings.Contains(string(raw), "@") {
		t.Fatalf("newest joins: %s", raw)
	}
}

func (f *pgIdentityFixture) sessionIDOf(pid, name string) string {
	f.t.Helper()
	var id string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT id FROM sessions WHERE project_id=$1 AND name=$2`, pid, name).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}
