package linearboard

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const addLead = "d1000000-0000-4000-8000-0000000000d1"

// teamAdds is the server side of an add, as the fake authority answers it.
type teamAdds struct {
	*adminAuthority
	reserves, settles []map[string]any
	replay            map[string]any
}

func (t *teamAdds) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "project_team_add_reserve":
		t.reserves = append(t.reserves, r.Args)
		if t.replay != nil {
			return bus.Result{Source: "local-authority", Data: t.replay}, nil
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "reserved", "by": "owner", "lane": "", "replay": false}}, nil
	case "project_team_add_settle":
		t.settles = append(t.settles, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"]}}, nil
	}
	return t.adminAuthority.Execute(ctx, r)
}

// adderFake adds teams to projectA: the snapshot reads its teams and lead,
// the add appends the team unless ignore is set, and leadAfter clears the
// lead with it; readErr fails from the third read on (the confirm's
// read-back after a preview).
type adderFake struct {
	*adminFake
	teams             []string
	lead              string
	reason            string
	adds              []string
	sendErr, readErr  error
	ignore, leadAfter bool
	reads             int
	// appearAt hides an added team until that read (Linear's reads lag);
	// leadGone makes reads after the add leave the lead out.
	appearAt int
	leadGone bool
	added    string
	// users is the workspace listing; me the viewer's ID.
	users []linearprovider.WorkspaceUser
	me    string
}

func (p *adderFake) ProjectTeamAddReason() string { return p.reason }
func (p *adderFake) Viewer(context.Context) (linearprovider.Viewer, error) {
	return linearprovider.Viewer{ID: p.me}, nil
}
func (p *adderFake) WorkspaceUsers(context.Context, linearprovider.PageOptions) (linearprovider.Page[linearprovider.WorkspaceUser], error) {
	return linearprovider.Page[linearprovider.WorkspaceUser]{Items: p.users, Complete: true}, nil
}
func (p *adderFake) AddProjectTeam(_ context.Context, project, team string) error {
	p.adds = append(p.adds, project+" "+team)
	if !p.ignore {
		p.teams = append(p.teams, team)
		p.added = team
	}
	if p.leadAfter {
		p.lead = ""
	}
	return p.sendErr
}
func (p *adderFake) ProjectSnapshot(_ context.Context, project string) (linearprovider.ProjectSnapshot, error) {
	p.reads++
	if p.reads > 2 && p.readErr != nil {
		return linearprovider.ProjectSnapshot{}, p.readErr
	}
	teams := []string{}
	for _, t := range p.teams {
		if t != p.added || p.reads >= p.appearAt {
			teams = append(teams, t)
		}
	}
	return linearprovider.ProjectSnapshot{ID: project, TeamIDs: teams, Lead: p.lead, LeadKnown: !(p.leadGone && p.added != ""), MembersKnown: true}, nil
}

func teamAddTest(t *testing.T) (*Service, *teamAdds, *adderFake, bus.Request) {
	t.Helper()
	s, a, p, scope := adminTest(t)
	auth := &teamAdds{adminAuthority: a}
	fake := &adderFake{adminFake: p, teams: []string{teamID}, lead: addLead}
	s.cfg.Executor = auth
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return fake, nil }
	scope.Session, a.actor, a.name = "", "", ""
	return s, auth, fake, scope
}

func preview(t *testing.T, s *Service, scope bus.Request) ProjectTeamAddResult {
	t.Helper()
	r, e := s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: projectA, TeamID: otherTeamID})
	if e != nil || r.Outcome != "preview" {
		t.Fatal(r, e)
	}
	return r
}

func confirm(s *Service, scope bus.Request, plan string) (ProjectTeamAddResult, error) {
	return s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: projectA, TeamID: otherTeamID, OperationID: "t-1", Confirm: true, ExpectedPlanSHA256: plan})
}

// The preview reads the project, says what is sent, and sends nothing;
// the confirm reserves first, sends the add, reads back before settling,
// and settles accepted.
func TestAddProjectTeamPreviewThenConfirm(t *testing.T) {
	s, a, p, scope := teamAddTest(t)
	r := preview(t, s, scope)
	if len(p.adds) != 0 || len(a.reserves) != 0 || !reflect.DeepEqual(r.Plan.Sends, []string{"id", "addTeams"}) || !reflect.DeepEqual(r.Plan.Teams, []string{teamID}) || !strings.Contains(r.Note, "the added team's own lead") {
		t.Fatal(r, p.adds, a.reserves)
	}
	got, e := confirm(s, scope, r.Plan.PlanSHA256)
	if e != nil || got.Outcome != "accepted" || len(got.Changed) != 0 || got.By != "owner" {
		t.Fatal(got, e)
	}
	if len(p.adds) != 1 || p.adds[0] != projectA+" "+otherTeamID || p.reads != 3 {
		t.Fatal(p.adds, p.reads)
	}
	res := a.reserves[0]
	if res["linear_project"] != projectA || res["team_id"] != otherTeamID || !reflect.DeepEqual(res["project_teams"], []string{teamID}) || res["plan_sha256"] != r.Plan.PlanSHA256 || res["instance"] == nil {
		t.Fatal(res)
	}
	if len(a.settles) != 1 || a.settles[0]["outcome"] != "accepted" || len(a.settles[0]["changed"].([]string)) != 0 {
		t.Fatal(a.settles)
	}
	// Now there: nothing is sent or reserved again.
	again, e := confirm(s, scope, r.Plan.PlanSHA256)
	if e != nil || again.Outcome != "already" || len(p.adds) != 1 || len(a.reserves) != 1 {
		t.Fatal(again, e)
	}
}

// Linear changing the lead with the add is accepted_with_changes, named
// and settled with the field name. A team the first read-back misses is
// read again after 2 s and 4 s more (F1): one that appears is accepted;
// one that never does is uncertain, unless Linear refused the add, which
// is not_sent with no wait. An unreadable read-back is uncertain; a lead
// a read leaves out is unverified (F2).
func TestAddProjectTeamReadBackOutcomes(t *testing.T) {
	rejected := &linearprovider.Error{Kind: "provider_rejected"}
	for name, c := range map[string]struct {
		set        func(*adderFake)
		outcome    string
		settle     string
		changed    []string
		unverified []string
		waits      int
	}{
		"lead":    {func(p *adderFake) { p.leadAfter = true }, "accepted_with_changes", "accepted", []string{"lead"}, nil, 0},
		"refused": {func(p *adderFake) { p.ignore, p.sendErr = true, rejected }, "not_sent", "not_sent", nil, nil, 0},
		"not sent": {func(p *adderFake) {
			p.ignore, p.sendErr = true, &linearprovider.Error{Kind: "unavailable", NotSent: true}
		}, "not_sent", "not_sent", nil, nil, 0},
		"no error, never appears":      {func(p *adderFake) { p.ignore = true }, "uncertain", "uncertain", nil, nil, 2},
		"transport error, never shows": {func(p *adderFake) { p.ignore, p.sendErr = true, &linearprovider.Error{Kind: "unavailable"} }, "uncertain", "uncertain", nil, nil, 2},
		"appears on the 2nd read":      {func(p *adderFake) { p.appearAt = 4 }, "accepted", "accepted", nil, nil, 1},
		"appears on the 3rd read":      {func(p *adderFake) { p.appearAt = 5 }, "accepted", "accepted", nil, nil, 2},
		"unreadable":                   {func(p *adderFake) { p.readErr = errors.New("timeout") }, "uncertain", "uncertain", nil, nil, 2},
		"lead unread":                  {func(p *adderFake) { p.leadGone = true }, "accepted", "accepted", nil, []string{"lead"}, 0},
	} {
		s, a, p, scope := teamAddTest(t)
		var waited []time.Duration
		old := teamAddWait
		teamAddWait = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
		r := preview(t, s, scope)
		c.set(p)
		got, _ := confirm(s, scope, r.Plan.PlanSHA256)
		teamAddWait = old
		if got.Outcome != c.outcome || len(c.changed) > 0 && !reflect.DeepEqual(got.Changed, c.changed) || !reflect.DeepEqual(got.Unverified, c.unverified) && len(c.unverified) > 0 {
			t.Fatal(name, got)
		}
		if len(waited) != c.waits || c.waits > 0 && waited[0] != 2*time.Second || c.waits > 1 && waited[1] != 4*time.Second {
			t.Fatal(name, "waits", waited)
		}
		if len(a.settles) != 1 || a.settles[0]["outcome"] != c.settle {
			t.Fatal(name, a.settles)
		}
		if c.changed != nil && !reflect.DeepEqual(a.settles[0]["changed"], c.changed) || c.unverified != nil && !reflect.DeepEqual(a.settles[0]["unverified"], c.unverified) {
			t.Fatal(name, a.settles[0])
		}
	}
}

// Refused before anything is reserved or sent: a name for the team or the
// project (R1), a project or team outside the binding, a team board, a
// plan that changed since the preview, a provider without the pinned
// addTeams, and a session that isn't the coordinator.
func TestAddProjectTeamRefusals(t *testing.T) {
	for name, run := range map[string]func(*Service, *teamAdds, *adderFake, bus.Request) error{
		"never a name": func(s *Service, _ *teamAdds, _ *adderFake, scope bus.Request) error {
			_, e := s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: projectA, TeamID: "Platform"})
			return e
		},
		"isn't in this binding": func(s *Service, _ *teamAdds, _ *adderFake, scope bus.Request) error {
			_, e := s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: newProject, TeamID: otherTeamID})
			return e
		},
		"isn't a write team": func(s *Service, _ *teamAdds, _ *adderFake, scope bus.Request) error {
			_, e := s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: projectA, TeamID: newProject})
			return e
		},
		"team board has no projects": func(s *Service, a *teamAdds, _ *adderFake, scope bus.Request) error {
			a.b.Mode, a.b.LinearProjectIDs = "team_board", []string{}
			_, e := s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: projectA, TeamID: otherTeamID})
			return e
		},
		"teams changed since the preview": func(s *Service, _ *teamAdds, p *adderFake, scope bus.Request) error {
			r := preview(t, s, scope)
			p.teams = append(p.teams, newProject)
			_, e := confirm(s, scope, r.Plan.PlanSHA256)
			return e
		},
		"can't add a team": func(s *Service, _ *teamAdds, p *adderFake, scope bus.Request) error {
			p.reason = "save_project schema changed"
			_, e := s.AddProjectTeam(context.Background(), scope, ProjectTeamAddInput{ProjectID: projectA, TeamID: otherTeamID})
			return e
		},
		"only the project's coordinator": func(s *Service, a *teamAdds, _ *adderFake, scope bus.Request) error {
			scope.Session, a.actor, a.name = "ses", "ses", "ses"
			r := preview(t, s, scope)
			_, e := confirm(s, scope, r.Plan.PlanSHA256)
			return e
		},
	} {
		s, a, p, scope := teamAddTest(t)
		if e := run(s, a, p, scope); e == nil || !strings.Contains(e.Error(), name) || len(a.reserves) != 0 || len(p.adds) != 0 {
			t.Fatal(name, e, a.reserves, p.adds)
		}
	}
}

// R3: a member without a session confirms as a team's lead only while
// Linear states their own user active and not a guest; the check runs on
// the confirm, before the reservation, and the preview skips it. The
// owner is never asked.
func TestAddProjectTeamTeamLeadEligible(t *testing.T) {
	user := func(active, guest bool) linearprovider.WorkspaceUser {
		return linearprovider.WorkspaceUser{ID: addLead, Active: active, ActiveKnown: true, Guest: guest, GuestKnown: true}
	}
	for name, c := range map[string]struct {
		users []linearprovider.WorkspaceUser
		want  string
	}{
		"active":   {[]linearprovider.WorkspaceUser{user(true, false)}, ""},
		"inactive": {[]linearprovider.WorkspaceUser{user(false, false)}, "active, non-guest"},
		"guest":    {[]linearprovider.WorkspaceUser{user(true, true)}, "active, non-guest"},
		"unlisted": {nil, "isn't among the workspace's users"},
		"unstated": {[]linearprovider.WorkspaceUser{{ID: addLead, Active: true, ActiveKnown: true}}, "active, non-guest"},
	} {
		s, a, p, scope := teamAddTest(t)
		a.role = "member"
		p.me, p.users = addLead, c.users
		r := preview(t, s, scope)
		got, e := confirm(s, scope, r.Plan.PlanSHA256)
		if c.want == "" {
			if e != nil || got.Outcome != "accepted" || len(a.reserves) != 1 {
				t.Fatal(name, got, e)
			}
			continue
		}
		if e == nil || !strings.Contains(e.Error(), c.want) || len(a.reserves) != 0 || len(p.adds) != 0 {
			t.Fatal(name, e, a.reserves, p.adds)
		}
	}
	// The owner confirms without the lookup: no users listed, accepted.
	s, a, _, scope := teamAddTest(t)
	r := preview(t, s, scope)
	if got, e := confirm(s, scope, r.Plan.PlanSHA256); e != nil || got.Outcome != "accepted" || len(a.reserves) != 1 {
		t.Fatal(got, e)
	}
}
