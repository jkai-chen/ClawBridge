package tui

import (
	"clawbridge/internal/bus"
	"context"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"
	"unicode"
)

type archiveExecutor struct{ requests []bus.Request }

func (e *archiveExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.requests = append(e.requests, r)
	after := r.Args["after"]
	rows := []map[string]any{{"id": "old", "name": "old", "status": "retired", "updated_at": "2020-01-01T00:00:00Z"}}
	more := true
	next := int64(17)
	if after == int64(17) {
		rows = []map[string]any{{"id": "new", "name": "new", "status": "active"}}
		more = false
		next = 18
	}
	return bus.Result{Source: "server", Data: map[string]any{"sessions": rows, "next_cursor": next, "has_more": more}}, nil
}
func archiveModel(t *testing.T) Model {
	t.Helper()
	m, e := New(Config{Mode: ModeLocal, Executor: &archiveExecutor{}, Workspace: "ws", Project: "pr", Session: "self", Instance: "bound", Now: func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestArchiveClassificationBoundaryAndRetainedRows(t *testing.T) {
	m := archiveModel(t)
	m.screen = ScreenSessions
	n := m.cfg.Now()
	stamp := func(d time.Duration) string { return n.Add(d).Format(time.RFC3339Nano) }
	m.rawSessions = []SessionRow{{ID: "equal", Status: "retired", UpdatedAt: stamp(-24 * time.Hour)}, {ID: "moved", Status: "moved", UpdatedAt: stamp(-25 * time.Hour)}, {ID: "active", Status: "active", UpdatedAt: stamp(-48 * time.Hour)}, {ID: "future", Status: "retired", UpdatedAt: stamp(time.Hour)}, {ID: "bad", Status: "retired", UpdatedAt: "invalid"}, {ID: "recent", Status: "retired", UpdatedAt: stamp(-23 * time.Hour)}}
	m = m.reclassify(n)
	if len(m.sessions) != 4 {
		t.Fatal(m.sessions)
	}
	m = m.SetFilter(FilterArchive)
	if len(m.sessions) != 2 {
		t.Fatal(m.sessions)
	}
	m = m.SetFilter(FilterAll).Select(2)
	m = m.SetFilter(FilterCurrent)
	if m.sessions[m.selected].ID != "active" {
		t.Fatal("identity selection lost")
	}
	if len(m.rawSessions) != 6 {
		t.Fatal("filter discarded raw data")
	}
	m.screen = ScreenInbox
	m.rawMessages = []MessageRow{{ID: "unread", CreatedAt: stamp(-100 * time.Hour)}, {ID: "read", ReadAt: stamp(-24 * time.Hour)}, {ID: "recent", ReadAt: stamp(-time.Hour)}}
	m = m.SetFilter(FilterArchive)
	if len(m.messages) != 1 || m.messages[0].ID != "read" {
		t.Fatal(m.messages)
	}
	m = m.SetFilter(FilterCurrent)
	if len(m.messages) != 2 {
		t.Fatal(m.messages)
	}
	m = m.SetFilter(FilterAll)
	m, _ = m.UpdateModel(ArchiveTickMsg{n.Add(24 * time.Hour)})
	if len(m.messages) != 3 {
		t.Fatal("tick lost raw rows")
	}
}
func TestArchivePagingEmptyFilterUsesExactAuthorityCursor(t *testing.T) {
	m := archiveModel(t)
	e := m.cfg.Executor.(*archiveExecutor)
	m, c := m.Show(ScreenSessions)
	m, _ = m.UpdateModel(c())
	if len(m.sessions) != 0 || !m.hasMore {
		t.Fatal("empty filter lost paging")
	}
	m, c = m.NextPage()
	if c == nil || !m.loading {
		t.Fatal("no next command")
	}
	m, _ = m.UpdateModel(c())
	if len(m.sessions) != 1 || m.pageIndex != 1 {
		t.Fatal(m.State())
	}
	if e.requests[1].Args["after"] != int64(17) || e.requests[1].Args["limit"] != 100 {
		t.Fatal(e.requests)
	}
	if _, ok := e.requests[1].Args["cursor"]; ok {
		t.Fatal("invalid cursor argument")
	}
	m, c = m.PreviousPage()
	m, _ = m.UpdateModel(c())
	if e.requests[2].Args["after"] != int64(0) {
		t.Fatal(e.requests)
	}
	m, c = m.Reload()
	m, _ = m.UpdateModel(c())
	if m.pageIndex != 0 {
		t.Fatal("reload did not reset")
	}
	for _, r := range e.requests {
		if r.Workspace != "ws" || r.Project != "pr" || r.Session != "self" || r.Args["instance"] != "bound" {
			t.Fatal("scope changed", r)
		}
	}
}
func TestResponsiveWholeScreenBoundsAndResizePurity(t *testing.T) {
	m := archiveModel(t)
	long := strings.Repeat("界é👩‍💻\x1b]52;c;payload\a", 20)
	m.workspace.Name = long
	m.project.Slug = long
	m.session.Name = long
	m.loadErr = long
	m.status = noteStatus(long)
	for i := 0; i < 40; i++ {
		m.projects = append(m.projects, ProjectRow{ID: fmt.Sprint(i), Slug: long, Name: long})
		m.sessions = append(m.sessions, SessionRow{ID: fmt.Sprint(i), Name: long, Owner: long, Host: long})
		m.messages = append(m.messages, MessageRow{ID: fmt.Sprint(i), Sender: long, Body: long})
		m.checks = append(m.checks, Check{ID: fmt.Sprint(i), Title: long, Detail: long, Fix: long})
	}
	m.plan = &SetupPlan{Title: long, Plan: long}
	for _, screen := range orderedScreens {
		for _, size := range [][2]int{{1, 1}, {12, 4}, {24, 9}, {40, 12}, {80, 24}, {215, 50}} {
			for _, editing := range []bool{false, true} {
				n := m
				n.screen = screen
				n.selected = 35
				if editing {
					n.inputMode = inputSend
					n.draft = long
				}
				n, c := n.UpdateModel(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				if c != nil {
					t.Fatal("resize requested work")
				}
				out := Render(n)
				lines := strings.Split(out, "\n")
				if len(lines) > size[1] {
					t.Fatalf("%s %v height %d", screen, size, len(lines))
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > size[0] {
						t.Fatalf("%s %v width %d", screen, size, ansi.StringWidth(line))
					}
				}
				if strings.Contains(ansi.Strip(out), "\x1b") || strings.Contains(out, "\a") {
					t.Fatal("terminal control leaked")
				}
				if n.selected != 35 || n.draft != func() string {
					if editing {
						return long
					}
					return ""
				}() || n.cfg.Instance != "bound" {
					t.Fatal("resize changed state")
				}
			}
		}
	}
	if len(m.cfg.Executor.(*archiveExecutor).requests) != 0 {
		t.Fatal("resize contacted authority")
	}
}

func TestResponsiveSelectedVisibilityAndFullDetailScroll(t *testing.T) {
	m := archiveModel(t)
	m.screen = ScreenSessions
	m = m.SetSize(40, 12)
	for i := 0; i < 80; i++ {
		m.sessions = append(m.sessions, SessionRow{ID: fmt.Sprint(i), Name: fmt.Sprintf("row-%02d", i), Owner: strings.Repeat("owner", 40) + "OWNER-END", Host: "host"})
	}
	m = m.Select(65)
	if !strings.Contains(ansi.Strip(m.View()), "row-65") {
		t.Fatal("selected row not visible")
	}
	for _, size := range [][2]int{{80, 24}, {24, 9}, {215, 50}, {40, 12}} {
		m, _ = m.UpdateModel(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if !strings.Contains(ansi.Strip(m.View()), "row-65") {
			t.Fatalf("selection hidden %v", size)
		}
	}
	found := false
	for i := 0; i < 200; i++ {
		if strings.Contains(ansi.Strip(m.View()), "OWNER-END") {
			found = true
			break
		}
		m = m.Scroll(1)
	}
	if !found {
		t.Fatal("full selected attribution not inspectable")
	}
	m = m.Select(2)
	if !strings.Contains(ansi.Strip(m.View()), "row-02") {
		t.Fatal("selection did not regain viewport")
	}
}
func TestArchiveRefreshPreservesIdentityAndInitialScreenLoadsItsPage(t *testing.T) {
	m := archiveModel(t)
	m.screen = ScreenSessions
	c := m.Init()
	msg := c().(loadedMsg)
	if msg.kind != loadSessions {
		t.Fatal("initial selected view loaded wrong data")
	}
	m, _ = m.UpdateModel(msg)
	m = m.SetFilter(FilterAll)
	m.rawSessions = []SessionRow{{ID: "a"}, {ID: "b"}}
	m = m.reclassify(m.cfg.Now()).Select(1)
	(&m).applyLoad(loadedMsg{kind: loadSessions, result: bus.Result{Data: map[string]any{"sessions": []map[string]any{{"id": "b"}, {"id": "a"}}}}})
	if m.sessions[m.selected].ID != "b" {
		t.Fatal("refresh lost immutable selection")
	}
}
func TestArchiveEditorBlocksFilterAndPagingAndKeepsDraft(t *testing.T) {
	m := archiveModel(t)
	m.screen = ScreenInbox
	m.hasMore = true
	m.nextCursor = 10
	m = m.BeginSend()
	m.draft = "peer|draft"
	n, c := m.NextPage()
	if c != nil || n.pageIndex != 0 {
		t.Fatal("paging during edit")
	}
	n = n.CycleFilter()
	if n.filter != FilterCurrent || n.draft != m.draft {
		t.Fatal("filter changed draft state")
	}
}

func TestArchiveTickCannotRetargetReplyDraft(t *testing.T) {
	m := archiveModel(t)
	m.screen = ScreenInbox
	n := m.cfg.Now()
	m.rawMessages = []MessageRow{{ID: "original", ReadAt: n.Add(-23 * time.Hour).Format(time.RFC3339Nano)}, {ID: "other"}}
	m = m.reclassify(n).BeginReply()
	m.draft = "draft"
	m, _ = m.UpdateModel(ArchiveTickMsg{n.Add(2 * time.Hour)})
	if m.replyTarget != "original" || m.draft != "draft" || len(m.messages) != 1 || m.messages[0].ID != "other" {
		t.Fatal("tick retargeted or lost draft")
	}
	m, c := m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	if c == nil {
		t.Fatal("no explicit reply command")
	}
	_ = c()
	e := m.cfg.Executor.(*archiveExecutor)
	if e.requests[len(e.requests)-1].Args["message_id"] != "original" {
		t.Fatal(e.requests)
	}
}
func TestArchiveFailedPagingRetainsLoadedPage(t *testing.T) {
	m := archiveModel(t)
	m.screen = ScreenSessions
	m.hasMore = true
	m.nextCursor = 17
	m.sessions = []SessionRow{{ID: "retained"}}
	m, _ = m.NextPage()
	m, _ = m.UpdateModel(loadedMsg{screen: ScreenSessions, request: m.request, kind: loadSessions, err: fmt.Errorf("denied")})
	if m.pageIndex != 0 || m.sessions[0].ID != "retained" || !m.hasMore {
		t.Fatal("failed page misrepresented retained data")
	}
}

func TestResponsiveSelectedIDsSanitizedWithoutChangingReplyTarget(t *testing.T) {
	id := "selected-" + strings.Repeat("identifier", 8) + "界é\x1b]52;c;payload\a\r\u202e-ID-END"
	for _, screen := range []Screen{ScreenSessions, ScreenInbox} {
		t.Run(string(screen), func(t *testing.T) {
			m := archiveModel(t)
			m.screen = screen
			m.sessions = []SessionRow{{ID: id, Name: "worker"}}
			m.messages = []MessageRow{{ID: id, Sender: "peer", Body: "body", CreatedAt: "2026-10-21T12:00:00Z"}}
			// A message shows its ID once opened (Enter).
			m.inboxOpen = screen == ScreenInbox
			m = m.SetSize(40, 12)
			// Inspect the entire wrapped detail as well as its reachable viewport tail.
			body := strings.ReplaceAll(ansi.Strip(renderBody(m)), "\n", "")
			if !strings.Contains(body, terminalText(id)) {
				t.Fatal("full sanitized selected ID missing from details")
			}
			found := false
			for i := 0; i < 80; i++ {
				out := m.View()
				lines := strings.Split(out, "\n")
				if len(lines) > 12 {
					t.Fatal("ID detail exceeded viewport height")
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > 40 {
						t.Fatal("ID detail exceeded viewport width")
					}
				}
				plain := ansi.Strip(out)
				if strings.ContainsAny(plain, "\x1b\a\r\u202e") {
					t.Fatal("ID detail leaked terminal controls")
				}
				compact := strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) || strings.ContainsRune("│╭╮╰╯─", r) {
						return -1
					}
					return r
				}, plain)
				if strings.Contains(compact, "ID-END") {
					found = true
				}
				m = m.Scroll(1)
			}
			if !found || m.sessions[0].ID != id || m.messages[0].ID != id {
				t.Fatal("ID tail inaccessible or stored identity changed")
			}
			e := m.cfg.Executor.(*archiveExecutor)
			if len(e.requests) != 0 {
				t.Fatal("ID inspection accessed authority")
			}
			if screen == ScreenInbox {
				m = m.BeginReply()
				m.draft = "reply"
				_, cmd := m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
				if cmd == nil {
					t.Fatal("reply command missing")
				}
				_ = cmd()
				r := e.requests[0]
				if r.Args["message_id"] != id || r.Session != "self" || r.Args["instance"] != "bound" {
					t.Fatal("ID presentation changed reply identity or actor")
				}
			}
		})
	}
}
