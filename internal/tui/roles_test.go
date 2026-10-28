package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	rolesTeamDropped = "33333333-3333-4333-8333-333333333333"
	rolesDana        = "d0000000-0000-4000-8000-000000000001"
	rolesAda         = "d0000000-0000-4000-8000-000000000002"
	rolesGrace       = "d0000000-0000-4000-8000-000000000003"
	rolesGhost       = "d0000000-0000-4000-8000-000000000004"
	rolesHopper      = "d0000000-0000-4000-8000-000000000005"
	rolesGuest       = "d0000000-0000-4000-8000-000000000006"
)

func rolesUser(id, name string, eligible bool) linearprovider.WorkspaceUser {
	return linearprovider.WorkspaceUser{ID: id, Name: name, Active: eligible, ActiveKnown: true, GuestKnown: true}
}

// rolesSource answers the board (two bound teams) and team roles: Dana
// leads the first team, Ada engineers in it (linked by the owner), Grace
// reviews in both, Ghost (no longer active) engineers in the second; the
// mapping still names a team the binding dropped. A change is previewed
// with counts, then stored at the next revision.
type rolesSource struct {
	testLinearSource
	owner    bool
	youLead  []string
	noNames  bool
	setErr   error
	revision int64
	// danaAlsoInTwo puts Dana, who leads the first team, in the second too.
	danaAlsoInTwo bool
	sets     []RolesChange
}

func (s *rolesSource) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	switch r.Operation {
	case "roles":
		s.calls = append(s.calls, r)
		v := TeamRolesView{Revision: s.revision, Owner: s.owner, YouLead: s.youLead,
			Teams: []RolesTeam{{TeamID: linearTestTeam, Leads: []string{rolesDana}, Bound: true}, {TeamID: rolesTeamDropped, Leads: []string{}, Bound: false}},
			People: []bus.TeamRolePerson{
				{LinearUserID: rolesDana, Role: "lead", Teams: s.danaTeams()},
				{LinearUserID: rolesAda, Role: "engineer", Teams: []string{linearTestTeam}, Principal: "ada@example.test"},
				{LinearUserID: rolesGrace, Role: "reviewer", Teams: []string{linearTestTeam, viewsTeamTwo}},
				{LinearUserID: rolesGhost, Role: "engineer", Teams: []string{viewsTeamTwo}},
			}}
		if s.noNames {
			v.NamesErr = "choose your Linear connection on the Linear screen first"
		} else {
			v.Users = []linearprovider.WorkspaceUser{rolesUser(rolesDana, "Dana Example", true), rolesUser(rolesAda, "Ada Lovelace", true),
				rolesUser(rolesGrace, "Grace \x1b[2JHopper", true), rolesUser(rolesGhost, "Ghost", false), rolesUser(rolesHopper, "Hopper Next", true),
				{ID: rolesGuest, Name: "A Guest", Active: true, ActiveKnown: true, Guest: true, GuestKnown: true}}
		}
		return LinearData{Roles: &v}, nil
	case "roles-set":
		s.calls = append(s.calls, r)
		s.sets = append(s.sets, *r.Roles)
		if s.setErr != nil {
			return LinearData{}, s.setErr
		}
		if !r.Roles.Confirm {
			return LinearData{RolesSet: &RolesSetResult{Preview: true, Revision: s.revision, Change: map[string]any{"added": float64(0), "removed": float64(0), "changed": float64(1), "teams_changed": float64(0)}}}, nil
		}
		s.revision++
		return LinearData{RolesSet: &RolesSetResult{Revision: s.revision}}, nil
	}
	d, err := s.testLinearSource.Do(ctx, r)
	d.BindingV2 = &bus.TaskboardBindingV2{MappingVersion: 2, Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam, viewsTeamTwo}, LinearDefaultTeamID: linearTestTeam}
	return d, err
}

func (s *rolesSource) danaTeams() []string {
	if s.danaAlsoInTwo {
		return []string{linearTestTeam, viewsTeamTwo}
	}
	return []string{linearTestTeam}
}

func (s *rolesSource) reads() int {
	n := 0
	for _, c := range s.calls {
		if c.Operation == "roles" {
			n++
		}
	}
	return n
}

func rolesOpen(t *testing.T, source *rolesSource, edit func(*Config)) Model {
	t.Helper()
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	// A person's TUI: roles are never changed with a session.
	cfg.Session = ""
	if edit != nil {
		edit(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(100, 40).Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if !strings.Contains(footerText(m), "R team roles") {
		t.Fatal("no R on the board:", footerText(m))
	}
	return do(m, "R")
}

// rolesSelect moves to the row named row.
func rolesSelect(t *testing.T, m Model, row string) Model {
	t.Helper()
	for i, r := range m.rolesRows() {
		if r == row {
			m.selected = i
			return m
		}
	}
	t.Fatalf("no row %s in %v", row, m.rolesRows())
	return m
}

// Per bound team, in the binding's order: its lead, then engineers and
// reviewers, by name, escaped; a team the binding dropped is marked; a
// user Linear doesn't call active is marked; a person the owner linked
// shows the account to the owner only.
func TestRolesShowsEachTeam(t *testing.T) {
	source := &rolesSource{owner: true, revision: 3}
	m := rolesOpen(t, source, nil)
	view := Render(m)
	for _, want := range []string{"Team roles: who leads each team", "Revision 3 · you can change: leads and everyone's roles (project owner)",
		"Lead      Dana Example", "Engineer  Ada Lovelace · linked by the owner (ada@example.test)", `Reviewer  Grace \x1b[2JHopper`,
		"Engineer  Ghost (inactive or a guest)", "(no longer bound; ignored)", "No lead."} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if strings.Index(view, "Ada Lovelace") > strings.Index(view, "Grace") {
		t.Fatal("engineers before reviewers:", view)
	}
	// Grace works in both bound teams, so shows under each.
	if strings.Count(view, "Grace") != 2 {
		t.Fatal("Grace under each of her teams:", view)
	}
	if m = do(m, "r"); source.reads() != 2 {
		t.Fatal("r didn't read again")
	}
	if back := do(m, "esc"); back.linear.step != "tasks" {
		t.Fatal("esc:", back.linear.step)
	}
}

// U2: anyone but the owner sees only that a person is linked, never the
// account; without names, people show by ID and the view says why.
func TestRolesLinkShownToTheOwnerOnly(t *testing.T) {
	m := rolesOpen(t, &rolesSource{}, nil)
	view := Render(m)
	if strings.Contains(view, "ada@example.test") || !strings.Contains(view, "Ada Lovelace · linked") || !strings.Contains(view, "you can change: nothing (read-only)") {
		t.Fatal(view)
	}
	m = rolesOpen(t, &rolesSource{noNames: true}, nil)
	if view = Render(m); !strings.Contains(view, "People show by ID: choose your Linear connection") || !strings.Contains(view, rolesAda) {
		t.Fatal(view)
	}
}

// rolesPickerText is the picker's part of the view.
func rolesPickerText(view string) string {
	_, after, _ := strings.Cut(view, "Choose ")
	return after
}

// The owner: l adds a lead from the workspace's active, non-guest users
// (not the team's leads); the server previews it, y stores it at that
// revision, and the view is read again.
func TestRolesOwnerSetsALead(t *testing.T) {
	source := &rolesSource{owner: true, revision: 3}
	m := rolesSelect(t, rolesOpen(t, source, nil), "team:"+linearTestTeam+":")
	for _, k := range []string{"l add a lead", "a add an engineer or reviewer"} {
		if !strings.Contains(footerText(m), k) {
			t.Fatal("no", k, "in", footerText(m))
		}
	}
	// e and x are for a person's row (x also a lead's), never a team's; e
	// never on a lead.
	if f := footerText(m); strings.Contains(f, "e engineer") || strings.Contains(f, "x remove") {
		t.Fatal("e or x on a team row:", f)
	}
	if f := footerText(rolesSelect(t, m, "lead:"+linearTestTeam+":"+rolesDana)); strings.Contains(f, "e engineer") || !strings.Contains(f, "x remove") {
		t.Fatal("on a lead row:", f)
	}
	m = do(m, "l")
	picker := rolesPickerText(Render(m))
	if !strings.HasPrefix(picker, "a lead for") || strings.Contains(picker, "› Dana") || strings.Contains(picker, "Ghost") || strings.Contains(picker, "A Guest") {
		t.Fatal("the picker offers the team's lead, an inactive user or a guest:", picker)
	}
	// Sorted by name: Ada, Grace, Hopper.
	m = do(m, "down", "down", "enter")
	if len(source.sets) != 1 || !reflect.DeepEqual(source.sets[0], RolesChange{Kind: "lead", Team: linearTestTeam, User: rolesHopper, Revision: 3}) {
		t.Fatalf("%+v", source.sets)
	}
	if m.roles.ask == nil || !strings.Contains(Render(m), "Hopper Next leads") || !strings.Contains(Render(m), "preview: 1 changed.") {
		t.Fatal(Render(m))
	}
	m = do(m, "y")
	if len(source.sets) != 2 || !source.sets[1].Confirm || source.sets[1].Revision != 3 || source.reads() != 2 || !strings.Contains(m.status.text, "saved at revision 4") {
		t.Fatalf("%+v %d %q", source.sets, source.reads(), m.status.text)
	}
	// Any key but y keeps things as they are.
	m = do(rolesSelect(t, m, "lead:"+linearTestTeam+":"+rolesDana), "x")
	if !strings.Contains(Render(m), "Dana Example stops leading") {
		t.Fatal(Render(m))
	}
	if m = do(m, "n"); len(source.sets) != 3 || m.roles.ask != nil || !strings.Contains(m.status.text, "Nothing was changed") {
		t.Fatalf("%+v %q", source.sets, m.status.text)
	}
}

// a adds someone to the team with a role (e or r); e switches a person
// between engineer and reviewer; x takes a person out of the team, or out
// of team roles when it's their only team.
func TestRolesOwnerChangesPeople(t *testing.T) {
	source := &rolesSource{owner: true, revision: 3}
	m := rolesSelect(t, rolesOpen(t, source, nil), "team:"+viewsTeamTwo+":")
	m = do(m, "a")
	picker := rolesPickerText(Render(m))
	// Not Dana (a lead), not Grace (in the team already).
	if strings.Contains(picker, "› Dana") || strings.Contains(picker, "  Dana") || strings.Contains(picker, "Grace") {
		t.Fatal(picker)
	}
	m = do(m, "enter", "r")
	got := source.sets[len(source.sets)-1]
	if got.Kind != "person" || got.User != rolesAda || got.Role != "reviewer" || strings.Join(got.Teams, ",") != linearTestTeam+","+viewsTeamTwo {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(Render(m), "Ada Lovelace joins") || !strings.Contains(Render(m), "(was engineer)") {
		t.Fatal(Render(m))
	}
	m = do(m, "esc")
	m = do(rolesSelect(t, m, "person:"+linearTestTeam+":"+rolesAda), "e")
	if got = source.sets[len(source.sets)-1]; got.Role != "reviewer" || strings.Join(got.Teams, ",") != linearTestTeam {
		t.Fatalf("%+v", got)
	}
	m = do(m, "esc")
	m = do(rolesSelect(t, m, "person:"+viewsTeamTwo+":"+rolesGrace), "x")
	if got = source.sets[len(source.sets)-1]; got.Role != "reviewer" || strings.Join(got.Teams, ",") != linearTestTeam || !strings.Contains(Render(m), "stops working in") {
		t.Fatalf("%+v", got)
	}
	m = do(m, "esc")
	m = do(rolesSelect(t, m, "person:"+linearTestTeam+":"+rolesAda), "x")
	if got = source.sets[len(source.sets)-1]; len(got.Teams) != 0 || !strings.Contains(Render(m), "is their only team") {
		t.Fatalf("%+v %s", got, Render(m))
	}
}

// A team's lead changes engineers and reviewers in the teams they lead,
// and nothing else: no l, no x on a lead, nothing in another team. The
// server is the authority; these keys only leave out what it refuses.
func TestRolesALeadEditsTheirTeam(t *testing.T) {
	source := &rolesSource{youLead: []string{linearTestTeam}, revision: 3}
	m := rolesOpen(t, source, nil)
	if !strings.Contains(Render(m), "you can change: engineers and reviewers in") {
		t.Fatal(Render(m))
	}
	m = rolesSelect(t, m, "person:"+linearTestTeam+":"+rolesAda)
	for _, k := range []string{"a add", "e engineer ⇄ reviewer", "x remove"} {
		if !strings.Contains(footerText(m), k) {
			t.Fatal("no", k, "in", footerText(m))
		}
	}
	if strings.Contains(footerText(m), "l add a lead") {
		t.Fatal("a lead offered l:", footerText(m))
	}
	m = do(rolesSelect(t, m, "lead:"+linearTestTeam+":"+rolesDana), "x")
	if len(source.sets) != 0 || strings.Contains(footerText(m), "x remove") {
		t.Fatalf("x on a lead for a lead: %+v %s", source.sets, footerText(m))
	}
	m = do(rolesSelect(t, m, "person:"+viewsTeamTwo+":"+rolesGhost), "e")
	if len(source.sets) != 0 {
		t.Fatal("an edit in a team they don't lead:", source.sets)
	}
	m = do(rolesSelect(t, m, "person:"+linearTestTeam+":"+rolesAda), "e")
	if len(source.sets) != 1 || source.sets[0].Role != "reviewer" {
		t.Fatalf("%+v", source.sets)
	}
	// The server's refusal is shown as it says it.
	source.setErr = errors.New("forbidden: a team's lead sets engineer and reviewer roles only in the teams they lead")
	m = do(m, "esc")
	m = do(rolesSelect(t, m, "person:"+linearTestTeam+":"+rolesGrace), "e")
	if !strings.Contains(m.status.text, "only in the teams they lead") || m.roles.ask != nil {
		t.Fatal(m.status.text)
	}
}

// Read-only for anyone else, and for a TUI with a session or run by an
// agent, which says why.
func TestRolesReadOnly(t *testing.T) {
	for name, c := range map[string]struct {
		source *rolesSource
		cfg    func(*Config)
		want   string
	}{
		"member":  {&rolesSource{}, nil, "you can change: nothing (read-only)"},
		"session": {&rolesSource{owner: true}, func(c *Config) { c.Session = "sess" }, "open the TUI without --session"},
		"agent":   {&rolesSource{owner: true}, func(c *Config) { c.AgentMarker = "CLAUDECODE" }, "CLAUDECODE says an agent runs this TUI"},
	} {
		m := rolesSelect(t, rolesOpen(t, c.source, c.cfg), "person:"+linearTestTeam+":"+rolesAda)
		for _, k := range []string{"l add a lead", "a add", "e engineer", "x remove"} {
			if strings.Contains(footerText(m), k) {
				t.Fatal(name, "offered", k)
			}
		}
		// The header says why; no key sends anything.
		if !strings.Contains(strings.ReplaceAll(Render(m), "\n", " "), c.want) && !strings.Contains(Render(m), c.want) {
			t.Fatalf("%s: no %q in\n%s", name, c.want, Render(m))
		}
		for _, k := range []string{"l", "a", "e", "x"} {
			if m = do(m, k); len(c.source.sets) != 0 {
				t.Fatalf("%s %s: %+v", name, k, c.source.sets)
			}
		}
	}
}

// A change made meanwhile: the server's conflict says to read again, and
// nothing is stored.
func TestRolesConflictReadsAgain(t *testing.T) {
	source := &rolesSource{owner: true, revision: 3, setErr: errors.New("conflict: team roles changed: current revision is 4; read them again")}
	m := do(rolesSelect(t, rolesOpen(t, source, nil), "person:"+linearTestTeam+":"+rolesAda), "e")
	if m.roles.ask != nil || !strings.Contains(m.status.text, "r reads them again, and nothing was changed") {
		t.Fatal(m.status.text)
	}
}

// A lead who works in a team without leading it (stopped leading it, or
// leads another) shows there, marked, so x can take them out; e, for
// engineers and reviewers, isn't offered. When the server refuses a change
// because a team still lists them as its lead, the status says what to do.
func TestRolesLeadNotLeadingATeam(t *testing.T) {
	source := &rolesSource{owner: true, revision: 3, danaAlsoInTwo: true}
	m := rolesOpen(t, source, nil)
	if view := Render(m); !strings.Contains(view, "Lead      Dana Example · not leading this team") || strings.Count(view, "Dana Example") != 2 {
		t.Fatal(view)
	}
	m = rolesSelect(t, m, "person:"+viewsTeamTwo+":"+rolesDana)
	if f := footerText(m); strings.Contains(f, "e engineer") || !strings.Contains(f, "x remove") {
		t.Fatal(f)
	}
	m = do(m, "x")
	if got := source.sets[0]; got.Role != "lead" || strings.Join(got.Teams, ",") != linearTestTeam {
		t.Fatalf("%+v", got)
	}
	m = do(m, "esc")
	source.setErr = errors.New("invalid: a team's lead is listed under people with role lead in that team")
	m = do(rolesSelect(t, m, "person:"+viewsTeamTwo+":"+rolesDana), "x")
	if !strings.Contains(m.status.text, "stop them leading first (x on their Lead row)") {
		t.Fatal(m.status.text)
	}
}

// U1: each key the Linear screen offers is listed once in its help.
func TestRolesHelpListsEachKeyOnce(t *testing.T) {
	m := rolesOpen(t, &rolesSource{owner: true}, nil)
	m.width = 120
	seen := map[string]int{}
	for _, line := range strings.Split(renderHelp(m), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			if f := strings.Fields(line); len(f) > 1 {
				seen[f[0]]++
			}
		}
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("%q listed %d times in\n%s", k, n, renderHelp(m))
		}
	}
	if seen["R"] != 1 || seen["l"] != 1 || seen["a"] != 1 {
		t.Fatal(seen)
	}
}
