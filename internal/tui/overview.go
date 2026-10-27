package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Overview is a person's home screen (TUI-UX-AUDIT PR D): what needs them,
// each line naming the tab that deals with it, then who and where they are.
// The attention lines come from reads that already exist, each best effort:
// one that fails, or that this person may not make, adds nothing.

// attentionItem is one thing that needs the person, and where it's dealt
// with.
type attentionItem struct {
	Text   string
	Screen Screen
}

// attentionMsg carries the attention lines, and the doctor's checks they
// were made from, so Doctor shows them at once.
type attentionMsg struct {
	request uint64
	items   []attentionItem
	checks  []Check
	ran     bool
	at      time.Time
	// intake is the project's intake summary, when it could be read.
	intake string
}

// checkScreen is the screen where a check's problem is dealt with, if a
// screen deals with it.
func checkScreen(id string) Screen {
	switch {
	case strings.HasPrefix(id, "linear."):
		return ScreenLinear
	case strings.HasPrefix(id, "dispatcher."):
		return ScreenModel
	case id == "auth.device":
		return ScreenAccount
	}
	return ""
}

// loadAttention makes the attention lines. It runs beside the Overview
// load, so a slow doctor never holds the screen.
func (m Model) loadAttention(requestID uint64) tea.Cmd {
	if m.cfg.Executor == nil {
		return nil
	}
	return func() tea.Msg {
		ctx := m.cfg.Context
		out := attentionMsg{request: requestID}
		if m.cfg.Doctor != nil {
			if checks, err := m.cfg.Doctor.Checks(ctx); err == nil {
				out.checks, out.ran, out.at = checks, true, m.cfg.Now()
				fail, warn := 0, 0
				for _, c := range checks {
					switch c.Status {
					case Fail:
						fail++
					case Warn:
						warn++
					}
				}
				parts := []string{}
				if fail > 0 {
					parts = append(parts, plural(fail, "check fails", "checks fail"))
				}
				if warn > 0 {
					parts = append(parts, plural(warn, "warning", "warnings"))
				}
				if len(parts) > 0 {
					out.items = append(out.items, attentionItem{Text: "Doctor: " + strings.Join(parts, ", "), Screen: ScreenDoctor})
				}
			}
		}
		if m.cfg.Linear != nil && m.cfg.Project != "" {
			if d, err := m.cfg.Linear.Do(ctx, LinearRequest{Operation: "profiles"}); err == nil && !d.Binding.Configured {
				text := "This project's Linear board isn't linked (optional)"
				if len(d.Profiles) == 0 {
					text = "Linear isn't connected (optional)"
				}
				out.items = append(out.items, attentionItem{Text: text, Screen: ScreenLinear})
			}
		}
		if m.cfg.Mode == ModeCloud {
			read := func(op string) map[string]any {
				r, err := m.cfg.Executor.Execute(ctx, m.accountRequest(op, map[string]any{}))
				if err != nil || r.Source == "offline-mirror" {
					return nil
				}
				return mapData(r.Data)
			}
			if t := mapData(read("owner_transfer_get")["pending"]); t != nil {
				me := text(read("whoami"), "principal")
				switch me {
				case text(t, "to"):
					out.items = append(out.items, attentionItem{Text: "Ownership offered to you by " + terminalText(text(t, "from")), Screen: ScreenAccount})
				case text(t, "from"):
					out.items = append(out.items, attentionItem{Text: "Your ownership offer to " + terminalText(text(t, "to")) + " is waiting for them", Screen: ScreenAccount})
				}
			}
			pending := 0
			for _, i := range records(read("invite_list")["invites"]) {
				if text(i, "status") == "pending" {
					pending++
				}
			}
			if pending > 0 {
				out.items = append(out.items, attentionItem{Text: plural(pending, "invite is", "invites are") + " waiting to be used", Screen: ScreenAccount})
			}
		}
		if m.cfg.Project != "" {
			// The project's intake in a line, a link to its view:
			// what's set and what waits, for any member.
			read := func(op string, args map[string]any) (any, bool) {
				r, err := m.cfg.Executor.Execute(ctx, m.intakeRequest(op, args))
				return r.Data, err == nil && r.Source != "offline-mirror"
			}
			policy, ok1 := read("intake_policy_get", map[string]any{})
			trusted, ok2 := read("intake_trust_list", map[string]any{})
			queued, ok3 := read("intake_list", map[string]any{"state": "queued"})
			if ok1 && ok2 && ok3 {
				out.intake = intakeSummary(mapData(policy), len(records(trusted)), len(records(queued)))
			}
		}
		if m.cfg.Project != "" {
			// Only a project owner may read it; anyone else is told
			// nothing here.
			if s, err := m.modelExec()(ctx, "dispatcher_model_doctor", map[string]any{}); err == nil && s != nil {
				mine := false
				for _, h := range records(s["holders"]) {
					if h["you"] != true {
						continue
					}
					mine = true
					if text(h, "test_failure") != "" {
						out.items = append(out.items, attentionItem{Text: "Your dispatcher model's last test failed", Screen: ScreenModel})
					} else if k := text(h, "key"); k != "" && k != "active" {
						out.items = append(out.items, attentionItem{Text: "Your dispatcher model key is " + terminalText(k), Screen: ScreenModel})
					}
				}
				if !mine && s["model_on"] != true {
					out.items = append(out.items, attentionItem{Text: "The dispatcher's model isn't set up (optional)", Screen: ScreenModel})
				}
			}
		}
		return out
	}
}

// plural is "1 thing" or "N things".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// applyAttention keeps the lines for the Overview that asked for them, and
// the checks for Doctor.
func (m Model) applyAttention(x attentionMsg) Model {
	if x.ran && !(m.screen == ScreenDoctor && m.loading) {
		m.checks, m.checksAt = x.checks, x.at
	}
	if x.request != m.request || m.screen != ScreenOverview {
		return m
	}
	m.attention, m.attentionLoaded = nil, true
	for _, a := range x.items {
		if m.hasTab(a.Screen) {
			m.attention = append(m.attention, a)
		}
	}
	m.intakeLine = ""
	if m.hasTab(ScreenLinear) {
		m.intakeLine = x.intake
	}
	if rows := len(m.overviewRows()); m.selected >= rows {
		m.selected = max(0, rows-1)
	}
	return m
}

// ago says how long since t: relative, as people read times (Q6).
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute ago", "minutes ago")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour ago", "hours ago")
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// provenanceWord says how the person signed in.
func provenanceWord(p string) string {
	switch p {
	case "google-id-token":
		return "Google"
	case "device-key":
		return "this device's key"
	case "":
		return "unknown sign-in"
	}
	return terminalText(p)
}

func renderOverview(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenOverview))
	b.WriteString("\n\n")
	if m.loadErr != "" {
		// PR B's body state: what failed, in words, then the rest.
		b.WriteString(errorBody(m, "the overview", m.loadErr) + "\n\n")
	}
	if m.waiting {
		return b.String() + renderWaiting(m)
	}
	b.WriteString("Needs attention\n")
	switch {
	case m.loadErr != "" && !m.attentionLoaded:
		// Nothing is checked while the overview can't load.
		b.WriteString(mutedStyle.Render("  Not checked: the overview didn't load.") + "\n")
	case len(m.attention) > 0:
		width := 0
		for _, a := range m.attention {
			width = max(width, len([]rune(a.Text)))
		}
		for i, a := range m.attention {
			prefix := "  "
			if i == m.selected {
				prefix = accentStyle.Render("› ")
			}
			b.WriteString(prefix + padRight(a.Text, min(width, 56)+2) + mutedStyle.Render(keyHint(m, ScreenOverview, "tab:"+string(a.Screen))) + "\n")
		}
	case m.attentionLoaded:
		b.WriteString(mutedStyle.Render("  Nothing needs you.") + "\n")
	default:
		b.WriteString(mutedStyle.Render("  Checking…") + "\n")
	}
	if m.intakeLine != "" {
		// The project's settings, linked: Enter opens them.
		b.WriteString("\nThis project\n")
		prefix := "  "
		if m.selected == len(m.attention) {
			prefix = accentStyle.Render("› ")
		}
		b.WriteString(prefix + m.intakeLine + "  " + mutedStyle.Render(keyHint(m, ScreenOverview, "tab:"+string(ScreenLinear))+" · Intake") + "\n")
	}
	b.WriteString("\n")
	identity := firstNonempty(terminalText(m.identity.Principal), terminalText(m.cfg.Principal), "not signed in")
	how := provenanceWord(firstNonempty(m.identity.Provenance, m.cfg.Provenance))
	if m.identity.Verified {
		how += ", verified"
	}
	b.WriteString(kv("Signed in", identity+" ("+how+")"))
	server := "not reached yet"
	switch {
	case terminalText(m.source) == "offline-mirror":
		server = "offline: a cached copy, read-only"
	case m.online != nil && *m.online:
		server = "online"
	case m.online != nil:
		server = "offline"
	}
	if s := terminalText(m.cfg.Server); s != "" {
		server = s + " · " + server
	}
	b.WriteString(kv("Server", server))
	b.WriteString(kv("Workspace", firstNonempty(terminalText(m.workspace.Name), terminalText(m.cfg.Workspace), "not configured")))
	b.WriteString(kv("Project", firstNonempty(terminalText(m.project.Slug), terminalText(m.cfg.Project), "not configured")))
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		// U21: inboxes are an agent session's; this profile has none.
		b.WriteString(kv("Inbox", "no session on this profile (agents' inboxes are per session)"))
	} else {
		b.WriteString(kv("Session", firstNonempty(terminalText(m.session.Name), terminalText(m.cfg.Session))))
		b.WriteString(kv("Unread", fmt.Sprintf("%d", len(m.messages))))
	}
	if len(m.sessions) > 0 {
		b.WriteString("\nSessions\n")
		for i, s := range m.sessions {
			if i >= 5 {
				break
			}
			b.WriteString(fmt.Sprintf("  %-24s %-10s %s\n", clip(terminalText(s.Name), 24), firstNonempty(terminalText(s.Status), "-"), firstNonempty(terminalText(s.AgentKind), "-")))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// doctorRows are the checks in the order shown: failures, then warnings,
// then the passes, which are folded unless asked for (U38).
func doctorRows(m Model) []Check {
	rank := map[Status]int{Fail: 0, Warn: 1, Pass: 2}
	out := append([]Check(nil), m.checks...)
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Status] < rank[out[j].Status] })
	if !m.doctorPasses {
		shown := out[:0]
		for _, c := range out {
			if c.Status != Pass {
				shown = append(shown, c)
			}
		}
		out = shown
	}
	return out
}

func renderDoctor(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenDoctor))
	b.WriteString("\n\n")
	if m.loadErr != "" {
		b.WriteString(errorBody(m, "the checks", m.loadErr) + "\n\n")
	}
	if m.loading && len(m.checks) == 0 {
		return b.String() + loadingBody("the checks")
	}
	if m.loading {
		// U39: the last results stay while the checks run again.
		b.WriteString(mutedStyle.Render("Running the checks again…") + "\n")
	}
	if len(m.checks) == 0 {
		if !m.loading && m.loadErr == "" {
			b.WriteString("No checks ran.")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	counts := map[Status]int{}
	for _, check := range m.checks {
		counts[check.Status]++
	}
	b.WriteString(fmt.Sprintf("%d fail · %d warn · %d pass", counts[Fail], counts[Warn], counts[Pass]))
	if !m.checksAt.IsZero() {
		b.WriteString(mutedStyle.Render(" · checked " + ago(m.cfg.Now(), m.checksAt)))
	}
	b.WriteString("\n\n")
	rows := doctorRows(m)
	for i, check := range rows {
		prefix := "  "
		if i == m.selected {
			prefix = accentStyle.Render("› ")
		}
		mark := strings.ToUpper(terminalText(string(check.Status)))
		switch check.Status {
		case Pass:
			mark = passStyle.Render(mark)
		case Warn:
			mark = warnStyle.Render(mark)
		default:
			mark = errorStyle.Render(mark)
		}
		b.WriteString(prefix + mark + "  " + firstNonempty(terminalText(check.Title), terminalText(check.ID)))
		if s := checkScreen(check.ID); s != "" && m.hasTab(s) && check.Status != Pass && i == m.selected {
			b.WriteString(mutedStyle.Render("  Enter: " + title(s)))
		}
		b.WriteString("\n")
		if terminalText(check.Detail) != "" {
			b.WriteString(mutedStyle.Render("       "+terminalText(check.Detail)) + "\n")
		}
		if terminalText(check.Fix) != "" {
			b.WriteString("       Fix: " + terminalText(check.Fix) + "\n")
		}
	}
	if !m.doctorPasses && counts[Pass] > 0 {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("\n%d passed (%s)", counts[Pass], keyHint(m, ScreenDoctor, "passes"))) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// selectedCheck is the highlighted check, as shown.
func (m Model) selectedCheck() (Check, bool) {
	rows := doctorRows(m)
	if m.selected < 0 || m.selected >= len(rows) {
		return Check{}, false
	}
	return rows[m.selected], true
}

var overviewBindings = []Binding{
	{ID: "open", Keys: []string{"enter"}, Labeler: func(m Model) string {
		if m.waiting {
			return "work in this project"
		}
		return "go there"
	}, When: func(m Model) bool {
		if m.waiting {
			return len(m.projects) > 1 && m.selected < len(m.projects)
		}
		return m.selected < len(m.overviewRows())
	}, Action: func(m Model, _ string) (Model, tea.Cmd) {
		if m.waiting {
			return m.takeProject(m.projects[m.selected].Slug)
		}
		if m.selected == len(m.attention) {
			// The intake line opens the Intake view itself.
			m.linear.step = "intake"
			m.intake = intakeState{}
			return m.Show(ScreenLinear)
		}
		return m.Show(m.attention[m.selected].Screen)
	}},
}

var doctorBindings = []Binding{
	{ID: "open", Keys: []string{"enter"}, Label: "go to its screen", When: func(m Model) bool {
		c, ok := m.selectedCheck()
		return ok && checkScreen(c.ID) != "" && m.hasTab(checkScreen(c.ID)) && c.Status != Pass
	}, Action: func(m Model, _ string) (Model, tea.Cmd) {
		c, _ := m.selectedCheck()
		return m.Show(checkScreen(c.ID))
	}},
	{ID: "passes", Keys: []string{"p"}, Labeler: func(m Model) string {
		if m.doctorPasses {
			return "hide passes"
		}
		return "show passes"
	}, Action: func(m Model, _ string) (Model, tea.Cmd) {
		m.doctorPasses = !m.doctorPasses
		m.selected = 0
		return m, nil
	}},
}

// overviewRows are Overview's selectable rows: the attention lines, then
// the intake line when there is one.
func (m Model) overviewRows() []string {
	out := make([]string, 0, len(m.attention)+1)
	for i := range m.attention {
		out = append(out, fmt.Sprint(i))
	}
	if m.intakeLine != "" {
		out = append(out, "intake")
	}
	return out
}
