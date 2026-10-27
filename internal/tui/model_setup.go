package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"clawbridge/internal/bus"
	"clawbridge/internal/dispatchermodel"
	tea "github.com/charmbracelet/bubbletea"
)

// The Model screen (DISPATCHER-AGENT-DESIGN §11.1, §11.9): the dispatcher
// model's status for this project, and the guided setup in the same steps
// and order as `dispatcher model setup`, through the same package. The key
// is typed into a field that is never drawn, and is wiped when the setup is
// sent, cancelled, or the screen is left.

const ScreenModel Screen = "model"
const DispatcherModel Screen = ScreenModel

// How the screen waits for the setup test: as long as the CLI does.
var (
	modelTestPoll = 2 * time.Second
	modelTestWait = 60 * time.Second
)

// maxModelKey is the longest key accepted, as in the shared package.
const maxModelKey = 16 << 10

type modelStep uint8

const (
	modelStepNone modelStep = iota
	modelStepProvider
	modelStepBaseURL
	modelStepModel
	modelStepKey
	modelStepCaps
	modelStepDisclosure
	modelStepSending
	modelStepTesting
)

var modelStepNames = map[modelStep]string{
	modelStepNone: "", modelStepProvider: "provider", modelStepBaseURL: "base_url", modelStepModel: "model",
	modelStepKey: "key", modelStepCaps: "caps", modelStepDisclosure: "disclosure", modelStepSending: "sending",
	modelStepTesting: "testing",
}

type modelSetupState struct {
	// status is dispatcher_model_doctor's answer, which the server gives
	// only to a project owner; denied says why there is none.
	status map[string]any
	denied string
	// outcome is the last setup's result, in the shared package's words.
	outcome string
	wizard  modelWizard
	// sends counts setups sent, so a reload while one is tested doesn't
	// lose its answer, and an answer to an older one is dropped.
	sends uint64
}

// modelWizard is one setup in progress. key is the only place the key
// lives before it is sent: it is never rendered, and wipeKey zeroes it in
// place, so every copy of the Model sees the zeros.
type modelWizard struct {
	step     modelStep
	choice   int
	provider dispatchermodel.Provider
	baseURL  string
	model    string
	input    string
	key      []rune
	caps     dispatchermodel.Caps
	capIndex int
	revision int64
}

// ModelSetupSnapshot is the Model screen's display state. It never holds
// the key, only whether one has been typed.
type ModelSetupSnapshot struct {
	Owner      bool
	Denied     string
	Step       string
	Provider   string
	Model      string
	KeyEntered bool
	Revision   int64
	Outcome    string
}

func (m Model) ModelSetupState() ModelSetupSnapshot {
	w := m.modelSetup.wizard
	return ModelSetupSnapshot{Owner: m.modelSetup.status != nil, Denied: m.modelSetup.denied, Step: modelStepNames[w.step],
		Provider: w.provider.ID, Model: w.model, KeyEntered: len(w.key) > 0, Revision: w.revision, Outcome: m.modelSetup.outcome}
}

type modelLoadedMsg struct {
	request uint64
	status  map[string]any
	err     error
}

type modelSentMsg struct {
	request  uint64
	revision int64
	err      error
}

type modelTestedMsg struct {
	request uint64
	outcome string
	err     error
}

// modelExec runs one operation in this TUI's trusted scope, as the CLI's
// setup does.
func (m Model) modelExec() dispatchermodel.Exec {
	executor, cfg := m.cfg.Executor, m.cfg
	return func(ctx context.Context, op string, args map[string]any) (map[string]any, error) {
		r, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: cfg.Workspace, Project: cfg.Project, Session: cfg.Session, Args: args})
		if err == nil && r.Source == "offline-mirror" {
			return nil, errors.New("the authority is offline; model setup needs the server")
		}
		return mapData(r.Data), err
	}
}

func (m Model) modelRefresh() (Model, tea.Cmd) {
	requestID := m.request
	if m.cfg.Executor == nil || m.cfg.Project == "" {
		return m, func() tea.Msg {
			return modelLoadedMsg{request: requestID, err: errors.New("the dispatcher model is set per project; open the TUI with a project")}
		}
	}
	exec, ctx := m.modelExec(), m.cfg.Context
	return m, func() tea.Msg {
		status, err := exec(ctx, "dispatcher_model_doctor", map[string]any{})
		return modelLoadedMsg{request: requestID, status: status, err: err}
	}
}

func (m Model) applyModelLoaded(x modelLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenModel {
		return m
	}
	m.loading = false
	m.source = "server"
	m.modelSetup.status, m.modelSetup.denied = nil, ""
	switch {
	case x.err != nil && strings.Contains(x.err.Error(), "owner permission"):
		m.modelSetup.denied = "Only a project owner sees and sets up the dispatcher model."
	case x.err != nil:
		m.errText = displayError(x.err)
	case x.status == nil:
		m.errText = "the server's answer had no model status"
	default:
		m.modelSetup.status = x.status
	}
	return m
}

// wipeKey zeroes the key in place and drops it.
func (w *modelWizard) wipeKey() {
	clear(w.key)
	w.key = nil
}

// closeModelSetup ends a setup, wiping the key: on cancel, on leaving the
// screen and on quitting.
func (m Model) closeModelSetup() Model {
	m.modelSetup.wizard.wipeKey()
	m.modelSetup.wizard = modelWizard{}
	return m
}

// appendKey adds typed runes to the key. A key that outgrows its buffer is
// moved to a bigger one and the old one zeroed, so no stale copy is left.
func appendKey(key, typed []rune) []rune {
	if len(key)+len(typed) <= cap(key) {
		return append(key, typed...)
	}
	grown := make([]rune, len(key), max(64, 2*(len(key)+len(typed))))
	copy(grown, key)
	clear(key)
	return append(grown, typed...)
}

func (m Model) modelKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	w := &m.modelSetup.wizard
	if key == "esc" {
		m = m.closeModelSetup()
		m.actionErr = "Setup cancelled. Nothing was set up."
		return m, nil
	}
	m.actionErr = ""
	switch w.step {
	case modelStepProvider:
		list := dispatchermodel.Providers()
		switch key {
		case "up", "k":
			w.choice = max(0, w.choice-1)
		case "down", "j":
			w.choice = min(len(list)-1, w.choice+1)
		case "enter":
			w.provider = list[w.choice]
			w.step = modelStepModel
			if w.provider.Custom {
				w.step = modelStepBaseURL
			}
		}
		return m, nil
	case modelStepKey:
		switch key {
		case "backspace", "ctrl+h":
			if n := len(w.key); n > 0 {
				w.key[n-1] = 0
				w.key = w.key[:n-1]
			}
		case "ctrl+u":
			w.wipeKey()
		case "enter":
			if len(w.key) == 0 {
				m.actionErr = "type or paste your key; Esc cancels"
				return m, nil
			}
			w.step = modelStepCaps
		default:
			if msg.Type != tea.KeyRunes {
				return m, nil
			}
			// A pasted key often ends in a newline (R143-L1): that is
			// dropped, while any other control character refuses the paste.
			typed := msg.Runes
			for len(typed) > 0 && (typed[len(typed)-1] == '\n' || typed[len(typed)-1] == '\r') {
				typed = typed[:len(typed)-1]
			}
			switch {
			case containsControl(typed):
				m.actionErr = "that paste has a control character in it; nothing was added. Paste the key again"
			case len(w.key)+len(typed) > maxModelKey:
				m.actionErr = "the key is longer than 16 KiB"
			default:
				w.key = appendKey(w.key, typed)
			}
			clear(msg.Runes)
		}
		return m, nil
	case modelStepDisclosure, modelStepBaseURL, modelStepModel, modelStepCaps:
		switch key {
		case "backspace", "ctrl+h":
			if r := []rune(w.input); len(r) > 0 {
				w.input = string(r[:len(r)-1])
			}
		case "enter":
			return m.modelNext()
		default:
			if msg.Type == tea.KeyRunes && !containsControl(msg.Runes) && len(w.input) < 2048 {
				w.input += string(msg.Runes)
			}
		}
	}
	return m, nil
}

// modelNext takes a typed answer, with the same rules as the CLI.
func (m Model) modelNext() (Model, tea.Cmd) {
	w := &m.modelSetup.wizard
	answer := strings.TrimSpace(w.input)
	w.input = ""
	switch w.step {
	case modelStepBaseURL:
		if err := dispatchermodel.ValidateBaseURL(answer); err != nil {
			m.actionErr = err.Error()
			return m, nil
		}
		w.baseURL, w.step = answer, modelStepModel
	case modelStepModel:
		shown, _ := dispatchermodel.ModelHints(w.provider.ID)
		answer = dispatchermodel.ModelAnswer(answer, shown)
		if answer == "" {
			m.actionErr = "type the model ID, as " + w.provider.Name + " names it; there is no default"
			return m, nil
		}
		// A key typed here echoed: the field is already cleared, and the
		// shared rule refuses it.
		if err := dispatchermodel.ValidateModel(answer); err != nil {
			m.actionErr = err.Error()
			return m, nil
		}
		w.model, w.step = answer, modelStepKey
	case modelStepCaps:
		field := dispatchermodel.CapFields[w.capIndex]
		if answer != "" {
			n, err := strconv.ParseInt(strings.ReplaceAll(answer, ",", ""), 10, 64)
			if err != nil {
				m.actionErr = field.Key + " must be a number"
				return m, nil
			}
			if err := w.caps.Set(field.Key, n); err != nil {
				m.actionErr = err.Error()
				return m, nil
			}
		}
		if w.capIndex < len(dispatchermodel.CapFields)-1 {
			w.capIndex++
			return m, nil
		}
		if err := w.caps.Validate(); err != nil {
			m.actionErr = err.Error()
			w.capIndex = 0
			for i, f := range dispatchermodel.CapFields {
				if w.caps.Get(f.Key) == 0 {
					w.capIndex = i
				}
			}
			return m, nil
		}
		w.step = modelStepDisclosure
	case modelStepDisclosure:
		if answer != "yes" {
			m = m.closeModelSetup()
			m.actionErr = "Setup stopped: the disclosure was not confirmed. Nothing was set up."
			return m, nil
		}
		return m.modelSend()
	}
	return m, nil
}

// modelSend sends the setup. The key moves into a byte buffer that the
// shared package zeroes once sent, and the typed runes are wiped now.
func (m Model) modelSend() (Model, tea.Cmd) {
	w := &m.modelSetup.wizard
	if m.writesBlocked() {
		m = m.closeModelSetup()
		m.actionErr = "the authority is offline; nothing was set up"
		return m, nil
	}
	key := make([]byte, 0, len(w.key)*utf8.UTFMax)
	for _, r := range w.key {
		key = utf8.AppendRune(key, r)
	}
	w.wipeKey()
	request := dispatchermodel.Request{Provider: w.provider, BaseURL: w.baseURL, Model: w.model, Key: key, Caps: w.caps, EgressConfirmed: true}
	w.step = modelStepSending
	m.modelSetup.sends++
	requestID, exec, ctx := m.modelSetup.sends, m.modelExec(), m.cfg.Context
	return m, func() tea.Msg {
		revision, err := dispatchermodel.Setup(ctx, exec, request)
		return modelSentMsg{request: requestID, revision: revision, err: err}
	}
}

func (m Model) applyModelSent(x modelSentMsg) (Model, tea.Cmd) {
	if x.request != m.modelSetup.sends || m.screen != ScreenModel || m.modelSetup.wizard.step != modelStepSending {
		return m, nil
	}
	if x.err != nil {
		m = m.closeModelSetup()
		m.actionErr = "Setup failed: " + displayError(x.err)
		return m, nil
	}
	m.modelSetup.wizard.step, m.modelSetup.wizard.revision = modelStepTesting, x.revision
	requestID, exec, ctx, revision := m.modelSetup.sends, m.modelExec(), m.cfg.Context, x.revision
	return m, func() tea.Msg {
		outcome, err := dispatchermodel.WaitForTest(ctx, exec, revision, modelTestPoll, modelTestWait)
		return modelTestedMsg{request: requestID, outcome: outcome, err: err}
	}
}

func (m Model) applyModelTested(x modelTestedMsg) (Model, tea.Cmd) {
	if x.request != m.modelSetup.sends || m.screen != ScreenModel || m.modelSetup.wizard.step != modelStepTesting {
		return m, nil
	}
	w := m.modelSetup.wizard
	m = m.closeModelSetup()
	if x.err != nil {
		m.actionErr = "The setup was sent, but its test could not be read: " + displayError(x.err)
		return m.Reload()
	}
	baseURL := w.provider.BaseURL
	if w.provider.Custom {
		baseURL = w.baseURL
	}
	m.modelSetup.outcome = dispatchermodel.OutcomeText(x.outcome, baseURL)
	if x.outcome == "active" {
		m.modelSetup.outcome += " Next, per project: dispatcher enable --engine model, or dispatcher model advise on (each with a preview)."
	}
	return m.Reload()
}

// modelStart opens a setup, for an owner whose status has loaded.
func (m Model) modelStart() Model {
	switch {
	case m.loading:
		m.actionErr = "wait for the screen to finish loading"
	case m.modelSetup.status == nil:
		m.actionErr = firstNonempty(m.modelSetup.denied, "the model status hasn't loaded; r reloads")
	case m.modelSetup.status["seal_configured"] != true:
		m.actionErr = "model setup is unavailable: the server has no model key seal key"
	case m.writesBlocked():
		m.actionErr = "the authority is offline; setup needs the server"
	default:
		m.modelSetup.outcome = ""
		m.modelSetup.wizard = modelWizard{step: modelStepProvider, caps: dispatchermodel.DefaultCaps()}
	}
	return m
}

func renderModelSetup(m Model) string {
	var b strings.Builder
	b.WriteString("Settings / Dispatcher / Model")
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("the model that decides claims, set up with your own key"))
	b.WriteString("\n\n")
	w := m.modelSetup.wizard
	if w.step != modelStepNone {
		renderModelWizard(&b, w)
		return b.String()
	}
	if m.modelSetup.outcome != "" {
		b.WriteString(warnStyle.Render(terminalText(m.modelSetup.outcome)) + "\n\n")
	}
	if m.modelSetup.denied != "" {
		b.WriteString(m.modelSetup.denied + "\nAn owner runs setup here, or with clawbridge dispatcher model setup.")
		return b.String()
	}
	s := m.modelSetup.status
	if s == nil {
		b.WriteString("Not loaded. r reloads.")
		return b.String()
	}
	if s["seal_configured"] != true {
		b.WriteString(kv("Setup", "unavailable: the server has no CLAWBRIDGE_MODEL_KEY_SEAL_KEY"))
	}
	on := "off (the rules decide)"
	if s["model_on"] == true {
		on = "on"
	}
	b.WriteString(kv("Model mode", on))
	var yours map[string]any
	for _, h := range records(s["holders"]) {
		if boolean(h, "you") {
			yours = h
		}
	}
	switch {
	case yours == nil:
		b.WriteString(kv("Your key", "none"))
	case text(yours, "key") == "active":
		b.WriteString(kv("Your key", fmt.Sprintf("active (revision %d)", integer(yours, "config_revision"))))
	default:
		b.WriteString(kv("Your key", fmt.Sprintf("setup test pending (revision %d)", integer(yours, "pending_revision"))))
	}
	if yours != nil {
		if f := text(yours, "test_failure"); f != "" {
			b.WriteString(kv("Last test", fmt.Sprintf("revision %d failed: %s", integer(yours, "test_revision"), terminalText(f))))
		}
		b.WriteString(kv("Your agent", firstNonempty(terminalText(text(yours, "agent")), "never seen")))
	}
	if n := len(records(s["holders"])); n > 0 {
		b.WriteString(kv("Owners with a key", strconv.Itoa(n)))
	}
	b.WriteString(mutedStyle.Render("\ns set up or change the model (provider, model, key, caps) · r reload · details: clawbridge doctor"))
	return b.String()
}

func renderModelWizard(b *strings.Builder, w modelWizard) {
	field := func(prompt, value string) {
		b.WriteString(prompt + "\n> " + terminalText(value) + "█\n")
	}
	if w.provider.ID != "" {
		b.WriteString(kv("Provider", terminalText(w.provider.Name+" ("+w.provider.ID+")")))
	}
	if w.baseURL != "" {
		b.WriteString(kv("Base URL", terminalText(w.baseURL)))
	}
	if w.model != "" {
		b.WriteString(kv("Model", terminalText(w.model)))
	}
	switch w.step {
	case modelStepProvider:
		b.WriteString("1. Provider (↑↓ select, Enter picks)\n")
		for i, p := range dispatchermodel.Providers() {
			line := fmt.Sprintf("  %d. %s (%s)", i+1, p.Name, p.ID)
			if i == w.choice {
				line = accentStyle.Render("›" + line[1:])
			}
			b.WriteString(line + "\n")
		}
	case modelStepBaseURL:
		field("1. Base URL (https://…/v1)", w.input)
	case modelStepModel:
		shown, more := dispatchermodel.ModelHints(w.provider.ID)
		if len(shown) == 0 {
			field("2. Model ID, as "+w.provider.Name+" names it (no default)", w.input)
			break
		}
		b.WriteString("2. Model: a number from the agent's catalogue, or any model ID (no default)\n")
		for i, m := range shown {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, terminalText(dispatchermodel.HintLine(m))))
		}
		b.WriteString(mutedStyle.Render("  "+dispatchermodel.HintsNote(more)) + "\n")
		field("Model", w.input)
	case modelStepKey:
		// Never the key, nor its length.
		state := "nothing typed yet"
		if len(w.key) > 0 {
			state = "typed (hidden)"
		}
		b.WriteString("3. " + w.provider.KeyHint + ": type or paste it; it is never shown. Ctrl-U clears it.\n> [" + state + "]\n")
	case modelStepCaps:
		b.WriteString("4. Caps (Enter keeps the value shown)\n")
		for i, f := range dispatchermodel.CapFields {
			current := strconv.FormatInt(w.caps.Get(f.Key), 10)
			if w.caps.Get(f.Key) == 0 {
				current = "required"
			}
			line := fmt.Sprintf("  %s [%s]", f.Label, current)
			if i == w.capIndex {
				line = accentStyle.Render("›"+line[1:]) + ": " + terminalText(w.input) + "█"
			}
			b.WriteString(line + "\n")
		}
	case modelStepDisclosure:
		b.WriteString("5. " + dispatchermodel.Disclosure(w.provider) + "\n\n")
		field(`Type "yes" to confirm`, w.input)
	case modelStepSending:
		b.WriteString("Sending the setup…\n")
	case modelStepTesting:
		b.WriteString(fmt.Sprintf("Testing the key in your agent (revision %d); this can take up to a minute.\n", w.revision))
	}
	b.WriteString(mutedStyle.Render("\nEnter next · Esc cancels (nothing is set up before the confirmation)"))
}
