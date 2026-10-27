package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The one keymap (live/TUI-UX-AUDIT.md §3.1). The key dispatch, the footer
// and the ? overlay all read these tables, so none can offer a key the
// others don't know. A key in no live binding does nothing.
//
// Everywhere: Enter opens or drills into the selected row and never writes
// by itself; Esc goes back exactly one level; q asks first while work is
// held. A screen's letters never reuse a global key. Left and Right are
// left for panes.

// Binding is one key, or a few keys for one action.
type Binding struct {
	// ID names the action for lookups (keyHint): "reload", "open",
	// "tab:doctor".
	ID string
	// Keys are bubbletea key strings; the first is the one shown.
	Keys []string
	// Show replaces the first key in the footer and overlay ("↑↓").
	Show string
	// Label says what it does, as the footer and overlay show it.
	Label string
	// Labeler, when set, gives a label that depends on the state.
	Labeler func(Model) string
	// When reports whether it applies now; a binding that doesn't is
	// neither shown nor dispatched. nil means always.
	When   func(Model) bool
	Action func(Model, string) (Model, tea.Cmd)
}

func (b Binding) live(m Model) bool { return b.When == nil || b.When(m) }

func (b Binding) label(m Model) string {
	if b.Labeler != nil {
		return b.Labeler(m)
	}
	return b.Label
}

func (b Binding) key() string {
	if b.Show != "" {
		return b.Show
	}
	return keyName(b.Keys[0])
}

// keyName is a key as people read it.
func keyName(k string) string {
	switch k {
	case "enter":
		return "Enter"
	case "esc":
		return "Esc"
	case " ":
		return "Space"
	case "tab":
		return "Tab"
	case "shift+tab":
		return "S-Tab"
	case "pgup":
		return "PgUp"
	case "pgdown":
		return "PgDn"
	}
	return k
}

// tabs are the screens this profile can use: the work screens, then the
// settings ones. Account needs a hosted workspace; the setup screen
// (Cloud) shows only while a setup adapter is wired.
func (m Model) tabs() []Screen {
	if m.waiting {
		// Only what works without a project.
		out := []Screen{ScreenOverview}
		if m.cfg.Mode == ModeCloud {
			out = append(out, ScreenAccount)
		}
		return append(out, ScreenDoctor)
	}
	out := []Screen{ScreenOverview, ScreenProjects, ScreenSessions, ScreenInbox, ScreenLinear, ScreenModel}
	if m.cfg.Mode == ModeCloud {
		out = append(out, ScreenAccount)
	}
	out = append(out, ScreenDoctor)
	if m.cfg.Setup != nil || m.screen == ScreenCloud {
		out = append(out, ScreenCloud)
	}
	return out
}

// hasTab reports whether s is one of this profile's tabs.
func (m Model) hasTab(s Screen) bool {
	for _, t := range m.tabs() {
		if t == s {
			return true
		}
	}
	return false
}

// tabStep is the screen delta tabs from this one, wrapping.
func (m Model) tabStep(delta int) Screen {
	tabs := m.tabs()
	i := -1
	for j, s := range tabs {
		if s == m.screen {
			i = j
		}
	}
	if i < 0 && delta < 0 {
		i = 0
	}
	n := len(tabs)
	return tabs[((i+delta)%n+n)%n]
}

// globalBindings work on every screen, after the screen's own.
func globalBindings(m Model) []Binding {
	out := []Binding{}
	for i, s := range m.tabs() {
		if i >= 9 {
			break
		}
		s := s
		out = append(out, Binding{ID: "tab:" + string(s), Keys: []string{strconv.Itoa(i + 1)}, Label: title(s), Action: func(m Model, _ string) (Model, tea.Cmd) { return m.Show(s) }})
	}
	return append(out,
		Binding{ID: "next", Keys: []string{"tab"}, Label: "next screen", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.Show(m.tabStep(1)) }},
		Binding{ID: "previous", Keys: []string{"shift+tab"}, Label: "previous screen", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.Show(m.tabStep(-1)) }},
		Binding{ID: "move", Keys: []string{"up", "k", "down", "j"}, Show: "↑↓", Label: "select", Action: func(m Model, key string) (Model, tea.Cmd) {
			if key == "up" || key == "k" {
				return m.Move(-1), nil
			}
			return m.Move(1), nil
		}},
		Binding{ID: "scroll", Keys: []string{"pgup", "pgdown"}, Show: "PgUp/PgDn", Label: "scroll", Action: func(m Model, key string) (Model, tea.Cmd) {
			if key == "pgup" {
				return m.Scroll(-max(1, m.height/2)), nil
			}
			return m.Scroll(max(1, m.height/2)), nil
		}},
		Binding{ID: "back", Keys: []string{"esc"}, Label: "back", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.back() }},
		Binding{ID: "reload", Keys: []string{"r"}, Label: "reload", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.Reload() }},
		Binding{ID: "help", Keys: []string{"?"}, Label: "help", Action: func(m Model, _ string) (Model, tea.Cmd) { m.help = true; return m, nil }},
		Binding{ID: "quit", Keys: []string{"q"}, Label: "quit", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.quit() }},
	)
}

// screenBindings are a screen's own keys.
func screenBindings(s Screen) []Binding {
	switch s {
	case ScreenOverview:
		return overviewBindings
	case ScreenDoctor:
		return doctorBindings
	case ScreenProjects:
		return projectBindings
	case ScreenSessions:
		return append(append([]Binding{}, sessionBindings...), pageBindings...)
	case ScreenInbox:
		return append(append([]Binding{}, inboxBindings...), pageBindings...)
	case ScreenLinear:
		return linearBindings
	case ScreenAccount:
		return accountBindings
	case ScreenModel:
		return modelBindings
	case ScreenCloud:
		return cloudBindings
	}
	return nil
}

var pageBindings = []Binding{
	{ID: "filter", Keys: []string{"v"}, Label: "filter", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.CycleFilter(), nil }},
	{ID: "next-page", Keys: []string{"]"}, Label: "next page", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.NextPage() }},
	{ID: "previous-page", Keys: []string{"["}, Label: "previous page", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.PreviousPage() }},
}

func hasMessage(m Model) bool { _, ok := m.SelectedMessage(); return ok }

var inboxBindings = []Binding{
	{ID: "open", Keys: []string{"enter"}, Label: "open", When: func(m Model) bool { return hasMessage(m) && !m.inboxOpen }, Action: func(m Model, _ string) (Model, tea.Cmd) {
		m.inboxOpen = true
		return m, nil
	}},
	{ID: "reply", Keys: []string{"p"}, Label: "reply", When: hasMessage, Action: func(m Model, _ string) (Model, tea.Cmd) { return m.BeginReply(), nil }},
	{ID: "send", Keys: []string{"n"}, Label: "new message", Action: func(m Model, _ string) (Model, tea.Cmd) { return m.BeginSend(), nil }},
	{ID: "mark-read", Keys: []string{"m"}, Label: "mark read", When: hasMessage, Action: func(m Model, _ string) (Model, tea.Cmd) {
		selected, _ := m.SelectedMessage()
		return m.MarkRead([]string{selected.ID})
	}},
}

// intakeEditable is the Intake view, read, for a project owner or lead.
func intakeEditable(m Model) bool {
	return m.linearStep() == "intake" && m.intake.loaded && m.intake.canEdit
}

// intakeOn says the selected Intake row is one of kinds.
func intakeOn(m Model, kinds ...string) bool {
	if m.linearStep() != "intake" {
		return false
	}
	kind, _ := m.intakeRow()
	for _, k := range kinds {
		if kind == k {
			return true
		}
	}
	return false
}

func linearAt(steps ...string) func(Model) bool {
	return func(m Model) bool {
		for _, s := range steps {
			if m.linearStep() == s {
				return true
			}
		}
		return false
	}
}

func linearKeyAction(m Model, key string) (Model, tea.Cmd) { return m.linearKey(key) }

var linearBindings = []Binding{
	{ID: "open", Keys: []string{"enter"}, Labeler: func(m Model) string {
		switch m.linearStep() {
		case "tasks":
			return "open"
		case "details":
			return "back to tasks"
		case "preview":
			return "link"
		case "unlink":
			return "unlink"
		}
		return "next"
	}, When: func(m Model) bool { return m.linearStep() != "intake" }, Action: linearKeyAction},
	{ID: "select", Keys: []string{" "}, Labeler: func(m Model) string {
		switch m.linearStep() {
		case "preview":
			return "people or agents close"
		case "intake":
			return "switch"
		}
		return "select"
	}, When: func(m Model) bool {
		return m.linearStep() == "teams" || m.linearStep() == "preview" || (m.linearStep() == "projects" && m.linear.bindingV2 != nil) ||
			(intakeEditable(m) && intakeOn(m, "mode", "signal"))
	}, Action: linearKeyAction},
	{ID: "intake", Keys: []string{"n"}, Label: "Intake", When: func(m Model) bool { return m.linearConfigured() && linearAt("profiles", "tasks", "details")(m) }, Action: func(m Model, _ string) (Model, tea.Cmd) { return m.openIntake() }},
	{ID: "ids", Keys: []string{"d"}, Labeler: func(m Model) string {
		if m.linear.showIDs {
			return "hide IDs"
		}
		return "show IDs"
	}, When: func(m Model) bool { return m.linearConfigured() || m.linearStep() == "preview" }, Action: func(m Model, _ string) (Model, tea.Cmd) {
		m.linear.showIDs = !m.linear.showIDs
		return m, nil
	}},
	{ID: "connect", Keys: []string{"+"}, Label: "connect", When: linearAt("profiles"), Action: linearKeyAction},
	{ID: "connect-write", Keys: []string{"w"}, Label: "connect with write", When: linearAt("profiles"), Action: linearKeyAction},
	{ID: "link", Keys: []string{"l"}, Label: "link a board", When: func(m Model) bool { return m.linear.profile != "" && linearAt("profiles", "tasks")(m) }, Action: linearKeyAction},
	{ID: "unlink", Keys: []string{"u"}, Label: "unlink", When: func(m Model) bool { return m.linearConfigured() && linearAt("profiles", "tasks", "details")(m) }, Action: linearKeyAction},
	{ID: "logout", Keys: []string{"x"}, Labeler: func(m Model) string {
		switch {
		case intakeOn(m, "trusted"):
			return "stop trusting"
		case intakeOn(m, "queue"):
			return "dismiss"
		}
		return "log out"
	}, When: func(m Model) bool {
		return (m.linearStep() == "profiles" && m.linearSelected() != "") || (intakeEditable(m) && intakeOn(m, "trusted", "queue"))
	}, Action: linearKeyAction},
	{ID: "intake-trust", Keys: []string{"a"}, Label: "trust someone", When: intakeEditable, Action: linearKeyAction},
	{ID: "intake-rearm", Keys: []string{"e"}, Label: "rearm", When: func(m Model) bool { return intakeEditable(m) && intakeOn(m, "queue") }, Action: linearKeyAction},
	{ID: "next-page", Keys: []string{"]"}, Label: "next page", When: linearAt("teams", "projects", "tasks"), Action: func(m Model, _ string) (Model, tea.Cmd) { return m.linearPage(true) }},
	{ID: "previous-page", Keys: []string{"["}, Label: "previous page", When: linearAt("teams", "projects", "tasks"), Action: func(m Model, _ string) (Model, tea.Cmd) { return m.linearPage(false) }},
}

func accountReady(m Model) bool { return m.cfg.Mode == ModeCloud && m.account.principal != "" }

func accountKeyAction(m Model, key string) (Model, tea.Cmd) { return m.accountKey(key) }

func offeredToMe(m Model) bool {
	return accountReady(m) && m.account.transfer != nil && text(m.account.transfer, "to") == m.account.principal
}

var accountBindings = []Binding{
	{ID: "add-device", Keys: []string{"a"}, Label: "add a device", When: accountReady, Action: accountKeyAction},
	{ID: "invite", Keys: []string{"i"}, Label: "invite", When: accountReady, Action: accountKeyAction},
	{ID: "label", Keys: []string{"l"}, Label: "label", When: accountReady, Action: accountKeyAction},
	{ID: "revoke", Keys: []string{"x"}, Label: "revoke", When: accountReady, Action: accountKeyAction},
	{ID: "confirm-device", Keys: []string{"w"}, Label: "confirm", When: accountReady, Action: accountKeyAction},
	{ID: "ack", Keys: []string{"e"}, Label: "mark the alerts seen", When: func(m Model) bool { return accountReady(m) && len(m.account.events) > 0 }, Action: accountKeyAction},
	{ID: "transfer", Keys: []string{"t"}, Label: "offer ownership", When: func(m Model) bool {
		return accountReady(m) && m.account.mainOwner && m.account.transfer == nil
	}, Action: accountKeyAction},
	{ID: "accept", Keys: []string{"y"}, Label: "accept ownership", When: offeredToMe, Action: accountKeyAction},
	{ID: "decline", Keys: []string{"d"}, Label: "decline ownership", When: offeredToMe, Action: accountKeyAction},
	{ID: "cancel-offer", Keys: []string{"c"}, Label: "cancel the offer", When: func(m Model) bool {
		return accountReady(m) && m.account.transfer != nil && text(m.account.transfer, "from") == m.account.principal
	}, Action: accountKeyAction},
}

var modelBindings = []Binding{
	{ID: "setup", Keys: []string{"s"}, Label: "set up the model", When: func(m Model) bool { return m.modelSetup.wizard.step == modelStepNone }, Action: func(m Model, _ string) (Model, tea.Cmd) { return m.modelStart(), nil }},
}

var cloudBindings = []Binding{
	{ID: "apply", Keys: []string{"a"}, Label: "confirm setup", When: func(m Model) bool { return m.plan != nil && m.cfg.Setup != nil }, Action: func(m Model, _ string) (Model, tea.Cmd) {
		m.confirming, m.confirm, m.status = true, "", status{}
		return m, nil
	}},
}

// binding finds the live binding for key: the screen's first, then the
// global ones.
func (m Model) binding(key string) (Binding, bool) {
	for _, set := range [][]Binding{screenBindings(m.screen), globalBindings(m)} {
		for _, b := range set {
			if !b.live(m) {
				continue
			}
			for _, k := range b.Keys {
				if k == key {
					return b, true
				}
			}
		}
	}
	return Binding{}, false
}

// keyHint is how a body message names an action of screen s: "r reload",
// "8 Doctor"; "" when no live binding has that ID.
func keyHint(m Model, s Screen, id string) string {
	m.screen = s
	for _, set := range [][]Binding{screenBindings(s), globalBindings(m)} {
		for _, b := range set {
			if b.ID == id && b.live(m) {
				return b.key() + " " + b.label(m)
			}
		}
	}
	return ""
}

// footerText is the screen's live keys, then help and quit; on a narrow
// terminal the screen's keys come first and the rest is cut.
func footerText(m Model) string {
	if m.screen == ScreenModel && m.modelSetup.wizard.step >= modelStepProvider && m.modelSetup.wizard.step <= modelStepDisclosure {
		return "Enter next · Esc cancel setup · Ctrl-C quit"
	}
	if m.screen == ScreenModel && m.modelSetup.wizard.step == modelStepSending {
		return "Sending… · Ctrl-C quit"
	}
	if m.screen == ScreenModel && m.modelSetup.wizard.step == modelStepTesting {
		return "Esc stops waiting · Ctrl-C quit"
	}
	if m.quitAsk {
		return "y quit · any other key stays"
	}
	if m.screen == ScreenAccount && m.account.invite != nil {
		return "↑↓ project · Space role · Enter send the invite · Esc back to the email"
	}
	if m.screen == ScreenLinear && m.linear.step == "intake" && m.intake.ask != nil {
		return "y do it · any other key keeps things as they are"
	}
	if m.screen == ScreenLinear && m.linear.step == "intake" && m.intake.picking {
		return "↑↓ member · Enter trust them · Esc close"
	}
	if m.help {
		return "Esc or ? closes help"
	}
	if m.inputMode != inputNone || m.confirming {
		if m.width < 40 {
			return "Esc cancel · Enter send"
		}
		return "Enter submit · Esc cancel · Ctrl-C quit"
	}
	parts := []string{}
	for _, b := range screenBindings(m.screen) {
		if b.live(m) {
			parts = append(parts, b.key()+" "+b.label(m))
		}
	}
	if m.backTarget() != "" {
		parts = append(parts, "Esc "+m.backTarget())
	}
	tail := "↑↓ select · r reload · ? help · q quit"
	if m.width < 100 {
		tail = "? help · q quit"
	}
	// Too narrow: the screen's last keys give way, and "? help" stays,
	// since help lists them all.
	for len(parts) > 0 && ansi.StringWidth(strings.Join(append(parts, tail), " · ")) > m.width {
		parts, tail = parts[:len(parts)-1], "… · ? help"
	}
	return strings.Join(append(parts, tail), " · ")
}

// screenAbout is what a screen is for, shown in its help.
var screenAbout = map[Screen]string{
	ScreenOverview: "Where you are: the workspace, project and session, and who you're signed in as.",
	ScreenProjects: "The workspace's projects you can see: your role, sessions, coordinator and Linear board. ● marks the project this profile works in. Enter opens one, read-only; s and l go to your own project's sessions and board.",
	ScreenSessions: "The project's sessions. ★ marks the coordinator; STATE is retired, moved, stale or the session's presence; n messages the selected session. v filters this page only; [ goes to the previous page you saw, ] to the server's next page. There is no search across pages.",
	ScreenInbox:    "The current session's messages. The server stamps delivery; a reload never marks a message read.",
	ScreenLinear:   "The project's Linear board: your personal Linear connection, and the board this project is linked to. Task ownership comes from the coordinator's issue assignment; selecting a row never claims or completes work. In Intake (n), only a needs-agent label, delegation or mention a trusted person makes in Linear itself counts; signals made through ClawBridge run as the app and are never trusted.",
	ScreenAccount:  "Your devices and the workspace's members and invites. Anything that revokes or mints a code asks you to type a word first.",
	ScreenModel:    "The dispatcher's model: the provider, model and key it uses, and its caps.",
	ScreenDoctor:   "Checks of this profile, the server and the project: pass, warn or fail.",
	ScreenCloud:    "Setting up a server. Nothing is created or changed until you confirm.",
}

// renderHelp is the ? overlay: this screen's keys, then everyone's.
func renderHelp(m Model) string {
	var b strings.Builder
	b.WriteString("Help · " + title(m.screen) + "\n")
	b.WriteString(mutedStyle.Render(screenAbout[m.screen]) + "\n\n")
	row := func(k, label string) { b.WriteString("  " + padRight(k, 11) + label + "\n") }
	own := screenBindings(m.screen)
	if len(own) > 0 {
		b.WriteString("This screen\n")
		for _, x := range own {
			label := x.label(m)
			if !x.live(m) {
				label += mutedStyle.Render(" (not now)")
			}
			row(x.key(), label)
		}
		b.WriteString("\n")
	}
	b.WriteString("Everywhere\n")
	tabs := []string{}
	for _, x := range globalBindings(m) {
		if strings.HasPrefix(x.ID, "tab:") {
			tabs = append(tabs, x.key()+" "+x.label(m))
			continue
		}
		row(x.key(), x.label(m))
	}
	row("1-"+strconv.Itoa(len(tabs)), "switch screen")
	b.WriteString("  " + strings.Join(tabs, " · "))
	// Wrapped at words here, so the body's hard wrap never splits one.
	return ansi.Wrap(strings.TrimRight(b.String(), "\n"), max(20, m.width-4), " ")
}

func padRight(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s + " "
}

// back is Esc: one level back, never more.
func (m Model) back() (Model, tea.Cmd) {
	switch {
	case m.screen == ScreenInbox && m.inboxOpen:
		m.inboxOpen = false
		return m, nil
	case m.screen == ScreenProjects && m.projectOpen:
		m.projectOpen = false
		return m, nil
	case m.screen == ScreenLinear:
		return m.linearBack()
	}
	return m.CancelInput(), nil
}

// backTarget names where Esc goes, for the footer; "" when nowhere.
func (m Model) backTarget() string {
	switch {
	case m.screen == ScreenInbox && m.inboxOpen, m.screen == ScreenProjects && m.projectOpen:
		return "back to the list"
	case m.screen == ScreenLinear:
		if prev := linearPrevious[m.linearStep()]; prev != "" {
			return "back to " + prev
		}
	}
	return ""
}

// holdsWork names work that quitting would lose, or "".
func (m Model) holdsWork() string {
	switch {
	case m.screen == ScreenLinear && linearAt("teams", "default", "projects", "preview")(m):
		return "the board you're linking"
	case m.modelSetup.wizard.step == modelStepSending || m.modelSetup.wizard.step == modelStepTesting:
		return "the model test that's running"
	}
	return ""
}

// quit asks first while work is held.
func (m Model) quit() (Model, tea.Cmd) {
	if what := m.holdsWork(); what != "" && !m.quitAsk {
		m.quitAsk = true
		m.status = noteStatus("Quit and lose " + what + "? y quits; any other key stays.")
		return m, nil
	}
	if m.linearCancel != nil {
		m.linearCancel()
	}
	return m.closeModelSetup(), tea.Quit
}
