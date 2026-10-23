package bus

import (
	"strings"
	"testing"
)

// The engineer is the display name stripped of anything that mentions,
// links or formats in Linear, never over 40 runes; without one it is no
// one, never an email.
func TestEngineerName(t *testing.T) {
	for _, c := range []struct{ display, want string }{
		{"Ada Lovelace", "Ada Lovelace"},
		{"", ""},
		{"  \t ", ""},
		{"@ada", "ada"},
		{"ada@example.test", "ada example.test"},
		{"**Ada** [x](https://evil.test) <b>`y`</b>", "Ada x https evil.test b y b"},
		{"Ada\n\nLovelace\u0007", "Ada Lovelace"},
		{"Ada · Lovelace", "Ada Lovelace"},
		{"@*_[]()<>#`~|!\\{}:/·", ""},
		{strings.Repeat("é", 40), strings.Repeat("é", 40)},
		{strings.Repeat("é", 41), strings.Repeat("é", 39) + "…"},
	} {
		got := EngineerName(c.display)
		if got != c.want {
			t.Fatalf("%q: got %q want %q", c.display, got, c.want)
		}
		if got != "" && !ValidEngineer(got) {
			t.Fatalf("%q is not a valid engineer", got)
		}
	}
	for _, bad := range []string{"", " a", "a\nb", "*a*", "a · b", strings.Repeat("a", 41)} {
		if ValidEngineer(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// A local store has no verified principal, only the OS user, so it names
// no engineer; the argument is a flag, never a name.
func TestLocalTaskboardAccessNamesNoEngineer(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set", "", taskboardArgs(0))
	for _, args := range []map[string]any{{"ledger": 1, "engineer": 1}, {"ledger": 1}, {"ledger": 1, "engineer": 0}} {
		v := h.call("taskboard_access", "a", args).(map[string]any)
		if len(v) != 6 || v["engineer"] != nil {
			t.Fatal("local engineer", args, v)
		}
		if a, e := ParseTaskboardAccess(v); e != nil || a.Engineer != "" {
			t.Fatal(a, e)
		}
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

// A note renders the frozen engineer only when there is one, so a row from
// before schema 11 posts the same bytes as before.
func TestMarkerBodyEngineer(t *testing.T) {
	row := map[string]any{"assignment_id": "asg_1", "session_name": "worker", "phase": "claim"}
	old := markerBody(row)
	if !strings.HasPrefix(old, "🤖 Claimed by ClawBridge agent session worker · ref cb-") {
		t.Fatal(old)
	}
	row["engineer"] = ""
	if markerBody(row) != old {
		t.Fatal("an empty engineer changed the note")
	}
	row["engineer"] = "Ada Lovelace"
	if got := markerBody(row); got != strings.Replace(old, "worker · ref", "worker · Ada Lovelace · ref", 1) {
		t.Fatal(got)
	}
	row["phase"], row["reason"] = "close", "released"
	if got := markerBody(row); !strings.HasPrefix(got, "🤖 Released by ClawBridge agent session worker · Ada Lovelace (") {
		t.Fatal(got)
	}
}
