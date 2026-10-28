package linearboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// leadFake is the roles fake with project leads: each project's teams and
// lead, every lead it was sent, and hooks to change a project between the
// plan and the write or to drop a write.
type leadFake struct {
	*rolesFake
	projects map[string]*linearprovider.ProjectLeadState
	sent     []string
	reads    map[string]int
	onRead   func(project string, n int)
	dropSet  bool
}

func (f *leadFake) ProjectLead(_ context.Context, project string) (linearprovider.ProjectLeadState, error) {
	f.reads[project]++
	if f.onRead != nil {
		f.onRead(project, f.reads[project])
	}
	st, ok := f.projects[project]
	if !ok {
		return linearprovider.ProjectLeadState{}, &linearprovider.Error{Kind: "provider_rejected"}
	}
	return *st, nil
}

func (f *leadFake) SetProjectLead(_ context.Context, project, lead string) error {
	f.sent = append(f.sent, project+"="+lead)
	if !f.dropSet {
		// As the probe found: the lead becomes a member, and stays one.
		st := f.projects[project]
		st.Lead = lead
		if lead != "" {
			st.Members++
		}
	}
	return nil
}

// leadTest is the roles env (Ada leads teamID; the other team has no
// lead) with projectA in teamID and projectB in the other team, neither
// with a lead.
func leadTest(t *testing.T) (*rolesEnv, *leadFake) {
	t.Helper()
	e := rolesDocTest(t)
	f := &leadFake{rolesFake: e.rf, reads: map[string]int{}, projects: map[string]*linearprovider.ProjectLeadState{
		projectA: {ID: projectA, TeamIDs: []string{teamID}, LeadKnown: true, MembersKnown: true},
		projectB: {ID: projectB, TeamIDs: []string{otherTeamID}, LeadKnown: true},
	}}
	e.s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { e.opens++; return f, nil }
	return e, f
}

func (e *rolesEnv) applyLeads(t *testing.T, in ApplyLeadsInput) ApplyLeadsResult {
	t.Helper()
	if in.Revision == 0 {
		in.Revision = 1
	}
	r, err := e.s.ApplyLeads(context.Background(), e.scope, in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func leadActions(r ApplyLeadsResult) string {
	var out []string
	for _, p := range r.Projects {
		s := p.Project[:4] + ":" + p.Action
		if p.Outcome != "" {
			s += "/" + p.Outcome
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// A preview writes nothing; a confirm sets the lead of the project whose
// team has one mapped lead, as that user's UUID only, reads it back and
// records it by outcome; the other team has no lead, so it's skipped.
func TestApplyLeadsSetsTheTeamLead(t *testing.T) {
	e, f := leadTest(t)
	if r := e.applyLeads(t, ApplyLeadsInput{}); leadActions(r) != "3333:set 4444:skip" || len(f.sent) != 0 || r.Projects[1].Reason != "the team has no mapped lead" || !strings.Contains(r.Notice, "member") || !strings.Contains(r.Projects[0].Member, "Linear also adds Ada Example as a project member") || r.Projects[1].Member != "" {
		t.Fatal(leadActions(r), r)
	}
	r := e.applyLeads(t, ApplyLeadsInput{Confirm: true})
	if leadActions(r) != "3333:set/accepted 4444:skip" || len(f.sent) != 1 || f.sent[0] != projectA+"="+rolesAda {
		t.Fatal(leadActions(r), f.sent)
	}
	if r = e.applyLeads(t, ApplyLeadsInput{Confirm: true}); leadActions(r) != "3333:none 4444:skip" || len(f.sent) != 1 {
		t.Fatal("again", leadActions(r))
	}
	got, err := e.run(context.Background(), bus.Request{Operation: "history", Workspace: "w", Project: "p", Args: map[string]any{}})
	if text := jsonText(got.Data); err != nil || !strings.Contains(text, `\"outcome\":\"accepted\"`) && !strings.Contains(text, `"outcome":"accepted"`) || !strings.Contains(text, "members_after") || strings.Contains(text, rolesAda) || strings.Contains(text, "Ada Example") {
		t.Fatal("history", jsonText(got.Data), err)
	}
}

// A project with another lead is never changed, except the one --replace
// names; --replace naming a project that doesn't differ is refused.
func TestApplyLeadsReplacesOnlyByName(t *testing.T) {
	e, f := leadTest(t)
	f.projects[projectA].Lead = rolesBen
	if r := e.applyLeads(t, ApplyLeadsInput{Confirm: true}); leadActions(r) != "3333:differs 4444:skip" || len(f.sent) != 0 {
		t.Fatal(leadActions(r))
	}
	if _, err := e.s.ApplyLeads(context.Background(), e.scope, ApplyLeadsInput{Revision: 1, Replace: projectB}); err == nil || !strings.Contains(err.Error(), "--replace names") {
		t.Fatal(err)
	}
	if r := e.applyLeads(t, ApplyLeadsInput{Replace: projectA, Confirm: true}); leadActions(r) != "3333:replace/accepted 4444:skip" || f.sent[0] != projectA+"="+rolesAda || !strings.Contains(r.Projects[0].Member, "The current lead stays a member") {
		t.Fatal(leadActions(r), f.sent)
	}
}

// Fail closed: a lead Linear's answer leaves out, a mapped lead who left
// the workspace, a stale revision, a session; a change between the plan and
// the write is drifted and not written; a read-back that disagrees is
// uncertain.
func TestApplyLeadsFailsClosed(t *testing.T) {
	e, f := leadTest(t)
	f.projects[projectA].LeadKnown = false
	if r := e.applyLeads(t, ApplyLeadsInput{Confirm: true}); leadActions(r) != "3333:skip 4444:skip" || len(f.sent) != 0 {
		t.Fatal("unknown lead", leadActions(r))
	}
	f.projects[projectA].LeadKnown = true
	f.users = f.users[1:]
	if r := e.applyLeads(t, ApplyLeadsInput{Confirm: true}); leadActions(r) != "3333:skip 4444:skip" || !strings.Contains(r.Projects[0].Reason, "active, non-guest") {
		t.Fatal("ineligible lead", leadActions(r))
	}
	f.users = e.rf.users
	f.users = append([]linearprovider.WorkspaceUser{{ID: rolesAda, Name: "Ada Example", Active: true, ActiveKnown: true, GuestKnown: true}}, f.users...)
	if _, err := e.s.ApplyLeads(context.Background(), e.scope, ApplyLeadsInput{Revision: 2}); err == nil || !strings.Contains(err.Error(), "current revision is 1") {
		t.Fatal("stale", err)
	}
	scope := e.scope
	scope.Session = "s"
	if _, err := e.s.ApplyLeads(context.Background(), scope, ApplyLeadsInput{Revision: 1}); err == nil || !strings.Contains(err.Error(), "a session can't") {
		t.Fatal("session", err)
	}
	f.onRead = func(project string, n int) {
		if project == projectA && n == 2 {
			f.projects[projectA].Lead = rolesBen
		}
	}
	f.reads = map[string]int{}
	if r := e.applyLeads(t, ApplyLeadsInput{Confirm: true}); leadActions(r) != "3333:set/drifted 4444:skip" || len(f.sent) != 0 {
		t.Fatal("drift", leadActions(r), f.sent)
	}
	f.onRead, f.dropSet = nil, true
	f.projects[projectA].Lead = ""
	if r := e.applyLeads(t, ApplyLeadsInput{Confirm: true}); leadActions(r) != "3333:set/uncertain 4444:skip" {
		t.Fatal("read-back", leadActions(r))
	}
}

// project create's lead: off adds nothing (and refuses --lead); on, the
// team's one lead, --lead only among the team's leads, several needs it.
func TestPlanCreateLead(t *testing.T) {
	e, _ := leadTest(t)
	ctx := context.Background()
	if got, err := e.s.PlanCreateLead(ctx, e.scope, teamID, ""); err != nil || got.Lead != "" || !strings.Contains(got.Note, "off") {
		t.Fatal(got, err)
	}
	if _, err := e.s.PlanCreateLead(ctx, e.scope, teamID, rolesAda); err == nil {
		t.Fatal("--lead while off")
	}
	if _, err := e.run(ctx, bus.Request{Operation: "project_lead_set", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "on"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.PlanCreateLead(ctx, e.scope, strings.ToUpper(teamID), ""); err != nil || got.Lead != rolesAda || got.Revision != 1 {
		t.Fatal(got, err)
	}
	if got, err := e.s.PlanCreateLead(ctx, e.scope, otherTeamID, ""); err != nil || got.Lead != "" || !strings.Contains(got.Note, "no mapped lead") {
		t.Fatal(got, err)
	}
	if _, err := e.s.PlanCreateLead(ctx, e.scope, teamID, rolesBen); err == nil || !strings.Contains(err.Error(), "mapped leads") {
		t.Fatal("a non-lead picked", err)
	}
	e.setRoles(t, 1, map[string]any{
		"teams":  []any{map[string]any{"team_id": teamID, "leads": []any{rolesAda, rolesBen}}},
		"people": []any{map[string]any{"linear_user_id": rolesAda, "role": "lead", "teams": []any{teamID}}, map[string]any{"linear_user_id": rolesBen, "role": "lead", "teams": []any{teamID}}},
	})
	if _, err := e.s.PlanCreateLead(ctx, e.scope, teamID, ""); err == nil || !strings.Contains(err.Error(), "--lead") {
		t.Fatal("several", err)
	}
	if got, err := e.s.PlanCreateLead(ctx, e.scope, teamID, rolesBen); err != nil || got.Lead != rolesBen {
		t.Fatal(got, err)
	}
}

// After create, the planned lead is set and read back.
func TestSetCreatedProjectLead(t *testing.T) {
	e, f := leadTest(t)
	f.projects[projectB].TeamIDs = []string{teamID}
	got, err := e.s.SetCreatedProjectLead(context.Background(), e.scope, projectB, CreateLead{Lead: rolesAda, Revision: 1})
	if err != nil || got != "accepted" || len(f.sent) != 1 || f.sent[0] != projectB+"="+rolesAda {
		t.Fatal(got, err, f.sent)
	}
	if got, err = e.s.SetCreatedProjectLead(context.Background(), e.scope, projectB, CreateLead{}); err != nil || got != "" || len(f.sent) != 1 {
		t.Fatal("no lead planned", got, err)
	}
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// A project in two bound teams with different mapped leads is skipped,
// never given either.
func TestApplyLeadsTeamsDisagree(t *testing.T) {
	e, f := leadTest(t)
	e.setRoles(t, 1, map[string]any{
		"teams": []any{map[string]any{"team_id": teamID, "leads": []any{rolesAda}}, map[string]any{"team_id": otherTeamID, "leads": []any{rolesBen}}},
		"people": []any{
			map[string]any{"linear_user_id": rolesAda, "role": "lead", "teams": []any{teamID}},
			map[string]any{"linear_user_id": rolesBen, "role": "lead", "teams": []any{otherTeamID}},
		},
	})
	f.projects[projectA].TeamIDs = []string{teamID, otherTeamID}
	r := e.applyLeads(t, ApplyLeadsInput{Revision: 2, Confirm: true})
	if leadActions(r) != "3333:skip 4444:set/accepted" || r.Projects[0].Reason != "its bound teams have different leads" || f.sent[0] != projectB+"="+rolesBen {
		t.Fatal(leadActions(r), r.Projects[0].Reason, f.sent)
	}
}
