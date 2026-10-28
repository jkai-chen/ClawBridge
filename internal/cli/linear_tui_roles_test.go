package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/tui"
)

const (
	tuiRolesTeam  = "00000000-0000-4000-8000-0000000000a1"
	tuiRolesTeam2 = "00000000-0000-4000-8000-0000000000a2"
	tuiRolesDana  = "00000000-0000-4000-8000-0000000000d1"
	tuiRolesAda   = "00000000-0000-4000-8000-0000000000d2"
	tuiRolesNew   = "00000000-0000-4000-8000-0000000000d3"
)

// tuiRolesExecutor answers team_roles_get at revision 3 (Dana leads the
// team, Ada engineers in it, linked by the owner), taskboard_access as
// role, and records each team_roles_set.
type tuiRolesExecutor struct {
	role     string
	requests []bus.Request
}

func (e *tuiRolesExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.requests = append(e.requests, r)
	switch r.Operation {
	case "team_roles_get":
		return bus.Result{Source: "server", Data: map[string]any{"revision": float64(3), "you_lead": []any{tuiRolesTeam},
			"teams":  []any{map[string]any{"team_id": tuiRolesTeam, "leads": []any{tuiRolesDana}, "team_bound": true}},
			"people": []any{map[string]any{"linear_user_id": tuiRolesDana, "role": "lead", "teams": []any{tuiRolesTeam}}, map[string]any{"linear_user_id": tuiRolesAda, "role": "engineer", "teams": []any{tuiRolesTeam}, "principal": "ada@example.test"}}}}, nil
	case "taskboard_access":
		return bus.Result{Source: "server", Data: map[string]any{"project_role": e.role}}, nil
	case "team_roles_set":
		if r.Args["confirm"] == true {
			return bus.Result{Source: "server", Data: map[string]any{"revision": float64(4)}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{"preview": true, "revision": float64(3), "change": map[string]any{"changed": float64(1)}}}, nil
	}
	return bus.Result{}, nil
}

func (e *tuiRolesExecutor) sets() []bus.Request {
	out := []bus.Request{}
	for _, r := range e.requests {
		if r.Operation == "team_roles_set" {
			out = append(out, r)
		}
	}
	return out
}

func tuiRolesSource(t *testing.T, role string) (*linearTUISource, *tuiRolesExecutor) {
	t.Helper()
	old, oldEnv := tuiRolesUsers, closesEnv
	t.Cleanup(func() { tuiRolesUsers, closesEnv = old, oldEnv })
	closesEnv = func() []string { return []string{"HOME=/x"} }
	tuiRolesUsers = func(context.Context, *linearboard.Service, bus.Request) ([]linearprovider.WorkspaceUser, error) {
		u := func(id, name string) linearprovider.WorkspaceUser {
			return linearprovider.WorkspaceUser{ID: id, Name: name, Active: true, ActiveKnown: true, GuestKnown: true}
		}
		return []linearprovider.WorkspaceUser{u(tuiRolesDana, "Dana"), u(tuiRolesAda, "Ada"), u(tuiRolesNew, "New")}, nil
	}
	e := &tuiRolesExecutor{role: role}
	return &linearTUISource{executor: e, scope: bus.Request{Workspace: "ws", Project: "prj", Session: "sess", Args: map[string]any{"instance": "inst"}}}, e
}

// The read carries the mapping, the teams the reader leads, whether they
// own the project, and names; the mapping is read with the TUI's session,
// the project role as the person.
func TestTUIRolesRead(t *testing.T) {
	s, e := tuiRolesSource(t, "owner")
	var d tui.LinearData
	if err := s.rolesView(context.Background(), &linearboard.Service{}, &d); err != nil || d.Roles == nil {
		t.Fatal(err, d.Roles)
	}
	v := d.Roles
	if v.Revision != 3 || !v.Owner || !reflect.DeepEqual(v.YouLead, []string{tuiRolesTeam}) || len(v.People) != 2 || v.People[1].Principal != "ada@example.test" || len(v.Users) != 3 || !v.Teams[0].Bound {
		t.Fatalf("%+v", v)
	}
	if get := e.requests[0]; get.Session != "sess" || get.Args["instance"] != "inst" {
		t.Fatal(get)
	}
	if access := e.requests[1]; access.Operation != "taskboard_access" || access.Session != "" || access.Args["ledger"] != 1 {
		t.Fatal(access)
	}
	// A member isn't the owner; without a connection, names say why.
	s, _ = tuiRolesSource(t, "member")
	d = tui.LinearData{}
	if err := s.rolesView(context.Background(), nil, &d); err != nil || d.Roles.Owner || !strings.Contains(d.Roles.NamesErr, "choose your Linear connection") {
		t.Fatalf("%+v %v", d.Roles, err)
	}
}

// A change is built from the stored mapping at its revision with linear
// roles' own helpers, sent without the session, and the teams and their
// leads are sent back as read for a person's change.
func TestTUIRolesSet(t *testing.T) {
	s, e := tuiRolesSource(t, "owner")
	s.scope.Session = ""
	service := &linearboard.Service{}
	var d tui.LinearData
	c := &tui.RolesChange{Kind: "person", User: tuiRolesAda, Role: "reviewer", Teams: []string{tuiRolesTeam, tuiRolesTeam2}, Revision: 3}
	if err := s.rolesSet(context.Background(), service, c, &d); err != nil || d.RolesSet == nil || !d.RolesSet.Preview || d.RolesSet.Change["changed"] != float64(1) {
		t.Fatalf("%+v %v", d.RolesSet, err)
	}
	set := e.sets()[0]
	mapping := set.Args["mapping"].(map[string]any)
	people := mapping["people"].([]any)
	ada := people[1].(map[string]any)
	if set.Session != "" || set.Args["confirm"] != false || set.Args["expected_revision"] != int64(3) || ada["role"] != "reviewer" || ada["principal"] != "ada@example.test" || len(ada["teams"].([]any)) != 2 {
		t.Fatalf("%+v", set)
	}
	if teams := mapping["teams"].([]any); len(teams) != 1 || teams[0].(map[string]any)["leads"].([]any)[0] != tuiRolesDana {
		t.Fatal("teams not sent back as read:", mapping["teams"])
	}
	// Confirmed, at the same revision: the stored revision comes back.
	c.Confirm = true
	d = tui.LinearData{}
	if err := s.rolesSet(context.Background(), service, c, &d); err != nil || d.RolesSet.Preview || d.RolesSet.Revision != 4 || e.sets()[1].Args["confirm"] != true {
		t.Fatalf("%+v %v", d.RolesSet, err)
	}
	// A lead: withLead, so the new lead joins people as a lead.
	d = tui.LinearData{}
	if err := s.rolesSet(context.Background(), service, &tui.RolesChange{Kind: "lead", Team: tuiRolesTeam, User: tuiRolesNew, Revision: 3}, &d); err != nil {
		t.Fatal(err)
	}
	mapping = e.sets()[2].Args["mapping"].(map[string]any)
	if leads := mapping["teams"].([]any)[0].(map[string]any)["leads"].([]any); len(leads) != 2 || leads[1] != tuiRolesNew {
		t.Fatal(leads)
	}
	// Stopping a lead takes them out of the team's leads.
	if err := s.rolesSet(context.Background(), service, &tui.RolesChange{Kind: "lead", Team: tuiRolesTeam, User: tuiRolesDana, Remove: true, Revision: 3}, &d); err != nil {
		t.Fatal(err)
	}
	if leads := e.sets()[3].Args["mapping"].(map[string]any)["teams"].([]any)[0].(map[string]any)["leads"].([]any); len(leads) != 0 {
		t.Fatal("a removed lead kept:", leads)
	}
	// A person with no team left is removed.
	if err := s.rolesSet(context.Background(), service, &tui.RolesChange{Kind: "person", User: tuiRolesAda, Role: "engineer", Revision: 3}, &d); err != nil {
		t.Fatal(err)
	}
	if people = e.sets()[4].Args["mapping"].(map[string]any)["people"].([]any); len(people) != 1 {
		t.Fatal(people)
	}
}

// Refused before anything is sent: with a session, under an agent, without
// a connection, at a stale revision, or for someone Linear doesn't list.
func TestTUIRolesSetRefusals(t *testing.T) {
	c := &tui.RolesChange{Kind: "person", User: tuiRolesAda, Role: "reviewer", Teams: []string{tuiRolesTeam}, Revision: 3}
	for want, run := range map[string]func(*linearTUISource) error{
		"open the TUI without --session": func(s *linearTUISource) error {
			return s.rolesSet(context.Background(), &linearboard.Service{}, c, &tui.LinearData{})
		},
		"CLAUDECODE says an agent runs this TUI": func(s *linearTUISource) error {
			s.scope.Session = ""
			closesEnv = func() []string { return []string{"CLAUDECODE=1"} }
			return s.rolesSet(context.Background(), &linearboard.Service{}, c, &tui.LinearData{})
		},
		"choose it on the Linear screen first": func(s *linearTUISource) error {
			s.scope.Session = ""
			return s.rolesSet(context.Background(), nil, c, &tui.LinearData{})
		},
		"current revision is 3": func(s *linearTUISource) error {
			s.scope.Session = ""
			stale := *c
			stale.Revision = 2
			return s.rolesSet(context.Background(), &linearboard.Service{}, &stale, &tui.LinearData{})
		},
		"isn't a user of this Linear workspace": func(s *linearTUISource) error {
			s.scope.Session = ""
			stranger := *c
			stranger.User = "00000000-0000-4000-8000-0000000000ff"
			return s.rolesSet(context.Background(), &linearboard.Service{}, &stranger, &tui.LinearData{})
		},
	} {
		s, e := tuiRolesSource(t, "owner")
		if err := run(s); err == nil || !strings.Contains(err.Error(), want) || len(e.sets()) != 0 {
			t.Fatal(want, err, e.sets())
		}
	}
}

// withPersonRole keeps the person's place and account link, dedupes their
// teams, leaves teams and leads alone, and removes a person with none.
func TestWithPersonRole(t *testing.T) {
	m := bus.TeamRoles{Teams: []bus.TeamRoleTeam{{TeamID: tuiRolesTeam, Leads: []string{tuiRolesDana}}},
		People: []bus.TeamRolePerson{{LinearUserID: tuiRolesAda, Role: "engineer", Teams: []string{tuiRolesTeam}, Principal: "ada@example.test"}, {LinearUserID: tuiRolesDana, Role: "lead", Teams: []string{tuiRolesTeam}}}}
	got := withPersonRole(m, tuiRolesAda, "reviewer", []string{tuiRolesTeam, tuiRolesTeam2, tuiRolesTeam})
	want := []bus.TeamRolePerson{{LinearUserID: tuiRolesAda, Role: "reviewer", Teams: []string{tuiRolesTeam, tuiRolesTeam2}, Principal: "ada@example.test"}, m.People[1]}
	if !reflect.DeepEqual(got.People, want) || !reflect.DeepEqual(got.Teams, m.Teams) {
		t.Fatalf("%+v", got)
	}
	if got = withPersonRole(m, tuiRolesNew, "engineer", []string{tuiRolesTeam}); len(got.People) != 3 || got.People[2].LinearUserID != tuiRolesNew {
		t.Fatalf("%+v", got.People)
	}
	if got = withPersonRole(m, tuiRolesAda, "engineer", nil); len(got.People) != 1 || got.People[0].LinearUserID != tuiRolesDana {
		t.Fatalf("%+v", got.People)
	}
}
