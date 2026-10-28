package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"clawbridge/internal/bus"
)

// maxOverviews caps the project_overview reads one Projects load makes;
// opening a project past it reads that one (TUI-UX-AUDIT L1).
const maxOverviews = 20

// projectOverview is one project as project_overview describes it: its
// sessions and counts, its coordinator and its Linear board, by name.
type projectOverview struct {
	Err               string
	Coordinator       string
	CoordinatorStatus string
	Active            int64
	Retired           int64
	Moved             int64
	Stale             int64
	Sessions          []SessionRow
	Truncated         bool
	Linear            boardSummary
}

// boardSummary is a project's Linear board: the teams and projects bound,
// by the names stored with it.
type boardSummary struct {
	Configured bool
	Unreadable bool
	Mode       string
	Teams      []boardName
	Projects   []boardName
	NamesAt    string
}

type boardName struct {
	ID, Name, Closes string
	Default          bool
}

func parseProjectOverview(data map[string]any) projectOverview {
	o := projectOverview{}
	if c := mapData(data["coordinator"]); c != nil {
		o.Coordinator, o.CoordinatorStatus = text(c, "name"), text(c, "status")
	}
	if c := mapData(data["counts"]); c != nil {
		o.Active, o.Retired, o.Moved, o.Stale = integer(c, "active"), integer(c, "retired"), integer(c, "moved"), integer(c, "stale")
	}
	o.Sessions = sessionRows(records(data["sessions"]))
	o.Truncated = boolean(data, "sessions_truncated")
	if l := mapData(data["linear"]); l != nil {
		o.Linear = boardSummary{Configured: boolean(l, "configured"), Unreadable: boolean(l, "unreadable"), Mode: text(l, "mode"), NamesAt: text(l, "names_updated_at")}
		for _, t := range records(l["teams"]) {
			o.Linear.Teams = append(o.Linear.Teams, boardName{ID: text(t, "id"), Name: text(t, "name"), Default: boolean(t, "default")})
		}
		for _, p := range records(l["projects"]) {
			o.Linear.Projects = append(o.Linear.Projects, boardName{ID: text(p, "id"), Name: text(p, "name"), Closes: text(p, "closes")})
		}
	}
	return o
}

// readOverviews reads project_overview for each listed project, as the
// person (no session), up to maxOverviews.
func (m Model) readOverviews(projects []ProjectRow) map[string]projectOverview {
	out := map[string]projectOverview{}
	for i, p := range projects {
		if i == maxOverviews {
			break
		}
		out[p.ID] = m.readOverview(p)
	}
	return out
}

// readOverview reads one project's overview, as the person.
func (m Model) readOverview(p ProjectRow) projectOverview {
	result, err := m.cfg.Executor.Execute(m.cfg.Context, bus.Request{Operation: "project_overview", Workspace: m.cfg.Workspace, Project: firstNonempty(p.Slug, p.ID), Args: map[string]any{}})
	if err != nil {
		return projectOverview{Err: displayError(err)}
	}
	return parseProjectOverview(mapData(result.Data))
}

// overviewMsg is one project's overview, read when it's opened.
type overviewMsg struct {
	request uint64
	id      string
	o       projectOverview
}

func (m Model) applyOverviewMsg(x overviewMsg) Model {
	if x.request != m.request || m.screen != ScreenProjects {
		return m
	}
	m.loading = false
	if m.overviews == nil {
		m.overviews = map[string]projectOverview{}
	}
	m.overviews[x.id] = x.o
	return m
}

// ownProject reports whether p is the project this profile works in.
func (m Model) ownProject(p ProjectRow) bool {
	return m.cfg.Project != "" && (p.Slug == m.cfg.Project || p.ID == m.cfg.Project) || p.ID != "" && p.ID == m.project.ID
}

// ownSlug names the profile's project by its slug when it's listed.
func (m Model) ownSlug() string {
	for _, p := range m.projects {
		if m.ownProject(p) {
			return p.Slug
		}
	}
	return firstNonempty(m.project.Slug, m.cfg.Project)
}

// selectedProject is the project under the cursor on Projects.
func (m Model) selectedProject() (ProjectRow, bool) {
	if m.screen != ScreenProjects || m.selected < 0 || m.selected >= len(m.projects) {
		return ProjectRow{}, false
	}
	return m.projects[m.selected], true
}

func (o projectOverview) sessionsCell() string {
	if o.Err != "" {
		return "-"
	}
	return fmt.Sprintf("%d of %d active", o.Active, o.Active+o.Retired+o.Moved)
}

func (b boardSummary) cell() string {
	switch {
	case !b.Configured:
		return "not linked"
	case b.Unreadable:
		return "unreadable"
	case b.Mode == "team_board":
		return plural(len(b.Teams), "team", "teams") + " · team board"
	}
	return plural(len(b.Teams), "team", "teams") + " · " + plural(len(b.Projects), "project", "projects")
}

// boardNames lists bound teams or projects by their stored names; one
// with no stored name stays an ID.
func boardNames(names []boardName) string {
	out := []string{}
	for _, n := range names {
		s := firstNonempty(terminalText(n.Name), terminalText(n.ID))
		if n.Default {
			s += " (default)"
		}
		if n.Closes != "" {
			s += " (" + closesWord(n.Closes) + " close)"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// renderProjects is the workspace's projects with what each holds, and
// Enter's read-only detail (TUI-UX-AUDIT U11, U12, U13).
func renderProjects(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenProjects))
	b.WriteString("\n\n")
	if body, ok := bodyState(m, "projects", len(m.projects) > 0); ok {
		return b.String() + body
	}
	if len(m.projects) == 0 {
		b.WriteString("No projects in this workspace yet. An owner creates one with clawbridge project create SLUG.")
		return b.String()
	}
	if p, ok := m.selectedProject(); ok && m.projectOpen {
		return b.String() + renderProjectDetail(m, p)
	}
	rows := make([][]string, len(m.projects))
	for i, p := range m.projects {
		mark := "  "
		if m.ownProject(p) {
			mark = "● "
		}
		o := m.overviews[p.ID]
		coordinator := "-"
		if o.Err == "" {
			coordinator = firstNonempty(o.Coordinator, "none")
		}
		linear := "-"
		if o.Err == "" {
			linear = o.Linear.cell()
		}
		rows[i] = []string{mark + p.Slug, firstNonempty(p.Role, "-"), o.sessionsCell(), coordinator, linear}
	}
	b.WriteString(renderTable(m, []string{"PROJECT", "ROLE", "SESSIONS", "COORDINATOR", "LINEAR"}, []int{24, 8, 16, 18, 22}, rows))
	b.WriteString("\n\n" + mutedStyle.Render("● the project this profile works in · Enter opens a project"))
	return strings.TrimRight(b.String(), "\n")
}

// renderProjectDetail is one project, read-only: who coordinates it, its
// sessions and its Linear board. Writes stay with the profile's project.
func renderProjectDetail(m Model, p ProjectRow) string {
	var b strings.Builder
	width := tableBudget(m)
	line := func(label, value string) {
		b.WriteString(ansi.Hardwrap(fmt.Sprintf("%-12s %s", label, value), width, true) + "\n")
	}
	b.WriteString(accentStyle.Render(terminalText(p.Slug)+" · "+terminalText(p.Name)) + "\n")
	if m.ownProject(p) {
		b.WriteString(mutedStyle.Render("Your profile works in this project.") + "\n\n")
	} else {
		own := firstNonempty(terminalText(m.ownSlug()), "no project")
		b.WriteString(warnStyle.Render(ansi.Hardwrap("Viewing "+terminalText(p.Slug)+" (read-only); your profile works in "+own+".", width, true)) + "\n\n")
	}
	line("Your role", firstNonempty(terminalText(p.Role), "-"))
	if p.GitRepo != "" {
		line("Git remote", terminalText(p.GitRepo))
	}
	b.WriteString(mutedStyle.Render(ansi.Hardwrap(fmt.Sprintf("%-12s %s", "ID", terminalText(p.ID)), width, true)) + "\n")
	o, ok := m.overviews[p.ID]
	switch {
	case !ok:
		b.WriteString(loadingBody("the project") + "\n")
		return strings.TrimRight(b.String(), "\n")
	case o.Err != "":
		b.WriteString("Couldn't read this project: " + terminalText(o.Err) + "\n")
		return strings.TrimRight(b.String(), "\n")
	}
	coordinator := "none chosen"
	if o.Coordinator != "" {
		coordinator = terminalText(o.Coordinator) + " (" + firstNonempty(terminalText(o.CoordinatorStatus), "-") + ")"
	}
	line("Coordinator", coordinator)
	line("Sessions", firstNonempty(sessionCounts(o.Sessions), "none"))
	switch l := o.Linear; {
	case !l.Configured:
		line("Linear", "not linked")
	case l.Unreadable:
		line("Linear", "linked, but the binding couldn't be read")
	default:
		line("Linear", "teams "+firstNonempty(boardNames(l.Teams), "-"))
		if l.Mode == "team_board" {
			line("", "the team board")
		} else {
			line("", "projects "+firstNonempty(boardNames(l.Projects), "-"))
		}
		if t, err := time.Parse(time.RFC3339Nano, l.NamesAt); err == nil {
			b.WriteString(mutedStyle.Render(fmt.Sprintf("%-12s names as of %s", "", ago(m.cfg.Now(), t))) + "\n")
		}
	}
	if len(o.Sessions) > 0 {
		b.WriteString("\n")
		for i, s := range o.Sessions {
			if i == 20 {
				b.WriteString(mutedStyle.Render(fmt.Sprintf("  … and %d more", len(o.Sessions)-20)) + "\n")
				break
			}
			b.WriteString(fmt.Sprintf("  %-24s %-10s %s\n", clip(terminalText(sessionName(s)), 24), stateWord(s), firstNonempty(terminalText(s.Issue), terminalText(s.AgentKind), "-")))
		}
		if o.Truncated {
			b.WriteString(mutedStyle.Render("  More sessions than shown here; the project's own Sessions tab lists them all.") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func projectOpenOwn(m Model) bool {
	p, ok := m.selectedProject()
	return ok && m.projectOpen && m.ownProject(p)
}

var projectBindings = []Binding{
	{ID: "open", Keys: []string{"enter"}, Label: "open", When: func(m Model) bool { _, ok := m.selectedProject(); return ok && !m.projectOpen }, Action: func(m Model, _ string) (Model, tea.Cmd) {
		m.projectOpen = true
		p, _ := m.selectedProject()
		if _, ok := m.overviews[p.ID]; ok {
			return m, nil
		}
		// Past the first maxOverviews: read this one now.
		m.request++
		m.loading = true
		request := m.request
		return m, func() tea.Msg { return overviewMsg{request: request, id: p.ID, o: m.readOverview(p)} }
	}},
	// U13: from the profile's own project to its sessions and board.
	{ID: "its-sessions", Keys: []string{"s"}, Label: "its sessions", When: projectOpenOwn, Action: func(m Model, _ string) (Model, tea.Cmd) { return m.Show(ScreenSessions) }},
	{ID: "its-linear", Keys: []string{"l"}, Label: "its Linear board", When: func(m Model) bool { return projectOpenOwn(m) && m.hasTab(ScreenLinear) }, Action: func(m Model, _ string) (Model, tea.Cmd) { return m.Show(ScreenLinear) }},
}
