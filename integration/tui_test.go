package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

type recordingTUIExecutor struct {
	mu       sync.Mutex
	requests []bus.Request
	source   string
	failOp   string
}

func (e *recordingTUIExecutor) Execute(_ context.Context, req bus.Request) (bus.Result, error) {
	e.mu.Lock()
	e.requests = append(e.requests, req)
	e.mu.Unlock()
	if req.Operation == e.failOp {
		return bus.Result{}, errors.New("forbidden: project role does not permit this operation")
	}
	switch req.Operation {
	case "whoami":
		return bus.Result{Source: e.source, Data: map[string]any{
			"workspace": map[string]any{"name": "workspace"},
			"project":   map[string]any{"slug": "project"},
			"session":   map[string]any{"name": "session", "role": "member"},
			"principal": "person@example.test", "identity_provenance": "google-id-token",
			"storage_mode": "cloud", "verified": e.source == "server", "online": e.source == "server",
			"mirror": map[string]any{"updated_at": "2026-10-20T10:00:00Z", "age_seconds": 61, "stale": e.source == "offline-mirror"},
		}}, nil
	case "list_projects":
		return bus.Result{Source: e.source, Data: map[string]any{"projects": []any{
			map[string]any{"id": "prj_visible", "slug": "project", "name": "Project", "role": "member"},
		}}}, nil
	case "list_sessions":
		return bus.Result{Source: e.source, Data: map[string]any{"sessions": []any{
			map[string]any{"id": "ses_visible", "name": "session", "role": "member", "agent_kind": "codex", "status": "active", "presence": "active"},
		}}}, nil
	case "inbox":
		return bus.Result{Source: e.source, Data: map[string]any{"messages": []any{
			map[string]any{"id": "msg_visible", "sender": "peer", "body": "visible inbox body", "kind": "message"},
		}}}, nil
	case "send", "reply", "mark_read":
		return bus.Result{Source: e.source, Data: map[string]any{"id": "msg_written"}}, nil
	default:
		return bus.Result{}, errors.New("unexpected operation " + req.Operation)
	}
}

func (e *recordingTUIExecutor) recorded() []bus.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]bus.Request(nil), e.requests...)
}

func runTUICmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	return cmd()
}

func applyTUIMsg(t *testing.T, model tui.Model, msg tea.Msg) tui.Model {
	t.Helper()
	if msg == nil {
		t.Fatalf("TUI command returned no message")
	}
	next, _ := model.UpdateModel(msg)
	return next
}

func assertTUIReadsPinned(t *testing.T, requests []bus.Request, wantProject bool) {
	t.Helper()
	for _, req := range requests {
		if req.Workspace != "workspace" {
			t.Errorf("%s workspace=%q", req.Operation, req.Workspace)
		}
		if wantProject && req.Operation != "list_projects" && req.Project != "project" {
			t.Errorf("%s project=%q", req.Operation, req.Project)
		}
		if req.Session != "" && req.Session != "session" {
			t.Errorf("%s session=%q", req.Operation, req.Session)
		}
		if req.Session == "session" && req.Args["instance"] != "instance-1" {
			t.Errorf("%s instance=%v, want pinned instance-1", req.Operation, req.Args["instance"])
		}
	}
}

func TestTUIReadSurfacesStayReadOnlyAndPinned(t *testing.T) {
	exec := &recordingTUIExecutor{source: "server"}
	model, err := tui.New(tui.Config{
		Executor: exec, Mode: tui.ModeCloud, Workspace: "workspace",
		Project: "project", Session: "session", Instance: "instance-1",
		Principal: "person@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	model, initCmd := model.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	model = applyTUIMsg(t, model, runTUICmd(t, initCmd))
	if got := model.State().Source; got != "server" {
		t.Fatalf("overview source=%q", got)
	}
	for _, screen := range []tui.Screen{
		tui.ScreenProjects, tui.ScreenSessions, tui.ScreenInbox,
		tui.ScreenDoctor, tui.ScreenOverview,
	} {
		var cmd tea.Cmd
		model, cmd = model.Show(screen)
		model = applyTUIMsg(t, model, runTUICmd(t, cmd))
		state := model.State()
		if state.Screen != screen {
			t.Fatalf("screen=%q, want %q", state.Screen, screen)
		}
		if !strings.Contains(tui.Render(model), string(screenTitle(screen))) {
			t.Fatalf("%s view title missing", screen)
		}
	}
	for _, req := range exec.recorded() {
		switch req.Operation {
		case "send", "reply", "mark_read":
			t.Fatalf("read-only navigation issued mutation %s", req.Operation)
		}
		if req.Operation != "list_projects" {
			for _, forbidden := range []string{"principal", "owner_principal", "created_by"} {
				if _, exists := req.Args[forbidden]; exists {
					t.Fatalf("%s supplied trusted override %s", req.Operation, forbidden)
				}
			}
		}
	}
	assertTUIReadsPinned(t, exec.recorded(), true)
}

func screenTitle(screen tui.Screen) string {
	text := string(screen)
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func TestTUICloudOfflineMutationsAreNotQueued(t *testing.T) {
	exec := &recordingTUIExecutor{source: "offline-mirror"}
	model, err := tui.New(tui.Config{
		Executor: exec, Mode: tui.ModeCloud, Workspace: "workspace",
		Project: "project", Session: "session", Instance: "instance-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	model, initCmd := model.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	model = applyTUIMsg(t, model, runTUICmd(t, initCmd))
	before := len(exec.recorded())
	for name, call := range map[string]func() (tui.Model, tea.Cmd){
		"send":        func() (tui.Model, tea.Cmd) { return model.Send("peer", "body", "send-id") },
		"reply":       func() (tui.Model, tea.Cmd) { return model.Reply("msg_visible", "body", "reply-id") },
		"mark-read":   func() (tui.Model, tea.Cmd) { return model.MarkRead([]string{"msg_visible"}) },
		"begin-send":  func() (tui.Model, tea.Cmd) { return model.BeginSend(), nil },
		"begin-reply": func() (tui.Model, tea.Cmd) { return model.BeginReply(), nil },
	} {
		t.Run(name, func(t *testing.T) {
			next, cmd := call()
			if cmd != nil {
				t.Fatalf("offline mutation %s returned a command", name)
			}
			if next.State().Error == "" {
				t.Fatalf("offline mutation %s did not report a warning", name)
			}
			if got := len(exec.recorded()); got != before {
				t.Fatalf("offline mutation %s reached executor: %d -> %d", name, before, got)
			}
		})
	}
}

func TestTUICloudForbiddenWriteDoesNotAppearSuccessful(t *testing.T) {
	exec := &recordingTUIExecutor{source: "server", failOp: "send"}
	model, err := tui.New(tui.Config{
		Executor: exec, Mode: tui.ModeCloud, Workspace: "workspace",
		Project: "project", Session: "session", Instance: "instance-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Send("peer", "must not succeed", "denied-send")
	model = applyTUIMsg(t, model, runTUICmd(t, cmd))
	state := model.State()
	if state.Error == "" || !strings.Contains(strings.ToLower(state.Error), "forbidden") {
		t.Fatalf("denied write did not surface authorization error: %+v", state)
	}
	if state.Apply != nil {
		t.Fatalf("denied write produced apply success: %+v", state.Apply)
	}
}

type recordingSetup struct {
	plan       tui.SetupPlan
	applyCalls []string
	planCalls  int
}

func (s *recordingSetup) Plan(context.Context) (tui.SetupPlan, error) {
	s.planCalls++
	return s.plan, nil
}

func (s *recordingSetup) Apply(_ context.Context, phrase string) (tui.ApplyResult, error) {
	s.applyCalls = append(s.applyCalls, phrase)
	return tui.ApplyResult{Summary: "synthetic plan applied"}, nil
}

func TestTUISetupRequiresVerbatimDisplayedPhrase(t *testing.T) {
	setup := &recordingSetup{plan: tui.SetupPlan{
		Title: "Create pilot", Plan: "project=claw-bridge-dev\nregion=europe-west2-a\nvm=instance-claw-bridge-dev",
		Resources:   []tui.Resource{{Kind: "compute", Name: "instance-claw-bridge-dev", Region: "europe-west2-a", Action: "create", Monthly: "GBP 12.00"}},
		MonthlyCost: "GBP 12.00", Confirmation: "APPLY instance-claw-bridge-dev", Region: "europe-west2-a",
	}}
	exec := &recordingTUIExecutor{source: "server"}
	model, err := tui.New(tui.Config{
		Executor: exec, Setup: setup, Mode: tui.ModeCloud,
		Workspace: "workspace", Project: "project", Session: "session", Instance: "instance-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(tui.ScreenCloud)
	model = applyTUIMsg(t, model, runTUICmd(t, cmd))
	state := model.State()
	if state.Plan == nil || state.Source != "setup-plan" {
		t.Fatalf("setup plan not loaded: %+v", state)
	}
	render := tui.Render(model)
	for _, text := range append(strings.Split(state.Plan.Plan, "\n"), state.Plan.MonthlyCost, state.Plan.Confirmation) {
		if !strings.Contains(render, text) {
			t.Fatalf("setup display omits %q:\n%s", text, render)
		}
	}
	for _, phrase := range []string{"", "apply instance-claw-bridge-dev", "APPLY another-vm"} {
		next, cmd := model.ApplySetup(phrase)
		if cmd != nil {
			t.Fatalf("mismatched confirmation %q returned apply command", phrase)
		}
		if next.State().Error == "" {
			t.Fatalf("mismatched confirmation %q produced no warning", phrase)
		}
	}
	if len(setup.applyCalls) != 0 {
		t.Fatalf("mismatched confirmations invoked apply: %v", setup.applyCalls)
	}
	model, cmd = model.ApplySetup(setup.plan.Confirmation)
	model = applyTUIMsg(t, model, runTUICmd(t, cmd))
	if len(setup.applyCalls) != 1 || setup.applyCalls[0] != setup.plan.Confirmation {
		t.Fatalf("exact confirmation not forwarded: %v", setup.applyCalls)
	}
	if model.State().Apply == nil {
		t.Fatal("successful synthetic apply not reflected in model")
	}
}

func TestTUILocalModeCannotApplyCloudSetup(t *testing.T) {
	setup := &recordingSetup{}
	model, err := tui.New(tui.Config{
		Executor: &recordingTUIExecutor{source: "local-authority"},
		Setup:    setup, Mode: tui.ModeLocal, Workspace: "workspace",
		Project: "project", Session: "session", Instance: "instance-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	next, cmd := model.ApplySetup("anything")
	if cmd != nil || len(setup.applyCalls) != 0 || next.State().Error == "" {
		t.Fatalf("local TUI allowed cloud setup apply: cmd=%v calls=%v state=%+v", cmd, setup.applyCalls, next.State())
	}
}
