package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type l1TUISource struct {
	mu    sync.Mutex
	calls []tui.LinearRequest
	hook  func(context.Context, tui.LinearRequest) (tui.LinearData, error)
}

func TestL1TUIPreviewKeepsOriginalRevisionAgainstRealStoreRace(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	provider := new(l1SyntheticProvider)
	service := l1ServiceFor(t, f.store, provider)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	source := &l1TUISource{hook: func(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
		d := tui.LinearData{}
		switch r.Operation {
		case "profiles":
			b, err := service.Binding(ctx, scope)
			d.Binding = b
			d.Profiles = []linearconn.Profile{{ID: l1ProfileID, Name: "synthetic profile", Access: linearconn.Write}}
			return d, err
		case "teams":
			d.Teams = linearprovider.Page[linearprovider.Team]{Items: []linearprovider.Team{{ID: l1TeamA, Name: "team"}}, Complete: true}
		case "projects":
			d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{}, Complete: true}
		case "preview":
			preview, err := service.Link(ctx, scope, r.Link)
			if err != nil {
				return d, err
			}
			d.Preview = preview
			// Another authorized operator changes settings after the preview.
			f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
			b, err := service.Binding(ctx, scope)
			d.Binding = b
			return d, err
		case "link":
			_, err := service.Link(ctx, scope, r.Link)
			return d, err
		default:
			t.Error("unexpected preview-race action", r.Operation)
		}
		return d, nil
	}}
	m := l1TUIModel(t, source)
	m, cmd := m.Show(tui.Linear)
	m = l1TUIApply(t, m, cmd)
	for _, key := range []string{"enter", " ", "enter", "enter", "enter"} {
		m, cmd = l1TUIKey(m, key)
		m = l1TUIApply(t, m, cmd)
	}
	if m.LinearState().Step != "preview" {
		t.Fatal("preview not reached")
	}
	if m.LinearState().Binding.Revision != 0 {
		t.Fatal("TUI silently adopted later metadata revision after displaying original preview")
	}
	observer := localNotificationAuthority(t, f)
	before := l1BindingSnapshot(t, observer)
	m, cmd = l1TUIKey(m, "enter")
	m = l1TUIApply(t, m, cmd)
	calls := source.recorded()
	last := calls[len(calls)-1]
	if last.Operation != "link" || last.Link.ExpectedRevision != 0 || !last.Link.Confirm {
		t.Fatal("confirm dropped original authoritative CAS revision")
	}
	if m.LinearState().Step == "tasks" {
		t.Fatal("stale preview reported a successful save")
	}
	if after := l1BindingSnapshot(t, observer); after != before {
		t.Fatal("stale TUI preview overwrote newer authoritative mapping")
	}
	if provider.writes.Load() != 0 {
		t.Fatal("settings preview/confirm invoked external issue mutation")
	}
}

func (s *l1TUISource) Do(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
	s.mu.Lock()
	s.calls = append(s.calls, r)
	s.mu.Unlock()
	return s.hook(ctx, r)
}
func (s *l1TUISource) recorded() []tui.LinearRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tui.LinearRequest(nil), s.calls...)
}
func l1TUIModel(t *testing.T, source tui.LinearSource) tui.Model {
	t.Helper()
	m, err := tui.New(tui.Config{Executor: &recordingTUIExecutor{source: "server"}, Mode: tui.ModeCloud, Workspace: "workspace", Project: "project", Session: "session", Instance: "instance-1", Context: context.Background(), Linear: source})
	if err != nil {
		t.Fatal(err)
	}
	return m.SetSize(100, 30)
}
func l1TUIKey(m tui.Model, key string) (tui.Model, tea.Cmd) {
	k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case "enter":
		k = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		k = tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		k = tea.KeyMsg{Type: tea.KeyDown}
	}
	return m.UpdateModel(k)
}
func l1TUIApply(t *testing.T, m tui.Model, cmd tea.Cmd) tui.Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	return applyTUIMsg(t, m, cmd())
}

func TestL1TUIExplicitPagedLinkPreviewCancelAndConfirm(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "confirm"}[confirm], func(t *testing.T) {
			source := &l1TUISource{}
			source.hook = func(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
				d := tui.LinearData{}
				switch r.Operation {
				case "profiles":
					d.Profiles = []linearconn.Profile{{ID: l1ProfileID, Name: "injected profile", Access: linearconn.Read}}
				case "teams":
					if r.Cursor == "" {
						d.Teams = linearprovider.Page[linearprovider.Team]{NextCursor: "teams-page2"}
					} else {
						if r.Cursor != "teams-page2" {
							t.Error("incorrect team cursor")
						}
						d.Teams = linearprovider.Page[linearprovider.Team]{Items: []linearprovider.Team{{ID: l1TeamA, Name: "same label"}, {ID: l1TeamB, Name: "same label"}}, Complete: true}
					}
				case "projects":
					if r.Cursor == "" {
						d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{{ID: l1Project, Name: "not all allowed teams", TeamIDs: []string{l1TeamA}}}, NextCursor: "projects-page2"}
					} else {
						if r.Cursor != "projects-page2" {
							t.Error("incorrect project cursor")
						}
						d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{{ID: l1Project, Name: "verified all teams", TeamIDs: []string{l1TeamA, l1TeamB}}}, Complete: true}
					}
				case "preview":
					if r.Link.Confirm {
						t.Error("preview committed")
					}
					d.Preview = "Explicit selected destination preview"
				case "link":
					if !r.Link.Confirm {
						t.Error("link omitted confirmation")
					}
					d.Binding = bus.TaskboardBinding{Configured: true, Revision: 1, LinearTeamIDs: r.Link.TeamIDs, LinearDefaultTeamID: r.Link.DefaultTeamID, LinearProjectID: r.Link.ProjectID}
				default:
					t.Errorf("unexpected TUI operation %s", r.Operation)
				}
				return d, nil
			}
			m := l1TUIModel(t, source)
			if len(source.recorded()) != 0 {
				t.Fatal("optional native source accessed at construction")
			}
			m, cmd := m.Show(tui.Linear)
			m = l1TUIApply(t, m, cmd)
			for _, key := range []string{"enter", "]", " ", "down", " ", "enter", "enter", "]", "down", "enter"} {
				m, cmd = l1TUIKey(m, key)
				m = l1TUIApply(t, m, cmd)
			}
			state := m.LinearState()
			if state.Step != "preview" || state.ProjectID != l1Project || state.DefaultTeamID != l1TeamA || state.TeamBoard || len(state.SelectedTeamIDs) != 2 {
				t.Fatalf("paged explicit mapping preview lost IDs: %+v", state)
			}
			for _, r := range source.recorded() {
				if r.Operation == "link" || r.Link.Confirm {
					t.Fatal("selection/preview mutated shared mapping before confirmation")
				}
			}
			key := "esc"
			if confirm {
				key = "enter"
			}
			m, cmd = l1TUIKey(m, key)
			m = l1TUIApply(t, m, cmd)
			links := 0
			for _, r := range source.recorded() {
				if r.Operation == "link" {
					links++
					if r.ProfileID != l1ProfileID || r.Link.ProjectID != l1Project || r.Link.DefaultTeamID != l1TeamA || r.Link.ExpectedRevision != 0 {
						t.Fatal("save lost explicit profile/scope/revision")
					}
				}
			}
			if confirm {
				if links != 1 || m.LinearState().Binding.Revision != 1 {
					t.Fatal("explicit confirmation did not save exactly once")
				}
			} else if links != 0 || m.LinearState().Step != "profiles" {
				t.Fatal("cancel saved or failed to leave preview")
			}
		})
	}
}

func TestL1TUICancelConnectAndDropLateResponse(t *testing.T) {
	for _, leave := range []bool{false, true} {
		t.Run(map[bool]string{false: "escape", true: "leave-screen"}[leave], func(t *testing.T) {
			entered, stopped := make(chan struct{}), make(chan struct{})
			source := &l1TUISource{hook: func(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
				if r.Operation == "profiles" {
					return tui.LinearData{}, nil
				}
				if r.Operation != "connect" {
					t.Errorf("unexpected operation %s", r.Operation)
					return tui.LinearData{}, nil
				}
				if r.Access != linearconn.Read || r.Name != "synthetic label" {
					t.Error("connect lost explicit name/default read access")
				}
				close(entered)
				<-ctx.Done()
				close(stopped)
				return tui.LinearData{Binding: bus.TaskboardBinding{Configured: true, Revision: 99}}, nil
			}}
			m := l1TUIModel(t, source)
			m, cmd := m.Show(tui.Linear)
			m = l1TUIApply(t, m, cmd)
			m, _ = l1TUIKey(m, "c")
			m, _ = l1TUIKey(m, "synthetic label")
			m, cmd = l1TUIKey(m, "enter")
			if cmd == nil {
				t.Fatal("explicit consent action did not dispatch source")
			}
			result := make(chan tea.Msg, 1)
			go func() { result <- cmd() }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("connect did not enter")
			}
			if leave {
				m, _ = m.Show(tui.Sessions)
			} else {
				m, _ = l1TUIKey(m, "esc")
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("cancel/navigation did not cancel active native action")
			}
			select {
			case msg := <-result:
				m, _ = m.UpdateModel(msg)
			case <-time.After(time.Second):
				t.Fatal("canceled action did not settle")
			}
			if m.LinearState().Binding.Revision != 0 {
				t.Fatal("late canceled native response repainted mapping")
			}
			if leave && m.CurrentScreen() != tui.Sessions {
				t.Fatal("late native response changed screen")
			}
		})
	}
}

func TestL1TUIHostileNativeDetailsWholeScreenBoundsAndInspection(t *testing.T) {
	hostile := "界e\u0301👩‍💻\x1b]52;c;injected\a\r\u202e"
	tail := "INSPECTABLE-END-OF-LONG-LINEAR-DESCRIPTION"
	issue := linearprovider.Issue{ID: l1IssueID, Identifier: "SYN-1", Title: hostile + strings.Repeat("title", 40), TeamID: l1TeamA, Description: strings.Repeat(hostile+"\nlong description\n", 80) + tail, Context: map[string]any{"parent": hostile}, ContextComplete: false}
	source := &l1TUISource{hook: func(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
		d := tui.LinearData{Binding: bus.TaskboardBinding{Configured: true, Revision: 1, LinearTeamIDs: []string{l1TeamA}, LinearDefaultTeamID: l1TeamA}}
		switch r.Operation {
		case "profiles":
			d.Profiles = []linearconn.Profile{{ID: l1ProfileID, Name: hostile, Access: linearconn.Read}}
		case "issues":
			d.Issues = linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{issue}, Complete: true}
		case "issue":
			d.Issue = &issue
		default:
			t.Error("unexpected native action", r.Operation)
		}
		return d, nil
	}}
	m := l1TUIModel(t, source)
	m, cmd := m.Show(tui.Linear)
	m = l1TUIApply(t, m, cmd)
	for _, key := range []string{"enter", "enter"} {
		m, cmd = l1TUIKey(m, key)
		m = l1TUIApply(t, m, cmd)
	}
	if m.LinearState().Issue == nil || m.LinearState().Issue.Description != issue.Description {
		t.Fatal("native detail model altered untrusted stored data")
	}
	calls := len(source.recorded())
	for _, w := range []int{8, 24, 32, 80, 140, 215} {
		for _, h := range []int{1, 8, 9, 12, 30, 50} {
			m = m.SetSize(w, h)
			view := m.View()
			plain := ansi.Strip(view)
			if strings.ContainsAny(plain, "\x1b\a\r\u202e") {
				t.Fatal("untrusted native controls escaped trusted rendering")
			}
			lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
			if len(lines) > h {
				t.Fatalf("native whole-screen height=%d >%d", len(lines), h)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > w {
					t.Fatalf("native whole-screen width >%d", w)
				}
			}
		}
	}
	m = m.SetSize(80, 30)
	found := false
	for n := 0; n < 700; n++ {
		if strings.Contains(ansi.Strip(m.View()), tail) {
			found = true
			break
		}
		m = m.Scroll(1)
	}
	if !found {
		t.Fatal("bounded native details cannot inspect description tail through scroll")
	}
	if len(source.recorded()) != calls {
		t.Fatal("resize/inspection performed native actions")
	}
}
