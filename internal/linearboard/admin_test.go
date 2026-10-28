package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	newInitiative = "e4000000-0000-4000-8000-000000000004"
	newLabel      = "e1000000-0000-4000-8000-000000000001"
	dupLabel      = "e2000000-0000-4000-8000-000000000002"
	newProject    = "e3000000-0000-4000-8000-000000000003"
)

// adminAuthority adds a project role, the approved label list and the
// admin audit to the V2 authority fake.
type adminAuthority struct {
	*authorityV2
	role       string
	policy     bus.LabelPolicy
	revision   int
	audits     []map[string]any
	sets       int
	policySets int
	// policyReads counts label_policy_get; afterPolicyRead runs after each.
	policyReads     int
	afterPolicyRead func(int)
	auditErr        error
}

func (a *adminAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "taskboard_access":
		result, e := a.authorityV2.Execute(ctx, r)
		access := result.Data.(bus.TaskboardAccess)
		access.ProjectRole = a.role
		result.Data = access
		return result, e
	case "label_policy_get":
		data := map[string]any{"revision": a.revision, "labels": a.policy.Labels, "renames": a.policy.Renames}
		a.policyReads++
		if a.afterPolicyRead != nil {
			a.afterPolicyRead(a.policyReads)
		}
		return bus.Result{Source: "local-authority", Data: data}, nil
	case "linear_admin_audit":
		if a.auditErr != nil {
			e := a.auditErr
			a.auditErr = nil
			return bus.Result{}, e
		}
		// Like the bus, recording is idempotent by operation ID.
		for _, seen := range a.audits {
			if seen["operation_id"] == r.Args["operation_id"] {
				return bus.Result{Source: "local-authority", Data: map[string]any{"recorded": true, "already": true}}, nil
			}
		}
		a.audits = append(a.audits, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"recorded": true}}, nil
	case "label_policy_set":
		a.policySets++
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": a.revision + 1}}, nil
	case "taskboard_set_v2":
		a.sets++
	}
	return a.authorityV2.Execute(ctx, r)
}

// adminFake is a V2 provider with the admin writes. Labels and projects are
// returned by name; each write is recorded.
type adminFake struct {
	*providerV2
	labels           map[string][]linearprovider.AdminLabel
	projects         map[string][]linearprovider.ProjectListing
	incomplete       bool
	afterWrite       func()
	writes           []string
	reason           string
	initiativeReason string
	initiatives      map[string][]linearprovider.Initiative
	members          map[string]bool
	// teamless makes label receipts leave the team out.
	teamless bool
}

func (p *adminFake) AdminReason() string      { return p.reason }
func (p *adminFake) InitiativeReason() string { return p.initiativeReason }
func (p *adminFake) InitiativesNamed(_ context.Context, name string) ([]linearprovider.Initiative, bool, error) {
	return p.initiatives[strings.ToLower(name)], !p.incomplete, nil
}
func (p *adminFake) CreateInitiative(_ context.Context, name, description string) (linearprovider.AdminReceipt, error) {
	p.write("initiative " + name + " " + description)
	p.initiatives[strings.ToLower(name)] = []linearprovider.Initiative{{ID: newInitiative, Name: name}}
	return linearprovider.AdminReceipt{ID: newInitiative, Name: name}, nil
}
func (p *adminFake) InitiativeHasProject(_ context.Context, initiative, project string) (bool, error) {
	return p.members[initiative+"/"+project], nil
}
func (p *adminFake) AddProjectToInitiative(_ context.Context, initiative, project string) (linearprovider.AdminReceipt, error) {
	p.write("add " + initiative + " " + project)
	p.members[initiative+"/"+project] = true
	return linearprovider.AdminReceipt{ID: initiative}, nil
}
func (p *adminFake) TeamLabelsNamed(_ context.Context, _, name string) ([]linearprovider.AdminLabel, bool, error) {
	return p.labels[strings.ToLower(name)], !p.incomplete, nil
}
func (p *adminFake) TeamProjectsNamed(_ context.Context, _, name string) ([]linearprovider.ProjectListing, bool, error) {
	return p.projects[strings.ToLower(name)], !p.incomplete, nil
}
func (p *adminFake) write(what string) {
	p.writes = append(p.writes, what)
	if p.afterWrite != nil {
		p.afterWrite()
	}
}
func (p *adminFake) CreateTeamLabel(_ context.Context, team, name string) (linearprovider.AdminReceipt, error) {
	p.write("label " + team + " " + name)
	r := linearprovider.AdminReceipt{ID: newLabel, Name: name, TeamID: team}
	if p.teamless {
		r.Unverified = []string{"team"}
	}
	return r, nil
}
func (p *adminFake) RenameTeamLabel(_ context.Context, team, id, name string) (linearprovider.AdminReceipt, error) {
	p.write("rename " + id + " " + name)
	return linearprovider.AdminReceipt{ID: id, Name: name, TeamID: team}, nil
}
func (p *adminFake) CreateTeamProject(_ context.Context, team, name string) (linearprovider.AdminReceipt, error) {
	p.write("project " + team + " " + name)
	p.projects[strings.ToLower(name)] = []linearprovider.ProjectListing{{Project: linearprovider.Project{ID: newProject, Name: name, TeamIDs: []string{team}}}}
	p.providerV2.projects[newProject] = []string{team}
	return linearprovider.AdminReceipt{ID: newProject, Name: name, TeamID: team}, nil
}

func adminTest(t *testing.T) (*Service, *adminAuthority, *adminFake, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	auth := &adminAuthority{authorityV2: a, role: "owner", revision: 3, policy: bus.LabelPolicy{Labels: []string{"Agent", "Bug"}, Renames: map[string]string{"Bugs": "Bug old"}}}
	fake := &adminFake{providerV2: p, labels: map[string][]linearprovider.AdminLabel{}, projects: map[string][]linearprovider.ProjectListing{}, initiatives: map[string][]linearprovider.Initiative{}, members: map[string]bool{}}
	s.cfg.Executor = auth
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return fake, nil }
	return s, auth, fake, scope
}

func label(id, name string, team string, known bool) linearprovider.AdminLabel {
	return linearprovider.AdminLabel{ID: id, Name: name, TeamID: team, TeamKnown: known}
}

func TestAdminLabelCreateFromTheApprovedListIsAudited(t *testing.T) {
	s, a, p, scope := adminTest(t)
	in := AdminInput{Kind: "label_create", OperationID: "lbl-1", TeamID: teamID, Name: "Bug"}
	r, e := s.Admin(context.Background(), scope, in)
	if e != nil || r.Outcome != "accepted" || r.ID != newLabel || r.PolicyRevision != 3 || r.Audit != "recorded" || len(p.writes) != 1 || p.writes[0] != "label "+teamID+" Bug" {
		t.Fatal(r, e, p.writes)
	}
	if len(a.audits) != 1 || a.audits[0]["policy_revision"] != 3 || a.audits[0]["kind"] != "label_create" || a.audits[0]["linear_id"] != newLabel {
		t.Fatal("audit", a.audits)
	}
	// The same operation is answered from the ledger.
	if again, e := s.Admin(context.Background(), scope, in); e != nil || again.ID != newLabel || len(p.writes) != 1 {
		t.Fatal("retry", again, e, p.writes)
	}
}

func TestAdminWritesAreRefusedBeforeLinear(t *testing.T) {
	for name, c := range map[string]struct {
		in    AdminInput
		setup func(*adminAuthority, *adminFake)
		want  string
	}{
		"a member":     {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug"}, func(a *adminAuthority, _ *adminFake) { a.role = "member" }, "project owner only"},
		"a lead":       {AdminInput{Kind: "project_create", OperationID: "x", TeamID: teamID, Name: "Q4"}, func(a *adminAuthority, _ *adminFake) { a.role = "lead" }, "project owner only"},
		"not approved": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Feature"}, nil, "not in the approved list"},
		// A sub-team never gets a copy of its bound parent's label, and the
		// parent having a name approves nothing (CROSS-TEAM-BINDING §3.2).
		"parent has it": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: otherTeamID, Name: "Bug"}, func(_ *adminAuthority, p *adminFake) {
			p.providerV2.parents = map[string]string{otherTeamID: teamID}
			p.providerV2.labels = []linearprovider.Label{{ID: dupLabel, Name: "Bug", TeamID: teamID, EligibilityKnown: true}}
		}, "bound parent team already has"},
		"parent approves nothing": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: otherTeamID, Name: "Feature"}, func(_ *adminAuthority, p *adminFake) {
			p.providerV2.parents = map[string]string{otherTeamID: teamID}
			p.providerV2.labels = []linearprovider.Label{{ID: dupLabel, Name: "Feature", TeamID: teamID, EligibilityKnown: true}}
		}, "not in the approved list"},
		"case differs":    {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "bug"}, nil, "not in the approved list"},
		"unbound team":    {AdminInput{Kind: "label_create", OperationID: "x", TeamID: "99999999-9999-4999-8999-999999999999", Name: "Bug"}, nil, "not in this project's binding"},
		"rename unlisted": {AdminInput{Kind: "label_rename", OperationID: "x", TeamID: teamID, From: "Bugs", Name: "Bug new"}, nil, "not in the approved list"},
		"incomplete walk": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug"}, func(_ *adminAuthority, p *adminFake) { p.incomplete = true }, "could not be read completely"},
		"workspace label": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug"}, func(_ *adminAuthority, p *adminFake) {
			p.labels["bug"] = []linearprovider.AdminLabel{label(dupLabel, "bug", "", true)}
		}, "exists_at_workspace_level"},
		"scope unknown": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug"}, func(_ *adminAuthority, p *adminFake) {
			p.labels["bug"] = []linearprovider.AdminLabel{label(dupLabel, "BUG", "", false)}
		}, "exists_scope_unknown"},
		"schema drift": {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug"}, func(_ *adminAuthority, p *adminFake) { p.reason = "save_issue_label now requires id" }, "unavailable"},
		"mention":      {AdminInput{Kind: "project_create", OperationID: "x", TeamID: teamID, Name: "ask @ann"}, nil, "mention"},
		"rename ambiguous": {AdminInput{Kind: "label_rename", OperationID: "x", TeamID: teamID, From: "Bugs", Name: "Bug old"}, func(_ *adminAuthority, p *adminFake) {
			p.labels["bugs"] = []linearprovider.AdminLabel{label(newLabel, "Bugs", teamID, true), label(dupLabel, "bugs", teamID, true)}
		}, "exactly one"},
		"rename a group": {AdminInput{Kind: "label_rename", OperationID: "x", TeamID: teamID, From: "Bugs", Name: "Bug old"}, func(_ *adminAuthority, p *adminFake) {
			l := label(newLabel, "Bugs", teamID, true)
			l.Group = true
			p.labels["bugs"] = []linearprovider.AdminLabel{l}
		}, "not a plain"},
		"rename a workspace label": {AdminInput{Kind: "label_rename", OperationID: "x", TeamID: teamID, From: "Bugs", Name: "Bug old"}, func(_ *adminAuthority, p *adminFake) {
			p.labels["bugs"] = []linearprovider.AdminLabel{label(newLabel, "Bugs", "", true)}
		}, "not a plain"},
		"rename target exists": {AdminInput{Kind: "label_rename", OperationID: "x", TeamID: teamID, From: "Bugs", Name: "Bug old"}, func(_ *adminAuthority, p *adminFake) {
			p.labels["bugs"] = []linearprovider.AdminLabel{label(newLabel, "Bugs", teamID, true)}
			p.labels["bug old"] = []linearprovider.AdminLabel{label(dupLabel, "Bug old", teamID, true)}
		}, "already exists"},
	} {
		s, a, p, scope := adminTest(t)
		if c.setup != nil {
			c.setup(a, p)
		}
		opens := 0
		open := s.cfg.Open
		s.cfg.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) { opens++; return open(ctx, h, ro) }
		if _, e := s.Admin(context.Background(), scope, c.in); e == nil || !strings.Contains(e.Error(), c.want) {
			t.Fatal(name, e)
		}
		if a.role != "owner" && opens != 0 {
			t.Fatal(name, "a non-owner reached Linear")
		}
		if len(p.writes) != 0 || len(a.audits) != 0 {
			t.Fatal(name, "wrote or audited", p.writes, a.audits)
		}
	}
}

// An existing team label or project is never written over, and a second
// operation ID cannot make a duplicate.
func TestAdminExistingNamesAreObservedUnchanged(t *testing.T) {
	s, _, p, scope := adminTest(t)
	p.labels["bug"] = []linearprovider.AdminLabel{label(dupLabel, "bug", teamID, true)}
	if r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "l", TeamID: teamID, Name: "Bug"}); e != nil || r.Outcome != "observed_unchanged" || r.ID != dupLabel || len(p.writes) != 0 {
		t.Fatal(r, e, p.writes)
	}
	first, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p-1", TeamID: teamID, Name: "Q4"})
	if e != nil || first.Outcome != "accepted" || len(p.writes) != 1 {
		t.Fatal(first, e)
	}
	second, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p-2", TeamID: teamID, Name: "q4"})
	if e != nil || second.Outcome != "observed_unchanged" || second.ID != newProject || len(p.writes) != 1 {
		t.Fatal("a second operation ID created a duplicate project", second, e, p.writes)
	}
}

func TestAdminLabelDuplicateRaceIsReported(t *testing.T) {
	s, _, p, scope := adminTest(t)
	p.afterWrite = func() {
		p.labels["bug"] = []linearprovider.AdminLabel{label(newLabel, "Bug", teamID, true), label(dupLabel, "Bug", teamID, true)}
	}
	r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "d", TeamID: teamID, Name: "Bug"})
	if e != nil || r.Outcome != "accepted_duplicate_observed" || len(r.DuplicateIDs) != 2 {
		t.Fatal(r, e)
	}
	s, _, p, scope = adminTest(t)
	p.afterWrite = func() { p.incomplete = true }
	if r, e = s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "d", TeamID: teamID, Name: "Bug"}); e != nil || r.Outcome != "accepted_duplicate_unknown" {
		t.Fatal(r, e)
	}
}

func TestAdminRenameSendsTheProvenLabel(t *testing.T) {
	s, a, p, scope := adminTest(t)
	p.labels["bugs"] = []linearprovider.AdminLabel{label(newLabel, "Bugs", teamID, true)}
	r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_rename", OperationID: "r", TeamID: teamID, From: "Bugs", Name: "Bug old"})
	if e != nil || r.Outcome != "accepted" || r.PreviousName != "Bugs" || len(p.writes) != 1 || p.writes[0] != "rename "+newLabel+" Bug old" {
		t.Fatal(r, e, p.writes)
	}
	if a.audits[0]["previous_name"] != "Bugs" || a.audits[0]["policy_revision"] != 3 {
		t.Fatal(a.audits)
	}
}

// A created project gets a binding proposal in a project-set binding and
// none in a team board; the binding itself is never written.
func TestAdminProjectProposalNeverBinds(t *testing.T) {
	s, a, _, scope := adminTest(t)
	r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p", TeamID: teamID, Name: "Q4"})
	if e != nil || r.Proposal == nil || !strings.Contains(r.ConfirmCommand, "--linear-project "+newProject) || !strings.Contains(r.ConfirmCommand, "--expected-revision 1 --confirm") || a.sets != 0 {
		t.Fatal(r, e, a.sets)
	}
	s, a, _, scope = adminTest(t)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", []string{}
	r, e = s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p", TeamID: teamID, Name: "Q4"})
	if e != nil || r.Proposal != nil || !strings.Contains(r.ProposalNote, "already in scope") || a.sets != 0 {
		t.Fatal("team board", r, e)
	}
}

// The admin writes are CLI-only: the agent-facing V2 adapter refuses them.
func TestAdminWritesAreNotAgentOperations(t *testing.T) {
	s, _, p, scope := adminTest(t)
	for _, op := range []string{"label_create", "label_rename", "project_create", "label_policy_set"} {
		if _, e := s.ExecuteV2(context.Background(), scope, op, map[string]any{"name": "Bug", "team_id": teamID, "operation_id": "x"}); e == nil {
			t.Fatal(op, "accepted by ExecuteV2")
		}
	}
	if len(p.writes) != 0 {
		t.Fatal(p.writes)
	}
}

// AC-M1: the approved list is read again in the lock; a change since the
// check refuses the write before it is sent.
func TestAdminLabelWriteRefusedWhenThePolicyChangesBeforeDispatch(t *testing.T) {
	s, a, p, scope := adminTest(t)
	a.afterPolicyRead = func(n int) {
		if n == 1 {
			a.revision = 4
		}
	}
	_, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "m1", TeamID: teamID, Name: "Bug"})
	if e == nil || !strings.Contains(e.Error(), "approved label list changed") || len(p.writes) != 0 || len(a.audits) != 0 {
		t.Fatal("a write checked under an old list was sent", e, p.writes)
	}
}

// AC-L1: a retry records the audit an earlier run failed to.
func TestAdminAuditIsRetriedWithTheOperation(t *testing.T) {
	s, a, p, scope := adminTest(t)
	a.auditErr = errors.New("authority unavailable")
	in := AdminInput{Kind: "label_create", OperationID: "l1", TeamID: teamID, Name: "Bug"}
	r, e := s.Admin(context.Background(), scope, in)
	if e != nil || !strings.HasPrefix(r.Audit, "failed") || len(a.audits) != 0 {
		t.Fatal(r, e)
	}
	if r, e = s.Admin(context.Background(), scope, in); e != nil || r.Audit != "recorded" || len(a.audits) != 1 || len(p.writes) != 1 {
		t.Fatal("retry did not record the audit, or wrote again", r, e, p.writes)
	}
}

// AC-L3: the post-create re-list proves a team the receipt left out.
func TestAdminRelistProvesTheLabelTeam(t *testing.T) {
	s, _, p, scope := adminTest(t)
	p.teamless = true
	p.afterWrite = func() { p.labels["bug"] = []linearprovider.AdminLabel{label(newLabel, "Bug", teamID, true)} }
	if r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "t", TeamID: teamID, Name: "Bug"}); e != nil || r.Unverified != nil {
		t.Fatal(r, e)
	}
	s, _, p, scope = adminTest(t)
	p.teamless = true
	p.afterWrite = func() { p.labels["bug"] = []linearprovider.AdminLabel{label(newLabel, "Bug", "", false)} }
	if r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "t", TeamID: teamID, Name: "Bug"}); e != nil || len(r.Unverified) != 1 {
		t.Fatal("a teamless row cleared unverified", r, e)
	}
}

// AC-L2: a policy file that repeats a key is refused before the authority.
func TestLabelPolicyFileRefusesRepeatedKeys(t *testing.T) {
	s, a, _, scope := adminTest(t)
	for _, raw := range []string{`{"labels":["A"],"labels":["B"]}`, `{"labels":["A"],"renames":{"x":"y","x":"z"}}`, `{"labels":["A"]} {}`, `not json`} {
		if _, e := s.SetLabelPolicyV2(context.Background(), scope, []byte(raw), 3); e == nil {
			t.Fatal("accepted", raw)
		}
	}
	if a.policySets != 0 {
		t.Fatal("authority reached")
	}
	if _, e := s.SetLabelPolicyV2(context.Background(), scope, []byte(`{"labels":["A"],"renames":{"x":"y"}}`), 3); e != nil || a.policySets != 1 {
		t.Fatal("a valid file refused", e)
	}
}

// Initiatives: created once by name (an existing one is returned, never
// updated), no team, no policy; a project joins only when it is in a bound
// team, verified by the initiative's readback.
func TestAdminInitiatives(t *testing.T) {
	s, a, p, scope := adminTest(t)
	in := AdminInput{Kind: "initiative_create", OperationID: "i-1", Name: "Product Review", Description: "Review projects"}
	r, e := s.Admin(context.Background(), scope, in)
	if e != nil || r.Outcome != "accepted" || r.ID != newInitiative || r.TeamID != "" || len(p.writes) != 1 || p.writes[0] != "initiative Product Review Review projects" {
		t.Fatal(r, e, p.writes)
	}
	if again, e := s.Admin(context.Background(), scope, AdminInput{Kind: "initiative_create", OperationID: "i-2", Name: "product review"}); e != nil || again.Outcome != "observed_unchanged" || again.ID != newInitiative || len(p.writes) != 1 {
		t.Fatal("an existing initiative was created again", again, e)
	}
	if a.audits[0]["kind"] != "initiative_create" || a.audits[0]["linear_id"] != newInitiative || a.audits[0]["linear_team_id"] != nil || a.audits[0]["policy_revision"] != nil {
		t.Fatal("audit", a.audits[0])
	}
	// A project of a bound team joins; it is recorded with both IDs.
	add := AdminInput{Kind: "initiative_add_project", OperationID: "a-1", InitiativeID: newInitiative, ProjectID: projectA}
	if r, e = s.Admin(context.Background(), scope, add); e != nil || r.Outcome != "accepted" || r.ID != newInitiative || r.ProjectID != projectA || p.writes[1] != "add "+newInitiative+" "+projectA {
		t.Fatal(r, e, p.writes)
	}
	if last := a.audits[len(a.audits)-1]; last["kind"] != "initiative_add_project" || last["linear_project_id"] != projectA {
		t.Fatal(last)
	}
	add.OperationID = "a-2"
	if r, e = s.Admin(context.Background(), scope, add); e != nil || r.Outcome != "observed_unchanged" || len(p.writes) != 2 {
		t.Fatal("an existing membership was sent again", r, e)
	}
	// A project outside every bound team is refused before Linear.
	p.providerV2.projects["e5000000-0000-4000-8000-000000000005"] = []string{"99999999-9999-4999-8999-999999999999"}
	if _, e = s.Admin(context.Background(), scope, AdminInput{Kind: "initiative_add_project", OperationID: "a-3", InitiativeID: newInitiative, ProjectID: "e5000000-0000-4000-8000-000000000005"}); e == nil || !strings.Contains(e.Error(), "not in a team of this project's binding") || len(p.writes) != 2 {
		t.Fatal("an unbound project joined", e)
	}
	// Owner only, and refused when the initiative tools drift.
	a.role = "member"
	if _, e = s.Admin(context.Background(), scope, AdminInput{Kind: "initiative_create", OperationID: "i-3", Name: "Other"}); e == nil || !strings.Contains(e.Error(), "owner only") {
		t.Fatal(e)
	}
	a.role = "owner"
	p.initiativeReason = "save_initiative no longer accepts name and description"
	if _, e = s.Admin(context.Background(), scope, AdminInput{Kind: "initiative_create", OperationID: "i-4", Name: "Other"}); e == nil || len(p.writes) != 2 {
		t.Fatal("drifted initiative tools used", e)
	}
	for name, bad := range map[string]AdminInput{
		"team on create":       {Kind: "initiative_create", OperationID: "x", Name: "N", TeamID: teamID},
		"mention":              {Kind: "initiative_create", OperationID: "x", Name: "N", Description: "ask @ann"},
		"name on add":          {Kind: "initiative_add_project", OperationID: "x", InitiativeID: newInitiative, ProjectID: projectA, Name: "N"},
		"non-UUID initiative":  {Kind: "initiative_add_project", OperationID: "x", InitiativeID: "Product Review", ProjectID: projectA},
		"description on label": {Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug", Description: "d"},
	} {
		if ValidateAdmin(bad) == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// A hosted server's answer arrives with json.Number values (the remote
// client decodes with UseNumber): a label create must still read the
// policy revision it is checked against.
func TestLabelCreateReadsAHostedPolicyRevision(t *testing.T) {
	s, a, p, scope := adminTest(t)
	s.cfg.Executor = numberAuthority{a}
	r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "label_create", OperationID: "lbl-hosted", TeamID: teamID, Name: "Bug"})
	if e != nil || r.Outcome != "accepted" || r.PolicyRevision != 3 || len(p.writes) != 1 {
		t.Fatal("a hosted label create failed:", r, e)
	}
}

// numberAuthority answers label_policy_get as a remote client decodes it.
type numberAuthority struct{ *adminAuthority }

func (n numberAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	out, e := n.adminAuthority.Execute(ctx, r)
	if m, ok := out.Data.(map[string]any); ok && r.Operation == "label_policy_get" {
		m["revision"] = json.Number(fmt.Sprint(m["revision"]))
	}
	return out, e
}
