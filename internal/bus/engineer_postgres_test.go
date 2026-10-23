package bus

import (
	"context"
	"strings"
	"testing"
)

// The marker's engineer is the session owner's recorded display name, which
// only that member sets, falling back to their verified email.
func TestPostgresEngineerIsTheOwnersDisplayNameOrEmail(t *testing.T) {
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
	if got := engineer(); got != EngineerName("", f.owner) {
		t.Fatal("fallback", got)
	}
	out, err := setName(f.owner, "  @Ada **Lovelace**  ")
	if err != nil || out.Data.(map[string]any)["display_name"] != "Ada Lovelace" {
		t.Fatal(out, err)
	}
	if got := engineer(); got != "Ada Lovelace" {
		t.Fatal("display name", got)
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
	// Clearing falls back to the email.
	if _, err = setName(f.owner, ""); err != nil {
		t.Fatal(err)
	}
	if got := engineer(); got != EngineerName("", f.owner) {
		t.Fatal("cleared", got)
	}
	var n int
	if err = f.s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM history WHERE entity='member' AND action='display_name_set' AND entity_key IN ($1,$2) AND changed_by_principal=entity_key`, f.owner, other).Scan(&n); err != nil || n != 4 {
		t.Fatal("audit", n, err)
	}
}
