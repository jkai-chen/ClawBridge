package guibackend

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/tui"
)

var update = flag.Bool("update", false, "rewrite the read-* RPC fixtures from the fakes below")

// readsExec answers the reads of the P2-5 pages with synthetic data and
// fails the test on any operation that is not a read the pages use. Its own
// list (not the production one) is what a write would have to slip past.
type readsExec struct {
	t        *testing.T
	mu       sync.Mutex
	ops      []string
	data     map[string]any
	failOps  map[string]error
	offline  bool
	requests []bus.Request
}

func (f *readsExec) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, r.Operation)
	f.requests = append(f.requests, r)
	if r.Session != "" {
		f.t.Errorf("%s was sent with session %q", r.Operation, r.Session)
	}
	if !expectedReads[r.Operation] {
		f.t.Errorf("a non-read operation reached the executor: %s", r.Operation)
		return bus.Result{}, errors.New("not a read")
	}
	if err := f.failOps[r.Operation]; err != nil {
		return bus.Result{}, err
	}
	key := r.Operation
	if r.Operation == "project_overview" {
		key += ":" + r.Project
	}
	data, ok := f.data[key]
	if !ok {
		return bus.Result{}, fmt.Errorf("no fake data for %s", key)
	}
	src := "server"
	if f.offline {
		src = "offline-mirror"
	}
	return bus.Result{Source: src, Data: data}, nil
}

func (f *readsExec) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("unused")
}

// expectedReads lists, by name, every read the pages are allowed to make.
var expectedReads = map[string]bool{
	"list_projects": true, "project_overview": true, "project_threads": true, "project_thread": true,
	"linear_app_status": true, "linear_write_status": true, "intake_policy_get": true, "intake_trust_list": true,
	"intake_list": true, "whoami": true, "dispatcher_model_doctor": true, "dispatcher_get": true,
	"device_list": true, "device_events": true, "owner_transfer_get": true, "invite_list": true, "user_list": true,
}

var expectedLinearReads = map[string]bool{"profiles": true, "issues": true, "issue": true, "intake": true, "project-status": true, "roles": true, "views": true, "cancel-kinds": true}

type fakeLinear struct {
	t   *testing.T
	mu  sync.Mutex
	ops []string
	req []tui.LinearRequest
	err map[string]error
}

func (f *fakeLinear) Do(_ context.Context, r tui.LinearRequest) (tui.LinearData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, r.Operation)
	f.req = append(f.req, r)
	if !expectedLinearReads[r.Operation] {
		f.t.Errorf("a non-read Linear action reached the source: %s", r.Operation)
		return tui.LinearData{}, errors.New("not a read")
	}
	if r.Confirm || r.Roles != nil || r.CancelKinds != nil {
		f.t.Errorf("%s carried a change", r.Operation)
	}
	if err := f.err[r.Operation]; err != nil {
		return tui.LinearData{}, err
	}
	switch r.Operation {
	case "profiles":
		b := bus.TaskboardBindingV2{Configured: true, Revision: 7, MappingVersion: 2, UpdatedAt: "2026-10-30T09:00:00Z", Provider: "linear", Mode: "project",
			LinearTeamIDs: []string{"team_atl", "team_hrb"}, LinearDefaultTeamID: "team_atl", LinearProjectIDs: []string{"lp_billing"},
			LinearOtherTeams: map[string]string{"team_ops": "read"},
			Lanes:            []bus.Lane{{Name: "backend", WriteTeamIDs: []string{"team_atl"}, DefaultTeamID: "team_atl"}}, Unpinned: "read_only"}
		return tui.LinearData{
			Profiles:       []linearconn.Profile{{ID: "lp_mara", Name: "mara", Access: linearconn.Write, AccountLabel: "Mara Lindqvist", WorkspaceLabel: "Northwind"}},
			BindingV2:      &b,
			WithoutProject: []linearboard.TeamWithoutProject{{TeamID: "team_hrb", Name: "Harbor", Inert: true}},
		}, nil
	case "issues":
		page := linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{
			{Issue: linearprovider.Issue{ID: "iss_1", Identifier: "ATL-212", Title: "Retry failed invoices", TeamID: "team_atl", State: "In Progress", Assignee: "Mara Lindqvist", URL: "https://linear.example.test/atl-212"}},
			{Issue: linearprovider.Issue{ID: "iss_2", Identifier: "HRB-88", Title: "Screen reader skips the totals row", TeamID: "team_hrb", State: "Todo"}},
		}, NextCursor: "cur_2", Complete: false}
		return tui.LinearData{IssuesV2: &page}, nil
	case "issue":
		return tui.LinearData{IssueV2: &linearprovider.IssueV2{Issue: linearprovider.Issue{ID: r.IssueID, Identifier: "ATL-212", Title: "Retry failed invoices", Description: "Back off, then retry up to 3 times.", TeamID: "team_atl", State: "In Progress"}}}, nil
	case "intake":
		return tui.LinearData{
			Members: []linearprovider.Member{
				{ID: "lin_mara", Name: "Mara Lindqvist", TeamID: "team_atl", Active: true},
				{ID: "lin_kofi", Name: "Kofi Mensah", TeamID: "team_atl", Active: true},
				{ID: "lin_ines", Name: "Ines Duarte", TeamID: "team_atl", Active: true},
				{ID: "lin_priya", Name: "Priya Raman", TeamID: "team_atl", Active: true},
				{ID: "lin_ines", Name: "Ines Duarte", TeamID: "team_hrb", Active: true},
				{ID: "lin_kofi", Name: "Kofi Mensah", TeamID: "team_hrb", Active: true},
				{ID: "lin_tomas", Name: "Tomás Okafor", TeamID: "team_hrb", Active: true},
				{ID: "lin_gone", Name: "Gone Person", TeamID: "team_hrb", Active: false},
			},
			IntakeLabels: []tui.IntakeLabel{{TeamID: "team_atl", Found: 1, Complete: true}, {TeamID: "team_hrb", Found: 0, Complete: true}},
		}, nil
	case "cancel-kinds":
		return tui.LinearData{CancelKindsView: &linearboard.CancelKindsView{Revision: 3, CancelKinds: []string{"duplicate"}, Kinds: []string{"duplicate", "canceled"}, Line: "Agents may cancel in: duplicate."}}, nil
	case "project-status":
		ps := tui.ProjectStatus{LinearProject: r.LinearProject, Suggestion: "completed", SuggestionNote: "Every issue is closed."}
		ps.Status.Name = "In progress"
		ps.Issues.Total, ps.Issues.Open, ps.Issues.Complete = 12, 0, true
		return tui.LinearData{ProjectStatus: &ps}, nil
	case "views":
		return tui.LinearData{Views: []tui.TeamViews{
			{TeamID: "team_atl", Views: []linearprovider.View{{ID: "vw_1", Name: "Billing board", Kind: "issue", TeamID: "team_atl", TeamKey: "ATL", TeamName: "Atlas"}}, OtherKinds: 1, WorkspaceViews: 2},
			{TeamID: "team_hrb", Err: "Linear refused the read"},
		}}, nil
	case "roles":
		return tui.LinearData{Roles: &tui.TeamRolesView{
			Revision: 7,
			Teams:    []tui.RolesTeam{{TeamID: "team_atl", Leads: []string{"lin_mara"}, Bound: true}, {TeamID: "team_hrb", Leads: []string{"lin_ines"}, Bound: true}},
			People: []bus.TeamRolePerson{
				{LinearUserID: "lin_mara", Role: "lead", Teams: []string{"team_atl"}, Principal: "mara@example.test"},
				{LinearUserID: "lin_kofi", Role: "engineer", Teams: []string{"team_atl", "team_hrb"}, Principal: "kofi@example.test"},
				{LinearUserID: "lin_ines", Role: "reviewer", Teams: []string{"team_atl", "team_hrb"}, Principal: "ines@example.test"},
				{LinearUserID: "lin_tomas", Role: "engineer", Teams: []string{"team_hrb"}, Principal: "tomas@example.test"},
			},
			YouLead: []string{"team_atl"}, Owner: true,
			Users: []linearprovider.WorkspaceUser{{ID: "lin_mara", Name: "Mara Lindqvist", Active: true}, {ID: "lin_priya", Name: "Priya Raman", Active: true}},
		}}, nil
	}
	return tui.LinearData{}, nil
}

func syntheticData() map[string]any {
	return map[string]any{
		"list_projects": map[string]any{"projects": []any{
			map[string]any{"id": "prj_atlas", "slug": "atlas-api", "name": "Atlas API", "role": "owner", "git_remote": "git@example.test:northwind/atlas-api.git"},
			map[string]any{"id": "prj_harbor", "slug": "harbor-web", "name": "Harbor Web", "role": "member"},
		}},
		"project_overview:atlas-api": map[string]any{
			"project":     map[string]any{"role": "owner"},
			"coordinator": map[string]any{"name": "lead-atlas", "status": "active"},
			"counts":      map[string]any{"active": 2, "retired": 1, "moved": 0, "stale": 0},
			"sessions":    []any{map[string]any{"id": "ses_1", "name": "atlas/billing-retry", "owner_principal": "mara@example.test", "status": "active", "agent_kind": "claude-code"}},
			"linear": map[string]any{"configured": true, "mode": "project", "names_updated_at": "2026-10-30T09:00:00Z",
				"status_policy": map[string]any{"agent_may_cancel_kinds": []any{"duplicate"}},
				"teams":         []any{map[string]any{"id": "team_atl", "name": "Atlas", "default": true}, map[string]any{"id": "team_hrb", "name": "Harbor"}},
				"projects":      []any{map[string]any{"id": "lp_billing", "name": "Billing retry", "closes": "agent"}}},
		},
		"project_overview:harbor-web": map[string]any{
			"project": map[string]any{"role": "member"}, "counts": map[string]any{"active": 1}, "sessions": []any{},
			"linear": map[string]any{"configured": false},
		},
		"project_threads": map[string]any{
			"threads": []any{
				map[string]any{"thread_id": "thr_1", "kind": "direct", "started_at": "2026-10-31T08:00:00Z", "last_at": "2026-10-31T09:30:00Z", "messages": 3, "participants": []any{"atlas/billing-retry", "lead-atlas"}, "participants_more": 0},
				map[string]any{"thread_id": "thr_2", "kind": "escalation", "started_at": "2026-10-30T14:00:00Z", "last_at": "2026-10-30T14:05:00Z", "messages": 1, "participants": []any{"harbor/a11y-audit"}, "participants_more": 2},
			},
			"next": "", "readable_from": "2026-10-01T00:00:00Z",
		},
		"project_thread": map[string]any{
			"messages": []any{
				map[string]any{"sequence": 1, "id": "msg_1", "sender": "atlas/billing-retry", "address": "lead-atlas", "reply_to": "", "kind": "question", "body": "May I take ATL-212?", "coordination": "", "created_at": "2026-10-31T08:00:00Z",
					"recipients": []any{map[string]any{"name": "lead-atlas", "delivered_at": "2026-10-31T08:00:01Z", "read_at": "2026-10-31T08:02:00Z"}}},
				map[string]any{"sequence": 2, "id": "msg_2", "sender": "lead-atlas", "address": "atlas/billing-retry", "reply_to": "msg_1", "kind": "answer", "body": "Yes, go ahead.", "coordination": "", "created_at": "2026-10-31T08:03:00Z", "recipients": []any{}},
			},
			"next": 0, "readable_from": "2026-10-01T00:00:00Z",
		},
		"intake_policy_get": map[string]any{"mode": "label", "revision": 4, "signals": []any{"S1", "S2"}},
		"intake_trust_list": []any{map[string]any{"linear_user_id": "lin_mara"}, map[string]any{"linear_user_id": "lin_kofi"}},
		"intake_list": []any{
			map[string]any{"id": "int_1", "issue": "HRB-91", "state": "queued", "created_at": "2026-10-31T09:00:00Z"},
			map[string]any{"id": "int_2", "issue": "HRB-70", "state": "done", "created_at": "2026-10-30T09:00:00Z"},
		},
		"linear_write_status": map[string]any{"writes": []any{map[string]any{"id": "w_1", "state": "queued", "operation": "comment", "issue": "ATL-212", "created_at": "2026-10-31T09:40:00Z"}}},
		"linear_app_status": map[string]any{
			"outbox": map[string]any{"queued": 1, "sending": 0, "dead_24h": 0, "expired_24h": 0, "uncertain_24h": 0, "oldest_queued_age_s": 360},
			"budget": map[string]any{"used": 143, "limit": 600, "source": "app", "window_resets_at": "2026-10-31T10:00:00Z", "by": map[string]any{"people": 20, "agents": 123}},
		},
		"dispatcher_model_doctor": map[string]any{
			"seal_configured": true, "seal_previous": false, "model_on": true,
			"holders": []any{map[string]any{"principal": "mara@example.test", "you": true, "key": "active", "config_revision": 2, "agent_status": "running", "agent": "running"}},
		},
		"dispatcher_get": map[string]any{
			"enabled": true, "mode": "model", "revision": 5, "updated_at": "2026-10-30T10:00:00Z",
			"model": map[string]any{"use": "engine", "provider_id": "example-ai", "model": "example-model-1", "key": "active", "agent": "running"},
		},
		"whoami": map[string]any{"principal": "mara@example.test", "workspace": map[string]any{"owner_principal": "mara@example.test"}},
		"device_list": map[string]any{"devices": []any{
			map[string]any{"device_id": "dev_1", "principal": "mara@example.test", "label": "mara-mbp", "kind": "device", "status": "active", "created_at": "2026-09-01T09:00:00Z", "last_used_at": "2026-10-31T09:50:00Z", "key_store": "keyring"},
			map[string]any{"device_id": "dev_2", "principal": "mara@example.test", "label": "ci-runner", "kind": "ci", "status": "active", "created_at": "2026-10-01T09:00:00Z", "expires_at": "2027-01-01T00:00:00Z"},
		}},
		"owner_transfer_get": map[string]any{"pending": nil},
		"device_events":      map[string]any{"events": []any{map[string]any{"event_id": "evt_1", "kind": "added", "label": "mara-air", "created_at": "2026-10-31T07:00:00Z", "acked_at": nil}}},
		"invite_list": map[string]any{"invites": []any{
			map[string]any{"invite_id": "inv_1", "email": "tomas@example.test", "status": "pending", "expires_at": "2026-11-06T00:00:00Z", "grants": map[string]any{"prj_harbor": "member"}},
		}},
		"user_list": map[string]any{"users": []any{
			map[string]any{"principal": "mara@example.test", "roles": []any{map[string]any{"slug": "atlas-api", "role": "owner"}}, "devices": []any{}, "invited_by": ""},
			map[string]any{"principal": "kofi@example.test", "roles": []any{map[string]any{"slug": "atlas-api", "role": "member"}}, "devices": []any{map[string]any{"device_id": "dev_9", "label": "kofi-pc"}}, "invited_by": "mara@example.test"},
			map[string]any{"principal": "ines@example.test", "roles": []any{map[string]any{"slug": "harbor-web", "role": "member"}}, "devices": []any{}, "invited_by": "mara@example.test"},
		}},
	}
}

func readsServer(t *testing.T, hosted bool) (*Server, *readsExec, *fakeLinear) {
	t.Helper()
	exec := &readsExec{t: t, data: syntheticData()}
	lin := &fakeLinear{t: t}
	deps := ReadDeps{Exec: exec, Linear: lin, Hosted: hosted, Scope: Scope{Workspace: "ws_example", Project: "atlas-api"}}
	if hosted {
		deps.DeviceID = "dev_1"
	}
	s := &Server{Hello: Hello{ClientVersion: "1.0.0-test", Mode: "cloud", Platform: "linux"}, Methods: ReadMethods(deps)}
	return s, exec, lin
}

func readCall(t *testing.T, s *Server, method, params string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
	rec := post(t, s.Handler(), "application/json", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d", method, rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

type readCase struct {
	file, description, method, params string
	hosted                            bool
}

var readCases = []readCase{
	{"read-projects-list.json", "projects.list answers list_projects with each project's overview", "projects.list", `{}`, true},
	{"read-project-threads.json", "project.threads answers project_threads for a project the viewer owns", "project.threads", `{"project":"atlas-api"}`, true},
	{"read-project-thread.json", "project.thread answers one thread's messages", "project.thread", `{"project":"atlas-api","thread_id":"thr_1"}`, true},
	{"read-linear-connection.json", "linear.connection lists the connections and the access of each", "linear.connection", `{}`, true},
	{"read-linear-binding.json", "linear.binding answers the binding v2 with the names stored beside it", "linear.binding", `{}`, true},
	{"read-linear-tasks.json", "linear.tasks answers one page of the board's issues", "linear.tasks", `{"profile":"lp_mara"}`, true},
	{"read-linear-issue.json", "linear.issue answers one issue", "linear.issue", `{"profile":"lp_mara","issue_id":"iss_1"}`, true},
	{"read-linear-intake.json", "linear.intake answers the policy, the trusted people, the queue and the label check", "linear.intake", `{"profile":"lp_mara"}`, true},
	{"read-linear-cancel-kinds.json", "linear.cancelKinds answers who may cancel", "linear.cancelKinds", `{"profile":"lp_mara"}`, true},
	{"read-linear-project-status.json", "linear.projectStatus answers a Linear project's status and a suggestion", "linear.projectStatus", `{"profile":"lp_mara","linear_project":"lp_billing","suggest":true}`, true},
	{"read-linear-views.json", "linear.views answers the bound teams' shared views", "linear.views", `{"profile":"lp_mara"}`, true},
	{"read-linear-queue.json", "linear.queue answers the queued writes and the hour's budget (hosted)", "linear.queue", `{}`, true},
	{"read-linear-queue-local.json", "linear.queue in local mode says it is not available", "linear.queue", `{}`, false},
	{"read-teams-roles.json", "teams.roles answers the roles mapping with names", "teams.roles", `{"profile":"lp_mara"}`, true},
	{"read-teams-members.json", "teams.members answers the bound teams' Linear members and who is trusted", "teams.members", `{"profile":"lp_mara"}`, true},
	{"read-dispatcher-status.json", "dispatcher.status answers the model doctor, the policy and the hour's budget", "dispatcher.status", `{}`, true},
	{"read-account-devices.json", "account.devices answers the devices, a pending ownership offer and the alerts (hosted)", "account.devices", `{}`, true},
	{"read-account-invites.json", "account.invites answers the invites with the project slugs", "account.invites", `{}`, true},
	{"read-account-users.json", "account.users answers the members (main owner only)", "account.users", `{}`, true},
	{"read-account-local.json", "account.devices in local mode says Account is for hosted workspaces", "account.devices", `{}`, false},
}

// TestReadFixtures plays each read-* fixture against the methods and the
// fakes above. -update rewrites the files from the fakes; the frontend's
// tests read the same files.
func TestReadFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "rpc")
	for _, c := range readCases {
		s, exec, lin := readsServer(t, c.hosted)
		got := readCall(t, s, c.method, c.params)
		if got["error"] != nil {
			t.Fatalf("%s: %v", c.file, got["error"])
		}
		path := filepath.Join(dir, c.file)
		if *update {
			doc := map[string]any{
				"description": c.description,
				"request":     map[string]any{"jsonrpc": "2.0", "id": 1, "method": c.method, "params": json.RawMessage(c.params)},
				"response":    got,
			}
			b, _ := json.MarshalIndent(doc, "", "  ")
			if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct{ Response map[string]any }
		if err = json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(path, err)
		}
		if !reflect.DeepEqual(got, fx.Response) {
			gb, _ := json.MarshalIndent(got, "", "  ")
			t.Fatalf("%s differs (run with -update):\n%s", c.file, gb)
		}
		for _, op := range exec.ops {
			if !expectedReads[op] {
				t.Errorf("%s ran %s", c.file, op)
			}
		}
		_ = lin
	}
}

func TestRequestsAreTheTUIsOwn(t *testing.T) {
	s, exec, lin := readsServer(t, true)
	readCall(t, s, "projects.list", `{}`)
	readCall(t, s, "project.threads", `{"project":"atlas-api","before":"cur"}`)
	readCall(t, s, "project.thread", `{"project":"atlas-api","thread_id":"thr_1","after":"4"}`)
	readCall(t, s, "linear.tasks", `{"profile":"lp_mara","cursor":"c","limit":500}`)
	want := []string{"list_projects", "project_overview", "project_overview", "project_threads", "project_thread"}
	if !reflect.DeepEqual(exec.ops, want) {
		t.Fatalf("ops %v", exec.ops)
	}
	for _, r := range exec.requests {
		if r.Workspace != "ws_example" || r.Session != "" || r.Args["instance"] != nil {
			t.Fatalf("request %+v", r)
		}
	}
	if r := exec.requests[1]; r.Project != "atlas-api" {
		t.Fatalf("overview %+v", r)
	}
	if r := exec.requests[3]; r.Project != "atlas-api" || r.Args["before"] != "cur" {
		t.Fatalf("threads %+v", r)
	}
	if r := exec.requests[4]; r.Args["thread_id"] != "thr_1" || r.Args["after"] != "4" {
		t.Fatalf("thread %+v", r)
	}
	if r := lin.req[0]; r.Operation != "issues" || r.ProfileID != "lp_mara" || r.Limit != 50 || r.Cursor != "c" {
		t.Fatalf("issues %+v", r)
	}
}

// TestNoPageWrites calls every method and checks that only reads reached
// the executor and the Linear source (the fakes also fail the test on one).
func TestNoPageWrites(t *testing.T) {
	for _, hosted := range []bool{true, false} {
		s, exec, lin := readsServer(t, hosted)
		methods := make([]string, 0, len(s.Methods))
		for name, m := range s.Methods {
			if m.Class != Read {
				t.Fatalf("%s is not Read-class", name)
			}
			methods = append(methods, name)
		}
		sort.Strings(methods)
		if len(methods) != 18 {
			t.Fatalf("methods %v", methods)
		}
		params := `{"project":"atlas-api","thread_id":"thr_1","profile":"lp_mara","issue_id":"i","linear_project":"lp_billing"}`
		for _, name := range methods {
			readCall(t, s, name, params)
		}
		for _, op := range exec.ops {
			if !expectedReads[op] {
				t.Errorf("executor ran %s", op)
			}
		}
		for _, op := range lin.ops {
			if !expectedLinearReads[op] {
				t.Errorf("Linear source ran %s", op)
			}
		}
		if !hosted {
			for _, op := range exec.ops {
				switch op {
				case "device_list", "invite_list", "user_list", "linear_write_status", "device_events":
					t.Errorf("local mode ran %s", op)
				}
			}
		}
	}
}

func TestReadGuardsRefuseWrites(t *testing.T) {
	exec := &recordingExec{}
	g := ReadOnly(exec)
	for _, op := range []string{"device_ack", "device_revoke", "invite_create", "comment_watch_budget_set", "intake_trust_add", "team_roles_set", "send", "dispatcher_model_setup", "owner_transfer_offer"} {
		if _, err := g.Execute(context.Background(), bus.Request{Operation: op}); err == nil {
			t.Errorf("%s passed the read guard", op)
		}
	}
	if _, err := g.Execute(context.Background(), bus.Request{Operation: "list_projects", Session: "s"}); err == nil {
		t.Error("a request with a session passed")
	}
	if exec.n != 0 {
		t.Fatalf("%d refused requests reached the executor", exec.n)
	}
	if _, err := g.Execute(context.Background(), bus.Request{Operation: "list_projects"}); err != nil || exec.n != 1 {
		t.Fatal(err, exec.n)
	}
	src := &recordingLinear{}
	l := ReadOnlyLinear(src)
	for _, r := range []tui.LinearRequest{
		{Operation: "connect"}, {Operation: "logout"}, {Operation: "names-refresh"}, {Operation: "link"}, {Operation: "unlink"},
		{Operation: "roles-set"}, {Operation: "cancel-kinds-set"}, {Operation: "project-status-set"}, {Operation: "preview"}, {Operation: "teams"},
		{Operation: "roles", Roles: &tui.RolesChange{}}, {Operation: "cancel-kinds", Confirm: true},
	} {
		if _, err := l.Do(context.Background(), r); err == nil {
			t.Errorf("%s passed the Linear read guard", r.Operation)
		}
	}
	if src.n != 0 {
		t.Fatalf("%d refused actions reached the source", src.n)
	}
	if _, err := l.Do(context.Background(), tui.LinearRequest{Operation: "roles"}); err != nil || src.n != 1 {
		t.Fatal(err, src.n)
	}
}

type recordingExec struct{ n int }

func (r *recordingExec) Execute(context.Context, bus.Request) (bus.Result, error) {
	r.n++
	return bus.Result{}, nil
}
func (r *recordingExec) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}

type recordingLinear struct{ n int }

func (r *recordingLinear) Do(context.Context, tui.LinearRequest) (tui.LinearData, error) {
	r.n++
	return tui.LinearData{}, nil
}

func TestRefusalsStayServerSide(t *testing.T) {
	s, exec, _ := readsServer(t, true)
	exec.failOps = map[string]error{"project_threads": errors.New("project threads are read by the project's owner")}
	m := readCall(t, s, "project.threads", `{"project":"harbor-web"}`)
	e, _ := m["error"].(map[string]any)
	if e == nil || !strings.Contains(fmt.Sprint(e["message"]), "owner") {
		t.Fatalf("reply %v", m)
	}
	exec.failOps = map[string]error{"dispatcher_model_doctor": errors.New("this needs owner permission")}
	m = readCall(t, s, "dispatcher.status", `{}`)
	res := m["result"].(map[string]any)
	if res["denied"] != "Only a project owner sees and sets up the dispatcher model." || res["doctor"] != nil || res["view"] == nil {
		t.Fatalf("reply %v", m)
	}
}

func TestParamsAndErrors(t *testing.T) {
	s, exec, lin := readsServer(t, true)
	for method, params := range map[string]string{
		"project.threads": `{}`, "project.thread": `{"project":"p"}`, "linear.issue": `{"profile":"x"}`,
		"linear.projectStatus": `{"profile":"x"}`, "linear.tasks": `"nope"`,
	} {
		m := readCall(t, s, method, params)
		if errorCode(m) != CodeParams {
			t.Errorf("%s %s: %v", method, params, m)
		}
	}
	// A Linear error reaches the page as the error text.
	lin.err = map[string]error{"views": errors.New("Linear is not connected")}
	if m := readCall(t, s, "linear.views", `{}`); !strings.Contains(fmt.Sprint(m["error"]), "not connected") {
		t.Fatal(m)
	}
	// Intake still answers, with a note, when the connection fails.
	lin.err = map[string]error{"intake": errors.New("no connection")}
	res := readCall(t, s, "linear.intake", `{}`)["result"].(map[string]any)
	if !strings.Contains(res["names_note"].(string), "no connection") {
		t.Fatal(res)
	}
	// An offline copy is refused where the page needs the server.
	exec.offline = true
	if m := readCall(t, s, "linear.intake", `{}`); m["error"] == nil {
		t.Fatal(m)
	}
	// No Linear source: the page gets the TUI's sentence.
	s2 := &Server{Methods: ReadMethods(ReadDeps{Exec: &readsExec{t: t, data: syntheticData()}, Scope: Scope{Workspace: "w", Project: "p"}})}
	if m := readCall(t, s2, "linear.connection", `{}`); !strings.Contains(fmt.Sprint(m["error"]), "unavailable") {
		t.Fatal(m)
	}
	// A connection error with a partial answer still lists the profiles.
	lin2 := &partialLinear{}
	s3 := &Server{Methods: ReadMethods(ReadDeps{Exec: &readsExec{t: t, data: syntheticData()}, Linear: lin2, Scope: Scope{Workspace: "w", Project: "p"}})}
	res = readCall(t, s3, "linear.connection", `{}`)["result"].(map[string]any)
	if len(res["profiles"].([]any)) != 1 || res["binding_error"] == nil {
		t.Fatal(res)
	}
}

type partialLinear struct{}

func (partialLinear) Do(context.Context, tui.LinearRequest) (tui.LinearData, error) {
	return tui.LinearData{Profiles: []linearconn.Profile{{ID: "p1", Name: "n"}}}, errors.New("binding unreadable")
}
