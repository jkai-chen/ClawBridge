package tui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Project status (TUI-UX-AUDIT U61) is a view inside the Linear screen, p
// from the board: each bound Linear project's status, read by the server's
// ClawBridge app. A read costs the project's Linear budget, so it is read
// when the view opens and on r, never with the board. A project owner or
// lead in a TUI without a session sets one: a type, the server's preview,
// then y; closing with issues open takes a second y. The server keeps who
// may set it and the project's close rule; a coordinator sets it from its
// session in the CLI.

// ProjectStatusName is one Linear project status.
type ProjectStatusName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ProjectStatus is the server's answer to a status read, preview or set.
type ProjectStatus struct {
	LinearProject string            `json:"linear_project"`
	Status        ProjectStatusName `json:"status"`
	Issues        struct {
		Total    int  `json:"total"`
		Open     int  `json:"open"`
		Complete bool `json:"complete"`
	} `json:"issues"`
	Suggestion       string             `json:"suggestion,omitempty"`
	SuggestionNote   string             `json:"suggestion_note,omitempty"`
	Preview          bool               `json:"preview,omitempty"`
	Refused          string             `json:"refused,omitempty"`
	ExpectedStatusID string             `json:"expected_status_id,omitempty"`
	NeedsAllowOpen   bool               `json:"needs_allow_open,omitempty"`
	To               *ProjectStatusName `json:"to,omitempty"`
	State            string             `json:"state,omitempty"`
	Error            string             `json:"error,omitempty"`
	Recorded         map[string]any     `json:"recorded,omitempty"`
}

// projectStatusTypes are the types a set names, in Linear's order.
var projectStatusTypes = []string{"backlog", "planned", "started", "completed", "canceled"}

type projectStatusRow struct {
	project string
	read    *ProjectStatus
	err     string
}

type projectStatusState struct {
	loaded  bool
	canEdit bool
	// roleUnknown says the viewer's role couldn't be read, so the view
	// is read-only for that reason.
	roleUnknown bool
	rows        []projectStatusRow
	// picking is the type picker for the selected project; ask is a
	// previewed set waiting for y.
	picking bool
	pick    int
	ask     *projectStatusAsk
}

// projectStatusAsk is a previewed set: the request to confirm and the
// words its preview says. open asks a second y before closing with issues
// open; opened says the first was given.
type projectStatusAsk struct {
	request LinearRequest
	preview string
	open    string
	opened  bool
}

type projectStatusLoadedMsg struct {
	request uint64
	state   projectStatusState
	err     error
}

// projectStatusDoneMsg is a preview's or a set's answer.
type projectStatusDoneMsg struct {
	request uint64
	sent    LinearRequest
	data    *ProjectStatus
	err     error
}

// projectStatusIDs are the board's Linear projects.
func (m Model) projectStatusIDs() []string {
	if m.linear.bindingV2 != nil {
		return m.linear.bindingV2.LinearProjectIDs
	}
	if m.linear.binding.LinearProjectID != "" {
		return []string{m.linear.binding.LinearProjectID}
	}
	return nil
}

// projectStatusName is a Linear project by its stored name, else its ID.
func (m Model) projectStatusName(id string) string {
	return firstNonempty(m.linearNames([]string{id}), terminalText(id))
}

func (m Model) openProjectStatus() (Model, tea.Cmd) {
	m.linear.step, m.selected = "status", 0
	m.projectStatus = projectStatusState{}
	return m.projectStatusLoad()
}

// projectStatusLoad reads each bound project's status, one by one; one
// that fails says why on its row.
func (m Model) projectStatusLoad() (Model, tea.Cmd) {
	requestID := m.request
	ids := m.projectStatusIDs()
	if m.cfg.Linear == nil || len(ids) == 0 {
		return m, func() tea.Msg {
			return projectStatusLoadedMsg{request: requestID, err: fmt.Errorf("the board has no Linear project to read a status of")}
		}
	}
	m.loading = true
	source, ctx, profile := m.cfg.Linear, m.cfg.Context, m.linear.profile
	executor, workspace, project := m.cfg.Executor, m.cfg.Workspace, m.cfg.Project
	// A person sets a status with no session (the CLI's rule): a TUI
	// opened with one reads only.
	session := m.cfg.Session
	return m, func() tea.Msg {
		s := projectStatusState{loaded: true}
		if session == "" {
			names := readStoredNames(ctx, executor, workspace, project)
			s.roleUnknown = names == nil || names.role == ""
			s.canEdit = names != nil && (names.role == "owner" || names.role == "lead")
		}
		for _, id := range ids {
			row := projectStatusRow{project: id}
			d, err := source.Do(ctx, LinearRequest{Operation: "project-status", ProfileID: profile, LinearProject: id})
			if err != nil {
				row.err = displayError(err)
			} else {
				row.read = d.ProjectStatus
			}
			s.rows = append(s.rows, row)
		}
		return projectStatusLoadedMsg{request: requestID, state: s}
	}
}

func (m Model) applyProjectStatus(x projectStatusLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "status" {
		return m
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m
	}
	m.projectStatus = x.state
	if m.selected >= len(m.projectStatus.rows) {
		m.selected = max(0, len(m.projectStatus.rows)-1)
	}
	return m
}

func (m Model) projectStatusRows() []string {
	rows := []string{}
	for _, r := range m.projectStatus.rows {
		rows = append(rows, r.project)
	}
	return rows
}

func (m Model) selectedProjectStatus() (projectStatusRow, bool) {
	if m.selected < 0 || m.selected >= len(m.projectStatus.rows) {
		return projectStatusRow{}, false
	}
	return m.projectStatus.rows[m.selected], true
}

// projectStatusKey runs a key on the view.
func (m Model) projectStatusKey(key string) (Model, tea.Cmd) {
	row, ok := m.selectedProjectStatus()
	if !m.projectStatus.loaded || !ok {
		return m, nil
	}
	switch key {
	case "enter":
		if !m.projectStatus.canEdit {
			m.status = noteStatus("Only a project owner or lead sets a Linear project's status, in a TUI opened without a session.")
			return m, nil
		}
		m.projectStatus.picking, m.projectStatus.pick, m.status = true, 0, status{}
		if row.read != nil {
			for i, t := range projectStatusTypes {
				if t == row.read.Status.Type {
					m.projectStatus.pick = i
				}
			}
		}
	case "g":
		return m.projectStatusSend(LinearRequest{Operation: "project-status", ProfileID: m.linear.profile, LinearProject: row.project, Suggest: true})
	}
	return m, nil
}

// projectStatusModalKey is a key while the picker is open or a preview
// waits for y.
func (m Model) projectStatusModalKey(key string) (Model, tea.Cmd) {
	if ask := m.projectStatus.ask; ask != nil {
		if key != "y" {
			m.projectStatus.ask = nil
			m.status = noteStatus("Nothing was changed.")
			return m, nil
		}
		if ask.open != "" && !ask.opened {
			ask.opened = true
			return m, nil
		}
		m.projectStatus.ask = nil
		return m.projectStatusSend(ask.request)
	}
	switch key {
	case "up", "k":
		m.projectStatus.pick = max(0, m.projectStatus.pick-1)
	case "down", "j":
		m.projectStatus.pick = min(len(projectStatusTypes)-1, m.projectStatus.pick+1)
	case "esc":
		m.projectStatus.picking = false
	case "enter":
		m.projectStatus.picking = false
		row, ok := m.selectedProjectStatus()
		if !ok {
			return m, nil
		}
		// One operation ID for the preview and its confirm: a confirm
		// sent twice is answered from the server's record.
		return m.projectStatusSend(LinearRequest{Operation: "project-status-set", ProfileID: m.linear.profile, LinearProject: row.project,
			StatusType: projectStatusTypes[m.projectStatus.pick], OperationID: newOperationID()})
	}
	return m, nil
}

// newOperationID is a fresh ID for one status set.
func newOperationID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "tui-" + hex.EncodeToString(b)
}

// projectStatusSend sends a suggest read, a preview or a confirmed set.
func (m Model) projectStatusSend(r LinearRequest) (Model, tea.Cmd) {
	if r.Operation == "project-status-set" && m.writesBlocked() {
		m.status = noteStatus("The authority is offline; setting a status needs the server.")
		return m, nil
	}
	m.loading, m.status = true, status{}
	return m.projectStatusRead(r)
}

// projectStatusRead sends r, keeping the status line as it is.
func (m Model) projectStatusRead(r LinearRequest) (Model, tea.Cmd) {
	m.loading = true
	requestID, source, ctx := m.request, m.cfg.Linear, m.cfg.Context
	return m, func() tea.Msg {
		d, err := source.Do(ctx, r)
		return projectStatusDoneMsg{request: requestID, sent: r, data: d.ProjectStatus, err: err}
	}
}

func (m Model) applyProjectStatusDone(x projectStatusDoneMsg) (Model, tea.Cmd) {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "status" {
		return m, nil
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m, nil
	}
	d := x.data
	if d == nil {
		m.status = failStatus("The server sent no status.")
		return m, nil
	}
	name := m.projectStatusName(x.sent.LinearProject)
	switch {
	case x.sent.Operation == "project-status":
		// A suggest: shown on the row, never applied.
		for i := range m.projectStatus.rows {
			if m.projectStatus.rows[i].project == x.sent.LinearProject {
				m.projectStatus.rows[i].read, m.projectStatus.rows[i].err = d, ""
			}
		}
		return m, nil
	case d.Recorded != nil:
		m.status = noteStatus(fmt.Sprintf("This change was sent before (%s); read again with r.", terminalText(text(d.Recorded, "state"))))
	case d.Preview && d.Refused != "":
		m.status = failStatus("Can't set " + name + " " + x.sent.StatusType + ": " + terminalText(d.Refused))
		return m, nil
	case d.Preview:
		confirm := x.sent
		confirm.Confirm, confirm.ExpectedStatus = true, d.ExpectedStatusID
		to := x.sent.StatusType
		if d.To != nil {
			to = terminalText(d.To.Name)
		}
		ask := &projectStatusAsk{request: confirm,
			preview: fmt.Sprintf("Set %s from %s to %s? (%d of %d issues open)", name, terminalText(d.Status.Name), to, d.Issues.Open, d.Issues.Total)}
		if d.NeedsAllowOpen {
			confirm.AllowOpen = true
			ask.request = confirm
			ask.open = fmt.Sprintf("%d issues of %s are still open", d.Issues.Open, name)
			if !d.Issues.Complete {
				ask.open = "Not all of " + name + "'s issues could be read; some may be open"
			}
		}
		m.projectStatus.ask = ask
		return m, nil
	case d.State == "accepted":
		m.status = okStatus(name + " is now " + firstNonempty(statusToName(d), x.sent.StatusType) + ".")
	case d.State == "unchanged":
		m.status = noteStatus(name + " already has that status.")
	case d.State == "uncertain":
		// Never sent again: the read below says whether it landed.
		m.status = failStatus("Not sure Linear took the change to " + name + ": check its status below before trying again.")
	default:
		m.status = failStatus(fmt.Sprintf("Linear didn't take the change (%s): %s.", terminalText(d.State), terminalText(firstNonempty(d.Error, "no reason given"))))
	}
	// Only the project set is read again: each read costs its budget.
	return m.projectStatusRead(LinearRequest{Operation: "project-status", ProfileID: m.linear.profile, LinearProject: x.sent.LinearProject})
}

func statusToName(d *ProjectStatus) string {
	if d.To == nil {
		return ""
	}
	return terminalText(d.To.Name)
}

func renderProjectStatus(m Model) string {
	var b strings.Builder
	s := m.projectStatus
	if !s.loaded {
		if m.loading {
			b.WriteString(mutedStyle.Render("Reading each Linear project's status (it costs the project's Linear budget)…") + "\n")
		}
		return b.String()
	}
	switch {
	case s.canEdit:
	case s.roleUnknown:
		b.WriteString(mutedStyle.Render("Read-only: couldn't read your role in this project.") + "\n")
	default:
		b.WriteString(mutedStyle.Render("Read-only: a project owner or lead sets a status, in a TUI opened without a session.") + "\n")
	}
	b.WriteString("\n")
	for i, r := range s.rows {
		prefix := "  "
		if i == m.selected {
			prefix = accentStyle.Render("› ")
		}
		line := padRight(clip(m.projectStatusName(r.project), 36), 38)
		switch {
		case r.err != "":
			line += errorStyle.Render("not read")
		case r.read != nil:
			line += padRight(terminalText(r.read.Status.Name), 14) + fmt.Sprintf("%d of %d issues open", r.read.Issues.Open, r.read.Issues.Total)
			if !r.read.Issues.Complete {
				line += mutedStyle.Render(" (not all read)")
			}
		}
		b.WriteString(prefix + line + "\n")
		if r.err != "" {
			b.WriteString(mutedStyle.Render("    "+r.err) + "\n")
		}
		if r.read != nil && r.read.Suggestion != "" {
			b.WriteString(mutedStyle.Render("    Suggested from its issues: "+terminalText(r.read.Suggestion)+" (a hint; nothing is set)") + "\n")
		} else if r.read != nil && r.read.SuggestionNote != "" {
			b.WriteString(mutedStyle.Render("    No suggestion: "+terminalText(r.read.SuggestionNote)) + "\n")
		}
	}
	if s.picking {
		b.WriteString("\nSet it to which status type?\n")
		for i, t := range projectStatusTypes {
			prefix := "  "
			if i == s.pick {
				prefix = accentStyle.Render("› ")
			}
			b.WriteString(prefix + t + "\n")
		}
	}
	if s.ask != nil {
		b.WriteString("\n" + warnStyle.Render(s.ask.preview) + "\n")
		if s.ask.open != "" {
			b.WriteString(errorStyle.Render(s.ask.open+"; setting it completed anyway leaves them open.") + "\n")
			if s.ask.opened {
				b.WriteString(warnStyle.Render("Set it completed with issues open? y does it; any other key keeps things as they are.") + "\n")
				return b.String()
			}
		}
		b.WriteString("y does it; any other key keeps things as they are.\n")
	}
	return b.String()
}
