package bus

import (
	"context"
	"strings"
	"testing"
)

// The marker's engineer is the session owner's recorded display name, which
// only that member sets; without one the marker names no one, never their
// email.
func TestPostgresEngineerIsTheOwnersRecordedName(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("w")
	engineer := func() any {
		t.Helper()
		out, err := f.in("taskboard_access", "w", map[string]any{"ledger": 1, "engineer": 1})
		if err != nil {
			t.Fatal(err)
		}
		return out.Data.(map[string]any)["engineer"]
	}
	setName := func(principal, name string) (Result, error) {
		subject := "sub-" + principal
		if principal == f.owner {
			subject = "sub-owner"
		}
		return f.as(principal, subject, Request{Operation: "member_display_name_set", Workspace: f.workspace, Args: map[string]any{"display_name": name}})
	}
	if got := engineer(); got != nil {
		t.Fatal("an engineer without a recorded name", got)
	}
	// Only when asked, and only for a session.
	for _, r := range []Request{
		{Operation: "taskboard_access", Workspace: f.workspace, Project: "p", Session: "w", Args: map[string]any{"instance": "w-i", "ledger": 1, "engineer": 0}},
		{Operation: "taskboard_access", Workspace: f.workspace, Project: "p", Args: map[string]any{"ledger": 1, "engineer": 1}},
	} {
		if out := f.must(f.owner, "sub-owner", r); out.Data.(map[string]any)["engineer"] != nil {
			t.Fatal("engineer unasked or without a session", r.Args, out.Data)
		}
	}
	out, err := setName(f.owner, "  @Ada **Lovelace**  ")
	if err != nil || out.Data.(map[string]any)["display_name"] != "Ada Lovelace" {
		t.Fatal(out, err)
	}
	if got := engineer(); got != "Ada Lovelace" {
		t.Fatal("display name", got)
	}
	// A name captured at connect keeps the one already recorded.
	out, err = f.as(f.owner, "sub-owner", Request{Operation: "member_display_name_set", Workspace: f.workspace, Args: map[string]any{"display_name": "Augusta", "if_unset": 1}})
	if err != nil || out.Data.(map[string]any)["kept"] != true || engineer() != "Ada Lovelace" {
		t.Fatal("capture replaced the override", out, err)
	}
	// Another member names only themselves.
	other := newID("m") + "@identity-test.example"
	f.member(other)
	if _, err = setName(other, "Mallory"); err != nil {
		t.Fatal(err)
	}
	if got := engineer(); got != "Ada Lovelace" {
		t.Fatal("another member renamed the owner", got)
	}
	if _, err = setName(newID("x")+"@identity-test.example", "Eve"); err == nil {
		t.Fatal("a non-member set a display name")
	}
	if _, err = setName(f.owner, strings.Repeat("a", 201)); err == nil {
		t.Fatal("an over-long name was accepted")
	}
	if _, err = setName(f.owner, strings.Repeat("a", 60)); err != nil {
		t.Fatal(err)
	}
	if got := engineer(); got != strings.Repeat("a", 39)+"…" {
		t.Fatal("cap", got)
	}
	// Clearing names no one again, never the email.
	if _, err = setName(f.owner, ""); err != nil {
		t.Fatal(err)
	}
	if got := engineer(); got != nil {
		t.Fatal("cleared", got)
	}
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "member_display_name_set", Workspace: f.workspace, Args: map[string]any{"display_name": "Augusta", "if_unset": 1}}); err != nil || engineer() != "Augusta" {
		t.Fatal("capture into an empty name", err)
	}
	var n int
	if err = f.s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM history WHERE entity='member' AND action='display_name_set' AND entity_key IN ($1,$2) AND changed_by_principal=entity_key`, f.owner, other).Scan(&n); err != nil || n != 5 {
		t.Fatal("audit", n, err)
	}
}

// Claim and release notes name the engineer frozen at confirm: a later
// rename or the owner losing access changes neither note.
func TestPostgresClaimAndReleaseNotesFreezeTheEngineer(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("coord")
	member := newID("m") + "@identity-test.example"
	f.member(member)
	f.must(member, "sub-member", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "member", Args: map[string]any{"instance": "member-i"}})
	rename := func(name string) {
		t.Helper()
		f.must(member, "sub-member", Request{Operation: "member_display_name_set", Workspace: f.workspace, Args: map[string]any{"display_name": name}})
	}
	rename("Ada Lovelace")
	in := func(op string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.in(op, "coord", a)
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	in("coordinator_set", map[string]any{"target_session": "coord"})
	proposed := f.must(member, "sub-member", Request{Operation: "assignment_propose", Workspace: f.workspace, Project: "p", Session: "member", Args: map[string]any{"instance": "member-i", "issue_uuid": watchIssue, "issue_identifier": "FIX-1"}})
	view := proposed.Data.(map[string]any)["assignment"].(map[string]any)
	in("assignment_confirm", map[string]any{"target_session": "member", "revision": view["revision"]})
	rename("Augusta King")
	g := in("marker_go", map[string]any{"assignment_id": view["id"], "phase": "claim", "part": "comment"})
	if body, _ := g["body"].(string); !strings.HasPrefix(body, "🤖 Claimed by ClawBridge agent session member · Ada Lovelace · ref cb-") {
		t.Fatal("claim note", g)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member}})
	var engineer string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT engineer FROM assignment_markers WHERE assignment_id=$1 AND phase='close'`, view["id"]).Scan(&engineer); err != nil || engineer != "Ada Lovelace" {
		t.Fatal("close engineer", engineer, err)
	}
}
