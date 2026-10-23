package bus

import (
	"strings"
	"testing"
)

// The engineer is the display name stripped of anything that mentions,
// links or formats in Linear, else the principal, never over 40 runes.
func TestEngineerName(t *testing.T) {
	for _, c := range []struct{ display, principal, want string }{
		{"Ada Lovelace", "ada@example.test", "Ada Lovelace"},
		{"", "ada@example.test", "ada@example.test"},
		{"  \t ", " ada@example.test ", "ada@example.test"},
		{"@ada", "ada@example.test", "ada"},
		{"**Ada** [x](https://evil.test) <b>`y`</b>", "p", "Ada x https evil.test b y b"},
		{"Ada\n\nLovelace\u0007", "p", "Ada Lovelace"},
		{"Ada · Lovelace", "p", "Ada Lovelace"},
		{"@*_[]()<>#`~|!\\{}:/·", "ada@example.test", "ada@example.test"},
		{strings.Repeat("é", 40), "p", strings.Repeat("é", 40)},
		{strings.Repeat("é", 41), "p", strings.Repeat("é", 39) + "…"},
		{"", strings.Repeat("a", 45) + "@example.test", strings.Repeat("a", 39) + "…"},
	} {
		got := EngineerName(c.display, c.principal)
		if got != c.want {
			t.Fatalf("%q/%q: got %q want %q", c.display, c.principal, got, c.want)
		}
		if !ValidEngineer(got) {
			t.Fatalf("%q is not a valid engineer", got)
		}
	}
	for _, bad := range []string{"", " a", "a\nb", "*a*", "a · b", strings.Repeat("a", 41)} {
		if ValidEngineer(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// taskboard_access names the engineer only when asked, and only from the
// authority's records: the argument is a flag, never a name.
func TestTaskboardAccessEngineerComesFromTheAuthority(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set", "", taskboardArgs(0))
	want := EngineerName("", h.s.principal)
	v := h.call("taskboard_access", "a", map[string]any{"ledger": 1, "engineer": 1}).(map[string]any)
	if v["engineer"] != want || len(v) != 7 {
		t.Fatal("engineer", v, want)
	}
	a, e := ParseTaskboardAccess(v)
	if e != nil || a.Engineer != want {
		t.Fatal(a, e)
	}
	for _, args := range []map[string]any{{"ledger": 1}, {"ledger": 1, "engineer": 0}} {
		if v := h.call("taskboard_access", "a", args).(map[string]any); len(v) != 6 || v["engineer"] != nil {
			t.Fatal("engineer without asking", args, v)
		}
	}
	// No session, no engineer.
	if v := h.call("taskboard_access", "", map[string]any{"ledger": 1, "engineer": 1}).(map[string]any); v["engineer"] != nil {
		t.Fatal("engineer without a session", v)
	}
	for _, spoof := range []any{"Mallory", map[string]any{"name": "Mallory"}, true} {
		h.fail("taskboard_access", "a", map[string]any{"ledger": 1, "engineer": spoof})
	}
	// A local store has no display names.
	h.fail("member_display_name_set", "", map[string]any{"display_name": "Mallory"})
}

func TestTaskboardAccessParserChecksTheEngineer(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"workspace_id": "w", "project_id": "p", "session_id": "s", "session_name": "a", "can_write": true, "project_role": "owner"}
	}
	for _, bad := range []any{"", "*Ada*", "a\nb", strings.Repeat("a", 41), 7} {
		m := base()
		m["engineer"] = bad
		if _, e := ParseTaskboardAccess(m); e == nil {
			t.Fatal("accepted engineer", bad)
		}
	}
	m := base()
	m["other"] = "x"
	if _, e := ParseTaskboardAccess(m); e == nil {
		t.Fatal("accepted an unknown seventh field")
	}
	m = base()
	m["engineer"] = "ada@example.test"
	if a, e := ParseTaskboardAccess(m); e != nil || a.Engineer != "ada@example.test" {
		t.Fatal(a, e)
	}
}
