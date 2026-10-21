package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type archiveProbeExecutor struct {
	failure error
	calls   []bus.Request
	pages   map[int64]map[string]any
}

func (e *archiveProbeExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.calls = append(e.calls, r)
	if e.failure != nil {
		return bus.Result{}, e.failure
	}
	cursor, ok := r.Args["after"].(int64)
	if !ok {
		return bus.Result{}, fmt.Errorf("after is not int64: %T", r.Args["after"])
	}
	if r.Operation != "list_sessions" && r.Operation != "inbox" {
		return bus.Result{}, fmt.Errorf("unexpected operation %s", r.Operation)
	}
	return bus.Result{Source: "server", Data: e.pages[cursor]}, nil
}
func archiveProbeModel(t *testing.T, e *archiveProbeExecutor, screen tui.Screen, now time.Time) tui.Model {
	t.Helper()
	m, err := tui.New(tui.Config{Mode: tui.ModeCloud, Executor: e, Workspace: "w", Project: "p", Session: "actor", Instance: "incarnation", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.Show(screen)
	if cmd == nil {
		t.Fatal("missing load")
	}
	m, _ = m.UpdateModel(cmd())
	if m.State().Error != "" {
		t.Fatal(m.State().Error)
	}
	return m
}
func archiveProbeIDs(m tui.Model) []string {
	ids := []string{}
	for _, r := range m.State().Sessions {
		ids = append(ids, r.ID)
	}
	for _, r := range m.State().Messages {
		ids = append(ids, r.ID)
	}
	return ids
}
func TestTUIArchiveExactBoundaryAndExclusions(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	old := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-24*time.Hour + time.Nanosecond).Format(time.RFC3339Nano)
	future := now.Add(time.Hour).Format(time.RFC3339Nano)
	for _, screen := range []tui.Screen{tui.Sessions, tui.Inbox} {
		t.Run(fmt.Sprint(screen), func(t *testing.T) {
			rows := []map[string]any{}
			for _, v := range []struct{ id, stamp, status string }{{"boundary", old, "retired"}, {"moved", old, "moved"}, {"recent", recent, "retired"}, {"missing", "", "retired"}, {"bad", "broken", "retired"}, {"future", future, "retired"}, {"active", old, "active"}, {"stale", old, "stale"}} {
				row := map[string]any{"id": v.id, "name": v.id, "status": v.status, "updated_at": v.stamp, "created_at": old, "read_at": v.stamp}
				if screen == tui.Inbox && (v.id == "active" || v.id == "stale") {
					row["read_at"] = ""
				}
				rows = append(rows, row)
			}
			key := "sessions"
			if screen == tui.Inbox {
				key = "messages"
			}
			e := &archiveProbeExecutor{pages: map[int64]map[string]any{0: {key: rows}}}
			m := archiveProbeModel(t, e, screen, now)
			if got := archiveProbeIDs(m); !reflect.DeepEqual(got, []string{"recent", "missing", "bad", "future", "active", "stale"}) {
				t.Fatalf("Current: %v", got)
			}
			m = m.SetFilter(tui.FilterArchive)
			if got := archiveProbeIDs(m); !reflect.DeepEqual(got, []string{"boundary", "moved"}) {
				t.Fatalf("Archive: %v", got)
			}
			m = m.SetFilter(tui.FilterAll)
			if len(archiveProbeIDs(m)) != 8 {
				t.Fatal("filter discarded raw rows")
			}
			m = m.SetFilter(tui.FilterArchive)
			m, _ = m.UpdateModel(tui.ArchiveTickMsg{Now: now.Add(time.Nanosecond)})
			if !reflect.DeepEqual(archiveProbeIDs(m), []string{"boundary", "moved", "recent"}) {
				t.Fatalf("tick did not reclassify retained rows: %v", archiveProbeIDs(m))
			}
			if len(e.calls) != 1 {
				t.Fatalf("filter/tick polled authority: %d", len(e.calls))
			}
		})
	}
}
func TestTUIArchiveEmptyFilteredPageScopedCursor(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	e := &archiveProbeExecutor{pages: map[int64]map[string]any{
		0:   {"sessions": []map[string]any{{"id": "current", "status": "active"}}, "has_more": true, "next_cursor": int64(321)},
		321: {"sessions": []map[string]any{{"id": "archived", "status": "retired", "updated_at": now.Add(-25 * time.Hour).Format(time.RFC3339Nano)}}, "has_more": false},
	}}
	m := archiveProbeModel(t, e, tui.Sessions, now).SetFilter(tui.FilterArchive)
	if len(m.State().Sessions) != 0 || !m.State().HasMore {
		t.Fatal("empty filtered page lost continuation")
	}
	m, cmd := m.NextPage()
	if cmd == nil {
		t.Fatal("empty page blocked next")
	}
	m, _ = m.UpdateModel(cmd())
	if m.State().Page != 2 || !reflect.DeepEqual(archiveProbeIDs(m), []string{"archived"}) {
		t.Fatalf("next: %+v", m.State())
	}
	m, cmd = m.PreviousPage()
	if cmd == nil {
		t.Fatal("no visited previous")
	}
	m, _ = m.UpdateModel(cmd())
	if m.State().Page != 1 || len(m.State().Sessions) != 0 {
		t.Fatal("previous lost filter/page")
	}
	if len(e.calls) != 3 {
		t.Fatalf("unexpected fetch count %d", len(e.calls))
	}
	for i, r := range e.calls {
		want := int64(0)
		if i == 1 {
			want = 321
		}
		if r.Workspace != "w" || r.Project != "p" || r.Session != "actor" || r.Args["instance"] != "incarnation" || r.Args["limit"] != 100 || r.Args["after"] != want {
			t.Fatalf("scope/cursor mismatch: %+v", r)
		}
	}
}

func TestTUIArchiveDiagnosticTailInspectable(t *testing.T) {
	e := &archiveProbeExecutor{failure: errors.New(strings.Repeat("diagnostic-context ", 80) + "DIAGNOSTIC-END")}
	m, err := tui.New(tui.Config{Mode: tui.ModeCloud, Executor: e, Workspace: "w", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.Show(tui.Sessions)
	m, _ = m.UpdateModel(cmd())
	m = m.SetSize(40, 12)
	found := false
	for i := 0; i < 180; i++ {
		if strings.Contains(ansi.Strip(m.View()), "DIAGNOSTIC-END") {
			found = true
			break
		}
		m = m.Scroll(1)
	}
	if !found {
		t.Fatal("long error tail inaccessible through content scrolling")
	}
	if len(e.calls) != 1 {
		t.Fatal("scroll contacted authority")
	}
}
func TestTUIArchiveSmallEditorRetainsCancelAndDraft(t *testing.T) {
	e := &archiveProbeExecutor{pages: map[int64]map[string]any{0: {"messages": []map[string]any{}}}}
	m := archiveProbeModel(t, e, tui.Inbox, time.Now()).BeginSend()
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("peer|DRAFT-PRESERVED")})
	for _, size := range [][2]int{{24, 9}, {32, 12}, {215, 50}, {24, 9}} {
		var cmd tea.Cmd
		m, cmd = m.UpdateModel(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if cmd != nil {
			t.Fatal("resize issued command")
		}
		output := ansi.Strip(m.View())
		if !strings.Contains(output, "Esc cancel") {
			t.Fatalf("cancel control hidden at %dx%d: %q", size[0], size[1], output)
		}
		lines := strings.Split(output, "\n")
		if len(lines) > size[1] {
			t.Fatal("row overflow")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("cell width overflow")
			}
		}
	}
	if len(e.calls) != 1 {
		t.Fatal("editor/resize contacted authority")
	}
	m = m.SetSize(100, 30)
	if !strings.Contains(ansi.Strip(m.View()), "DRAFT-PRESERVED") {
		t.Fatal("resize lost draft")
	}
}

func TestTUIArchiveFailedPageRestoresLastSuccessfulPage(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	e := &archiveProbeExecutor{pages: map[int64]map[string]any{
		0:   {"sessions": []map[string]any{{"id": "first", "status": "active"}}, "has_more": true, "next_cursor": int64(321)},
		321: {"sessions": []map[string]any{{"id": "second", "status": "active"}}, "has_more": false},
	}}
	m := archiveProbeModel(t, e, tui.Sessions, now)
	e.failure = errors.New("page transport unavailable")
	m, cmd := m.NextPage()
	m, _ = m.UpdateModel(cmd())
	if m.State().Page != 1 || m.State().Cursor != 0 || m.State().Error == "" || !m.State().HasMore || !reflect.DeepEqual(archiveProbeIDs(m), []string{"first"}) {
		t.Fatalf("failed next did not roll back: %+v", m.State())
	}
	e.failure = nil
	m, cmd = m.NextPage()
	m, _ = m.UpdateModel(cmd())
	if m.State().Page != 2 || m.State().Cursor != 321 || !reflect.DeepEqual(archiveProbeIDs(m), []string{"second"}) {
		t.Fatal("retry lost exact continuation")
	}
	e.failure = errors.New("previous unavailable")
	m, cmd = m.PreviousPage()
	m, _ = m.UpdateModel(cmd())
	if m.State().Page != 2 || m.State().Cursor != 321 || !reflect.DeepEqual(archiveProbeIDs(m), []string{"second"}) {
		t.Fatal("failed previous did not restore successful page")
	}
	e.failure = nil
	m, cmd = m.PreviousPage()
	m, _ = m.UpdateModel(cmd())
	if m.State().Page != 1 || m.State().Cursor != 0 {
		t.Fatal("previous retry lost history")
	}
}
