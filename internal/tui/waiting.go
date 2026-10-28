package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"clawbridge/internal/bus"
)

// Waiting for a project: a profile that joined a workspace but has no
// project yet (an invite with no grants) opens here instead of failing. It
// says who to ask, checks again on r and every waitingPoll, and takes the
// project once one is shared: the only one at once, or the one picked.

// waitingPoll is how often the waiting screen checks again; tests shorten
// it.
var waitingPoll = 30 * time.Second

type waitingTickMsg struct{ request uint64 }

type waitingLoadedMsg struct {
	request  uint64
	projects []ProjectRow
	owner    string
	me       string
	name     string
	err      error
}

func (m Model) waitingTick() tea.Cmd {
	request := m.request
	return tea.Tick(waitingPoll, func(time.Time) tea.Msg { return waitingTickMsg{request: request} })
}

// loadWaiting asks which projects this person has, and who owns the
// workspace.
func (m Model) loadWaiting() (Model, tea.Cmd) {
	m.loading = true
	requestID := m.request
	return m, func() tea.Msg {
		out := waitingLoadedMsg{request: requestID}
		r, err := m.cfg.Executor.Execute(m.cfg.Context, bus.Request{Operation: "list_projects", Workspace: m.cfg.Workspace, Args: map[string]any{}})
		if err != nil {
			out.err = err
			return out
		}
		out.projects = projectRows(records(mapData(r.Data)["projects"]))
		if who, e := m.cfg.Executor.Execute(m.cfg.Context, bus.Request{Operation: "whoami", Workspace: m.cfg.Workspace, Args: map[string]any{}}); e == nil {
			d := mapData(who.Data)
			w := mapData(d["workspace"])
			out.me, out.owner, out.name = text(d, "principal"), text(w, "owner_principal"), text(w, "name")
		}
		return out
	}
}

func (m Model) applyWaiting(x waitingLoadedMsg) (Model, tea.Cmd) {
	if x.request != m.request || !m.waiting {
		return m, nil
	}
	m.loading = false
	m.loadErr = ""
	if x.err != nil {
		m.loadErr = x.err.Error()
		return m, m.waitingTick()
	}
	m.waitingChecked = true
	if x.owner != "" {
		m.waitingOwner = x.owner
	}
	if x.me != "" {
		m.identity.Principal = x.me
	}
	if x.name != "" {
		m.workspace.Name = x.name
	}
	m.projects = x.projects
	m.selected = min(m.selected, max(0, len(m.projects)-1))
	if len(m.projects) == 1 {
		return m.takeProject(m.projects[0].Slug)
	}
	return m, m.waitingTick()
}

// takeProject hands the project to the profile: through ProjectFound,
// which saves it and reopens the TUI on it, or, with none, here.
func (m Model) takeProject(slug string) (Model, tea.Cmd) {
	if m.cfg.ProjectFound != nil {
		if err := m.cfg.ProjectFound(slug); err != nil {
			m.status = failStatus("found project " + slug + " but couldn't save it: " + err.Error())
			return m, nil
		}
		m.status = okStatus("Project " + slug + " is yours now; opening it…")
		return m, tea.Quit
	}
	m.waiting, m.cfg.Project = false, slug
	m.project = ProjectView{Slug: slug}
	return m.Show(ScreenOverview)
}

func renderWaiting(m Model) string {
	var b strings.Builder
	b.WriteString("Waiting for a project\n\n")
	workspace := firstNonempty(terminalText(m.workspace.Name), terminalText(m.cfg.Workspace), "this workspace")
	if len(m.projects) > 1 {
		b.WriteString("You have projects in " + workspace + ". Pick the one this profile works in:\n\n")
		for i, p := range m.projects {
			prefix := "  "
			if i == m.selected {
				prefix = accentStyle.Render("› ")
			}
			b.WriteString(prefix + terminalText(p.Slug))
			if name := terminalText(p.Name); name != "" && name != p.Slug {
				b.WriteString(mutedStyle.Render("  " + name))
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	if !m.waitingChecked {
		// Before the first answer, neither create nor ask: it depends on
		// whose workspace it is.
		b.WriteString(loadingBody("your projects and role in " + workspace))
		return b.String()
	}
	me := firstNonempty(terminalText(m.identity.Principal), terminalText(m.cfg.Principal), "YOUR_EMAIL")
	if m.waitingOwner != "" && m.waitingOwner == m.identity.Principal {
		// The owner after a bootstrap join: nobody to ask.
		b.WriteString(workspace + " has no project yet. Create the first one:\n")
		b.WriteString("  clawbridge project create SLUG\n\n")
	} else {
		b.WriteString("You've joined " + workspace + ", but no project has been shared with you yet.\n\n")
		owner := firstNonempty(terminalText(m.waitingOwner), "the workspace owner")
		b.WriteString("Ask " + owner + " to grant you one:\n")
		b.WriteString("  clawbridge --project PROJECT project role set " + me + " member\n\n")
	}
	b.WriteString(mutedStyle.Render("This screen checks again every " + waitingPoll.String() + " (" + keyHint(m, ScreenOverview, "reload") + " now) and opens the project once it's yours."))
	return b.String()
}
