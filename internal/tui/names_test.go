package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// namesSource is the Linear source of a linked board; names-refresh fails
// with err when set.
type namesSource struct {
	testLinearSource
	err error
}

func (s *namesSource) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	if r.Operation == "names-refresh" {
		s.calls = append(s.calls, r)
		return LinearData{}, s.err
	}
	return s.testLinearSource.Do(ctx, r)
}

func (s *namesSource) ops() []string {
	out := []string{}
	for _, c := range s.calls {
		out = append(out, c.Operation)
	}
	return out
}

func namesView(t *testing.T, role string, source *namesSource) Model {
	t.Helper()
	source.binding = bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}
	a := &fakeAuthority{results: map[string]bus.Result{"project_overview": {Source: "server", Data: map[string]any{
		"project": map[string]any{"slug": "core", "role": role},
		"linear": map[string]any{"configured": true, "teams": []any{map[string]any{"id": linearTestTeam, "name": "Eng \x1b[2J", "default": true}},
			"names_updated_at": "2026-10-27T08:00:00Z"},
	}}}}
	cfg := testConfig(a)
	cfg.Linear = source
	cfg.Now = func() time.Time { return time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) }
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(100, 40)
	m, cmd := m.Show(ScreenLinear)
	return quickSettle(m, cmd)
}

// U31, U32: the board by the names stored with it, how old they are, and
// r refreshing them for an owner or lead before the reload.
func TestLinearShowsStoredNamesAndRefreshesThem(t *testing.T) {
	source := &namesSource{}
	m := namesView(t, "owner", source)
	view := Render(m)
	if !strings.Contains(view, `Board: linked · teams Eng \x1b[2J · the team board`) || !strings.Contains(view, "Names as of 2 hours ago") || strings.Contains(view, "r refreshes") {
		t.Fatalf("names before a connection is chosen:\n%s", view)
	}
	m = do(m, "enter")
	if !strings.Contains(Render(m), "Names as of 2 hours ago; r refreshes them from Linear") {
		t.Fatal(Render(m))
	}
	before := len(source.calls)
	m = do(m, "r")
	if ops := source.ops()[before:]; len(ops) != 2 || ops[0] != "names-refresh" || ops[1] != "issues" || source.calls[before].ProfileID != "profile" {
		t.Fatal("r:", ops)
	}
	// Another load doesn't refresh again; only r does.
	before = len(source.calls)
	m = do(m, "esc", "enter")
	for _, op := range source.ops()[before:] {
		if op == "names-refresh" {
			t.Fatal("refreshed without r:", source.ops()[before:])
		}
	}
	source.err = errors.New("Linear said no")
	m = do(m, "r")
	if !strings.Contains(Render(m), "Names not refreshed: Linear said no") || m.status.text != "" {
		t.Fatalf("a failed refresh:\n%s", Render(m))
	}
}

// A member reads the names; r reloads without refreshing them.
func TestLinearNamesRefreshOnlyForOwnerOrLead(t *testing.T) {
	source := &namesSource{}
	m := namesView(t, "member", source)
	m = do(m, "enter", "r")
	for _, op := range source.ops() {
		if op == "names-refresh" {
			t.Fatal("a member refreshed names")
		}
	}
	if view := Render(m); !strings.Contains(view, "teams Eng") || strings.Contains(view, "r refreshes") {
		t.Fatal(view)
	}
}

// Without stored names the board is counted, never listed as IDs.
func TestLinearBoardWithoutNamesIsCounted(t *testing.T) {
	m := Model{linear: linearState{binding: bus.TaskboardBinding{Configured: true, LinearTeamIDs: []string{linearTestTeam, "team-two"}}}}
	if got := linearBoardLine(m); got != "Board: linked · 2 teams · the team board" {
		t.Fatal(got)
	}
	m.linear.stored = map[string]string{linearTestTeam: "Eng"}
	if got := linearBoardLine(m); got != "Board: linked · 2 teams · the team board" {
		t.Fatal("partly named:", got)
	}
	m.linear.stored["team-two"] = "Ops"
	if got := linearBoardLine(m); got != "Board: linked · teams Eng, Ops · the team board" {
		t.Fatal(got)
	}
}
