package tui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

var (
	accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	passStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

// Render returns a terminal-safe view for a Model. It has no side effects.
func Render(m Model) string {
	width := m.width
	if width < 56 {
		width = 56
	}
	if width > 140 {
		width = 140
	}

	var b strings.Builder
	b.WriteString(accentStyle.Render("ClawBridge"))
	b.WriteString("  ")
	b.WriteString(modeLabel(m))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(scopeLine(m)))
	b.WriteString("\n")
	b.WriteString(renderTabs(m.screen))
	b.WriteString("\n\n")

	if m.loading {
		b.WriteString(mutedStyle.Render("Refreshing through the pinned application boundary…"))
		b.WriteString("\n\n")
	}
	if source := sourceLine(m); source != "" {
		b.WriteString(source)
		b.WriteString("\n\n")
	}
	if terminalText(m.errText) != "" {
		b.WriteString(errorStyle.Render("Error  "))
		b.WriteString(terminalText(m.errText))
		b.WriteString("\n\n")
	}
	if terminalText(m.actionErr) != "" {
		b.WriteString(warnStyle.Render("Notice  "))
		b.WriteString(terminalText(m.actionErr))
		b.WriteString("\n\n")
	}

	body := renderBody(m)
	b.WriteString(boxStyle.Width(max(10, width-4)).Render(body))
	b.WriteString("\n")
	b.WriteString(renderInput(m))
	if m.inputMode != inputNone || m.confirming || terminalText(m.actionErr) != "" {
		b.WriteString("\n")
	}
	b.WriteString(mutedStyle.Render(helpText))
	return b.String()
}

// RenderPlain removes Lip Gloss ANSI styling from Render's model output.
func RenderPlain(m Model) string {
	// Lip Gloss does not emit styling in a non-TTY test renderer. Keeping this
	// function explicit documents that callers can use Render safely in tests.
	return Render(m)
}

func modeLabel(m Model) string {
	label := terminalText(string(m.cfg.Mode))
	if m.cfg.Mode == ModeCloud && terminalText(m.source) == "offline-mirror" {
		label += " / read-only mirror"
	}
	if m.cfg.Mode == ModeCloud && m.identity.Verified {
		label += " / verified identity"
	}
	return mutedStyle.Render(label)
}

func scopeLine(m Model) string {
	workspace := firstNonempty(terminalText(m.workspace.Name), terminalText(m.cfg.Workspace), "unresolved workspace")
	project := firstNonempty(terminalText(m.project.Slug), terminalText(m.cfg.Project), "unresolved project")
	session := firstNonempty(terminalText(m.session.Name), terminalText(m.cfg.Session), "no bound session")
	return fmt.Sprintf("%s / %s / %s", workspace, project, session)
}

func renderTabs(current Screen) string {
	var parts []string
	for _, s := range orderedScreens {
		label := fmt.Sprintf("%d %s", screenOrder[s]+1, title(s))
		if s == current {
			parts = append(parts, accentStyle.Render("["+label+"]"))
		} else {
			parts = append(parts, mutedStyle.Render(label))
		}
	}
	return strings.Join(parts, "  ")
}

func sourceLine(m Model) string {
	if terminalText(m.source) == "" {
		return ""
	}
	if terminalText(m.source) == "offline-mirror" {
		fresh := "read-only mirror"
		if terminalText(m.fresh.UpdatedAt) != "" {
			fresh += " · snapshot " + terminalText(m.fresh.UpdatedAt)
		}
		if m.fresh.AgeSeconds > 0 {
			fresh += fmt.Sprintf(" · %ds old", m.fresh.AgeSeconds)
		}
		return warnStyle.Render("Source  ") + fresh + " · writes unavailable"
	}
	if terminalText(m.source) == "setup-plan" {
		return mutedStyle.Render("Source  setup plan (read-only until confirmed)")
	}
	if terminalText(m.source) == "doctor" {
		return mutedStyle.Render("Source  doctor")
	}
	return mutedStyle.Render("Source  ") + terminalText(m.source)
}

func renderBody(m Model) string {
	switch m.screen {
	case ScreenOverview:
		return renderOverview(m)
	case ScreenProjects:
		return renderProjects(m)
	case ScreenSessions:
		return renderSessions(m)
	case ScreenInbox:
		return renderInbox(m)
	case ScreenCloud:
		return renderCloud(m)
	case ScreenDoctor:
		return renderDoctor(m)
	default:
		return "Unknown screen"
	}
}

func renderOverview(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenOverview))
	b.WriteString("\n\n")
	b.WriteString(kv("Workspace", firstNonempty(terminalText(m.workspace.Name), terminalText(m.cfg.Workspace), "not configured")))
	b.WriteString(kv("Project", firstNonempty(terminalText(m.project.Slug), terminalText(m.cfg.Project), "not configured")))
	b.WriteString(kv("Session", firstNonempty(terminalText(m.session.Name), terminalText(m.cfg.Session), "not bound")))
	identity := firstNonempty(terminalText(m.identity.Principal), terminalText(m.cfg.Principal), "not established")
	provenance := firstNonempty(terminalText(m.identity.Provenance), terminalText(m.cfg.Provenance), "unknown provenance")
	verified := "unverified"
	if m.identity.Verified {
		verified = "verified"
	}
	b.WriteString(kv("Identity", identity+" · "+provenance+" · "+verified))
	online := "unknown"
	if m.online != nil {
		online = "offline"
		if *m.online {
			online = "online"
		}
	}
	if terminalText(m.source) == "offline-mirror" {
		online = "offline · scoped cached read"
	}
	b.WriteString(kv("Authority", firstNonempty(terminalText(m.source), "not loaded")+" · "+online))
	b.WriteString("\n")
	b.WriteString(kv("Sessions", fmt.Sprintf("%d in pinned project", len(m.sessions))))
	b.WriteString(kv("Unread", fmt.Sprintf("%d in current session", len(m.messages))))
	if len(m.sessions) > 0 {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render("Pinned project sessions"))
		b.WriteString("\n")
		for i, s := range m.sessions {
			if i >= 5 {
				break
			}
			b.WriteString(fmt.Sprintf("  %-24s %-10s %s\n", clip(terminalText(s.Name), 24), firstNonempty(terminalText(s.Status), "-"), firstNonempty(terminalText(s.AgentKind), "-")))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderProjects(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenProjects))
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("workspace-scoped, read-only view"))
	b.WriteString("\n\n")
	if len(m.projects) == 0 {
		b.WriteString("No projects returned for this workspace.")
		return b.String()
	}
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%-28s %-10s %s\n", "SLUG", "ROLE", "NAME")))
	for i, p := range m.projects {
		line := fmt.Sprintf("%-28s %-10s %s", clip(terminalText(p.Slug), 28), clip(firstNonempty(terminalText(p.Role), "-"), 10), clip(terminalText(p.Name), 42))
		if i == m.selected {
			line = accentStyle.Render("› " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderSessions(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenSessions))
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("pinned project routing"))
	b.WriteString("\n\n")
	if len(m.sessions) == 0 {
		b.WriteString("No sessions returned for this project.")
		return b.String()
	}
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%-22s %-10s %-12s %-10s %s\n", "NAME", "ROLE", "AGENT", "STATUS", "PRESENCE")))
	for i, s := range m.sessions {
		presence := firstNonempty(terminalText(s.Presence), terminalText(s.Status), "-")
		line := fmt.Sprintf("%-22s %-10s %-12s %-10s %s", clip(terminalText(s.Name), 22), clip(terminalText(s.Role), 10), clip(terminalText(s.AgentKind), 12), clip(terminalText(s.Status), 10), presence)
		if i == m.selected {
			line = accentStyle.Render("› " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderInbox(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenInbox))
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("server stamps delivery; refresh never marks read"))
	b.WriteString("\n\n")
	if len(m.messages) == 0 {
		b.WriteString("No messages in the current session inbox.")
		return b.String()
	}
	for i, message := range m.messages {
		prefix := "  "
		if i == m.selected {
			prefix = accentStyle.Render("› ")
		}
		status := "delivered"
		if message.DeliveredAt == "" {
			status = "pending delivery"
		}
		if message.ReadAt != "" {
			status = "read"
		}
		b.WriteString(prefix)
		b.WriteString(accentStyle.Render(firstNonempty(terminalText(message.Sender), "unknown sender")))
		b.WriteString(" · ")
		b.WriteString(firstNonempty(terminalText(message.Kind), "message"))
		b.WriteString(" · ")
		b.WriteString(status)
		b.WriteString(" · ")
		b.WriteString(clip(firstNonempty(terminalText(message.CreatedAt), terminalText(message.ID)), 36))
		b.WriteString("\n    ")
		b.WriteString(clip(strings.ReplaceAll(terminalText(message.Body), "\n", " "), 92))
		b.WriteString("\n")
		if terminalText(message.ReplyTo) != "" {
			b.WriteString(mutedStyle.Render("    reply to " + terminalText(message.ReplyTo)))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderCloud(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenCloud))
	b.WriteString("\n\n")
	if m.cfg.Setup == nil && m.cfg.CloudInfo != "" {
		b.WriteString(terminalText(m.cfg.CloudInfo))
		return b.String()
	}
	if m.plan == nil {
		b.WriteString("No setup plan loaded.")
		return b.String()
	}
	p := m.plan
	b.WriteString(accentStyle.Render("Read-only plan review"))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("No resource is created or changed while this screen loads."))
	b.WriteString("\n\n")
	if terminalText(p.Title) != "" {
		b.WriteString(terminalText(p.Title))
		b.WriteString("\n\n")
	}
	for _, line := range strings.Split(terminalText(p.Plan), "\n") {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if len(p.Resources) > 0 {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render("Resources"))
		b.WriteString("\n")
		for _, r := range p.Resources {
			b.WriteString(fmt.Sprintf("  %-9s %-32s %-16s %-12s %s\n",
				clip(terminalText(r.Action), 9), clip(terminalText(r.Name), 32), clip(terminalText(r.Region), 16), clip(terminalText(r.Kind), 12), terminalText(r.Monthly)))
		}
	}
	b.WriteString("\n")
	b.WriteString(kv("Estimated cost", firstNonempty(terminalText(p.MonthlyCost), "not supplied")))
	if terminalText(p.Region) != "" {
		b.WriteString(kv("Region", terminalText(p.Region)))
	}
	if p.RetentionDays > 0 {
		b.WriteString(kv("Retention", fmt.Sprintf("%d days", p.RetentionDays)))
	}
	if terminalText(p.ResidencyNotes) != "" {
		b.WriteString(kv("Residency", terminalText(p.ResidencyNotes)))
	}
	b.WriteString(kv("Exact confirmation", firstNonempty(terminalText(p.Confirmation), "not supplied")))
	if m.apply != nil {
		b.WriteString("\n")
		b.WriteString(passStyle.Render("Applied  "))
		b.WriteString(terminalText(m.apply.Summary))
		b.WriteString("\n")
		for _, item := range m.apply.Applied {
			b.WriteString("  · " + terminalText(item) + "\n")
		}
	} else {
		b.WriteString("\n")
		b.WriteString(warnStyle.Render("Write boundary  "))
		b.WriteString("Press `a`, review the displayed plan, then type its exact confirmation phrase. Nothing is applied before Enter.")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderDoctor(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenDoctor))
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("stable checks: pass / warn / fail"))
	b.WriteString("\n\n")
	if len(m.checks) == 0 {
		b.WriteString("No diagnostic results.")
		return b.String()
	}
	counts := map[Status]int{}
	for _, check := range m.checks {
		counts[check.Status]++
	}
	b.WriteString(fmt.Sprintf("%d pass · %d warn · %d fail\n\n", counts[Pass], counts[Warn], counts[Fail]))
	for i, check := range m.checks {
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
		b.WriteString(prefix)
		b.WriteString(mark)
		b.WriteString("  ")
		b.WriteString(firstNonempty(terminalText(check.Title), terminalText(check.ID)))
		b.WriteString("\n")
		if terminalText(check.Detail) != "" {
			b.WriteString(mutedStyle.Render("       " + terminalText(check.Detail)))
			b.WriteString("\n")
		}
		if terminalText(check.Fix) != "" {
			b.WriteString("       Fix: " + terminalText(check.Fix) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderInput(m Model) string {
	if m.confirming {
		label := "Confirmation"
		if m.plan != nil {
			label = "Type exactly: " + terminalText(m.plan.Confirmation)
		}
		return warnStyle.Render(label) + "\n> " + terminalText(m.confirm) + "█"
	}
	switch m.inputMode {
	case inputReply:
		return mutedStyle.Render("Reply (Enter sends, Esc cancels)") + "\n> " + terminalText(m.draft) + "█"
	case inputSend:
		return mutedStyle.Render("Send (recipient|message; Enter sends, Esc cancels)") + "\n> " + terminalText(m.draft) + "█"
	}
	return ""
}

func kv(label, value string) string {
	return fmt.Sprintf("%-15s %s\n", label, value)
}

func clip(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	r := []rune(text)
	if limit <= 1 {
		return string(r[:limit])
	}
	return string(r[:limit-1]) + "…"
}

func title(s Screen) string {
	switch s {
	case ScreenOverview:
		return "Overview"
	case ScreenProjects:
		return "Projects"
	case ScreenSessions:
		return "Sessions"
	case ScreenInbox:
		return "Inbox"
	case ScreenCloud:
		return "Cloud"
	case ScreenDoctor:
		return "Doctor"
	default:
		return terminalText(string(s))
	}
}

// terminalText escapes untrusted terminal controls before trusted styling.
// Display escaping never changes the stored data or command/action input.
func terminalText(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || isDirectionalControl(r)) {
			if r <= 0xff {
				fmt.Fprintf(&b, "\\x%02x", r)
			} else {
				fmt.Fprintf(&b, "\\u%04x", r)
			}
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Preserve shaping characters such as ZWJ while making bidi overrides visible.
func isDirectionalControl(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
