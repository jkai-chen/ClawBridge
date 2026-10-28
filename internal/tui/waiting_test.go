package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"clawbridge/internal/bus"
)

func projectsResult(slugs ...string) bus.Result {
	rows := []any{}
	for _, s := range slugs {
		rows = append(rows, map[string]any{"id": "prj_" + s, "slug": s, "name": strings.ToUpper(s)})
	}
	return bus.Result{Source: "server", Data: map[string]any{"projects": rows}}
}

func waitingModel(t *testing.T, a *fakeAuthority, found func(string) error) Model {
	t.Helper()
	cfg := testConfig(a)
	cfg.Project, cfg.Session, cfg.Instance = "", "", ""
	cfg.WaitingForProject, cfg.ProjectFound = true, found
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(80, 24)
	msg := m.Init()()
	m, _ = m.UpdateModel(msg)
	return m
}

// No project yet: say who to ask, check again on r and on a timer, and
// take the project once there is one.
func TestWaitingForAProject(t *testing.T) {
	a := &fakeAuthority{results: map[string]bus.Result{"list_projects": projectsResult(), "whoami": {Source: "server", Data: map[string]any{
		"principal": "human@example.test", "workspace": map[string]any{"name": "pilot", "owner_principal": "owner@example.test"}}}}}
	var saved []string
	m := waitingModel(t, a, func(slug string) error { saved = append(saved, slug); return nil })
	view := Render(m)
	for _, want := range []string{"Waiting for a project", "no project has been shared with you yet", "Ask owner@example.test", "clawbridge --project PROJECT project role set human@example.test member", "checks again every"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	golden(t, "overview-waiting-80x24", view)
	old := waitingPoll
	waitingPoll = time.Millisecond
	t.Cleanup(func() { waitingPoll = old })
	if got := m.tabs(); len(got) != 3 || got[1] != ScreenAccount || got[2] != ScreenDoctor {
		t.Fatal("tabs while waiting:", got)
	}
	before := a.count("list_projects")
	m = do(m, "r")
	if a.count("list_projects") <= before {
		t.Fatal("r didn't check again")
	}
	// The timer checks again too; a project shared meanwhile is taken.
	a.mu.Lock()
	a.results["list_projects"] = projectsResult("api")
	a.mu.Unlock()
	m, cmd := m.UpdateModel(waitingTickMsg{request: m.request})
	m, cmd = m.UpdateModel(cmd())
	if len(saved) != 1 || saved[0] != "api" || cmd == nil || cmd() != tea.Quit() {
		t.Fatal("the project wasn't taken:", saved, Render(m))
	}
}

// Nothing yet, or a failed check: it keeps checking.
func TestWaitingKeepsChecking(t *testing.T) {
	for name, a := range map[string]*fakeAuthority{
		"empty":  {results: map[string]bus.Result{"list_projects": projectsResult()}},
		"failed": {errs: map[string]error{"list_projects": errors.New("server unreachable")}},
	} {
		cfg := testConfig(a)
		cfg.Project, cfg.Session, cfg.Instance, cfg.WaitingForProject = "", "", "", true
		m, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m, cmd := m.UpdateModel(m.Init()())
		if cmd == nil || !m.waiting {
			t.Fatal(name, "stopped checking")
		}
		if name == "failed" && !strings.Contains(Render(m.SetSize(80, 24)), "server unreachable") {
			t.Fatal("the failure wasn't shown")
		}
	}
}

// The workspace owner (after a bootstrap join) has nobody to ask: they're
// told how to create the first project.
func TestWaitingOwnerCreatesTheFirstProject(t *testing.T) {
	a := &fakeAuthority{results: map[string]bus.Result{"list_projects": projectsResult(), "whoami": {Source: "server", Data: map[string]any{
		"principal": "owner@example.test", "workspace": map[string]any{"name": "pilot", "owner_principal": "owner@example.test"}}}}}
	view := Render(waitingModel(t, a, nil))
	if !strings.Contains(view, "pilot has no project yet. Create the first one:") || !strings.Contains(view, "clawbridge project create SLUG") || strings.Contains(view, "Ask ") {
		t.Fatal(view)
	}
	// The owner comes from the workspace-only whoami: no project, no
	// session, which the server answers for a member with no project.
	for _, c := range a.calls {
		if c.request.Operation == "whoami" && (c.request.Project != "" || c.request.Session != "" || len(c.request.Args) != 0) {
			t.Fatal("whoami named a scope the waiting profile doesn't have:", c.request)
		}
	}
}

// Several projects: the person picks one. With no saver, the TUI goes on
// with it.
func TestWaitingPicksAmongProjects(t *testing.T) {
	a := &fakeAuthority{results: map[string]bus.Result{"list_projects": projectsResult("api", "web")}}
	m := waitingModel(t, a, nil)
	if view := Render(m); !strings.Contains(view, "Pick the one this profile works in") || !strings.Contains(view, "web") {
		t.Fatal(view)
	}
	m = do(m, "down", "enter")
	if m.waiting || m.cfg.Project != "web" || m.screen != ScreenOverview || len(m.tabs()) < 8 {
		t.Fatal("didn't go on with web:", m.cfg.Project, m.waiting)
	}
}

// Before the first check answers, the screen says it's checking: not the
// member's "ask the owner", which would be wrong for the owner.
func TestWaitingChecksBeforeSayingWhatToDo(t *testing.T) {
	cfg := testConfig(&fakeAuthority{})
	cfg.Project, cfg.Session, cfg.Instance = "", "", ""
	cfg.WaitingForProject = true
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	view := Render(m.SetSize(80, 24))
	if !strings.Contains(view, "Loading your projects and role in") || strings.Contains(view, "Ask ") || strings.Contains(view, "Create the first") {
		t.Fatal(view)
	}
}
