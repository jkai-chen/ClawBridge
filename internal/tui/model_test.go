package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	tea "github.com/charmbracelet/bubbletea"
)

type call struct {
	request bus.Request
}

type fakeAuthority struct {
	mu       sync.Mutex
	calls    []call
	results  map[string]bus.Result
	errs     map[string]error
	source   string
	mirror   map[string]any
	messages []map[string]any
}

func (f *fakeAuthority) Execute(_ context.Context, request bus.Request) (bus.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{request: request})
	if err := f.errs[request.Operation]; err != nil {
		return bus.Result{}, err
	}
	if result, ok := f.results[request.Operation]; ok {
		return result, nil
	}
	source := f.source
	if source == "" {
		source = "server"
	}
	data := map[string]any{}
	switch request.Operation {
	case "whoami":
		data = map[string]any{
			"workspace":           map[string]any{"id": "ws_1", "name": "pilot", "mode": "server", "retention_days": 90},
			"project":             map[string]any{"id": "prj_1", "slug": "core", "name": "Core"},
			"session":             map[string]any{"id": "ses_1", "name": "engineer", "role": "lead", "agent_kind": "codex", "status": "active"},
			"principal":           "human@example.test",
			"identity_provenance": "google-id-token",
			"storage_mode":        "cloud",
			"verified":            true,
			"online":              source == "server",
		}
	case "list_projects":
		data = map[string]any{"projects": []any{map[string]any{"id": "prj_1", "slug": "core", "name": "Core", "role": "owner", "git_remote": "github.com/acme/core"}}}
	case "list_sessions":
		data = map[string]any{"sessions": []any{map[string]any{"id": "ses_1", "name": "engineer", "role": "lead", "agent_kind": "codex", "status": "active", "presence": "active", "stale": false}}}
	case "inbox":
		data = map[string]any{"messages": f.messages}
	case "send", "reply":
		data = map[string]any{"id": "msg_sent"}
	case "mark_read":
		data = map[string]any{"marked": 1}
	}
	if f.mirror != nil {
		data["mirror"] = f.mirror
	}
	return bus.Result{Source: source, Data: data}, nil
}

func (f *fakeAuthority) callIndex(operation string) int {
	for i, call := range f.calls {
		if call.request.Operation == operation {
			return i
		}
	}
	return -1
}

func runCommand(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	return cmd()
}

func testConfig(authority Executor) Config {
	return Config{
		Executor: authority, Mode: ModeCloud, Workspace: "ws_1", Project: "prj_1",
		Session: "engineer", Instance: "inst_one", Principal: "human@example.test",
		Provenance: "google-id-token",
	}
}

func TestNewRequiresExplicitModeAndExecutor(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("accepted rootless TUI")
	}
	if _, err := New(Config{Executor: &fakeAuthority{}, Mode: "automatic"}); err == nil {
		t.Fatal("accepted implicit mode")
	}
}

func TestCloudRejectsWritableLocalAuthority(t *testing.T) {
	store, err := bus.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := New(Config{Executor: store, Mode: ModeCloud}); err == nil {
		t.Fatal("cloud TUI accepted writable local authority")
	}
}

func TestOverviewPinsScopeAndShowsProvenance(t *testing.T) {
	a := &fakeAuthority{}
	model, err := New(testConfig(a))
	if err != nil {
		t.Fatal(err)
	}
	msg := runCommand(t, model.Init()).(loadedMsg)
	model, _ = model.UpdateModel(msg)
	snapshot := model.State()
	if snapshot.Source != "server" || snapshot.Identity.Provenance != "google-id-token" || !snapshot.Identity.Verified {
		t.Fatalf("identity/source not displayed: %+v", snapshot)
	}
	if snapshot.Workspace.Name != "pilot" || snapshot.Project.Slug != "core" || snapshot.Session.Name != "engineer" {
		t.Fatalf("wrong immutable scope: %+v", snapshot)
	}
	for _, call := range a.calls {
		if call.request.Workspace != "ws_1" || call.request.Project != "prj_1" || (call.request.Operation != "whoami" && call.request.Session != "engineer") {
			t.Fatalf("scope drifted: %+v", call.request)
		}
		if call.request.Operation == "inbox" && call.request.Args["instance"] != "inst_one" {
			t.Fatal("instance not pinned")
		}
	}
	view := Render(model)
	for _, expected := range []string{"Overview", "verified", "google-id-token", "pilot › core › engineer"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view missing %q:\n%s", expected, view)
		}
	}
}

func TestEveryScreenAndProjectsErrorAreActionable(t *testing.T) {
	a := &fakeAuthority{errs: map[string]error{"list_projects": errors.New("unsupported operation \"list_projects\"")}}
	model, err := New(testConfig(a))
	if err != nil {
		t.Fatal(err)
	}
	model = model.SetSize(100, 30)
	for _, screen := range orderedScreens {
		var cmd tea.Cmd
		model, cmd = model.Show(screen)
		if screen == ScreenCloud || screen == ScreenDoctor {
			continue
		}
		model, _ = model.UpdateModel(runCommand(t, cmd))
		if model.CurrentScreen() != screen {
			t.Fatalf("wrong screen %s", model.CurrentScreen())
		}
		if screen == ScreenProjects && !strings.Contains(model.State().Error, "Confirm the ClawBridge client and server versions") {
			t.Fatalf("projects error not actionable: %q", model.State().Error)
		}
	}
}

func TestSetupOnlyCloudStartsOnPlan(t *testing.T) {
	setup := SetupFuncs{PlanFunc: func(context.Context) (SetupPlan, error) {
		return SetupPlan{
			Plan: "Create VM", MonthlyCost: "GBP 12",
			Confirmation: "APPLY instance-claw-bridge-dev",
		}, nil
	}}
	model, err := New(Config{Mode: ModeCloud, Setup: setup, AllowSetupOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if model.CurrentScreen() != ScreenCloud {
		t.Fatalf("setup-only screen=%s, want cloud", model.CurrentScreen())
	}
	model, _ = model.UpdateModel(runCommand(t, model.Init()))
	if model.State().Plan == nil || !strings.Contains(Render(model), "Create VM") {
		t.Fatal("setup-only cloud plan not loaded")
	}
}

func TestCloudShowsReadOnlyPlanBeforeExactConfirmation(t *testing.T) {
	a := &fakeAuthority{}
	calls := 0
	setup := SetupFuncs{
		PlanFunc: func(context.Context) (SetupPlan, error) {
			return SetupPlan{
				Title: "Create pilot", Plan: "Create one VM\nUse europe-west2-a",
				Resources:   []Resource{{Kind: "vm", Name: "instance-clawbridge", Region: "europe-west2-a", Action: "create", Monthly: "£12"}},
				MonthlyCost: "£12 monthly", Confirmation: "create pilot",
			}, nil
		},
		ApplyFunc: func(_ context.Context, phrase string) (ApplyResult, error) {
			calls++
			if phrase != "create pilot" {
				t.Fatalf("wrong phrase passed to apply: %q", phrase)
			}
			return ApplyResult{Summary: "Created pilot"}, nil
		},
	}
	cfg := testConfig(a)
	cfg.Setup = setup
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(ScreenCloud)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if calls != 0 {
		t.Fatal("plan load invoked apply")
	}
	if !strings.Contains(Render(model), "Nothing is applied before Enter") {
		t.Fatal("write boundary not visible")
	}
	model, _ = model.ApplySetup("wrong")
	if calls != 0 || !strings.Contains(model.State().Error, "does not match") {
		t.Fatalf("wrong phrase was not refused: %+v", model.State())
	}
	model, cmd = model.ApplySetup("create pilot")
	if cmd == nil {
		t.Fatal("exact phrase did not invoke apply")
	}
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if calls != 1 || model.State().Apply == nil {
		t.Fatal("apply result not retained")
	}
}

func TestCloudApplyRequiresDisplayedCost(t *testing.T) {
	a := &fakeAuthority{}
	cfg := testConfig(a)
	cfg.Setup = SetupFuncs{
		PlanFunc: func(context.Context) (SetupPlan, error) {
			return SetupPlan{Plan: "Create VM", Confirmation: "go"}, nil
		},
		ApplyFunc: func(context.Context, string) (ApplyResult, error) {
			t.Fatal("apply called without displayed cost")
			return ApplyResult{}, nil
		},
	}
	model, _ := New(cfg)
	model, cmd := model.Show(ScreenCloud)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	model, _ = model.ApplySetup("go")
	if !strings.Contains(model.State().Error, "cost") {
		t.Fatal("missing cost did not block apply")
	}
}

func TestOfflineInboxNeverQueuesMutation(t *testing.T) {
	a := &fakeAuthority{
		source: "offline-mirror", mirror: map[string]any{"updated_at": "2026-10-20T10:00:00Z", "age_seconds": 73, "stale": true},
		messages: []map[string]any{{"id": "msg_1", "sender": "reviewer", "body": "cached", "thread_id": "msg_1"}},
	}
	model, err := New(testConfig(a))
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(ScreenInbox)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if model.State().Source != "offline-mirror" || !model.State().Freshness.Stale {
		t.Fatal("offline source/freshness not shown")
	}
	model = model.BeginReply()
	model, _ = model.Reply("msg_1", "must not queue", "offline")
	model, _ = model.MarkRead([]string{"msg_1"})
	model, _ = model.Send("lead", "must not queue", "offline")
	if a.callIndex("reply") != -1 || a.callIndex("mark_read") != -1 || a.callIndex("send") != -1 {
		t.Fatal("offline write reached the executor")
	}
	if !strings.Contains(Render(model), "writes unavailable") {
		t.Fatal("offline write boundary not visible")
	}
}

func TestExplicitMutationsRetainPinnedScope(t *testing.T) {
	a := &fakeAuthority{messages: []map[string]any{{"id": "msg_1", "sender": "reviewer", "body": "hello", "thread_id": "msg_1"}}}
	model, _ := New(testConfig(a))
	model, cmd := model.Show(ScreenInbox)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	model, cmd = model.Reply("msg_1", "answer", "reply-op")
	if cmd == nil {
		t.Fatal("reply did not issue an explicit command")
	}
	model, cmd = model.UpdateModel(runCommand(t, cmd))
	if cmd == nil {
		t.Fatal("successful reply did not refresh via a read")
	}
	call := a.calls[a.callIndex("reply")]
	if call.request.Workspace != "ws_1" || call.request.Project != "prj_1" || call.request.Session != "engineer" || call.request.Args["instance"] != "inst_one" || call.request.Args["message_id"] != "msg_1" {
		t.Fatalf("mutation scope drifted: %+v", call.request)
	}
}

func TestDoctorStableStatusesAndMissingAdapter(t *testing.T) {
	a := &fakeAuthority{}
	cfg := testConfig(a)
	cfg.Doctor = DoctorFunc(func(context.Context) ([]Check, error) {
		return []Check{{ID: "authority", Title: "Authority", Status: Pass}, {ID: "token", Title: "Credentials", Status: Warn, Fix: "Run gcloud auth login"}}, nil
	})
	model, _ := New(cfg)
	model, cmd := model.Show(ScreenDoctor)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	view := Render(model)
	if !strings.Contains(view, "1 pass · 1 warn · 0 fail") || !strings.Contains(view, "Run gcloud auth login") {
		t.Fatalf("doctor output incomplete:\n%s", view)
	}

	cfg.Doctor = nil
	model, _ = New(cfg)
	model, cmd = model.Show(ScreenDoctor)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if !strings.Contains(model.State().Error, "doctor adapter is not configured") {
		t.Fatalf("missing doctor adapter not actionable: %q", model.State().Error)
	}
}

func TestNoninteractiveTerminalFailsWithoutHanging(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	if err := CheckTerminal(input, &output); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("noninteractive terminal not rejected: %v", err)
	}
}

func TestChatKeysAndGeneratedOperationID(t *testing.T) {
	a := &fakeAuthority{messages: []map[string]any{{"id": "msg_1", "sender": "reviewer", "body": "hello", "thread_id": "msg_1"}}}
	cfg := testConfig(a)
	cfg.Now = func() time.Time { return time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) }
	model, _ := New(cfg)
	model, cmd := model.Show(ScreenInbox)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	model, _ = model.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if model.inputMode != inputReply {
		t.Fatal("reply key did not enter draft")
	}
	for _, r := range "hello" {
		model, _ = model.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	model, cmd = model.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not send explicit reply")
	}
	_ = cmd()
	call := a.calls[a.callIndex("reply")]
	op, _ := call.request.Args["operation_id"].(string)
	if !strings.HasPrefix(op, "tui-reply-20261020T100000") || op == "" {
		t.Fatalf("unexpected generated operation id %q", op)
	}
}

// Enter opens a message and writes nothing; Esc closes it; m marks it read
// (TUI-UX-AUDIT Q4).
func TestEnterOpensAndMMarksInboxMessageRead(t *testing.T) {
	a := &fakeAuthority{messages: []map[string]any{{"id": "msg_1", "sender": "reviewer", "body": "hello", "thread_id": "msg_1"}}}
	model, _ := New(testConfig(a))
	model, cmd := model.Show(ScreenInbox)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	model, cmd = model.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !model.inboxOpen || !strings.Contains(Render(model), "Inbox › from reviewer") || !strings.Contains(Render(model), "Message msg_1") {
		t.Fatal("enter did not open the message:", Render(model))
	}
	model, _ = model.UpdateModel(tea.KeyMsg{Type: tea.KeyEsc})
	if model.inboxOpen {
		t.Fatal("esc left the message open")
	}
	model, cmd = model.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if cmd == nil {
		t.Fatal("m did not issue the explicit mark-read command")
	}
	_ = cmd()
	if a.callIndex("mark_read") < 0 {
		t.Fatal("mark_read was not issued")
	}
}

func TestActorlessMutationsNeverReachExecutor(t *testing.T) {
	for _, actor := range [][2]string{{"", ""}, {"session", ""}, {"", "instance"}} {
		authority := &fakeAuthority{}
		model, err := New(Config{Executor: authority, Mode: ModeCloud, Workspace: "w", Project: "p", Session: actor[0], Instance: actor[1]})
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []func(Model) (Model, tea.Cmd){
			func(m Model) (Model, tea.Cmd) { return m.Send("peer", "body", "op") },
			func(m Model) (Model, tea.Cmd) { return m.Reply("message", "body", "op") },
			func(m Model) (Model, tea.Cmd) { return m.MarkRead([]string{"message"}) },
		} {
			updated, cmd := action(model)
			if cmd != nil {
				cmd()
				t.Fatal("actorless action created authority command")
			}
			if !strings.Contains(updated.actionErr, "session") || !strings.Contains(updated.actionErr, "instance") {
				t.Fatal("missing actor warning")
			}
		}
		if len(authority.calls) != 0 {
			t.Fatal("actorless mutation reached executor")
		}
	}
}
