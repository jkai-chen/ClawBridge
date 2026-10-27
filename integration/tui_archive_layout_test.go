package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type responsiveProbe struct {
	source                             string
	calls                              []bus.Request
	rows                               []map[string]any
	doctorCalls, planCalls, applyCalls int
}

func (e *responsiveProbe) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.calls = append(e.calls, r)
	data := map[string]any{}
	switch r.Operation {
	case "whoami":
		data = map[string]any{"workspace": map[string]any{"id": "w", "name": "workspace-界é"}, "project": map[string]any{"id": "p", "slug": "project-👩‍💻"}, "session": map[string]any{"id": "actor", "name": "actor-name"}, "principal": "human@example.test", "verified": true}
	case "list_projects":
		data["projects"] = e.rows
	case "list_sessions":
		data["sessions"] = e.rows
	case "inbox":
		data["messages"] = e.rows
	case "send", "reply":
		data["id"] = "sent"
	default:
		return bus.Result{}, fmt.Errorf("unexpected op %s", r.Operation)
	}
	data["has_more"] = true
	data["next_cursor"] = int64(900)
	source := e.source
	if source == "" {
		source = "server"
	}
	return bus.Result{Source: source, Data: data}, nil
}
func (e *responsiveProbe) Checks(context.Context) ([]tui.Check, error) {
	e.doctorCalls++
	return []tui.Check{{ID: "diagnostic", Title: "check-界", Status: tui.Warn, Detail: strings.Repeat("diagnostic-界é ", 30) + "CHECK-END", Fix: "safe fix\x1b]52;c;bad\a\u202e"}}, nil
}
func (e *responsiveProbe) Plan(context.Context) (tui.SetupPlan, error) {
	e.planCalls++
	return tui.SetupPlan{Title: "review-only", Plan: strings.Repeat("plan-界 ", 40) + "PLAN-END", Confirmation: "confirm"}, nil
}
func (e *responsiveProbe) Apply(context.Context, string) (tui.ApplyResult, error) {
	e.applyCalls++
	return tui.ApplyResult{}, fmt.Errorf("must not apply")
}
func responsiveModel(t *testing.T, screen tui.Screen) (tui.Model, *responsiveProbe) {
	t.Helper()
	e := &responsiveProbe{}
	for i := 0; i < 45; i++ {
		e.rows = append(e.rows, map[string]any{"id": fmt.Sprintf("id-%02d", i), "name": fmt.Sprintf("row-%02d", i), "slug": fmt.Sprintf("row-%02d", i), "sender": fmt.Sprintf("row-%02d", i), "status": "active", "body": strings.Repeat("body-界é👩‍💻 ", 15) + "BODY-END", "owner_principal": strings.Repeat("owner", 30) + "OWNER-END", "host": "host\x1b]52;c;unsafe\a\u202e", "instance": "PEER-SECRET-INSTANCE"})
	}
	m, err := tui.New(tui.Config{Mode: tui.ModeCloud, Executor: e, Doctor: e, Setup: e, Workspace: "w", Project: "p", Session: "actor", Instance: "incarnation", InitialScreen: screen, Now: func() time.Time { return time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.Show(screen)
	if cmd == nil {
		t.Fatal("missing initial load")
	}
	m, _ = m.UpdateModel(cmd())
	if m.State().Error != "" {
		t.Fatal(m.State().Error)
	}
	return m, e
}
func assertResponsiveBounds(t *testing.T, m tui.Model, w, h int) {
	t.Helper()
	out := ansi.Strip(m.View())
	lines := strings.Split(out, "\n")
	if len(lines) > h {
		t.Fatalf("%s %dx%d rows=%d", m.CurrentScreen(), w, h, len(lines))
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > w {
			t.Fatalf("%s %dx%d width=%d", m.CurrentScreen(), w, h, ansi.StringWidth(line))
		}
	}
	if strings.ContainsAny(out, "\x1b\a\r\u202e") || strings.Contains(out, "PEER-SECRET-INSTANCE") {
		t.Fatal("unsafe controls or peer instance exposed")
	}
}
func TestTUIArchiveAllScreenResizeBoundsAndPurity(t *testing.T) {
	for _, screen := range []tui.Screen{tui.Overview, tui.Projects, tui.Sessions, tui.Inbox, tui.Cloud, tui.Doctor} {
		t.Run(string(screen), func(t *testing.T) {
			m, e := responsiveModel(t, screen)
			if screen == tui.Projects || screen == tui.Sessions || screen == tui.Inbox {
				m = m.Select(35)
			}
			before := m.State()
			calls, checks, plans := len(e.calls), e.doctorCalls, e.planCalls
			for _, w := range []int{8, 12, 23, 24, 32, 40, 80, 141, 215} {
				for _, h := range []int{1, 4, 8, 9, 12, 24, 50} {
					n, cmd := m.UpdateModel(tea.WindowSizeMsg{Width: w, Height: h})
					if cmd != nil {
						t.Fatal("resize issued work")
					}
					assertResponsiveBounds(t, n, w, h)
					s := n.State()
					if s.Selected != before.Selected || s.Workspace != before.Workspace || s.Project != before.Project || s.Session != before.Session || s.Identity != before.Identity {
						t.Fatal("resize changed selection/scope/identity")
					}
					if w < 24 || h < 9 {
						_, quit := n.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
						if quit == nil {
							t.Fatal("tiny view cannot quit")
						}
					}
				}
			}
			if len(e.calls) != calls || e.doctorCalls != checks || e.planCalls != plans || e.applyCalls != 0 {
				t.Fatal("resize caused adapter activity")
			}
		})
	}
}
func inspectResponsive(t *testing.T, m tui.Model, needle string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if strings.Contains(compactResponsive(m.View()), compactResponsive(needle)) {
			return
		}
		m = m.Scroll(1)
	}
	t.Fatalf("detail %s not inspectable", needle)
}
func TestTUIArchiveSelectedRowAndDetailsRemainInspectable(t *testing.T) {
	for _, screen := range []tui.Screen{tui.Projects, tui.Sessions, tui.Inbox} {
		t.Run(string(screen), func(t *testing.T) {
			m, e := responsiveModel(t, screen)
			m = m.Select(35)
			for _, size := range [][2]int{{40, 12}, {215, 50}, {24, 9}, {80, 24}, {32, 12}} {
				m, _ = m.UpdateModel(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				out := ansi.Strip(m.View())
				if !strings.Contains(out, "row-35") {
					t.Fatalf("selected row hidden at %v: %q", size, out)
				}
			}
			calls := len(e.calls)
			if screen == tui.Sessions {
				inspectResponsive(t, m, "OWNER-END")
			}
			if screen == tui.Inbox {
				inspectResponsive(t, m, "BODY-END")
			}
			if len(e.calls) != calls {
				t.Fatal("inspection fetched authority")
			}
		})
	}
	for _, tc := range []struct {
		screen tui.Screen
		tail   string
	}{{tui.Cloud, "PLAN-END"}, {tui.Doctor, "CHECK-END"}} {
		m, _ := responsiveModel(t, tc.screen)
		inspectResponsive(t, m.SetSize(40, 12), tc.tail)
	}
}
func TestTUIArchiveDraftAndConfirmationKeysStayIsolated(t *testing.T) {
	m, e := responsiveModel(t, tui.Inbox)
	m = m.BeginSend()
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("peer|DRAFT-界é👩‍💻")})
	calls := len(e.calls)
	for _, key := range []string{"v", "]", "["} {
		var cmd tea.Cmd
		m, cmd = m.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Fatal("edit key triggered work")
		}
	}
	n, cmd := m.NextPage()
	if cmd != nil || n.State().Page != 1 {
		t.Fatal("public paging active while editing")
	}
	m = n.CycleFilter()
	for _, size := range [][2]int{{8, 1}, {24, 9}, {215, 50}, {32, 12}} {
		m, _ = m.UpdateModel(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertResponsiveBounds(t, m, size[0], size[1])
		if size[0] >= 24 && size[1] >= 9 && !strings.Contains(ansi.Strip(m.View()), "Esc cancel") {
			t.Fatal("missing editor cancel")
		}
	}
	if m.State().Filter != tui.FilterCurrent || m.State().Page != 1 || len(e.calls) != calls {
		t.Fatal("edit changed page/filter/authority")
	}
	m, cmd = m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("draft submit missing")
	}
	m, _ = m.UpdateModel(cmd())
	last := e.calls[len(e.calls)-1]
	if last.Operation != "send" || last.Workspace != "w" || last.Project != "p" || last.Session != "actor" || last.Args["instance"] != "incarnation" || last.Args["body"] != "DRAFT-界é👩‍💻v][" {
		t.Fatalf("draft or pinned actor changed: %+v", last)
	}
	c, ce := responsiveModel(t, tui.Cloud)
	c, _ = c.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	for _, key := range []string{"v", "]", "["} {
		c, cmd = c.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Fatal("confirmation issued work")
		}
	}
	c, _ = c.UpdateModel(tea.WindowSizeMsg{Width: 24, Height: 9})
	if !c.State().Confirming || !strings.Contains(ansi.Strip(c.View()), "Esc cancel") {
		t.Fatal("resize lost confirmation or cancel")
	}
	c, _ = c.UpdateModel(tea.KeyMsg{Type: tea.KeyEsc})
	if c.State().Confirming || ce.applyCalls != 0 {
		t.Fatal("cancel applied plan")
	}
}

// Keep normalization scoped to assertions; stored Unicode must remain unchanged.
func compactResponsive(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("│╭╮╰╯─", r) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func TestTUIArchiveRefreshAndFilterPreserveSelectedID(t *testing.T) {
	m, e := responsiveModel(t, tui.Sessions)
	m = m.Select(35)
	e.rows[0], e.rows[35] = e.rows[35], e.rows[0]
	m, cmd := m.Reload()
	m, _ = m.UpdateModel(cmd())
	if got := m.State().Sessions[m.State().Selected].ID; got != "id-35" {
		t.Fatalf("refresh selected %s", got)
	}
	m = m.SetFilter(tui.FilterAll).SetFilter(tui.FilterCurrent)
	if got := m.State().Sessions[m.State().Selected].ID; got != "id-35" {
		t.Fatalf("filter selected %s", got)
	}
	e.rows = e.rows[1:]
	m, cmd = m.Reload()
	m, _ = m.UpdateModel(cmd())
	if m.State().Selected < 0 || m.State().Selected >= len(m.State().Sessions) {
		t.Fatal("disappeared selection not clamped")
	}
	for _, r := range e.calls {
		if r.Workspace != "w" || r.Project != "p" || r.Session != "actor" || r.Args["instance"] != "incarnation" {
			t.Fatalf("refresh changed actor: %+v", r)
		}
	}
}
func TestTUIArchiveMirrorViewRemainsScopedReadOnly(t *testing.T) {
	m, e := responsiveModel(t, tui.Inbox)
	e.source = "offline-mirror"
	m, cmd := m.Reload()
	m, _ = m.UpdateModel(cmd())
	before := len(e.calls)
	m = m.SetSize(80, 24)
	inspectResponsive(t, m, "Mirrorcoverageisincomplete")
	m = m.SetFilter(tui.FilterAll).BeginSend()
	if cmdModel, command := m.Send("peer", "body", "test-op"); command != nil {
		_ = cmdModel
		t.Fatal("mirror exposed send command")
	}
	if len(e.calls) != before || m.State().Source != "offline-mirror" {
		t.Fatal("offline browsing changed authority/source")
	}
}

func TestTUIArchiveReplyTargetStableAcrossTick(t *testing.T) {
	m, e := responsiveModel(t, tui.Inbox)
	e.rows[0]["read_at"] = "2026-10-20T14:00:01Z"
	m, cmd := m.Reload()
	m, _ = m.UpdateModel(cmd())
	m = m.Select(0).BeginReply()
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("reply-original")})
	calls := len(e.calls)
	m, _ = m.UpdateModel(tui.ArchiveTickMsg{Now: time.Date(2026, 10, 21, 14, 0, 2, 0, time.UTC)})
	if m.State().Messages[0].ID != "id-01" {
		t.Fatal("tick fixture did not remove original recipient row")
	}
	m, cmd = m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("reply submit missing")
	}
	m, _ = m.UpdateModel(cmd())
	if len(e.calls) != calls+1 {
		t.Fatal("tick/submit issued extra requests")
	}
	r := e.calls[len(e.calls)-1]
	if r.Operation != "reply" || r.Args["message_id"] != "id-00" || r.Args["body"] != "reply-original" || r.Session != "actor" || r.Args["instance"] != "incarnation" {
		t.Fatalf("reply target changed during tick: %+v", r)
	}
}

func TestTUIArchiveSelectedImmutableIDInspectable(t *testing.T) {
	for _, screen := range []tui.Screen{tui.Projects, tui.Sessions, tui.Inbox} {
		t.Run(string(screen), func(t *testing.T) {
			m, e := responsiveModel(t, screen)
			e.rows[35]["id"] = "selected-immutable-" + strings.Repeat("identifier", 8) + "-ID-END"
			m, cmd := m.Reload()
			m, _ = m.UpdateModel(cmd())
			m = m.Select(35).SetSize(40, 12)
			if screen == tui.Inbox {
				// A message shows its ID once opened.
				m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
			}
			inspectResponsive(t, m, e.rows[35]["id"].(string))
		})
	}
}
