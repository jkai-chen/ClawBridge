package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"clawbridge/internal/linearprovider"
)

// Shared views (TUI-UX-AUDIT U63) is a view inside the Linear screen, v
// from the board: each bound team's shared Linear views, both kinds, read
// with the viewer's Linear connection. It is read-only: a project owner
// writes views in the CLI (linear view create, update, delete).

// TeamViews is one bound team's views; Err says why they couldn't be read,
// More that there were more than were read.
type TeamViews struct {
	TeamID         string
	Views          []linearprovider.View
	OtherKinds     int
	WorkspaceViews int
	More           bool
	Err            string
}

type viewsState struct {
	loaded bool
	teams  []TeamViews
}

type viewsLoadedMsg struct {
	request uint64
	teams   []TeamViews
	err     error
}

func (m Model) openViews() (Model, tea.Cmd) {
	m.linear.step, m.selected = "views", 0
	m.views = viewsState{}
	return m.viewsLoad()
}

// viewsLoad reads the views, on open and on r.
func (m Model) viewsLoad() (Model, tea.Cmd) {
	requestID := m.request
	if m.cfg.Linear == nil {
		return m, func() tea.Msg {
			return viewsLoadedMsg{request: requestID, err: fmt.Errorf("Linear isn't available in this TUI")}
		}
	}
	m.loading = true
	source, ctx, profile := m.cfg.Linear, m.cfg.Context, m.linear.profile
	return m, func() tea.Msg {
		d, err := source.Do(ctx, LinearRequest{Operation: "views", ProfileID: profile})
		return viewsLoadedMsg{request: requestID, teams: d.Views, err: err}
	}
}

func (m Model) applyViews(x viewsLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "views" {
		return m
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m
	}
	m.views = viewsState{loaded: true, teams: x.teams}
	if n := len(m.viewRows()); m.selected >= n {
		m.selected = max(0, n-1)
	}
	return m
}

// allViews are the views in the order shown, team by team.
func (m Model) allViews() []linearprovider.View {
	out := []linearprovider.View{}
	for _, t := range m.views.teams {
		out = append(out, t.Views...)
	}
	return out
}

func (m Model) viewRows() []string {
	rows := []string{}
	for _, v := range m.allViews() {
		rows = append(rows, v.ID)
	}
	return rows
}

func renderViews(m Model) string {
	var b strings.Builder
	width := tableBudget(m)
	if !m.views.loaded {
		if m.loading {
			b.WriteString(mutedStyle.Render("Reading the shared views with your Linear connection…") + "\n")
		}
		return b.String()
	}
	b.WriteString(mutedStyle.Render(ansi.Wrap("Read-only. A project owner writes views in their own terminal with clawbridge linear view create|update|delete; view show ID has a view's details (d shows IDs).", width, "")) + "\n")
	i := 0
	for _, t := range m.views.teams {
		team := firstNonempty(m.linearNames([]string{t.TeamID}), terminalText(t.TeamID))
		b.WriteString("\n" + accentStyle.Render(team) + "\n")
		if t.Err != "" {
			b.WriteString(errorStyle.Render("  not read") + "\n")
			b.WriteString(mutedStyle.Render(indentWrap(terminalText(t.Err), "    ", width)) + "\n")
		} else if len(t.Views) == 0 {
			b.WriteString(mutedStyle.Render("  No shared views.") + "\n")
		}
		for _, v := range t.Views {
			prefix := "  "
			if i == m.selected {
				prefix = accentStyle.Render("› ")
			}
			b.WriteString(prefix + padRight(clip(terminalText(v.Name), 40), 42) + v.Kind + "\n")
			if i == m.selected {
				if v.URL != "" {
					b.WriteString(mutedStyle.Render(indentWrap(terminalText(v.URL), "    ", width)) + "\n")
				}
				if m.linear.showIDs {
					b.WriteString(mutedStyle.Render("    ID "+terminalText(v.ID)) + "\n")
				}
			}
			i++
		}
		left := []string{}
		if t.More {
			left = append(left, "more views than shown")
		}
		if t.OtherKinds > 0 {
			left = append(left, plural(t.OtherKinds, "view of another kind", "views of other kinds"))
		}
		if t.WorkspaceViews > 0 {
			left = append(left, plural(t.WorkspaceViews, "workspace view", "workspace views"))
		}
		if len(left) > 0 {
			b.WriteString(mutedStyle.Render(indentWrap("Not listed: "+strings.Join(left, ", ")+".", "  ", width)) + "\n")
		}
	}
	return b.String()
}

// indentWrap wraps text to width with every line indented.
func indentWrap(text, indent string, width int) string {
	lines := strings.Split(ansi.Wrap(text, max(1, width-len(indent)), ""), "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}
