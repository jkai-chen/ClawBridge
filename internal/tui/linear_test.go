package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const linearTestTeam = "11111111-2222-3333-4444-555555555555"

type testLinearSource struct {
	calls           []LinearRequest
	binding         bus.TaskboardBinding
	previewRevision *int64
}

func (s *testLinearSource) Do(_ context.Context, r LinearRequest) (LinearData, error) {
	s.calls = append(s.calls, r)
	d := LinearData{Binding: s.binding}
	switch r.Operation {
	case "profiles":
		d.Profiles = []linearconn.Profile{{ID: "profile", Name: "Linear", Access: linearconn.Read}}
	case "teams":
		d.Teams = linearprovider.Page[linearprovider.Team]{Items: []linearprovider.Team{{ID: linearTestTeam, Name: "Team"}}, Complete: true}
	case "projects":
		d.Projects = linearprovider.Page[linearprovider.Project]{Complete: true}
	case "preview":
		d.Preview = "Team board preview"
		if s.previewRevision != nil {
			d.Binding.Revision = *s.previewRevision
		}
	case "link":
		s.binding = bus.TaskboardBinding{Configured: true, Revision: 1, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}
		d.Binding = s.binding
	}
	return d, nil
}
func linearKeyTest(m Model, key string) (Model, tea.Cmd) { return m.linearKey(key) }
func TestLinearTUIExplicitTeamBoardPreviewAndConfirm(t *testing.T) {
	source := &testLinearSource{}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.calls) != 0 {
		t.Fatal("startup invoked Linear")
	}
	model, cmd := model.Show(ScreenLinear)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if len(source.calls) != 1 || source.calls[0].Operation != "profiles" {
		t.Fatal(source.calls)
	}
	model, cmd = linearKeyTest(model, "enter")
	model, _ = model.UpdateModel(runCommand(t, cmd))
	model, cmd = linearKeyTest(model, " ")
	if cmd != nil {
		t.Fatal("selecting team dispatched")
	}
	model, cmd = linearKeyTest(model, "enter")
	if cmd != nil {
		t.Fatal("default step dispatched")
	}
	model, cmd = linearKeyTest(model, "enter")
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if model.LinearState().Step != "projects" || len(model.linear.projects) != 0 {
		t.Fatal("empty projects not explicit")
	}
	model, cmd = linearKeyTest(model, "enter")
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if model.LinearState().Step != "preview" || !model.LinearState().TeamBoard {
		t.Fatal("team board did not preview")
	}
	for _, r := range source.calls {
		if r.Operation == "link" || r.Link.Confirm {
			t.Fatal("selection committed without confirmation")
		}
	}
	model, cmd = linearKeyTest(model, "enter")
	model, _ = model.UpdateModel(runCommand(t, cmd))
	last := source.calls[len(source.calls)-1]
	if last.Operation != "link" || !last.Link.Confirm || last.Link.DefaultTeamID != linearTestTeam {
		t.Fatal(last)
	}
	if model.LinearState().Binding.Revision != 1 || model.LinearState().Step != "tasks" {
		t.Fatal(model.LinearState())
	}
}

type blockedLinearSource struct {
	entered chan struct{}
	stopped chan struct{}
}

func (s *blockedLinearSource) Do(ctx context.Context, _ LinearRequest) (LinearData, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.stopped)
	return LinearData{Profiles: []linearconn.Profile{{ID: "stale"}}}, nil
}
func TestLinearTUICancelDropsStaleResponse(t *testing.T) {
	source := &blockedLinearSource{make(chan struct{}), make(chan struct{})}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(ScreenLinear)
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	model, _ = model.Show(ScreenOverview)
	select {
	case <-source.stopped:
	case <-time.After(time.Second):
		t.Fatal("leaving view did not cancel")
	}
	model, _ = model.UpdateModel(<-result)
	if model.CurrentScreen() != ScreenOverview || len(model.linear.profiles) != 0 {
		t.Fatal("stale response repainted")
	}
}
func TestLinearTUIHostileDetailsStayBounded(t *testing.T) {
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = &testLinearSource{}
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model.screen = ScreenLinear
	model.linear.step = "details"
	model.linear.issue = &linearprovider.Issue{ID: linearTestTeam, Title: "\x1b[2J" + strings.Repeat("long", 400), Description: strings.Repeat("\x1b]52;c;payload\a\nlong task description\n", 300)}
	for _, size := range [][2]int{{100, 30}, {30, 12}, {10, 5}} {
		model = model.SetSize(size[0], size[1])
		view := Render(model)
		if strings.Contains(view, "\x1b]52;") || strings.Contains(view, "\x1b[2J") {
			t.Fatal("provider terminal control escaped")
		}
		lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
		if len(lines) > size[1] {
			t.Fatalf("height %d>%d", len(lines), size[1])
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("width %d>%d", ansi.StringWidth(line), size[0])
			}
		}
	}
}

func TestLinearTUIPreviewCannotPromoteRevision(t *testing.T) {
	newer := int64(1)
	source := &testLinearSource{previewRevision: &newer}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(ScreenLinear)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	// Drive only the public Model update/source boundary: profile, team selection,
	// default team, explicit team board and preview.
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune(" ")}, {Type: tea.KeyEnter}, {Type: tea.KeyEnter}, {Type: tea.KeyEnter}} {
		model, cmd = model.UpdateModel(key)
		if cmd != nil {
			model, _ = model.UpdateModel(runCommand(t, cmd))
		}
	}
	if model.LinearState().Step != "preview" || model.LinearState().Binding.Revision != 0 {
		t.Fatal("new metadata changed preview precondition", model.LinearState())
	}
	preview := source.calls[len(source.calls)-1]
	if preview.Operation != "preview" || preview.Link.ExpectedRevision != 0 {
		t.Fatal(preview)
	}
	model, cmd = model.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	model, _ = model.UpdateModel(runCommand(t, cmd))
	confirm := source.calls[len(source.calls)-1]
	if confirm.Operation != "link" || !confirm.Link.Confirm || confirm.Link.ExpectedRevision != 0 {
		t.Fatal("confirmation promoted CAS revision", confirm)
	}
}
