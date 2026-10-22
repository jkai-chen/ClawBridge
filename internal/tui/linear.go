package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	tea "github.com/charmbracelet/bubbletea"
)

const ScreenLinear Screen = "linear"
const Linear Screen = ScreenLinear

// LinearSource receives only explicit operator actions. Implementations do not
// read credentials or contact Linear while loading safe profile/binding metadata.
type LinearSource interface {
	Do(context.Context, LinearRequest) (LinearData, error)
}
type LinearRequest struct {
	Operation, ProfileID, Name string
	Access                     linearconn.Access
	Limit                      int
	Cursor                     string
	IssueID                    string
	Link                       linearboard.LinkInput
	ExpectedRevision           int64
	Confirm                    bool
}
type LinearData struct {
	Profiles []linearconn.Profile
	Binding  bus.TaskboardBinding
	Teams    linearprovider.Page[linearprovider.Team]
	Projects linearprovider.Page[linearprovider.Project]
	Issues   linearprovider.Page[linearprovider.Issue]
	Issue    *linearprovider.Issue
	Preview  any
}
type linearState struct {
	step                 string
	profile              string
	profiles             []linearconn.Profile
	binding              bus.TaskboardBinding
	teams                []linearprovider.Team
	projects             []linearprovider.Project
	issues               []linearprovider.Issue
	issue                *linearprovider.Issue
	chosen               []string
	defaultTeam, project string
	teamBoard            bool
	cursor, next         string
	visited              []string
	page                 int
	connectAccess        linearconn.Access
	preview              string
}
type LinearSnapshot struct {
	Step, ProfileID          string
	Binding                  bus.TaskboardBinding
	SelectedTeamIDs          []string
	DefaultTeamID, ProjectID string
	TeamBoard                bool
	NextCursor               string
	Issue                    *linearprovider.Issue
}

func (m Model) LinearState() LinearSnapshot {
	return LinearSnapshot{m.linear.step, m.linear.profile, m.linear.binding, append([]string(nil), m.linear.chosen...), m.linear.defaultTeam, m.linear.project, m.linear.teamBoard, m.linear.next, m.linear.issue}
}

type linearLoadedMsg struct {
	request   uint64
	operation string
	data      LinearData
	err       error
}

func (m Model) linearCommand(request LinearRequest) (Model, tea.Cmd) {
	if m.cfg.Linear == nil {
		m.request++
		id := m.request
		return m, func() tea.Msg {
			return linearLoadedMsg{request: id, operation: request.Operation, err: errors.New("Native Linear integration unavailable; use clawbridge linear --help")}
		}
	}
	if m.linearCancel != nil {
		m.linearCancel()
	}
	m.request++
	m.loading = true
	m.actionErr = ""
	m.errText = ""
	timeout := 45 * time.Second
	if request.Operation == "connect" {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(m.cfg.Context, timeout)
	m.linearCancel = cancel
	requestID := m.request
	source := m.cfg.Linear
	return m, func() tea.Msg {
		defer cancel()
		data, err := source.Do(ctx, request)
		return linearLoadedMsg{requestID, request.Operation, data, err}
	}
}
func (m Model) linearRefresh() (Model, tea.Cmd) {
	if m.linear.step == "" {
		m.linear.step = "profiles"
	}
	request := LinearRequest{ProfileID: m.linear.profile, Limit: 50, Cursor: m.linear.cursor}
	switch m.linear.step {
	case "teams":
		request.Operation = "teams"
	case "projects":
		request.Operation = "projects"
	case "tasks":
		request.Operation = "issues"
	case "details":
		request.Operation = "issue"
		if m.linear.issue != nil {
			request.IssueID = m.linear.issue.ID
		}
	default:
		request.Operation = "profiles"
	}
	return m.linearCommand(request)
}
func (m Model) applyLinear(x linearLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenLinear {
		return m
	}
	m.loading = false
	m.linearCancel = nil
	if x.err != nil {
		m.actionErr = displayError(x.err)
		return m
	}
	m.source = "linear"
	switch x.operation {
	case "profiles", "connect", "logout":
		m.linear.profiles = append([]linearconn.Profile(nil), x.data.Profiles...)
		m.linear.binding = x.data.Binding
		m.linear.step = "profiles"
		m.selected = 0
		if x.operation == "connect" {
			m.actionErr = "Personal connection saved. Select a profile to continue."
		}
		if x.operation == "logout" {
			m.actionErr = "Personal profile logged out. Shared project binding preserved."
		}
	case "teams":
		m.linear.teams = append([]linearprovider.Team(nil), x.data.Teams.Items...)
		m.linear.next = x.data.Teams.NextCursor
		m.selected = 0
	case "projects":
		m.linear.projects = nil
		for _, p := range x.data.Projects.Items {
			ok := true
			for _, id := range m.linear.chosen {
				if !linearHas(p.TeamIDs, id) {
					ok = false
				}
			}
			if ok {
				m.linear.projects = append(m.linear.projects, p)
			}
		}
		m.linear.next = x.data.Projects.NextCursor
		m.selected = 0
	case "preview":
		m.linear.preview = fmt.Sprint(x.data.Preview)
		m.linear.step = "preview"
		// Keep the revision submitted for this preview. A later metadata read
		// must never promote the confirmation's compare-and-swap precondition.
	case "link":
		m.linear.binding = x.data.Binding
		m.linear.step = "tasks"
		m.linear.cursor = ""
		m.linear.visited = []string{""}
		m.linear.page = 0
		m.actionErr = "Shared board saved. Press r to read tasks."
	case "unlink-preview":
		m.linear.preview = fmt.Sprint(x.data.Preview)
		m.linear.step = "unlink"
	case "unlink":
		m.linear.binding = x.data.Binding
		m.linear.step = "profiles"
		m.actionErr = "Project unlinked. Personal connections preserved."
	case "issues":
		m.linear.issues = append([]linearprovider.Issue(nil), x.data.Issues.Items...)
		m.linear.next = x.data.Issues.NextCursor
		m.selected = 0
	case "issue":
		m.linear.issue = x.data.Issue
		m.linear.step = "details"
		m.scroll = 0
	}
	return m
}
func linearHas(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func (m Model) linearRows() []string {
	rows := []string{}
	switch m.linear.step {
	case "profiles":
		for _, p := range m.linear.profiles {
			rows = append(rows, p.ID)
		}
	case "teams":
		for _, t := range m.linear.teams {
			rows = append(rows, t.ID)
		}
	case "default":
		rows = append(rows, m.linear.chosen...)
	case "projects":
		rows = append(rows, "team-board")
		for _, p := range m.linear.projects {
			rows = append(rows, p.ID)
		}
	case "tasks":
		for _, i := range m.linear.issues {
			rows = append(rows, i.ID)
		}
	}
	return rows
}
func (m Model) linearSelected() string {
	rows := m.linearRows()
	if m.selected < 0 || m.selected >= len(rows) {
		return ""
	}
	return rows[m.selected]
}
func (m Model) linearPage(next bool) (Model, tea.Cmd) {
	if m.linear.step != "teams" && m.linear.step != "projects" && m.linear.step != "tasks" {
		return m, nil
	}
	if next {
		if m.linear.next == "" {
			m.actionErr = "No next page; this is the current response, not an all-workspace search."
			return m, nil
		}
		m.linear.cursor = m.linear.next
		m.linear.page++
		m.linear.visited = append(append([]string(nil), m.linear.visited[:min(len(m.linear.visited), m.linear.page)]...), m.linear.cursor)
	} else {
		if m.linear.page < 1 {
			return m, nil
		}
		m.linear.page--
		m.linear.cursor = m.linear.visited[m.linear.page]
	}
	return m.linearRefresh()
}
func (m Model) linearLink(confirm bool) (Model, tea.Cmd) {
	return m.linearCommand(LinearRequest{Operation: func() string {
		if confirm {
			return "link"
		}
		return "preview"
	}(), ProfileID: m.linear.profile, Link: linearboard.LinkInput{TeamIDs: append([]string(nil), m.linear.chosen...), DefaultTeamID: m.linear.defaultTeam, ProjectID: m.linear.project, ExpectedRevision: m.linear.binding.Revision, TeamBoard: m.linear.teamBoard, Confirm: confirm}})
}
func (m Model) linearKey(key string) (Model, tea.Cmd) {
	if key == "esc" {
		if m.linearCancel != nil {
			m.linearCancel()
			m.linearCancel = nil
		}
		m.request++
		m.loading = false
		m.inputMode = inputNone
		m.draft = ""
		m.linear.step = "profiles"
		m.selected = 0
		return m, nil
	}
	if key == "c" || key == "w" {
		m.inputMode = inputLinearName
		m.draft = ""
		m.linear.connectAccess = linearconn.Read
		if key == "w" {
			m.linear.connectAccess = linearconn.Write
		}
		return m, nil
	}
	if key == "l" {
		if m.linear.profile == "" {
			m.actionErr = "Select a personal connection first."
			return m, nil
		}
		m.linear.step = "teams"
		m.linear.cursor = ""
		m.linear.visited = []string{""}
		m.linear.page = 0
		m.linear.chosen = nil
		return m.linearRefresh()
	}
	if key == "x" && m.linear.step == "profiles" {
		if id := m.linearSelected(); id != "" {
			return m.linearCommand(LinearRequest{Operation: "logout", ProfileID: id})
		}
	}
	if key == "u" && m.linear.binding.Configured {
		return m.linearCommand(LinearRequest{Operation: "unlink-preview", ExpectedRevision: m.linear.binding.Revision})
	}
	if key == " " && m.linear.step == "teams" {
		id := m.linearSelected()
		if id == "" {
			return m, nil
		}
		ids := append([]string(nil), m.linear.chosen...)
		if linearHas(ids, id) {
			next := []string{}
			for _, v := range ids {
				if v != id {
					next = append(next, v)
				}
			}
			ids = next
		} else {
			if len(ids) >= 20 {
				m.actionErr = "Select at most20 teams."
				return m, nil
			}
			ids = append(ids, id)
		}
		m.linear.chosen = ids
		return m, nil
	}
	if key != "enter" {
		return m, nil
	}
	switch m.linear.step {
	case "profiles":
		id := m.linearSelected()
		if id == "" {
			m.actionErr = "Connect Linear with c, or use Later by choosing another view."
			return m, nil
		}
		m.linear.profile = id
		m.linear.cursor = ""
		m.linear.page = 0
		m.linear.visited = []string{""}
		if m.linear.binding.Configured {
			m.linear.step = "tasks"
		} else {
			m.linear.step = "teams"
		}
		return m.linearRefresh()
	case "teams":
		if len(m.linear.chosen) == 0 {
			m.actionErr = "Space selects allowed teams; Enter continues after selection."
			return m, nil
		}
		m.linear.step = "default"
		m.selected = 0
		return m, nil
	case "default":
		m.linear.defaultTeam = m.linearSelected()
		if m.linear.defaultTeam == "" {
			return m, nil
		}
		m.linear.step = "projects"
		m.linear.cursor = ""
		m.linear.visited = []string{""}
		m.linear.page = 0
		return m.linearRefresh()
	case "projects":
		id := m.linearSelected()
		if id == "" {
			return m, nil
		}
		m.linear.teamBoard = id == "team-board"
		m.linear.project = ""
		if !m.linear.teamBoard {
			m.linear.project = id
		}
		return m.linearLink(false)
	case "preview":
		return m.linearLink(true)
	case "unlink":
		return m.linearCommand(LinearRequest{Operation: "unlink", ExpectedRevision: m.linear.binding.Revision, Confirm: true})
	case "tasks":
		id := m.linearSelected()
		if id != "" {
			return m.linearCommand(LinearRequest{Operation: "issue", ProfileID: m.linear.profile, IssueID: id})
		}
	case "details":
		m.linear.step = "tasks"
		return m, nil
	}
	return m, nil
}
func renderLinear(m Model) string {
	var b strings.Builder
	b.WriteString("Settings / Integrations / Linear\n\n")
	b.WriteString("Optional native connection. c Connect read · w Connect write · Later: choose another view.\n")
	b.WriteString("Task ownership comes from the coordinator's issue assignment; selecting a row never claims or completes work.\n")
	b.WriteString(fmt.Sprintf("Shared board: configured=%t · revision=%d\n", m.linear.binding.Configured, m.linear.binding.Revision))
	if m.linear.binding.Configured {
		b.WriteString("Allowed teams: " + strings.Join(m.linear.binding.LinearTeamIDs, ", ") + "\nDefault team: " + m.linear.binding.LinearDefaultTeamID + "\n")
		project := m.linear.binding.LinearProjectID
		if project == "" {
			project = "Team board — no Linear project"
		}
		b.WriteString("Board: " + project + "\n")
	}
	b.WriteString("Personal profile: " + terminalText(m.linear.profile) + " · workspace UUID may be unavailable\n")
	b.WriteString("Step: " + terminalText(m.linear.step) + " · ↑↓ choose · Enter continue · Esc profiles · l relink · u unlink · [ ] pages\n\n")
	if m.linear.step == "preview" || m.linear.step == "unlink" {
		b.WriteString("REVIEW BEFORE SAVE. Enter confirms; Esc cancels.\n" + terminalText(m.linear.preview) + "\nAllowed team IDs: " + strings.Join(m.linear.chosen, ", ") + "\nDefault: " + m.linear.defaultTeam + "\nProject: " + m.linear.project + fmt.Sprintf(" · team board=%t\n", m.linear.teamBoard))
		return b.String()
	}
	rows := m.linearRows()
	if len(rows) == 0 && m.linear.step != "details" {
		b.WriteString("No rows on this page. Empty results do not prove absence from other pages.\n")
	}
	for index, id := range rows {
		prefix := "  "
		if index == m.selected {
			prefix = "› "
		}
		label := id
		switch m.linear.step {
		case "profiles":
			p := m.linear.profiles[index]
			label = fmt.Sprintf("%s · %s · %s", p.Name, p.ID, p.Access)
		case "teams":
			t := m.linear.teams[index]
			check := "[ ]"
			if linearHas(m.linear.chosen, t.ID) {
				check = "[x]"
			}
			label = fmt.Sprintf("%s %s (%s) · %s", check, t.Name, t.Key, t.ID)
		case "projects":
			if index == 0 {
				label = "Team board — no Linear project (explicit choice)"
			} else {
				p := m.linear.projects[index-1]
				label = p.Name + " · " + p.ID
			}
		case "tasks":
			i := m.linear.issues[index]
			label = fmt.Sprintf("%s · %s · %s · %s", i.Identifier, i.Title, i.State, i.Assignee)
		}
		b.WriteString(prefix + terminalText(label) + "\n")
	}
	if m.linear.step == "teams" {
		b.WriteString("\nSpace toggles a team; Enter chooses default. Selected: " + strings.Join(m.linear.chosen, ", ") + "\n")
	}
	if m.linear.step == "profiles" {
		b.WriteString("\nx logs out only the selected personal connection; shared binding is retained.\n")
	}
	if m.linear.step == "details" && m.linear.issue != nil {
		i := m.linear.issue
		b.WriteString(fmt.Sprintf("Issue %s · %s\nID: %s\nTeam: %s · Project: %s\nState: %s · Assignee: %s\n\n%s\n\nParent / subissues / dependencies / assignment context:\n%s\nContext complete: %t\n", terminalText(i.Identifier), terminalText(i.Title), i.ID, i.TeamID, i.ProjectID, terminalText(i.State), terminalText(i.Assignee), terminalText(i.Description), terminalText(fmt.Sprint(i.Context)), i.ContextComplete))
	}
	if m.linear.next != "" {
		b.WriteString("\nMore provider rows available: ] next page.\n")
	}
	if m.inputMode == inputLinearName {
		b.WriteString("\nNew connection label (" + string(m.linear.connectAccess) + "): " + terminalText(m.draft) + "\nEnter starts explicit OAuth consent; Esc cancels.\n")
	}
	return b.String()
}
