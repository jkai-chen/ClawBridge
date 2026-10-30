package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// Who may cancel (AGENT-CANCEL R11) is a view inside the Linear screen, C
// from the board: the project's kinds, each switched between "agents may
// cancel or mark Duplicate" and not. Enter previews the result. A result
// that adds a kind is typed by the person, as the CLI's is: the kind names,
// comma-separated, exactly as shown; a result that only removes kinds asks
// y. Both go to the server at the revision that was read. The server decides
// who may change it; the TUI only refuses before sending what it would.

// CancelKindsChange is a who-may-cancel change: the full list of kinds
// agents may cancel in, at the status policy revision the view read.
type CancelKindsChange struct {
	Kinds    []string
	Revision int64
}

// cancelAsk is a previewed change waiting for the person: typed when it
// adds kinds, y when it only removes.
type cancelAsk struct {
	kinds []string
	typed bool
	words string
}

type cancelState struct {
	loaded bool
	view   linearboard.CancelKindsView
	// want is each kind's switch, by the kind as the project spells it.
	want map[string]bool
	ask  *cancelAsk
}

// cancelMsg is a read, or a change, of who may cancel; change is nil for a
// read.
type cancelMsg struct {
	request uint64
	change  *CancelKindsChange
	view    linearboard.CancelKindsView
	err     error
}

func (m Model) openCancelKinds() (Model, tea.Cmd) {
	m.linear.step, m.selected = "cancel", 0
	m.cancel = cancelState{}
	return m.cancelLoad()
}

// cancelLoad reads the view, on open and on r.
func (m Model) cancelLoad() (Model, tea.Cmd) {
	requestID := m.request
	if m.cfg.Linear == nil {
		return m, func() tea.Msg {
			return cancelMsg{request: requestID, err: errors.New("Linear isn't available in this TUI")}
		}
	}
	m.loading = true
	source, ctx, profile := m.cfg.Linear, m.cfg.Context, m.linear.profile
	return m, func() tea.Msg {
		d, err := source.Do(ctx, LinearRequest{Operation: "cancel-kinds", ProfileID: profile})
		var v linearboard.CancelKindsView
		if d.CancelKindsView != nil {
			v = *d.CancelKindsView
		} else if err == nil {
			err = errors.New("the server's answer had no who-may-cancel setting")
		}
		return cancelMsg{request: requestID, view: v, err: err}
	}
}

func (m Model) applyCancel(x cancelMsg) (Model, tea.Cmd) {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "cancel" {
		return m, nil
	}
	m.loading = false
	if x.err != nil {
		m.cancel.ask = nil
		if x.change != nil && strings.Contains(x.err.Error(), "status policy changed") {
			// Someone else changed it first: read it again and say so.
			m.status = errStatus(fmt.Errorf("%w; it was read again, and nothing was changed", x.err))
			return m.cancelLoad()
		}
		m.status = errStatus(x.err)
		return m, nil
	}
	m.cancel = cancelState{loaded: true, view: x.view, want: map[string]bool{}}
	for _, k := range x.view.Kinds {
		m.cancel.want[k] = cancelHas(x.view.CancelKinds, k)
	}
	if n := len(x.view.Kinds); m.selected >= n {
		m.selected = max(0, n-1)
	}
	if x.change != nil {
		m.status = okStatus("Who may cancel saved. " + x.view.Line)
	}
	return m, nil
}

func cancelHas(kinds []string, kind string) bool {
	return slices.ContainsFunc(kinds, func(k string) bool { return strings.EqualFold(k, kind) })
}

// cancelWritable is false in a TUI with a session or run by an agent: who
// may cancel is a person's decision (the server refuses a session's).
func (m Model) cancelWritable() bool {
	return m.cfg.Session == "" && m.cfg.AgentMarker == ""
}

func (m Model) cancelRefusal() string {
	if m.cfg.Session != "" {
		return "who may cancel is never set with a session; open the TUI without --session to change it. Nothing was changed"
	}
	return "letting agents cancel issues is a person's decision, and " + m.cfg.AgentMarker + " says an agent runs this TUI; run the TUI yourself. Nothing was changed"
}

// cancelDesired is the list the switches make, in the project's kind order.
func (m Model) cancelDesired() []string {
	out := []string{}
	for _, k := range m.cancel.view.Kinds {
		if m.cancel.want[k] {
			out = append(out, k)
		}
	}
	return out
}

// cancelKey runs a key on the view.
func (m Model) cancelKey(key string) (Model, tea.Cmd) {
	if !m.cancel.loaded || m.loading {
		return m, nil
	}
	kinds := m.cancel.view.Kinds
	if len(kinds) == 0 {
		return m, nil
	}
	switch key {
	case " ":
		if !m.cancelWritable() {
			m.status = noteStatus(m.cancelRefusal())
			return m, nil
		}
		if m.selected < 0 || m.selected >= len(kinds) {
			return m, nil
		}
		want := map[string]bool{}
		for k, v := range m.cancel.want {
			want[k] = v
		}
		want[kinds[m.selected]] = !want[kinds[m.selected]]
		m.cancel.want, m.cancel.ask = want, nil
	case "enter":
		return m.cancelPreview()
	}
	return m, nil
}

// cancelPreview shows the result and asks the person: typed names when it
// adds a kind, y when it only removes.
func (m Model) cancelPreview() (Model, tea.Cmd) {
	if !m.cancelWritable() {
		m.status = noteStatus(m.cancelRefusal())
		return m, nil
	}
	next, current := m.cancelDesired(), m.cancel.view.CancelKinds
	added := false
	for _, k := range next {
		added = added || !cancelHas(current, k)
	}
	if !added && len(next) == len(current) {
		m.status = noteStatus("Nothing to change. " + m.cancel.view.Line)
		return m, nil
	}
	words := "Result: " + bus.CancelKindsLine(next)
	if added {
		words = "Agents will be allowed to cancel or mark Duplicate issues in the Linear projects of kind " + strings.Join(next, ", ") + ", without a person.\n" + words
	}
	m.cancel.ask = &cancelAsk{kinds: next, typed: added, words: words}
	m.status = status{}
	if added {
		m.inputMode, m.draft = inputLinearCancelKinds, ""
	}
	return m, nil
}

// cancelModalKey is a key while a removal waits for y.
func (m Model) cancelModalKey(key string) (Model, tea.Cmd) {
	ask := m.cancel.ask
	m.cancel.ask = nil
	if key != "y" {
		m.status = noteStatus("Nothing was changed.")
		return m, nil
	}
	return m.cancelSend(ask.kinds)
}

// cancelTyped checks the kind names typed: exactly the list shown.
func (m Model) cancelTyped() (Model, tea.Cmd) {
	ask := m.cancel.ask
	typed := []string{}
	for _, name := range strings.Split(m.draft, ",") {
		typed = append(typed, strings.TrimSpace(name))
	}
	m.inputMode, m.draft = inputNone, ""
	if ask == nil || !slices.Equal(typed, ask.kinds) {
		want := ""
		if ask != nil {
			want = strings.Join(ask.kinds, ", ")
		}
		m.cancel.ask = nil
		m.status = noteStatus(fmt.Sprintf("the names typed are not %q; nothing was changed", want))
		return m, nil
	}
	m.cancel.ask = nil
	return m.cancelSend(ask.kinds)
}

// cancelSend sets the list at the revision that was read.
func (m Model) cancelSend(kinds []string) (Model, tea.Cmd) {
	if !m.cancelWritable() {
		m.status = noteStatus(m.cancelRefusal())
		return m, nil
	}
	if m.writesBlocked() {
		m.status = noteStatus("The authority is offline; who may cancel needs the server.")
		return m, nil
	}
	m.loading = true
	change := &CancelKindsChange{Kinds: append([]string{}, kinds...), Revision: m.cancel.view.Revision}
	requestID, source, ctx, profile := m.request, m.cfg.Linear, m.cfg.Context, m.linear.profile
	return m, func() tea.Msg {
		d, err := source.Do(ctx, LinearRequest{Operation: "cancel-kinds-set", ProfileID: profile, CancelKinds: change})
		var v linearboard.CancelKindsView
		if d.CancelKindsView != nil {
			v = *d.CancelKindsView
		} else if err == nil {
			err = errors.New("the server's answer had no who-may-cancel setting")
		}
		return cancelMsg{request: requestID, change: change, view: v, err: err}
	}
}

func renderCancelKinds(m Model) string {
	var b strings.Builder
	if !m.cancel.loaded {
		if m.loading {
			b.WriteString("Reading who may cancel…\n")
		}
		return b.String()
	}
	v := m.cancel.view
	fmt.Fprintf(&b, "Revision %d · %s\n", v.Revision, terminalText(v.Line))
	if len(v.Kinds) == 0 {
		b.WriteString("\nThis project has no kinds yet: the project owner sets them with clawbridge linear kind set\n")
		return b.String()
	}
	if !m.cancelWritable() {
		b.WriteString(warnStyle.Render(terminalText(m.cancelRefusal())) + "\n")
	}
	b.WriteString("\nSpace switches whether agents may cancel or mark Duplicate issues in the Linear projects of a kind; Enter previews.\n")
	for i, k := range v.Kinds {
		prefix, check := "  ", "[ ]"
		if i == m.selected {
			prefix = "› "
		}
		if m.cancel.want[k] {
			check = "[x]"
		}
		b.WriteString(prefix + check + " " + terminalText(k) + "\n")
	}
	if ask := m.cancel.ask; ask != nil && (!ask.typed || m.inputMode == inputLinearCancelKinds) {
		b.WriteString("\n" + terminalText(ask.words) + "\n")
	}
	return b.String()
}

// cancelKeysOn: Space switches a kind once the view has some.
func cancelKeysOn(m Model) bool {
	return m.linearStep() == "cancel" && m.cancel.loaded && len(m.cancel.view.Kinds) > 0
}
