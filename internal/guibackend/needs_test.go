package guibackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
)

// scriptExec answers each operation from a table and records the calls.
type scriptExec struct {
	mu     sync.Mutex
	ops    []string
	data   map[string]any
	err    map[string]error
	mirror map[string]bool
}

func (f *scriptExec) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, r.Operation)
	if e := f.err[r.Operation]; e != nil {
		return bus.Result{}, e
	}
	src := "server"
	if f.mirror[r.Operation] {
		src = "offline-mirror"
	}
	d, ok := f.data[r.Operation]
	if !ok {
		return bus.Result{}, errors.New("unexpected " + r.Operation)
	}
	return bus.Result{Source: src, Data: d}, nil
}

func syntheticExec() *scriptExec {
	return &scriptExec{
		data: map[string]any{
			"dispatcher_escalations_list": map[string]any{"escalations": []any{
				map[string]any{"id": "esc_1", "session": "atlas-billing", "issue_identifier": "ATL-212", "revision": "14", "reason_code": "overlap",
					"state": "open", "kind": "claim", "question": "Take ATL-212? It overlaps a claim.", "options": []any{"approve", "release", "wait"}, "created_at": "2026-01-02T03:00:00Z"},
				map[string]any{"id": "esc_2", "session": "done", "issue_identifier": "ATL-1", "revision": "1", "state": "answered", "created_at": "2026-01-01T00:00:00Z"},
			}},
			"list_sessions": map[string]any{"sessions": []any{
				map[string]any{"id": "ses_h", "name": "harbor-a11y", "assignment": map[string]any{"id": "asg_1", "state": "proposed", "issue_identifier": "HRB-88", "revision": 3, "proposed_at": "2026-01-02T03:10:00Z"}},
			}},
			"intake_list": []any{
				map[string]any{"issue_uuid": "11111111-1111-1111-1111-111111111111", "issue_identifier": "HRB-91", "state": "queued", "offers": 0, "signals": "[\"S1\"]", "created_at": "2026-01-02T03:20:00Z"},
			},
			"user_list": map[string]any{"users": []any{map[string]any{"principal": "mara@example.test"}}},
			"team_roles_get": map[string]any{"people": []any{
				map[string]any{"linear_user_id": "lin_priya", "role": "engineer", "teams": []any{"team_hrb"}},
				map[string]any{"linear_user_id": "lin_mara", "role": "lead", "teams": []any{"team_hrb"}, "principal": "mara@example.test"},
				map[string]any{"linear_user_id": "lin_ghost", "role": "engineer", "teams": []any{"team_hrb"}, "principal": "gone@example.test"},
			}},
			"list_projects": map[string]any{"projects": []any{map[string]any{"id": "prj_example01", "slug": "atlas-api", "name": "Atlas API"}}},
			"list_members":  map[string]any{"members": []any{map[string]any{"principal": "mara@example.test", "role": "owner"}, map[string]any{"principal": "kofi@example.test", "role": "member"}}},
		},
		err: map[string]error{}, mirror: map[string]bool{},
	}
}

func syntheticDoctor(context.Context) (doctor.Report, error) {
	return doctor.Report{Status: doctor.Warn, GeneratedAt: "2026-01-02T03:04:05Z", Checks: []doctor.Check{
		{ID: "profile", Title: "Profile", Status: doctor.Pass, Message: "fine"},
		{ID: "mcp.codex.clawbridge", Title: "MCP entry", Status: doctor.Warn, Message: "points at an older binary", Action: "clawbridge mcp install"},
		{ID: "x", Title: "Broken", Status: doctor.Fail, Message: "bad"},
		{ID: "note", Title: "Note", Status: doctor.Info, Message: "info"},
	}}, nil
}

func needsCfg(f *scriptExec, principal string) NeedsConfig {
	return NeedsConfig{Exec: f, Scope: Scope{Workspace: "ws_example", Project: "atlas"}, Doctor: syntheticDoctor, Mode: "cloud", Principal: principal}
}

var needsAllowedOps = map[string]bool{"list_projects": true, "dispatcher_escalations_list": true, "list_sessions": true, "intake_list": true, "user_list": true, "team_roles_get": true, "list_members": true}

func assertOnlyReads(t *testing.T, f *scriptExec) {
	t.Helper()
	for _, op := range f.ops {
		if !needsAllowedOps[op] {
			t.Errorf("needs.list called %s", op)
		}
	}
}

func kindOf(r NeedsResult, k string) NeedsKind {
	for _, x := range r.Kinds {
		if x.Kind == k {
			return x
		}
	}
	return NeedsKind{}
}

func TestComposeEveryKind(t *testing.T) {
	f := syntheticExec()
	r := Compose(context.Background(), needsCfg(f, "mara@example.test"), time.Unix(0, 0))
	want := map[string]int{KindEscalation: 1, KindClaim: 1, KindIntake: 1, KindPerson: 2, KindDoctor: 2}
	for k, n := range want {
		if got := kindOf(r, k); got.Status != "ok" || got.Count != n {
			t.Errorf("%s = %+v, want %d ok", k, got, n)
		}
	}
	if len(r.Items) != 7 || r.Viewer != "owner" {
		t.Fatalf("items %d, viewer %q", len(r.Items), r.Viewer)
	}
	byID := map[string]NeedsItem{}
	for _, it := range r.Items {
		byID[it.Kind+"/"+it.ID] = it
		if it.Project != "atlas" {
			t.Errorf("%s project %q", it.ID, it.Project)
		}
		for _, a := range it.Actions {
			if a.Enabled || a.Reason != "Not in this build yet." {
				t.Errorf("%s action %+v is not disabled", it.ID, a)
			}
		}
	}
	esc := byID["escalation/esc_1"]
	if esc.Facts["session"] != "atlas-billing" || esc.Facts["issue"] != "ATL-212" || esc.Facts["revision"] != "14" || esc.Preview != "Take ATL-212? It overlaps a claim." || esc.Actor.State != "yes" {
		t.Errorf("escalation %+v", esc)
	}
	claim := byID["claim/asg_1"]
	if claim.Facts["revision"] != "3" || claim.Facts["issue"] != "HRB-88" || len(claim.Actions) != 3 {
		t.Errorf("claim %+v", claim)
	}
	if byID["intake/11111111-1111-1111-1111-111111111111"].Facts["issue"] != "HRB-91" {
		t.Errorf("intake %+v", byID)
	}
	if byID["person/lin_priya"].Facts["reason"] != "no_link" || byID["person/lin_ghost"].Facts["reason"] != "link_not_a_member" || byID["person/lin_mara"].ID != "" {
		t.Errorf("people %+v", byID)
	}
	if d := byID["doctor/mcp.codex.clawbridge"]; d.Preview != "points at an older binary" || d.Facts["fix"] != "clawbridge mcp install" {
		t.Errorf("doctor %+v", d)
	}
	assertOnlyReads(t, f)
	// The claims read asks for proposed assignments only, the intake read for queued.
}

func TestComposeRequestArguments(t *testing.T) {
	var got []bus.Request
	f := syntheticExec()
	cfg := needsCfg(f, "mara@example.test")
	cfg.Exec = recorder{f, &got}
	Compose(context.Background(), cfg, time.Now())
	for _, r := range got {
		switch r.Operation {
		case "list_sessions":
			if r.Args["assignment_state"] != "proposed" {
				t.Errorf("list_sessions args %v", r.Args)
			}
		case "intake_list":
			if r.Args["state"] != "queued" {
				t.Errorf("intake_list args %v", r.Args)
			}
		}
		if r.Workspace != "ws_example" || r.Project != "atlas" {
			t.Errorf("%s routed to %s/%s", r.Operation, r.Workspace, r.Project)
		}
	}
}

type recorder struct {
	Reader
	into *[]bus.Request
}

func (r recorder) Execute(ctx context.Context, q bus.Request) (bus.Result, error) {
	*r.into = append(*r.into, q)
	return r.Reader.Execute(ctx, q)
}

func TestComposeRefusedKindIsUnavailableOthersStay(t *testing.T) {
	f := syntheticExec()
	f.err["dispatcher_escalations_list"] = errors.New("forbidden: project lead or owner permission required")
	f.mirror["intake_list"] = true
	r := Compose(context.Background(), needsCfg(f, "mara@example.test"), time.Now())
	if k := kindOf(r, KindEscalation); k.Status != "unavailable" || !strings.Contains(k.Reason, "forbidden") || k.Count != 0 {
		t.Errorf("escalation %+v", k)
	}
	if k := kindOf(r, KindIntake); k.Status != "unavailable" || !strings.Contains(k.Reason, "offline copy") {
		t.Errorf("intake %+v", k)
	}
	if k := kindOf(r, KindClaim); k.Status != "ok" || k.Count != 1 {
		t.Errorf("claim %+v", k)
	}
	for _, it := range r.Items {
		if it.Kind == KindEscalation || it.Kind == KindIntake {
			t.Errorf("item of an unavailable kind: %+v", it)
		}
	}
}

func TestComposeUnlinkedNeedsUserList(t *testing.T) {
	f := syntheticExec()
	f.err["user_list"] = errors.New("refused: only the workspace owner may list users")
	r := Compose(context.Background(), needsCfg(f, "kofi@example.test"), time.Now())
	k := kindOf(r, KindPerson)
	if k.Status != "unavailable" || !strings.Contains(k.Reason, "workspace owner") {
		t.Errorf("person %+v", k)
	}
	for _, it := range r.Items {
		if it.Kind == KindPerson {
			t.Errorf("guessed a person: %+v", it)
		}
		if it.Kind == KindClaim && (it.Actor.State != "no" || !strings.Contains(it.Actor.Text, "Only the project owner or a team lead")) {
			t.Errorf("member's claim actor %+v", it.Actor)
		}
	}
	if r.Viewer != "member" {
		t.Errorf("viewer %q", r.Viewer)
	}
}

func TestComposeLeadAndLocalMode(t *testing.T) {
	f := syntheticExec()
	f.data["list_members"] = map[string]any{"members": []any{map[string]any{"principal": "mara@example.test", "role": "member"}}}
	r := Compose(context.Background(), needsCfg(f, "mara@example.test"), time.Now())
	for _, it := range r.Items {
		if it.Kind == KindClaim && it.Actor.State != "depends" {
			t.Errorf("a team lead's claim actor %+v", it.Actor)
		}
	}
	cfg := needsCfg(syntheticExec(), "")
	cfg.Mode = "local"
	if r = Compose(context.Background(), cfg, time.Now()); r.Viewer != "owner" {
		t.Errorf("local viewer %q", r.Viewer)
	}
}

func TestExecutorWrapperRefusesWrites(t *testing.T) {
	f := syntheticExec()
	_, err := readOnly{f}.Execute(context.Background(), bus.Request{Operation: "assignment_confirm"})
	if err == nil || len(f.ops) != 0 {
		t.Fatalf("a write reached the executor: %v %v", err, f.ops)
	}
	if _, err = (readOnly{f}).Execute(context.Background(), bus.Request{Operation: "list_sessions", Session: "s"}); err == nil || len(f.ops) != 0 {
		t.Fatalf("a read naming a session reached the executor: %v %v", err, f.ops)
	}
	for _, op := range []string{"inbox", "mark_read", "send"} {
		if needsReadOps[op] {
			t.Errorf("%s is allowed for needs.list", op)
		}
	}
}

func TestNeedsListMethodIsRead(t *testing.T) {
	m := NeedsList(needsCfg(syntheticExec(), "mara@example.test"))
	if m.Class != Read {
		t.Fatal("needs.list must be Read-class")
	}
	out, err := m.Call(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = json.Marshal(out); err != nil {
		t.Fatal(err)
	}
}

func TestNeedsWatcherNotifiesOnlyOnChange(t *testing.T) {
	f := syntheticExec()
	cfg := needsCfg(f, "mara@example.test")
	var w NeedsWatcher
	at := time.Unix(0, 0)
	if w.Step(Compose(context.Background(), cfg, at)) {
		t.Error("the baseline announced")
	}
	if w.Step(Compose(context.Background(), cfg, at.Add(time.Hour))) {
		t.Error("an unchanged set announced (only the time differs)")
	}
	f.data["intake_list"] = []any{}
	if !w.Step(Compose(context.Background(), cfg, at)) {
		t.Error("a removed item did not announce")
	}
	if w.Step(Compose(context.Background(), cfg, at)) {
		t.Error("announced twice for one change")
	}
	f.err["intake_list"] = errors.New("boom")
	if !w.Step(Compose(context.Background(), cfg, at)) {
		t.Error("a kind going unavailable did not announce")
	}
}

func TestPushNeedsChangedSendsOnlyOnChange(t *testing.T) {
	f := syntheticExec()
	s := &Server{}
	s.init()
	c := &client{events: make(chan []byte, 16)}
	s.clients[c] = struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { PushNeedsChanged(ctx, s, needsCfg(f, "mara@example.test"), 10*time.Millisecond); close(done) }()
	time.Sleep(80 * time.Millisecond)
	if len(c.events) != 0 {
		t.Fatalf("%d events with nothing changed", len(c.events))
	}
	f.mu.Lock()
	f.data["intake_list"] = []any{}
	f.mu.Unlock()
	select {
	case ev := <-c.events:
		if !strings.HasPrefix(string(ev), "event: needs.changed\ndata: ") || !strings.Contains(string(ev), `"count":6`) {
			t.Errorf("event %q", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no needs.changed after a change")
	}
	cancel()
	<-done
}

// The History decision: reading a session's inbox as that session stamps
// delivery and presence (see bus/inbox_sideeffect_test.go), so P2 builds no
// sessions.history and no read path to inbox.
func TestNoSessionHistoryInThisBuild(t *testing.T) {
	for _, op := range []string{"inbox", "mark_read", "message_status"} {
		if needsReadOps[op] {
			t.Errorf("%s is readable by the UI", op)
		}
	}
}

func TestComposeShowsTheProjectSlugNotTheID(t *testing.T) {
	f := syntheticExec()
	cfg := needsCfg(f, "mara@example.test")
	cfg.Scope.Project = "prj_example01"
	r := Compose(context.Background(), cfg, time.Now())
	if len(r.Items) == 0 {
		t.Fatal("no items")
	}
	projects := 0
	for _, op := range f.ops {
		if op == "list_projects" {
			projects++
		}
	}
	if projects != 1 {
		t.Errorf("list_projects read %d times in one cycle", projects)
	}
	for _, it := range r.Items {
		if it.Project != "atlas-api" {
			t.Errorf("%s project %q", it.ID, it.Project)
		}
	}
}

func TestComposeKeepsTheIDWhenNoProjectMatches(t *testing.T) {
	f := syntheticExec()
	cfg := needsCfg(f, "mara@example.test")
	cfg.Scope.Project = "prj_unknown99"
	for _, it := range Compose(context.Background(), cfg, time.Now()).Items {
		if it.Project != "prj_unknown99" {
			t.Errorf("%s project %q", it.ID, it.Project)
		}
	}
	f.err["list_projects"] = errors.New("refused")
	for _, it := range Compose(context.Background(), cfg, time.Now()).Items {
		if it.Project != "prj_unknown99" {
			t.Errorf("after a failed read, %s project %q", it.ID, it.Project)
		}
	}
}
