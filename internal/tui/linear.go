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
	LinkV2                     *linearboard.LinkInputV2
	ExpectedRevision           int64
	Confirm                    bool
}
type LinearData struct {
	Profiles  []linearconn.Profile
	Binding   bus.TaskboardBinding
	BindingV2 *bus.TaskboardBindingV2
	IssuesV2  *linearprovider.Page[linearprovider.IssueV2]
	IssueV2   *linearprovider.IssueV2
	Teams     linearprovider.Page[linearprovider.Team]
	Projects  linearprovider.Page[linearprovider.Project]
	Issues    linearprovider.Page[linearprovider.Issue]
	Issue     *linearprovider.Issue
	Preview   any
	// Closes is who closes each project of a link preview (A3), and
	// MinimalPolicy what creating the minimal status policy changes.
	Closes        []linearboard.ClosesLine
	MinimalPolicy string
	// ClosesOutcomes is what a confirmed link did about who closes.
	ClosesOutcomes []linearboard.ClosesOutcome
}
type linearState struct {
	step                 string
	profile              string
	profiles             []linearconn.Profile
	binding              bus.TaskboardBinding
	bindingV2            *bus.TaskboardBindingV2
	issueV2              *linearprovider.IssueV2
	selectedProjects     []string
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
	// A3: who closes. closes is this link's answers by project ID; lines
	// the preview's per-project rule; minimal the minimal policy's
	// consequences, when the link would create it; typing the projects
	// whose names are still to be typed before agents may close them.
	closes  map[string]string
	lines   []linearboard.ClosesLine
	minimal string
	typing  []linearboard.ClosesLine
}
type LinearSnapshot struct {
	Step, ProfileID          string
	Binding                  bus.TaskboardBinding
	BindingV2                *bus.TaskboardBindingV2
	SelectedProjectIDs       []string
	IssueV2                  *linearprovider.IssueV2
	SelectedTeamIDs          []string
	DefaultTeamID, ProjectID string
	TeamBoard                bool
	NextCursor               string
	Issue                    *linearprovider.Issue
}

func (m Model) LinearState() LinearSnapshot {
	return LinearSnapshot{Step: m.linear.step, ProfileID: m.linear.profile, Binding: m.linear.binding, BindingV2: m.linear.bindingV2, SelectedProjectIDs: append([]string{}, m.linear.selectedProjects...), IssueV2: m.linear.issueV2, SelectedTeamIDs: append([]string(nil), m.linear.chosen...), DefaultTeamID: m.linear.defaultTeam, ProjectID: m.linear.project, TeamBoard: m.linear.teamBoard, NextCursor: m.linear.next, Issue: m.linear.issue}
}

func (m Model) linearConfigured() bool {
	if m.linear.bindingV2 != nil {
		return m.linear.bindingV2.Configured
	}
	return m.linear.binding.Configured
}
func (m Model) linearRevision() int64 {
	if m.linear.bindingV2 != nil {
		return m.linear.bindingV2.Revision
	}
	return m.linear.binding.Revision
}
func (m *Model) applyLinearBinding(data LinearData) {
	if data.BindingV2 != nil {
		value := *data.BindingV2
		value.LinearTeamIDs = append([]string{}, value.LinearTeamIDs...)
		value.LinearProjectIDs = append([]string{}, value.LinearProjectIDs...)
		m.linear.bindingV2 = &value
		m.linear.binding = bus.TaskboardBinding{} // no fabricated legacy team board
	} else {
		m.linear.binding = data.Binding
		m.linear.bindingV2 = nil
	}
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
		m.applyLinearBinding(x.data)
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
			ok := m.linear.bindingV2 == nil
			for _, id := range m.linear.chosen {
				if m.linear.bindingV2 != nil {
					ok = ok || linearHas(p.TeamIDs, id)
				} else if !linearHas(p.TeamIDs, id) {
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
		m.linear.lines = append([]linearboard.ClosesLine(nil), x.data.Closes...)
		m.linear.minimal = x.data.MinimalPolicy
		// The review opens on its first project; a re-preview after c
		// keeps the highlighted one.
		if m.linear.step != "preview" {
			m.selected = 0
		}
		m.selected = min(m.selected, max(0, len(m.linear.lines)-1))
		m.linear.step = "preview"
		// Keep the revision submitted for this preview. A later metadata read
		// must never promote the confirmation's compare-and-swap precondition.
	case "link":
		m.applyLinearBinding(x.data)
		m.linear.step = "tasks"
		m.linear.cursor = ""
		m.linear.visited = []string{""}
		m.linear.page = 0
		m.actionErr = "Shared board saved. Press r to read tasks."
		for _, o := range x.data.ClosesOutcomes {
			if o.Outcome == "set" {
				m.actionErr += " " + o.LinearProject + ": closed by " + closesWord(o.Closes) + "."
			} else {
				m.actionErr += " " + o.LinearProject + ": who closes was not set (" + o.Error + "); re-run " + o.Rerun
			}
		}
		m.linear.closes, m.linear.lines, m.linear.minimal = nil, nil, ""
	case "unlink-preview":
		m.linear.preview = fmt.Sprint(x.data.Preview)
		m.linear.step = "unlink"
	case "unlink":
		m.applyLinearBinding(x.data)
		m.linear.step = "profiles"
		m.actionErr = "Project unlinked. Personal connections preserved."
	case "issues":
		m.linear.issues = append([]linearprovider.Issue(nil), x.data.Issues.Items...)
		m.linear.next = x.data.Issues.NextCursor
		if x.data.IssuesV2 != nil {
			m.linear.issues = nil
			for _, issue := range x.data.IssuesV2.Items {
				m.linear.issues = append(m.linear.issues, issue.Issue)
			}
			m.linear.next = x.data.IssuesV2.NextCursor
		}
		m.selected = 0
	case "issue":
		m.linear.issue = x.data.Issue
		m.linear.issueV2 = x.data.IssueV2
		if x.data.IssueV2 != nil {
			value := x.data.IssueV2.Issue
			m.linear.issue = &value
		}
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
	case "preview":
		for _, l := range m.linear.lines {
			rows = append(rows, l.LinearProject)
		}
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
	operation := "preview"
	if confirm {
		operation = "link"
	}
	request := LinearRequest{Operation: operation, ProfileID: m.linear.profile, Link: linearboard.LinkInput{TeamIDs: append([]string{}, m.linear.chosen...), DefaultTeamID: m.linear.defaultTeam, ProjectID: m.linear.project, ExpectedRevision: m.linearRevision(), TeamBoard: m.linear.teamBoard, Confirm: confirm}}
	if m.linear.bindingV2 != nil {
		request.LinkV2 = &linearboard.LinkInputV2{TeamIDs: append([]string{}, m.linear.chosen...), DefaultTeamID: m.linear.defaultTeam, ProjectIDs: append([]string{}, m.linear.selectedProjects...), ExpectedRevision: m.linearRevision(), TeamBoard: m.linear.teamBoard, Confirm: confirm}
		if len(m.linear.closes) > 0 && !m.linear.teamBoard {
			request.LinkV2.Closes = map[string]string{}
			for id, who := range m.linear.closes {
				request.LinkV2.Closes[id] = who
			}
			// Only linearClosesTyped confirms with agents in it: the
			// person typed each such project's name here.
			request.LinkV2.Via = linearboard.ClosesViaTUI
			request.LinkV2.MinimalPolicy = confirm && m.linear.minimal != ""
		}
	}
	return m.linearCommand(request)
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
	if key == "c" && m.linear.step == "preview" {
		return m.linearToggleCloses()
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
		m.linear.selectedProjects = nil
		m.linear.project = ""
		m.linear.teamBoard = false
		m.linear.closes, m.linear.lines, m.linear.minimal, m.linear.typing = nil, nil, "", nil
		return m.linearRefresh()
	}
	if key == "x" && m.linear.step == "profiles" {
		if id := m.linearSelected(); id != "" {
			return m.linearCommand(LinearRequest{Operation: "logout", ProfileID: id})
		}
	}
	if key == "u" && m.linearConfigured() {
		return m.linearCommand(LinearRequest{Operation: "unlink-preview", ExpectedRevision: m.linearRevision()})
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
				m.actionErr = "Select at most 20 teams."
				return m, nil
			}
			ids = append(ids, id)
		}
		m.linear.chosen = ids
		return m, nil
	}
	if key == " " && m.linear.step == "projects" && m.linear.bindingV2 != nil {
		id := m.linearSelected()
		if id == "" {
			return m, nil
		}
		if id == "team-board" {
			m.linear.teamBoard = true
			m.linear.selectedProjects = nil
			m.linear.project = ""
			return m, nil
		}
		m.linear.teamBoard = false
		if linearHas(m.linear.selectedProjects, id) {
			ids := []string{}
			for _, value := range m.linear.selectedProjects {
				if value != id {
					ids = append(ids, value)
				}
			}
			m.linear.selectedProjects = ids
		} else {
			if len(m.linear.selectedProjects) >= 50 {
				m.actionErr = "Select at most 50 projects."
				return m, nil
			}
			m.linear.selectedProjects = append(append([]string{}, m.linear.selectedProjects...), id)
		}
		m.linear.project = ""
		if len(m.linear.selectedProjects) == 1 {
			m.linear.project = m.linear.selectedProjects[0]
		}
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
		if m.linearConfigured() {
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
		if m.linear.bindingV2 != nil {
			if len(m.linear.selectedProjects) == 0 && !m.linear.teamBoard {
				if id == "team-board" {
					m.linear.teamBoard = true
				} else {
					m.linear.selectedProjects = []string{id}
				}
			}
			m.linear.project = ""
			if len(m.linear.selectedProjects) == 1 {
				m.linear.project = m.linear.selectedProjects[0]
			}
		} else {
			m.linear.teamBoard = id == "team-board"
			m.linear.project = ""
			if !m.linear.teamBoard {
				m.linear.project = id
			}
		}
		return m.linearLink(false)
	case "preview":
		// Agents close a project's issues only once its name is typed.
		// The names come from what the link will send, checked against a
		// preview of exactly that (R114-M1).
		if m.loading {
			m.actionErr = "wait for the preview to finish, then press Enter"
			return m, nil
		}
		typing, err := m.linearClosesToType()
		if err != nil {
			m.actionErr = err.Error()
			return m, nil
		}
		m.linear.typing = typing
		if len(m.linear.typing) > 0 {
			m.inputMode, m.draft, m.actionErr = inputLinearClosesName, "", ""
			return m, nil
		}
		return m.linearLink(true)
	case "unlink":
		return m.linearCommand(LinearRequest{Operation: "unlink", ExpectedRevision: m.linearRevision(), Confirm: true})
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

// closesWord names a closes value as people read it.
func closesWord(who string) string {
	if who == linearboard.ClosesAgent {
		return "agents"
	}
	return "people"
}

// linearToggleCloses switches the highlighted project of a link preview
// between people and agents, and previews again so the line, and the
// minimal policy's consequences, come from the server.
func (m Model) linearToggleCloses() (Model, tea.Cmd) {
	if m.loading {
		m.actionErr = "wait for the preview to finish"
		return m, nil
	}
	if m.linear.bindingV2 == nil || m.linear.teamBoard || m.selected < 0 || m.selected >= len(m.linear.lines) {
		return m, nil
	}
	l := m.linear.lines[m.selected]
	now := l.Set
	if now == "" {
		now = linearboard.ClosesHuman
		if l.Closes == "agents" {
			now = linearboard.ClosesAgent
		}
	}
	next := linearboard.ClosesAgent
	if now == linearboard.ClosesAgent {
		next = linearboard.ClosesHuman
	}
	closes := map[string]string{}
	for id, who := range m.linear.closes {
		closes[id] = who
	}
	closes[l.LinearProject] = next
	m.linear.closes = closes
	return m.linearLink(false)
}

// linearClosesToType is every project the link would let agents close
// that they don't already: each needs its name typed. An answer the shown
// preview doesn't reflect means the preview is stale, and nothing goes.
func (m Model) linearClosesToType() ([]linearboard.ClosesLine, error) {
	lines := map[string]linearboard.ClosesLine{}
	for _, l := range m.linear.lines {
		lines[l.LinearProject] = l
	}
	var out []linearboard.ClosesLine
	for _, l := range m.linear.lines {
		want := m.linear.closes[l.LinearProject]
		switch {
		case want == linearboard.ClosesAgent && l.Set == linearboard.ClosesAgent:
			out = append(out, l)
		case want == linearboard.ClosesAgent && l.Closes == "agents":
		case want == linearboard.ClosesAgent || l.Set != "" && l.Set != want:
			return nil, errors.New("the preview is out of date; press c again or reopen the link review. Nothing was changed")
		}
	}
	for id, who := range m.linear.closes {
		if _, shown := lines[id]; !shown && who == linearboard.ClosesAgent {
			return nil, errors.New("the preview is out of date; reopen the link review. Nothing was changed")
		}
	}
	return out, nil
}

// linearClosesTyped checks the name typed for the next project agents
// would close; once every name matches, the link is confirmed.
func (m Model) linearClosesTyped() (Model, tea.Cmd) {
	if len(m.linear.typing) == 0 {
		m.inputMode = inputNone
		return m, nil
	}
	want := m.linear.typing[0].Name
	if want == "" || m.draft != want {
		m.inputMode, m.draft, m.linear.typing = inputNone, "", nil
		m.actionErr = fmt.Sprintf("the name typed is not %q; nothing was changed", want)
		return m, nil
	}
	m.linear.typing, m.draft = m.linear.typing[1:], ""
	if len(m.linear.typing) > 0 {
		return m, nil
	}
	m.inputMode = inputNone
	return m.linearLink(true)
}

func renderLinear(m Model) string {
	var b strings.Builder
	b.WriteString("Settings / Integrations / Linear\n\n")
	b.WriteString("Optional native connection. c Connect read · w Connect write · Later: choose another view.\n")
	b.WriteString("Task ownership comes from the coordinator's issue assignment; selecting a row never claims or completes work.\n")
	b.WriteString(fmt.Sprintf("Shared board: configured=%t · revision=%d\n", m.linearConfigured(), m.linearRevision()))
	if m.linearConfigured() {
		if v := m.linear.bindingV2; v != nil {
			b.WriteString("Allowed teams: " + strings.Join(v.LinearTeamIDs, ", ") + "\nDefault team: " + v.LinearDefaultTeamID + "\n")
			if v.Mode == "team_board" {
				b.WriteString("Board: Team board — no Linear project\n")
			} else {
				b.WriteString("Project set: " + strings.Join(v.LinearProjectIDs, ", ") + "\n")
			}
		} else {
			b.WriteString("Allowed teams: " + strings.Join(m.linear.binding.LinearTeamIDs, ", ") + "\nDefault team: " + m.linear.binding.LinearDefaultTeamID + "\n")
			project := m.linear.binding.LinearProjectID
			if project == "" {
				project = "Team board — no Linear project"
			}
			b.WriteString("Board: " + project + "\n")
		}
	}
	b.WriteString("Personal profile: " + terminalText(m.linear.profile) + " · workspace UUID may be unavailable\n")
	b.WriteString("Step: " + terminalText(m.linear.step) + " · ↑↓ choose · Enter continue · Esc profiles · l relink · u unlink · [ ] pages\n\n")
	if m.linear.step == "preview" || m.linear.step == "unlink" {
		projects := m.linear.project
		if m.linear.bindingV2 != nil {
			projects = strings.Join(m.linear.selectedProjects, ", ")
		}
		b.WriteString("REVIEW BEFORE SAVE. Enter confirms; Esc cancels.\n")
		if m.linear.step == "preview" && len(m.linear.lines) > 0 {
			b.WriteString("\nWho closes each project's issues? Enter keeps people; c switches the highlighted one between people and agents.\n")
			for i, l := range m.linear.lines {
				prefix := "  "
				if i == m.selected {
					prefix = "› "
				}
				line := firstNonempty(l.Name, l.LinearProject) + " · " + l.LinearProject + " · closes: " + l.Closes
				if l.Set != "" {
					line += " → " + closesWord(l.Set) + " (this link sets it)"
				}
				b.WriteString(prefix + terminalText(line) + "\n")
			}
			if m.linear.minimal != "" {
				b.WriteString(warnStyle.Render(terminalText(m.linear.minimal)) + "\nTyping the project's name to let agents close also creates that policy.\n")
			}
			b.WriteString("\n")
		}
		b.WriteString(terminalText(m.linear.preview) + "\nAllowed team IDs: " + strings.Join(m.linear.chosen, ", ") + "\nDefault: " + m.linear.defaultTeam + "\nProject selection: " + projects + fmt.Sprintf(" · team board=%t\n", m.linear.teamBoard))
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
				if m.linear.bindingV2 != nil {
					check := "[ ] "
					if linearHas(m.linear.selectedProjects, p.ID) {
						check = "[x] "
					}
					label = check + label
				}
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
	if m.linear.step == "projects" && m.linear.bindingV2 != nil {
		b.WriteString("\nSpace toggles up to 50 projects, or explicitly selects Team board. Enter previews. Selected project IDs: " + strings.Join(m.linear.selectedProjects, ", ") + "\n")
	}
	if m.linear.step == "profiles" {
		b.WriteString("\nx logs out only the selected personal connection; shared binding is retained.\n")
	}
	if m.linear.step == "details" && m.linear.issue != nil {
		i := m.linear.issue
		b.WriteString(fmt.Sprintf("Issue %s · %s\nID: %s\nTeam: %s · Project: %s\nState: %s · Assignee: %s\n\n%s\n\nParent / subissues / dependencies / assignment context:\n%s\nContext complete: %t\n", terminalText(i.Identifier), terminalText(i.Title), i.ID, i.TeamID, i.ProjectID, terminalText(i.State), terminalText(i.Assignee), terminalText(i.Description), terminalText(fmt.Sprint(i.Context)), i.ContextComplete))
	}
	if m.linear.step == "details" && m.linear.issueV2 != nil {
		i := m.linear.issueV2
		value := func(known bool, id string) string {
			if !known {
				return "unavailable"
			}
			if id == "" {
				return "none"
			}
			return terminalText(id)
		}
		labels := "unavailable"
		if i.LabelsKnown {
			labels = strings.Join(i.LabelIDs, ", ")
			if labels == "" {
				labels = "none"
			}
		}
		// Native Linear may report a parent only by its display identifier.
		parent := i.ParentID
		if parent == "" {
			parent = i.ParentIdentifier
		}
		b.WriteString("\nVerified L2 context\nProject ID: " + value(i.ProjectKnown, i.ProjectID) + "\nParent ID: " + value(i.ParentKnown, parent) + "\nAssignee ID: " + value(i.AssigneeKnown, i.AssigneeID) + "\nLabel IDs: " + terminalText(labels) + "\n")
	}
	if m.linear.next != "" {
		b.WriteString("\nMore provider rows available: ] next page.\n")
	}
	if m.inputMode == inputLinearClosesName && len(m.linear.typing) > 0 {
		b.WriteString("\nAgents will close the issues of Linear project " + terminalText(fmt.Sprintf("%q", m.linear.typing[0].Name)) + ", completing them without a person. Type its name to confirm; Esc cancels.\n")
	}
	if m.inputMode == inputLinearName {
		b.WriteString("\nNew connection label (" + string(m.linear.connectAccess) + "): " + terminalText(m.draft) + "\nEnter starts explicit OAuth consent; Esc cancels.\n")
	}
	return b.String()
}
