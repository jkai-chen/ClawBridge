package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ArchiveFilter is a derived, page-local display filter, never a mutation.
type ArchiveFilter uint8

const (
	FilterCurrent ArchiveFilter = iota
	FilterArchive
	FilterAll
)

func (f ArchiveFilter) String() string {
	switch f {
	case FilterArchive:
		return "Archive"
	case FilterAll:
		return "All"
	default:
		return "Current"
	}
}

// ArchiveTickMsg reclassifies retained rows without contacting authority.
type ArchiveTickMsg struct{ Now time.Time }

func archiveTick() tea.Cmd {
	return tea.Tick(time.Minute, func(t time.Time) tea.Msg { return ArchiveTickMsg{t} })
}
func archivedStamp(stamp string, now time.Time) bool {
	t, e := time.Parse(time.RFC3339Nano, stamp)
	return e == nil && !t.After(now) && now.Sub(t) >= 24*time.Hour
}
func (m Model) reclassify(now time.Time) Model {
	old := ""
	rows := m.currentRows()
	if m.selected >= 0 && m.selected < len(rows) {
		old = rows[m.selected]
	}
	m.classifiedAt = now
	if m.rawSessions != nil {
		m.sessions = nil
		for _, r := range m.rawSessions {
			a := (r.Status == "retired" || r.Status == "moved") && archivedStamp(r.UpdatedAt, now)
			if m.filter == FilterAll || a == (m.filter == FilterArchive) {
				m.sessions = append(m.sessions, r)
			}
		}
	}
	if m.rawMessages != nil {
		m.messages = nil
		for _, r := range m.rawMessages {
			a := archivedStamp(r.ReadAt, now)
			if m.filter == FilterAll || a == (m.filter == FilterArchive) {
				m.messages = append(m.messages, r)
			}
		}
	}
	rows = m.currentRows()
	m.selected = min(m.selected, max(0, len(rows)-1))
	for i, id := range rows {
		if id == old {
			m.selected = i
			break
		}
	}
	return m
}

// SetFilter retains loaded raw rows and changes only the displayed subset.
func (m Model) SetFilter(f ArchiveFilter) Model {
	if (m.screen != ScreenSessions && m.screen != ScreenInbox) || f > FilterAll || m.inputMode != inputNone || m.confirming {
		return m
	}
	m.filter = f
	m.scroll = 0
	m.scrollManual = false
	return m.reclassify(m.cfg.Now())
}
func (m Model) CycleFilter() Model { return m.SetFilter((m.filter + 1) % 3) }
func (m Model) pageCursor() int64 {
	if m.pageIndex >= 0 && m.pageIndex < len(m.pages) {
		return m.pages[m.pageIndex]
	}
	return 0
}
func (m Model) NextPage() (Model, tea.Cmd) {
	if m.inputMode != inputNone || m.confirming || m.loading || !m.hasMore || (m.screen != ScreenSessions && m.screen != ScreenInbox) || m.nextCursor <= m.pageCursor() {
		return m, nil
	}
	m = m.stagePage()
	m.pages = append(append([]int64(nil), m.pages[:m.pageIndex+1]...), m.nextCursor)
	m.pageIndex++
	m.scroll = 0
	m.scrollManual = false
	return m.refresh()
}
func (m Model) PreviousPage() (Model, tea.Cmd) {
	if m.inputMode != inputNone || m.confirming || m.loading || m.pageIndex == 0 || (m.screen != ScreenSessions && m.screen != ScreenInbox) {
		return m, nil
	}
	m = m.stagePage()
	m.pageIndex--
	m.scroll = 0
	m.scrollManual = false
	return m.refresh()
}

// Scroll moves the content viewport without authority access.
func (m Model) Scroll(lines int) Model {
	_, body, budget := layoutParts(m)
	m.scroll = max(0, min(max(0, len(body)-budget), bodyOffset(m, body, budget)+lines))
	m.scrollManual = true
	return m
}

// Failed paging retains the last successfully loaded page and its navigation.
type pagingState struct {
	pages []int64
	index int
}

func (m Model) stagePage() Model {
	if m.pageRollback == nil {
		m.pageRollback = &pagingState{append([]int64(nil), m.pages...), m.pageIndex}
	}
	return m
}
func (m Model) restorePage() Model {
	if m.pageRollback != nil {
		m.pages = m.pageRollback.pages
		m.pageIndex = m.pageRollback.index
		m.pageRollback = nil
	}
	return m
}
