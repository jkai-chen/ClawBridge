package tui

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Team roles (TEAM-ROLES-TUI-PLAN) is a view inside the Linear screen, R
// from the board: each bound team's lead, engineers and reviewers, by
// Linear name read with the viewer's own connection. The project owner
// sets leads and people's roles; a team's lead sets engineer and reviewer
// roles in the teams they lead; anyone else reads. Every change is the
// server's preview first, then y at that revision. The server decides who
// may change what: the keys only leave out what it would refuse.

// TeamRolesView is the view's read: the mapping, the teams the viewer
// leads, whether they own the project, and the workspace's users for names
// (NamesErr says why there are none).
type TeamRolesView struct {
	Revision int64
	Teams    []RolesTeam
	People   []bus.TeamRolePerson
	YouLead  []string
	Owner    bool
	Users    []linearprovider.WorkspaceUser
	NamesErr string
}

// RolesTeam is a team of the mapping and its leads; Bound is false once
// the binding has dropped it.
type RolesTeam struct {
	TeamID string
	Leads  []string
	Bound  bool
}

// RolesChange is one edit, sent as a preview and then, with Confirm, at
// the same Revision. Kind "lead" makes User a lead of Team, or stops it
// (Remove); kind "person" gives User Role in Teams, or removes them when
// Teams is empty.
type RolesChange struct {
	Kind     string
	Team     string
	User     string
	Role     string
	Teams    []string
	Remove   bool
	Revision int64
	Confirm  bool
}

// RolesSetResult is the server's answer to a change: the preview's counts,
// or the stored revision.
type RolesSetResult struct {
	Preview  bool
	Revision int64
	Change   map[string]any
}

type rolesState struct {
	loaded bool
	view   TeamRolesView
	// ask is a change waiting for y, with its preview's words.
	ask      *RolesChange
	askWords string
	// picking is the person picker for kind (lead, engineer or reviewer)
	// in team; pick is its cursor.
	picking   string
	pickTeam  string
	pick      int
	pickRoles bool
	pickUser  string
}

type rolesLoadedMsg struct {
	request uint64
	view    TeamRolesView
	err     error
}

type rolesSetMsg struct {
	request uint64
	change  RolesChange
	result  RolesSetResult
	err     error
}

func (m Model) openRoles() (Model, tea.Cmd) {
	m.linear.step, m.selected = "roles", 0
	m.roles = rolesState{}
	return m.rolesLoad()
}

// rolesLoad reads the view, on open and on r.
func (m Model) rolesLoad() (Model, tea.Cmd) {
	requestID := m.request
	if m.cfg.Linear == nil {
		return m, func() tea.Msg {
			return rolesLoadedMsg{request: requestID, err: errors.New("Linear isn't available in this TUI")}
		}
	}
	m.loading = true
	source, ctx, profile := m.cfg.Linear, m.cfg.Context, m.linear.profile
	return m, func() tea.Msg {
		d, err := source.Do(ctx, LinearRequest{Operation: "roles", ProfileID: profile})
		if err == nil && d.Roles == nil {
			err = errors.New("the server's answer had no team roles")
		}
		var v TeamRolesView
		if d.Roles != nil {
			v = *d.Roles
		}
		return rolesLoadedMsg{request: requestID, view: v, err: err}
	}
}

func (m Model) applyRoles(x rolesLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "roles" {
		return m
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m
	}
	m.roles = rolesState{loaded: true, view: x.view}
	if n := len(m.rolesRows()); m.selected >= n {
		m.selected = max(0, n-1)
	}
	return m
}

// rolesWritable is false in a TUI with a session or run by an agent: a
// role is a person's own change (the server refuses a session's).
func (m Model) rolesWritable() bool {
	return m.cfg.Session == "" && m.cfg.AgentMarker == ""
}

// rolesTeams are the teams shown: the binding's, in its order, then any
// the mapping still names that the binding dropped.
func (m Model) rolesTeams() []RolesTeam {
	byID := map[string]RolesTeam{}
	for _, t := range m.roles.view.Teams {
		byID[t.TeamID] = t
	}
	out := []RolesTeam{}
	seen := map[string]bool{}
	if b := m.linear.bindingV2; b != nil {
		for _, id := range b.LinearTeamIDs {
			t := byID[id]
			t.TeamID, t.Bound = id, true
			out, seen[id] = append(out, t), true
		}
	}
	for _, t := range m.roles.view.Teams {
		if !seen[t.TeamID] {
			t.Bound = t.Bound && m.linear.bindingV2 == nil
			out, seen[t.TeamID] = append(out, t), true
		}
	}
	return out
}

// rolesPeople are a team's people who don't lead it: a lead who works in
// it without leading it (stopped leading, or leads another team), then
// engineers, then reviewers, by name.
func (m Model) rolesPeople(team string) []bus.TeamRolePerson {
	leads := map[string]bool{}
	for _, t := range m.roles.view.Teams {
		if t.TeamID == team {
			for _, l := range t.Leads {
				leads[l] = true
			}
		}
	}
	out := []bus.TeamRolePerson{}
	for _, p := range m.roles.view.People {
		if !leads[p.LinearUserID] && slices.Contains(p.Teams, team) {
			out = append(out, p)
		}
	}
	order := map[string]int{"lead": 0, "engineer": 1, "reviewer": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return order[out[i].Role] < order[out[j].Role]
		}
		return strings.ToLower(m.roleName(out[i].LinearUserID)) < strings.ToLower(m.roleName(out[j].LinearUserID))
	})
	return out
}

// rolesRows are the selectable rows: each team, its leads, its people.
func (m Model) rolesRows() []string {
	rows := []string{}
	for _, t := range m.rolesTeams() {
		rows = append(rows, "team:"+t.TeamID+":")
		for _, l := range t.Leads {
			rows = append(rows, "lead:"+t.TeamID+":"+l)
		}
		for _, p := range m.rolesPeople(t.TeamID) {
			rows = append(rows, "person:"+t.TeamID+":"+p.LinearUserID)
		}
	}
	return rows
}

func (m Model) rolesRow() (kind, team, user string) {
	rows := m.rolesRows()
	if m.selected < 0 || m.selected >= len(rows) {
		return "", "", ""
	}
	parts := strings.SplitN(rows[m.selected], ":", 3)
	return parts[0], parts[1], parts[2]
}

func (m Model) rolesPerson(id string) (bus.TeamRolePerson, bool) {
	for _, p := range m.roles.view.People {
		if p.LinearUserID == id {
			return p, true
		}
	}
	return bus.TeamRolePerson{}, false
}

// roleName is a Linear user by name, as the viewer's connection knows
// them, else by ID; one Linear doesn't call active and non-guest says so.
func (m Model) roleName(id string) string {
	for _, u := range m.roles.view.Users {
		if strings.EqualFold(u.ID, id) {
			name := terminalText(firstNonempty(u.Name, u.ID))
			if !u.Eligible() {
				name += " (inactive or a guest)"
			}
			if m.linear.showIDs {
				name += " (" + id + ")"
			}
			return name
		}
	}
	if len(m.roles.view.Users) > 0 {
		return "unknown Linear user " + terminalText(id)
	}
	return terminalText(id)
}

// rolesCanTeam says the viewer may change people in team: the owner, or
// its lead.
func (m Model) rolesCanTeam(team string) bool {
	return m.rolesWritable() && (m.roles.view.Owner || slices.Contains(m.roles.view.YouLead, team))
}

func (m Model) rolesCanLeads() bool {
	return m.rolesWritable() && m.roles.view.Owner
}

// rolesKeyOn is the selected row's kind for a role key's When: l and the
// lead's x for the owner; a, e and a person's x for the owner or the
// team's lead.
func rolesKeyOn(m Model, key string) bool {
	if m.linearStep() != "roles" || !m.roles.loaded {
		return false
	}
	kind, team, user := m.rolesRow()
	switch key {
	case "l":
		return kind != "" && m.rolesCanLeads()
	case "a":
		return kind != "" && m.rolesCanTeam(team)
	case "e":
		p, _ := m.rolesPerson(user)
		return kind == "person" && p.Role != "lead" && m.rolesCanTeam(team)
	case "x":
		return (kind == "lead" && m.rolesCanLeads()) || (kind == "person" && m.rolesCanTeam(team))
	}
	return false
}

// rolesKey runs a key on the view; every change is a preview first.
func (m Model) rolesKey(key string) (Model, tea.Cmd) {
	if !m.roles.loaded {
		return m, nil
	}
	if !rolesKeyOn(m, key) {
		if key == "l" || key == "a" || key == "e" || key == "x" {
			m.status = noteStatus(m.rolesReadOnlyWords())
		}
		return m, nil
	}
	kind, team, user := m.rolesRow()
	rev := m.roles.view.Revision
	switch key {
	case "l", "a":
		role := "lead"
		if key == "a" {
			role = ""
		}
		if len(m.rolesPickable(role, team)) == 0 {
			m.status = noteStatus(firstNonempty(m.roles.view.NamesErr, "Nobody else in the Linear workspace can be added here.") +
				" Choose people from Linear's active, non-guest users, read with your Linear connection.")
			return m, nil
		}
		m.roles.picking, m.roles.pickTeam, m.roles.pick, m.roles.pickRoles, m.status = role, team, 0, false, status{}
		if role == "" {
			m.roles.picking = "person"
		}
		return m, nil
	case "e":
		p, _ := m.rolesPerson(user)
		next := map[string]string{"engineer": "reviewer", "reviewer": "engineer"}[p.Role]
		return m.rolesPreview(RolesChange{Kind: "person", User: user, Role: next, Teams: p.Teams, Revision: rev},
			m.roleName(user)+": "+p.Role+" → "+next+" in every team they work in?")
	case "x":
		if kind == "lead" {
			return m.rolesPreview(RolesChange{Kind: "lead", Team: team, User: user, Remove: true, Revision: rev},
				m.roleName(user)+" stops leading "+m.rolesTeamName(team)+"?")
		}
		p, _ := m.rolesPerson(user)
		teams := []string{}
		for _, t := range p.Teams {
			if t != team {
				teams = append(teams, t)
			}
		}
		words := m.roleName(user) + " stops working in " + m.rolesTeamName(team) + "?"
		if len(teams) == 0 {
			words = "Remove " + m.roleName(user) + " from team roles? " + m.rolesTeamName(team) + " is their only team."
		}
		return m.rolesPreview(RolesChange{Kind: "person", User: user, Role: p.Role, Teams: teams, Revision: rev}, words)
	}
	return m, nil
}

func (m Model) rolesReadOnlyWords() string {
	switch {
	case m.cfg.Session != "":
		return "Team roles are changed by a person, not a session: open the TUI without --session."
	case m.cfg.AgentMarker != "":
		return m.cfg.AgentMarker + " says an agent runs this TUI; team roles are a person's to change."
	}
	return "Only the project owner changes leads; a team's lead changes engineers and reviewers in the teams they lead."
}

func (m Model) rolesTeamName(team string) string {
	return firstNonempty(m.linearNames([]string{team}), terminalText(team))
}

// rolesPickable are the users a picker offers, by name: active, non-guest
// users of the Linear workspace; for a lead, anyone not leading team yet;
// for a person, anyone not a lead and not in team yet.
func (m Model) rolesPickable(role, team string) []linearprovider.WorkspaceUser {
	leads := map[string]bool{}
	for _, t := range m.roles.view.Teams {
		if t.TeamID == team {
			for _, l := range t.Leads {
				leads[l] = true
			}
		}
	}
	out := []linearprovider.WorkspaceUser{}
	for _, u := range m.roles.view.Users {
		if !u.Eligible() {
			continue
		}
		p, mapped := m.rolesPerson(u.ID)
		switch {
		case role == "lead" && leads[u.ID]:
			continue
		case role != "lead" && mapped && (p.Role == "lead" || slices.Contains(p.Teams, team)):
			continue
		}
		out = append(out, u)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// rolesModalKey is a key while a preview waits for y, or the picker is
// open (a person, then for a new person their role).
func (m Model) rolesModalKey(key string) (Model, tea.Cmd) {
	if ask := m.roles.ask; ask != nil {
		m.roles.ask = nil
		if key != "y" {
			m.status = noteStatus("Nothing was changed.")
			return m, nil
		}
		c := *ask
		c.Confirm = true
		return m.rolesSend(c)
	}
	team, rev := m.roles.pickTeam, m.roles.view.Revision
	if m.roles.pickRoles {
		switch key {
		case "esc":
			m.roles.picking, m.roles.pickRoles = "", false
		case "e", "r":
			role := map[string]string{"e": "engineer", "r": "reviewer"}[key]
			user := m.roles.pickUser
			m.roles.picking, m.roles.pickRoles = "", false
			teams := []string{team}
			if p, ok := m.rolesPerson(user); ok {
				teams = append(append([]string{}, p.Teams...), team)
			}
			words := m.roleName(user) + " becomes " + article(role) + " in " + m.rolesTeamName(team) + "?"
			if p, ok := m.rolesPerson(user); ok && p.Role != role {
				words = m.roleName(user) + " joins " + m.rolesTeamName(team) + ", and is " + article(role) + " in every team they work in (was " + p.Role + ")?"
			}
			return m.rolesPreview(RolesChange{Kind: "person", User: user, Role: role, Teams: teams, Revision: rev}, words)
		}
		return m, nil
	}
	role := m.roles.picking
	if role == "person" {
		role = ""
	}
	pickable := m.rolesPickable(role, team)
	switch key {
	case "up", "k":
		m.roles.pick = max(0, m.roles.pick-1)
	case "down", "j":
		m.roles.pick = min(max(0, len(pickable)-1), m.roles.pick+1)
	case "esc":
		m.roles.picking = ""
	case "enter":
		if m.roles.pick >= len(pickable) {
			m.roles.picking = ""
			return m, nil
		}
		u := pickable[m.roles.pick]
		if role == "lead" {
			m.roles.picking = ""
			return m.rolesPreview(RolesChange{Kind: "lead", Team: team, User: u.ID, Revision: rev},
				terminalText(u.Name)+" leads "+m.rolesTeamName(team)+"? Their role becomes lead.")
		}
		m.roles.pickUser, m.roles.pickRoles = u.ID, true
	}
	return m, nil
}

func article(role string) string {
	if role == "engineer" {
		return "an engineer"
	}
	return "a " + role
}

// rolesPreview asks the server what the change does; nothing is stored.
func (m Model) rolesPreview(c RolesChange, words string) (Model, tea.Cmd) {
	m.roles.askWords = words
	return m.rolesSend(c)
}

// rolesSend sends a change: without Confirm the server previews it, with
// Confirm it stores it at the previewed revision.
func (m Model) rolesSend(c RolesChange) (Model, tea.Cmd) {
	if !m.rolesWritable() {
		m.status = noteStatus(m.rolesReadOnlyWords())
		return m, nil
	}
	if m.writesBlocked() {
		m.status = noteStatus("The authority is offline; team roles need the server.")
		return m, nil
	}
	m.loading = true
	requestID, source, ctx, profile := m.request, m.cfg.Linear, m.cfg.Context, m.linear.profile
	return m, func() tea.Msg {
		d, err := source.Do(ctx, LinearRequest{Operation: "roles-set", ProfileID: profile, Roles: &c})
		var r RolesSetResult
		if d.RolesSet != nil {
			r = *d.RolesSet
		} else if err == nil {
			err = errors.New("the server's answer had no result")
		}
		return rolesSetMsg{request: requestID, change: c, result: r, err: err}
	}
}

func (m Model) applyRolesSet(x rolesSetMsg) (Model, tea.Cmd) {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "roles" {
		return m, nil
	}
	m.loading = false
	if x.err != nil {
		if strings.Contains(x.err.Error(), "team roles changed") {
			m.status = errStatus(fmt.Errorf("%w; r reads them again, and nothing was changed", x.err))
			return m, nil
		}
		if strings.Contains(x.err.Error(), "listed under people with role lead") {
			m.status = errStatus(fmt.Errorf("%w; stop them leading first (x on their Lead row)", x.err))
			return m, nil
		}
		m.status = errStatus(x.err)
		return m, nil
	}
	if !x.change.Confirm {
		c := x.change
		m.roles.ask = &c
		m.roles.askWords += " " + rolesChangeWords(x.result.Change)
		return m, nil
	}
	m.status = okStatus(fmt.Sprintf("Team roles saved at revision %d.", x.result.Revision))
	return m.rolesLoad()
}

// rolesChangeWords is the server's preview: how many people and teams the
// change touches.
func rolesChangeWords(c map[string]any) string {
	parts := []string{}
	for _, k := range []struct{ key, words string }{{"added", "added"}, {"removed", "removed"}, {"changed", "changed"}, {"teams_changed", "teams' leads changed"}} {
		if n := asCount(c[k.key]); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k.words))
		}
	}
	if len(parts) == 0 {
		return "The server's preview: no change."
	}
	return "The server's preview: " + strings.Join(parts, ", ") + "."
}

func renderRoles(m Model) string {
	var b strings.Builder
	if !m.roles.loaded {
		if m.loading {
			b.WriteString("Reading team roles…\n")
		}
		return b.String()
	}
	v := m.roles.view
	can := "nothing (read-only)"
	switch {
	case !m.rolesWritable():
		can = "nothing here (" + strings.TrimSuffix(m.rolesReadOnlyWords(), ".") + ")"
	case v.Owner:
		can = "leads and everyone's roles (project owner)"
	case len(v.YouLead) > 0:
		can = "engineers and reviewers in " + m.linearNames(v.YouLead) + " (you lead it)"
	}
	fmt.Fprintf(&b, "Revision %d · you can change: %s\n", v.Revision, can)
	if v.NamesErr != "" {
		b.WriteString("People show by ID: " + terminalText(v.NamesErr) + "\n")
	}
	b.WriteString("\n")
	rows, i := m.rolesRows(), 0
	if len(rows) == 0 {
		b.WriteString("No bound teams yet; link a board first.\n")
	}
	line := func(s string) {
		prefix := "  "
		if i == m.selected {
			prefix = "› "
		}
		b.WriteString(prefix + s + "\n")
		i++
	}
	for _, t := range m.rolesTeams() {
		head := m.rolesTeamName(t.TeamID)
		if !t.Bound {
			head += " (no longer bound; ignored)"
		}
		line(head)
		if len(t.Leads) == 0 {
			b.WriteString("      No lead.\n")
		}
		for _, l := range t.Leads {
			line("  Lead      " + m.roleName(l) + m.roleLink(l))
		}
		for _, p := range m.rolesPeople(t.TeamID) {
			note := ""
			if p.Role == "lead" {
				note = " · not leading this team"
			}
			line("  " + map[string]string{"lead": "Lead      ", "engineer": "Engineer  ", "reviewer": "Reviewer  "}[p.Role] + m.roleName(p.LinearUserID) + note + m.roleLink(p.LinearUserID))
		}
	}
	if m.roles.picking != "" {
		b.WriteString("\n" + m.rolesPicker())
	}
	if m.roles.ask != nil {
		b.WriteString("\n" + m.roles.askWords + "\n")
	}
	return b.String()
}

// roleLink is the person's ClawBridge account as the owner linked it: the
// account itself for the owner, only that there is one for anyone else.
func (m Model) roleLink(id string) string {
	p, ok := m.rolesPerson(id)
	if !ok || p.Principal == "" {
		return ""
	}
	if m.roles.view.Owner {
		return " · linked by the owner (" + terminalText(p.Principal) + ")"
	}
	return " · linked"
}

func (m Model) rolesPicker() string {
	var b strings.Builder
	team := m.rolesTeamName(m.roles.pickTeam)
	if m.roles.pickRoles {
		fmt.Fprintf(&b, "%s in %s as: e an engineer · r a reviewer\n", m.roleName(m.roles.pickUser), team)
		return b.String()
	}
	role := m.roles.picking
	what := "a lead for " + team
	if role == "person" {
		role, what = "", "someone to work in "+team
	}
	fmt.Fprintf(&b, "Choose %s (active, non-guest users of the Linear workspace):\n", what)
	for i, u := range m.rolesPickable(role, m.roles.pickTeam) {
		prefix := "  "
		if i == m.roles.pick {
			prefix = "› "
		}
		name := terminalText(u.Name)
		if m.linear.showIDs {
			name += " (" + u.ID + ")"
		}
		b.WriteString(prefix + name + "\n")
	}
	return b.String()
}
