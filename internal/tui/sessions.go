package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// stateWord is a session's state in one word: retired or moved first,
// then stale, then its presence (TUI-UX-AUDIT U14).
func stateWord(s SessionRow) string {
	switch {
	case s.Status == "retired" || s.Status == "moved":
		return s.Status
	case s.Stale:
		return "stale"
	}
	return firstNonempty(s.Presence, s.Status, "-")
}

// sessionName is the NAME cell: the name, and ★ on the coordinator (U16).
func sessionName(s SessionRow) string {
	if s.Coordinator {
		return "★ " + s.Name
	}
	return s.Name
}

// sessionCounts says how many sessions are in each state, in the words
// the Sessions tab shows, so Overview and Sessions agree (U22).
func sessionCounts(rows []SessionRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		switch w := stateWord(r); w {
		case "retired", "moved", "stale":
			counts[w]++
		default:
			counts["active"]++
		}
	}
	out := []string{}
	for _, w := range []string{"active", "stale", "retired", "moved"} {
		if counts[w] > 0 {
			out = append(out, fmt.Sprintf("%d %s", counts[w], w))
		}
	}
	return strings.Join(out, " · ")
}

// renderSessions is the project's sessions: the columns that matter at
// any width, OWNER and HOST from 140 columns, and the selected session's
// details with short labels and its ID last (U14, U15, U24).
func renderSessions(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenSessions))
	b.WriteString("\n\n")
	if body, ok := bodyState(m, "sessions", len(m.sessions) > 0); ok {
		return b.String() + body
	}
	if len(m.sessions) == 0 {
		b.WriteString("No sessions yet. An agent joins with clawbridge register NAME in its checkout.")
		return b.String()
	}
	headers, widths := []string{"NAME", "STATE", "ISSUE", "CONTEXT", "AGENT"}, []int{22, 10, 16, 18, 12}
	wide := m.width >= 140
	if wide {
		headers, widths = append(headers, "OWNER", "HOST"), append(widths, 28, 22)
	}
	rows := make([][]string, len(m.sessions))
	for i, s := range m.sessions {
		rows[i] = []string{sessionName(s), stateWord(s), firstNonempty(s.Issue, "-"), firstNonempty(s.Context, "-"), firstNonempty(s.AgentKind, "-")}
		if wide {
			rows[i] = append(rows[i], firstNonempty(s.Owner, "Not recorded"), firstNonempty(s.Host, "Not recorded"))
		}
	}
	b.WriteString(renderTable(m, headers, widths, rows))
	if m.selected >= 0 && m.selected < len(m.sessions) {
		b.WriteString("\n\n" + renderSessionDetails(m, m.sessions[m.selected]))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderSessionDetails(m Model, row SessionRow) string {
	var b strings.Builder
	head := terminalText(row.Name)
	if row.Coordinator {
		head += " ★ coordinator"
	}
	b.WriteString(accentStyle.Render(head) + "\n")
	state := stateWord(row)
	if p := firstNonempty(row.Presence, row.Status); p != "" && p != state {
		state += " (" + p + ")"
	}
	context := "Not reported"
	if row.ContextDetail != "" {
		context = row.ContextDetail
		if t, err := time.Parse(time.RFC3339Nano, row.ContextAt); err == nil {
			context += ", " + ago(m.cfg.Now(), t)
		}
	}
	details := []struct{ label, value string }{
		{"State", state},
		{"Role", firstNonempty(row.Role, "-") + " · agent " + firstNonempty(row.AgentKind, "-")},
		{"Issue", firstNonempty(row.Assignment, "None")},
		{"Context", context},
		{"Owner", firstNonempty(row.Owner, "Not recorded")},
		{"Host", selfReported(row.Host, "")},
		{"Account", selfReported(row.Account, ", not verified identity")},
	}
	for _, d := range details {
		value := strings.NewReplacer("\n", " ", "\t", " ").Replace(terminalText(d.value))
		b.WriteString(ansi.Hardwrap(fmt.Sprintf("%-8s %s", d.label, value), tableBudget(m), true) + "\n")
	}
	b.WriteString(mutedStyle.Render(ansi.Hardwrap(fmt.Sprintf("%-8s %s", "ID", terminalText(row.ID)), tableBudget(m), true)) + "\n")
	if m.cfg.Mode == ModeLocal {
		b.WriteString(ansi.Hardwrap("Local OS-user authority; no Google verification claimed.", tableBudget(m), true))
	}
	return b.String()
}

// selfReported is a label the session reported about itself, said so.
func selfReported(value, more string) string {
	if value == "" {
		return "Not recorded"
	}
	return value + " (self-reported" + more + ")"
}

// selectedSession is the session row under the cursor on Sessions.
func (m Model) selectedSession() (SessionRow, bool) {
	if m.screen != ScreenSessions || m.selected < 0 || m.selected >= len(m.sessions) {
		return SessionRow{}, false
	}
	return m.sessions[m.selected], true
}

var sessionBindings = []Binding{
	// U26: a message to the selected session, its name filled in.
	{ID: "message", Keys: []string{"n"}, Label: "message it", When: func(m Model) bool { _, ok := m.selectedSession(); return ok }, Action: func(m Model, _ string) (Model, tea.Cmd) {
		s, _ := m.selectedSession()
		return m.BeginSendTo(s.Name), nil
	}},
}
