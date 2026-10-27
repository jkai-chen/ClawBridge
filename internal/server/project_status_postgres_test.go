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
