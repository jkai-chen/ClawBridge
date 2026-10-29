package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// End to end on Postgres: linear_app_project_status[_set] through the
// AppWriter, the bus steps and a fake of the app's Linear calls.

const (
	pstProject = "f2000000-0000-4000-8000-0000000000b1"
	pstOther   = "f2000000-0000-4000-8000-0000000000b2"
)

// fakeStatusLinear stands in for the app's GraphQL reads and its move.
type fakeStatusLinear struct {
	mu      sync.Mutex
	facts   projectStatusFacts
	outcome string
	code    string
	reads   int
	sent    []string
}

func (f *fakeStatusLinear) read(context.Context, string) (projectStatusFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.facts, nil
}

func (f *fakeStatusLinear) set(_ context.Context, _ *linearboard.Service, _ bus.Request, lp, to string) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, lp+"|"+to)
	if f.outcome == "accepted" {
		f.facts.Status = ProjectStatus{to, "Doing", "started"}
	}
	return f.outcome, f.code
}

func (f *fakeStatusLinear) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, len(f.sent)
}

// newPSFixture is the app writer fixture with a project-set binding of
// pstProject, the agent session as the project's coordinator, and a fake
// Linear: the project planned with one started and one backlog issue.
func newPSFixture(t *testing.T) (*awFixture, *fakeStatusLinear) {
	t.Helper()
	f := newAWFixture(t, awOrg)
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{
		"instance": awSession + "-i", "expected_revision": 1, "mode": "project_set",
		"linear_team_ids": []string{awTeam}, "linear_default_team_id": awTeam,
		"linear_project_ids": []string{pstProject}, "linear_workspace_id": awOrg,
	}})
	f.must(bus.Request{Operation: "coordinator_set", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i", "target_session": awSession}})
	fake := &fakeStatusLinear{outcome: "accepted", facts: projectStatusFacts{
		Status:   ProjectStatus{pstPlanned, "Planned", "planned"},
		Statuses: []ProjectStatus{{pstPlanned, "Planned", "planned"}, {pstStarted, "Doing\u202e", "started"}},
		Issues:   projectIssues{Total: 2, Open: 2, ByType: map[string]int{"started": 1, "backlog": 1}, Complete: true},
	}}
	f.writer.StatusLinear = fake
	return f, fake
}

func (f *awFixture) projectStatus(op, session string, args map[string]any) (map[string]any, error) {
	if session != "" {
		args["instance"] = session + "-i"
	}
	out, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: op, Workspace: f.workspace, Project: "p", Session: session, Args: args})
	return awJSON(f.t, out.Data), err
}

func psSetArgs(to, opID string, more map[string]any) map[string]any {
	a := map[string]any{"linear_project": pstProject, "type": to, "operation_id": opID}
	for k, v := range more {
		a[k] = v
	}
	return a
}

// A read answers the status, the counts and the suggestion; the
// coordinator previews, then confirms with the status the preview saw;
// the move is sent once and audited as the session; the same
// operation_id then answers from the record.
func TestPostgresProjectStatusReadPreviewAndSet(t *testing.T) {
	f, fake := newPSFixture(t)
	read, err := f.projectStatus("linear_app_project_status", awSession, map[string]any{"linear_project": pstProject, "suggest": true})
	if err != nil || read["suggestion"] != "started" || read["flow"] != "off" || read["status"].(map[string]any)["type"] != "planned" {
		t.Fatal(read, err)
	}
	preview, err := f.projectStatus("linear_app_project_status_set", awSession, psSetArgs("started", "op-1", nil))
	if err != nil || preview["preview"] != true || preview["expected_status_id"] != pstPlanned || preview["to"].(map[string]any)["id"] != pstStarted {
		t.Fatal(preview, err)
	}
	if _, sent := fake.counts(); sent != 0 {
		t.Fatal("a preview sent")
	}
	// M2: a confirm on a status that moved since is refused.
	if _, err = f.projectStatus("linear_app_project_status_set", awSession, psSetArgs("started", "op-2", map[string]any{"confirm": true, "expected_status_id": pstStarted})); err == nil || !strings.Contains(err.Error(), "changed since the preview") {
		t.Fatal(err)
	}
	before, _ := fake.counts()
	if _, err = f.projectStatus("linear_app_project_status_set", awSession, psSetArgs("started", "op-3", map[string]any{"confirm": true})); err == nil || !strings.Contains(err.Error(), "confirm needs expected_status_id") {
		t.Fatal("a confirm without the expected status:", err)
	}
	if after, _ := fake.counts(); after != before {
		t.Fatal("a confirm without the expected status read Linear")
	}
	set, err := f.projectStatus("linear_app_project_status_set", awSession, psSetArgs("started", "op-1", map[string]any{"confirm": true, "expected_status_id": pstPlanned}))
	if err != nil || set["state"] != "accepted" {
		t.Fatal(set, err)
	}
	if fake.sent[0] != pstProject+"|"+pstStarted {
		t.Fatal(fake.sent)
	}
	reads, _ := fake.counts()
	replay, err := f.projectStatus("linear_app_project_status_set", awSession, psSetArgs("started", "op-1", map[string]any{"confirm": true, "expected_status_id": pstPlanned}))
	if rec, _ := replay["recorded"].(map[string]any); err != nil || rec["state"] != "accepted" {
		t.Fatal(replay, err)
	}
	if again, sent := fake.counts(); again != reads || sent != 1 {
		t.Fatal("a replay reached Linear:", again, sent)
	}
	hist := awJSON(t, f.must(bus.Request{Operation: "history", Workspace: f.workspace, Project: "p", Args: map[string]any{"entity": "project_status"}}).Data)
	rows, _ := hist["events"].([]any)
	if rows == nil {
		rows, _ = hist["history"].([]any)
	}
	found := false
	for _, r := range rows {
		m := r.(map[string]any)
		found = found || m["action"] == "set" && m["changed_by_session"] != nil
	}
	if !found {
		t.Fatal("no audited set naming the session:", hist)
	}
}

// Who closes decides completed: where people close, the coordinator is
// refused before Linear is read, in words naming the rule; the owner,
// without a session, may, with allow_open while issues are open.
func TestPostgresProjectStatusClosingFollowsTheCloseRule(t *testing.T) {
	f, fake := newPSFixture(t)
	_, err := f.projectStatus("linear_app_project_status_set", awSession, psSetArgs("completed", "op-1", nil))
	if err == nil || !strings.Contains(err.Error(), "forbidden: people close Linear project") {
		t.Fatal(err)
	}
	if reads, _ := fake.counts(); reads != 0 {
		t.Fatal("a refused close read Linear")
	}
	fake.facts.Statuses = append(fake.facts.Statuses, ProjectStatus{"f1000000-0000-4000-8000-0000000000a9", "Done", "completed"})
	preview, err := f.projectStatus("linear_app_project_status_set", "", psSetArgs("completed", "op-1", nil))
	if err != nil || preview["needs_allow_open"] != true {
		t.Fatal(preview, err)
	}
	confirm := map[string]any{"confirm": true, "expected_status_id": pstPlanned}
	if _, err = f.projectStatus("linear_app_project_status_set", "", psSetArgs("completed", "op-1", confirm)); err == nil || !strings.Contains(err.Error(), "allow_open") {
		t.Fatal(err)
	}
	confirm["allow_open"] = true
	if set, err := f.projectStatus("linear_app_project_status_set", "", psSetArgs("completed", "op-1", confirm)); err != nil || set["state"] != "accepted" {
		t.Fatal(set, err)
	}
}

// M1 and the setter rule: a Linear project of the same workspace outside
// the binding, or a session that isn't the coordinator, is refused
// before Linear is read; a client calling a bus step itself is refused.
func TestPostgresProjectStatusRefusals(t *testing.T) {
	f, fake := newPSFixture(t)
	if _, err := f.projectStatus("linear_app_project_status", awSession, map[string]any{"linear_project": pstOther}); err == nil || !strings.Contains(err.Error(), "not in this project's binding") {
		t.Fatal(err)
	}
	f.must(bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "other", Args: map[string]any{"instance": "other-i", "role": "worker"}})
	if _, err := f.projectStatus("linear_app_project_status_set", "other", psSetArgs("started", "op-1", nil)); err == nil || !strings.Contains(err.Error(), "designated coordinator session") {
		t.Fatal(err)
	}
	stranger := bus.Identity{Principal: awUnique("stranger") + "@appwrite-test.example", Issuer: bus.GoogleIssuer, Subject: "sub-stranger"}
	if _, err := f.writer.Execute(context.Background(), stranger, bus.Request{Operation: "linear_app_project_status", Workspace: f.workspace, Project: "p", Args: map[string]any{"linear_project": pstProject}}); err == nil || !strings.Contains(err.Error(), "membership") {
		t.Fatal("a stranger:", err)
	}
	if reads, _ := fake.counts(); reads != 0 {
		t.Fatal("a refused request read Linear")
	}
	if _, err := f.store.ExecuteAsIdentity(context.Background(), f.identity(), bus.Request{Operation: "project_status_begin", Workspace: f.workspace, Project: "p", Args: map[string]any{"linear_project": pstProject}}); err == nil || !strings.Contains(err.Error(), "ClawBridge app") {
		t.Fatal("a client ran a server step:", err)
	}
	for _, args := range []map[string]any{
		{"linear_project": pstProject, "query": "{ viewer { id } }"},
		{"linear_project": "not-a-project"},
		{"linear_project": pstProject, "suggest": "yes"},
	} {
		if _, err := f.projectStatus("linear_app_project_status", awSession, args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

// M5: a read-back showing another status leaves the set uncertain, and
// the same operation_id never sends it again.
func TestPostgresProjectStatusUncertainIsNeverResent(t *testing.T) {
	f, fake := newPSFixture(t)
	fake.outcome, fake.code = "uncertain", "read_back_mismatch"
	args := psSetArgs("started", "op-1", map[string]any{"confirm": true, "expected_status_id": pstPlanned})
	set, err := f.projectStatus("linear_app_project_status_set", awSession, args)
	if err != nil || set["state"] != "uncertain" || set["error"] != "read_back_mismatch" {
		t.Fatal(set, err)
	}
	replay, err := f.projectStatus("linear_app_project_status_set", awSession, args)
	if rec, _ := replay["recorded"].(map[string]any); err != nil || rec["state"] != "uncertain" {
		t.Fatal(replay, err)
	}
	if _, sent := fake.counts(); sent != 1 {
		t.Fatal("an uncertain set was sent again")
	}
}

// While the dispatcher is on, a domain's coordinator session sets a
// project's status through the app when every team the project carries is
// a write team of its lane; a cross-lane project, or a team list that
// wasn't read in full, is refused at the claim before any Linear write.
func TestPostgresProjectStatusDomainCoordinator(t *testing.T) {
	f, fake := newPSFixture(t)
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{
		"instance": awSession + "-i", "expected_revision": 2, "mode": "project_set",
		"linear_team_ids": []string{awTeam, awOtherTeam}, "linear_default_team_id": awTeam,
		"linear_project_ids": []string{pstProject}, "linear_workspace_id": awOrg,
		"unpinned": "ai",
		"lanes": []any{
			map[string]any{"name": "ai", "write_team_ids": []any{awTeam}, "default_team_id": awTeam},
			map[string]any{"name": "platform", "write_team_ids": []any{awOtherTeam}, "default_team_id": awOtherTeam},
		},
	}})
	f.must(bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "lead-ai", Args: map[string]any{"instance": "lead-ai-i", "role": "worker"}})
	on := awJSON(t, f.must(bus.Request{Operation: "dispatcher_enable", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "confirm": true}}).Data)
	f.must(bus.Request{Operation: "dispatcher_domain_edit", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": on["revision"], "domain": "all-work", "action": "coordinator", "target_session": "lead-ai"}})
	set := func(session, op string) (map[string]any, error) {
		return f.projectStatus("linear_app_project_status_set", session, psSetArgs("started", op, map[string]any{"confirm": true, "expected_status_id": pstPlanned}))
	}
	// Off until the operator has probed private-team visibility.
	if _, err := set("lead-ai", "op-off"); err == nil || !strings.Contains(err.Error(), "project_status_domain_unproven") {
		t.Fatal("the domain coordinator set the status before the probe:", err)
	}
	f.store.SetProjectStatusDomainProven(true)
	// The project coordinator is the dispatcher now: the former one is
	// refused, before Linear is read.
	if _, err := set(awSession, "op-0"); err == nil || !strings.Contains(err.Error(), "a domain's coordinator session") {
		t.Fatal("a session that is neither coordinator set the status:", err)
	}
	// Teams read in full, all in lane ai.
	fake.facts.Teams = []string{awTeam}
	out, err := set("lead-ai", "op-1")
	if err != nil || out["state"] != "accepted" {
		t.Fatal(out, err)
	}
	_, sent := fake.counts()
	if sent != 1 {
		t.Fatal("sent:", sent)
	}
	refuse := func(op string, teams []string, more bool) {
		t.Helper()
		fake.mu.Lock()
		fake.facts.Status, fake.facts.Teams, fake.facts.TeamsMore = ProjectStatus{pstPlanned, "Planned", "planned"}, teams, more
		fake.mu.Unlock()
		if _, err := set("lead-ai", op); err == nil || !strings.Contains(err.Error(), "outside_lane") {
			t.Fatal(op, "was not refused as outside_lane:", err)
		}
		if _, n := fake.counts(); n != sent {
			t.Fatal(op, "reached Linear:", n)
		}
	}
	refuse("op-2", []string{awTeam, awOtherTeam}, false)
	refuse("op-3", []string{awTeam}, true)
	refuse("op-4", nil, false)
}

// The app's project status read decodes the project's teams: lower-cased,
// a next page marks the list partial, and a missing list is partial too.
func TestPostgresProjectStatusReadDecodesTeams(t *testing.T) {
	f, _ := newPSFixture(t)
	reply := func(teams any) map[string]any {
		project := map[string]any{"id": pstProject, "status": map[string]any{"id": pstPlanned, "name": "Planned", "type": "planned"}}
		if teams != nil {
			project["teams"] = teams
		}
		return map[string]any{"project": project, "projectStatuses": map[string]any{"nodes": []any{
			map[string]any{"id": pstPlanned, "name": "Planned", "type": "planned", "archivedAt": nil},
		}, "pageInfo": map[string]any{"hasNextPage": false}}}
	}
	upper := strings.ToUpper(awTeam)
	for name, c := range map[string]struct {
		teams any
		want  []string
		more  bool
	}{
		"teams lower-cased": {map[string]any{"nodes": []any{map[string]any{"id": upper}, map[string]any{"id": awOtherTeam}}, "pageInfo": map[string]any{"hasNextPage": false}}, []string{awTeam, awOtherTeam}, false},
		"a next page":       {map[string]any{"nodes": []any{map[string]any{"id": awTeam}}, "pageInfo": map[string]any{"hasNextPage": true}}, []string{awTeam}, true},
		"no teams field":    {nil, nil, true},
	} {
		f.api.mu.Lock()
		f.api.projectStatus = reply(c.teams)
		f.api.mu.Unlock()
		got, err := appProjectStatus{f.writer}.read(context.Background(), pstProject)
		if err != nil {
			t.Fatal(name, err)
		}
		if got.TeamsMore != c.more || strings.Join(got.Teams, ",") != strings.Join(c.want, ",") || got.Status.ID != pstPlanned {
			t.Fatal(name, got.Teams, got.TeamsMore, got.Status)
		}
	}
}
