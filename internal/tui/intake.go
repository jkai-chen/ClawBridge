package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Intake (TUI-UX-AUDIT U59) is a view inside the Linear screen, n from the
// board: the project's intake mode and signals, the Linear users it
// trusts, by name, the queue, and whether each bound team has its
// needs-agent label. A project owner or lead changes them, each through a
// y/N preview; anyone else reads. The person acts as themselves, with no
// session (intake needs none).

// intakeSignals are the signals the view switches; S4 and S5 stay CLI-only.
var intakeSignals = []struct{ ID, Words string }{
	{"S1", "the needs-agent label"},
	{"S2", "delegating the issue to the ClawBridge app"},
	{"S3", "@-mentioning the app in a comment"},
}

// IntakeLabel is one bound team's needs-agent check: Found labels named
// exactly that (live, not a group), Complete when every page was read.
type IntakeLabel struct {
	TeamID   string
	Found    int
	Complete bool
}

type intakeState struct {
	loaded   bool
	canEdit  bool
	mode     string
	signals  []string
	revision int
	trusted  []string
	queue    []map[string]any
	// members are the bound teams' active Linear members, by the
	// viewer's own connection; names only, never stored.
	members   []linearprovider.Member
	namesNote string
	labels    []IntakeLabel
	labelNote string
	// ask is a change waiting for y; picking is the member picker.
	ask     *intakeChange
	picking bool
	pick    int
}

// intakeChange is one change and the words its preview says.
type intakeChange struct {
	op      string
	args    map[string]any
	preview string
}

type intakeLoadedMsg struct {
	request uint64
	state   intakeState
	err     error
}

type intakeDoneMsg struct {
	request uint64
	err     error
}

// intakeRequest is a request as the person, with no session.
func (m Model) intakeRequest(op string, args map[string]any) bus.Request {
	return bus.Request{Operation: op, Workspace: m.cfg.Workspace, Project: m.cfg.Project, Args: args}
}

// openIntake shows the view, reading it.
func (m Model) openIntake() (Model, tea.Cmd) {
	m.linear.step, m.selected = "intake", 0
	m.intake.ask, m.intake.picking = nil, false
	return m.intakeLoad()
}

// intakeLoad reads the view: the policy, the trusted list, the queue and
// the viewer's role through the authority; names and the label check
// through the viewer's Linear connection, best effort.
func (m Model) intakeLoad() (Model, tea.Cmd) {
	requestID := m.request
	if m.cfg.Executor == nil || m.cfg.Project == "" {
		return m, func() tea.Msg {
			return intakeLoadedMsg{request: requestID, err: errors.New("intake is set per project; open the TUI with a project")}
		}
	}
	m.loading = true
	executor, ctx, source, profile, mode := m.cfg.Executor, m.cfg.Context, m.cfg.Linear, m.linear.profile, m.cfg.Mode
	return m, func() tea.Msg {
		read := func(op string) (any, error) {
			r, err := executor.Execute(ctx, m.intakeRequest(op, map[string]any{}))
			if err == nil && r.Source == "offline-mirror" {
				return nil, errors.New("the authority is offline; intake needs the server")
			}
			return r.Data, err
		}
		var s intakeState
		policy, err := read("intake_policy_get")
		if err != nil {
			return intakeLoadedMsg{request: requestID, err: err}
		}
		p := mapData(policy)
		s.mode, s.revision = text(p, "mode"), asCount(p["revision"])
		for _, v := range listOf(p["signals"]) {
			s.signals = append(s.signals, fmt.Sprint(v))
		}
		trusted, err := read("intake_trust_list")
		if err != nil {
			return intakeLoadedMsg{request: requestID, err: err}
		}
		for _, r := range records(trusted) {
			s.trusted = append(s.trusted, text(r, "linear_user_id"))
		}
		queue, err := read("intake_list")
		if err != nil {
			return intakeLoadedMsg{request: requestID, err: err}
		}
		for _, r := range records(queue) {
			if st := text(r, "state"); st == "queued" || st == "offered" || st == "dismissed" {
				s.queue = append(s.queue, r)
			}
		}
		// Controls for a project owner or lead; locally, the person
		// owns everything.
		s.canEdit = mode != ModeCloud
		if mode == ModeCloud {
			me := text(mapData(must(read("whoami"))), "principal")
			for _, r := range records(mapData(must(read("list_members")))["members"]) {
				if text(r, "principal") == me && (text(r, "role") == "owner" || text(r, "role") == "lead") {
					s.canEdit = true
				}
			}
		}
		switch {
		case source == nil:
			s.namesNote = "Linear isn't set up here, so trusted users show by ID."
		default:
			d, err := source.Do(ctx, LinearRequest{Operation: "intake", ProfileID: profile})
			if err != nil {
				s.namesNote = "Names and the label check need your Linear connection: " + displayError(err)
			} else {
				for _, mb := range d.Members {
					if mb.Active {
						s.members = append(s.members, mb)
					}
				}
				s.labels = d.IntakeLabels
			}
		}
		s.loaded = true
		return intakeLoadedMsg{request: requestID, state: s}
	}
}

// must drops a best-effort read's error.
func must(v any, _ error) any { return v }

// listOf is v as a list, whatever its element type.
func listOf(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out
	}
	return nil
}

// asCount is a JSON number as an int.
func asCount(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func (m Model) applyIntake(x intakeLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "intake" {
		return m
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m
	}
	m.intake = x.state
	if rows := m.intakeRows(); m.selected >= len(rows) {
		m.selected = max(0, len(rows)-1)
	}
	return m
}

// intakeRows are the selectable rows: the mode, the signals, each trusted
// user, each queued or dismissed issue.
func (m Model) intakeRows() []string {
	rows := []string{"mode"}
	for _, s := range intakeSignals {
		rows = append(rows, "signal:"+s.ID)
	}
	for _, id := range m.intake.trusted {
		rows = append(rows, "trusted:"+id)
	}
	for _, q := range m.intake.queue {
		rows = append(rows, "queue:"+text(q, "issue_uuid"))
	}
	return rows
}

func (m Model) intakeRow() (kind, id string) {
	rows := m.intakeRows()
	if m.selected < 0 || m.selected >= len(rows) {
		return "", ""
	}
	kind, id, _ = strings.Cut(rows[m.selected], ":")
	return kind, id
}

// memberName is a Linear user by name, when the viewer's connection knows
// them, else by ID.
func (m Model) memberName(id string) string {
	for _, mb := range m.intake.members {
		if mb.ID == id {
			name := terminalText(mb.Name)
			if m.linear.showIDs {
				name += " (" + id + ")"
			}
			return name
		}
	}
	return terminalText(id)
}

func hasSignal(signals []string, id string) bool {
	for _, s := range signals {
		if s == id {
			return true
		}
	}
	return false
}

// intakeKey runs a key on the view; changes ask first.
func (m Model) intakeKey(key string) (Model, tea.Cmd) {
	if !m.intake.loaded {
		return m, nil
	}
	kind, id := m.intakeRow()
	refuse := func() (Model, tea.Cmd) {
		m.status = noteStatus("Only a project owner or lead changes intake.")
		return m, nil
	}
	switch key {
	case " ":
		if !m.intake.canEdit {
			return refuse()
		}
		switch kind {
		case "mode":
			next := "off"
			if m.intake.mode == "off" {
				next = "notify"
			}
			words := map[string]string{"off": "off: nothing is queued", "notify": "notify: the coordinator is told of each issue"}
			return m.intakeAsk("intake_policy_set", map[string]any{"mode": next, "expected_revision": m.intake.revision},
				"Intake mode "+m.intake.mode+" → "+words[next]+"?"), nil
		case "signal":
			next := []any{}
			on := hasSignal(m.intake.signals, id)
			for _, s := range m.intake.signals {
				if s != id {
					next = append(next, s)
				}
			}
			verb := "Stop listening for"
			if !on {
				next = append(next, id)
				verb = "Listen for"
			}
			return m.intakeAsk("intake_policy_set", map[string]any{"signals": next, "expected_revision": m.intake.revision},
				verb+" "+id+" ("+signalWords(id)+")?"), nil
		}
	case "a":
		if !m.intake.canEdit {
			return refuse()
		}
		if len(m.intakePickable()) == 0 {
			note := firstNonempty(m.intake.namesNote, "Every active member of the board's teams is trusted already.")
			if m.intake.namesNote != "" {
				note += " Use clawbridge intake trust add LINEAR_USER_ID."
			}
			m.status = noteStatus(note)
			return m, nil
		}
		m.intake.picking, m.intake.pick, m.status = true, 0, status{}
		return m, nil
	case "x":
		if !m.intake.canEdit {
			return refuse()
		}
		switch kind {
		case "trusted":
			return m.intakeAsk("intake_trust_remove", map[string]any{"linear_user_id": id},
				"Stop trusting "+m.memberName(id)+"? Their signals will only notify the coordinator."), nil
		case "queue":
			return m.intakeAsk("intake_dismiss", map[string]any{"issue_uuid": id}, "Dismiss "+m.issueName(id)+" from the queue?"), nil
		}
	case "e":
		if !m.intake.canEdit {
			return refuse()
		}
		if kind == "queue" {
			return m.intakeAsk("intake_rearm", map[string]any{"issue_uuid": id}, "Rearm "+m.issueName(id)+", so its next signal counts again?"), nil
		}
	}
	return m, nil
}

func signalWords(id string) string {
	for _, s := range intakeSignals {
		if s.ID == id {
			return s.Words
		}
	}
	return id
}

func (m Model) issueName(id string) string {
	for _, q := range m.intake.queue {
		if text(q, "issue_uuid") == id {
			return firstNonempty(terminalText(text(q, "issue_identifier")), terminalText(id))
		}
	}
	return terminalText(id)
}

func (m Model) intakeAsk(op string, args map[string]any, preview string) Model {
	m.intake.ask = &intakeChange{op: op, args: args, preview: preview}
	m.status = status{}
	return m
}

// intakePickable are the active members not yet trusted, by name.
func (m Model) intakePickable() []linearprovider.Member {
	out := []linearprovider.Member{}
	for _, mb := range m.intake.members {
		trusted := false
		for _, id := range m.intake.trusted {
			trusted = trusted || id == mb.ID
		}
		if !trusted {
			out = append(out, mb)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// intakeModalKey is a key while a preview waits for y, or the picker is
// open.
func (m Model) intakeModalKey(key string) (Model, tea.Cmd) {
	if ask := m.intake.ask; ask != nil {
		m.intake.ask = nil
		if key != "y" {
			return m, nil
		}
		return m.intakeDo(*ask)
	}
	pickable := m.intakePickable()
	switch key {
	case "up", "k":
		m.intake.pick = max(0, m.intake.pick-1)
	case "down", "j":
		m.intake.pick = min(max(0, len(pickable)-1), m.intake.pick+1)
	case "esc":
		m.intake.picking = false
	case "enter":
		m.intake.picking = false
		if m.intake.pick < len(pickable) {
			mb := pickable[m.intake.pick]
			return m.intakeAsk("intake_trust_add", map[string]any{"linear_user_id": mb.ID},
				"Trust "+terminalText(mb.Name)+" (Linear user "+mb.ID+")? Their label, delegation or mention can then go beyond telling the coordinator."), nil
		}
	}
	return m, nil
}

// intakeDo makes a confirmed change, then reads the view again.
func (m Model) intakeDo(c intakeChange) (Model, tea.Cmd) {
	if m.writesBlocked() {
		m.status = noteStatus("The authority is offline; intake changes need the server.")
		return m, nil
	}
	m.loading = true
	requestID, executor, ctx := m.request, m.cfg.Executor, m.cfg.Context
	request := m.intakeRequest(c.op, c.args)
	return m, func() tea.Msg {
		_, err := executor.Execute(ctx, request)
		return intakeDoneMsg{request: requestID, err: err}
	}
}

func (m Model) applyIntakeDone(x intakeDoneMsg) (Model, tea.Cmd) {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "intake" {
		return m, nil
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m, nil
	}
	return m.intakeLoad()
}

// intakeLabelLine says whether a team's needs-agent label is usable by S1.
func (m Model) intakeLabelLine(l IntakeLabel) string {
	team := firstNonempty(m.linearNames([]string{l.TeamID}), terminalText(l.TeamID))
	switch {
	case !l.Complete:
		return "needs-agent: couldn't read all of team " + team + "'s labels"
	case l.Found == 1:
		return "needs-agent label found in team " + team
	case l.Found == 0:
		return "needs-agent label missing in team " + team + ": add a label named exactly needs-agent in Linear"
	}
	return fmt.Sprintf("%d labels named needs-agent in team %s: S1 ignores the team until only one is left", l.Found, team)
}

func renderIntake(m Model) string {
	var b strings.Builder
	s := m.intake
	if !s.loaded {
		if m.loading {
			b.WriteString(mutedStyle.Render("Reading intake…") + "\n")
		}
		return b.String()
	}
	if !s.canEdit {
		b.WriteString(mutedStyle.Render("Read-only: a project owner or lead changes intake.") + "\n")
	}
	row := 0
	line := func(text string) {
		prefix := "  "
		if row == m.selected {
			prefix = accentStyle.Render("› ")
		}
		b.WriteString(prefix + text + "\n")
		row++
	}
	b.WriteString("\nMode and signals\n")
	mode := map[string]string{"off": "off: nothing is queued", "notify": "notify: the coordinator is told of each issue", "offer": "offer", "auto": "auto"}[s.mode]
	line("Mode: " + firstNonempty(mode, terminalText(s.mode)))
	for _, sig := range intakeSignals {
		mark := "[ ]"
		if hasSignal(s.signals, sig.ID) {
			mark = "[x]"
		}
		line(mark + " " + sig.ID + " " + sig.Words)
	}
	b.WriteString("\nTrusted Linear users (their signals can go beyond telling the coordinator)\n")
	// #207: a write made through ClawBridge runs as the app, never as the
	// person, so only what they do in Linear itself is theirs.
	b.WriteString(mutedStyle.Render("  Only a label, delegation or mention they make in Linear itself counts; anything done through ClawBridge runs as the app and is never trusted.") + "\n")
	if len(s.trusted) == 0 {
		b.WriteString(mutedStyle.Render("  Nobody yet: every signal only notifies the coordinator.") + "\n")
	}
	for _, id := range s.trusted {
		line(m.memberName(id))
	}
	if s.namesNote != "" {
		b.WriteString(mutedStyle.Render("  "+s.namesNote) + "\n")
	}
	b.WriteString("\nQueue\n")
	if len(s.queue) == 0 {
		b.WriteString(mutedStyle.Render("  Nothing queued.") + "\n")
	}
	for _, q := range s.queue {
		reason := ""
		if r := terminalText(text(q, "reason")); r != "" && r != text(q, "state") {
			reason = " · " + r
		}
		line(padRight(firstNonempty(terminalText(text(q, "issue_identifier")), terminalText(text(q, "issue_uuid"))), 12) + padRight(terminalText(text(q, "state")), 10) + terminalText(text(q, "signals")) + reason)
	}
	if len(s.labels) > 0 {
		b.WriteString("\nLabel check (S1)\n")
		for _, l := range s.labels {
			b.WriteString("  " + m.intakeLabelLine(l) + "\n")
		}
	}
	if s.picking {
		b.WriteString("\nTrust whom? (active members of the board's teams)\n")
		for i, mb := range m.intakePickable() {
			prefix := "  "
			if i == s.pick {
				prefix = accentStyle.Render("› ")
			}
			name := terminalText(mb.Name)
			if m.linear.showIDs {
				name += mutedStyle.Render("  " + mb.ID)
			}
			b.WriteString(prefix + name + "\n")
		}
	}
	if s.ask != nil {
		b.WriteString("\n" + warnStyle.Render(s.ask.preview) + " y does it; any other key keeps things as they are.\n")
	}
	return b.String()
}

// intakeSummary is Overview's one line about intake: its mode, how many
// are trusted and how many issues wait.
func intakeSummary(policy map[string]any, trusted, queued int) string {
	return fmt.Sprintf("Intake: %s · %d trusted · %d queued", firstNonempty(terminalText(text(policy, "mode")), "notify"), trusted, queued)
}
