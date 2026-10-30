package tui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"clawbridge/internal/clirender"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	return renderResponsive(m)
}

// RenderPlain removes Lip Gloss ANSI styling from Render's model output.
func RenderPlain(m Model) string {
	// Lip Gloss does not emit styling in a non-TTY test renderer. Keeping this
	// function explicit documents that callers can use Render safely in tests.
	return Render(m)
}

// badge is the mode, and offline when the view is a read-only mirror.
func badge(m Model) string {
	label := terminalText(string(m.cfg.Mode))
	if m.cfg.Mode == ModeCloud && terminalText(m.source) == "offline-mirror" {
		return warnStyle.Render(label + " · offline, read-only")
	}
	return mutedStyle.Render(label)
}

// crumbs is where the person is: the workspace, project and session by
// name, then any detail the screen has open.
func crumbs(m Model) []string {
	out := []string{firstNonempty(terminalText(m.workspace.Name), terminalText(m.cfg.Workspace), "no workspace")}
	if p := firstNonempty(terminalText(m.project.Slug), terminalText(m.cfg.Project)); p != "" {
		out = append(out, p)
	}
	if s := firstNonempty(terminalText(m.session.Name), terminalText(m.cfg.Session)); s != "" {
		out = append(out, s)
	}
	switch {
	case m.help:
		out = append(out, title(m.screen), "Help")
	case m.screen == ScreenInbox && m.inboxOpen:
		if msg, ok := m.SelectedMessage(); ok {
			out = append(out, "Inbox", "from "+firstNonempty(terminalText(msg.Sender), "unknown sender"))
		}
	}
	return out
}

func headerLine(m Model) string {
	return accentStyle.Render("ClawBridge") + " · " + strings.Join(crumbs(m), " › ") + "  " + badge(m)
}

// scopeLine is the scope as a path, for the compact header.
func scopeLine(m Model) string { return strings.Join(crumbs(m), " › ") }

// shortTitle names a tab in the compact bar.
var shortTitle = map[Screen]string{
	ScreenOverview: "Ovw", ScreenProjects: "Prj", ScreenSessions: "Ses", ScreenInbox: "Inb", ScreenLinear: "Lin",
	ScreenModel: "Dsp", ScreenAccount: "Acc", ScreenDoctor: "Doc", ScreenCloud: "Cld",
}

// renderTabs is the tab bar: full names when they fit, short ones under
// 100 columns with the current tab named in full, and at the narrowest
// only the current tab.
func renderTabs(m Model) string {
	bar := func(short bool) string {
		var parts []string
		for i, s := range m.tabs() {
			name := title(s)
			if short && s != m.screen {
				name = shortTitle[s]
			}
			label := fmt.Sprintf("%d %s", i+1, name)
			if s == m.screen {
				parts = append(parts, accentStyle.Render("["+label+"]"))
			} else {
				parts = append(parts, mutedStyle.Render(label))
			}
		}
		sep := "  "
		if short {
			sep = " "
		}
		return strings.Join(parts, sep)
	}
	if full := bar(false); m.width >= 100 && ansi.StringWidth(full) <= m.width {
		return full
	}
	if short := bar(true); ansi.StringWidth(short) <= m.width {
		return short
	}
	for i, s := range m.tabs() {
		if s == m.screen {
			return fmt.Sprintf("%d %s · Tab screens", i+1, title(s))
		}
	}
	return title(m.screen) + " · Tab screens"
}

// statusLine is the row under the tabs: an offline copy, then what the
// last action did (status). A failed load shows in the body (§3.5).
func statusLine(m Model) string {
	parts := []string{}
	if terminalText(m.source) == "offline-mirror" {
		fresh := "Offline: cached copy"
		if terminalText(m.fresh.UpdatedAt) != "" {
			fresh += " from " + terminalText(m.fresh.UpdatedAt)
		}
		if m.fresh.AgeSeconds > 0 {
			fresh += fmt.Sprintf(" (%ds old)", m.fresh.AgeSeconds)
		}
		parts = append(parts, warnStyle.Render(fresh+", read-only: writes unavailable"))
	}
	if m.cfg.OutboxWaiting != nil {
		if n := m.cfg.OutboxWaiting(); n > 0 {
			parts = append(parts, warnStyle.Render(fmt.Sprintf("outbox: %d waiting", n)))
		}
	}
	if line := m.status.line(); line != "" {
		parts = append(parts, line)
	} else if m.loading {
		parts = append(parts, busyStatus("Loading…").line())
	}
	return strings.Join(parts, " · ")
}

func renderBody(m Model) string {
	if m.help {
		return renderHelp(m)
	}
	switch m.screen {
	case ScreenLinear:
		return renderLinear(m)
	case ScreenAccount:
		return renderAccount(m)
	case ScreenModel:
		return renderModelSetup(m)
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

func tableBudget(m Model) int {
	width := m.width
	if width <= 0 {
		width = 80
	}
	return max(1, width-4)
}

// renderTable shares the header/data gutter and measures terminal cells, not
// bytes or runes. Keep each row within the box so wrapping cannot split columns.
func renderTable(m Model, headers []string, preferred []int, rows [][]string) string {
	budget := tableBudget(m)
	gutter := min(2, max(0, budget-1))
	available := budget - gutter
	// Drop trailing columns before they become unreadable; the selected details
	// retain every field even when only the name fits.
	count := min(len(headers), max(1, (available+1)/9))
	widths := append([]int(nil), preferred[:count]...)
	total := count - 1
	for _, w := range widths {
		total += w
	}
	for total > available {
		largest := 0
		for i := range widths {
			if widths[i] > widths[largest] {
				largest = i
			}
		}
		widths[largest]--
		total--
	}
	line := func(values []string, prefix string) string {
		cells := make([]string, count)
		for i, w := range widths {
			// Newlines and tabs belong in message bodies, not single-line table cells.
			text := strings.NewReplacer("\n", " ", "\t", " ").Replace(terminalText(values[i]))
			text = ansi.Truncate(text, w, "…")
			cells[i] = text + strings.Repeat(" ", max(0, w-ansi.StringWidth(text)))
		}
		return prefix + strings.Join(cells, " ")
	}
	var b strings.Builder
	b.WriteString(mutedStyle.Render(line(headers, strings.Repeat(" ", gutter))))
	for i, row := range rows {
		b.WriteByte('\n')
		prefix := strings.Repeat(" ", gutter)
		if i == m.selected && gutter == 2 {
			prefix = "› "
		}
		text := line(row, prefix)
		if i == m.selected {
			text = accentStyle.Render(text)
		}
		b.WriteString(text)
	}
	return b.String()
}

// messageStatus is a message's delivery as a word.
func messageStatus(message MessageRow) string {
	switch {
	case message.ReadAt != "":
		return "read"
	case message.DeliveredAt == "":
		return "pending delivery"
	}
	return "delivered"
}

// renderMessage is one message opened (Enter): all of it, what it answers,
// and the rest of its thread on this page.
func renderMessage(m Model, message MessageRow) string {
	var b strings.Builder
	b.WriteString(accentStyle.Render(firstNonempty(terminalText(message.Sender), "unknown sender")))
	b.WriteString(" · " + firstNonempty(terminalText(message.Kind), "message") + " · " + messageStatus(message) + " · " + terminalText(clirender.WhenExact(m.now(), message.CreatedAt)) + "\n\n")
	b.WriteString(terminalText(message.Body) + "\n")
	if message.ReplyTo != "" {
		parent := mutedStyle.Render("an earlier message, not on this page")
		for _, other := range m.messages {
			if other.ID == message.ReplyTo {
				first, _, _ := strings.Cut(terminalText(other.Body), "\n")
				parent = firstNonempty(terminalText(other.Sender), "unknown sender") + ": " + clip(first, 60)
			}
		}
		b.WriteString("\nIn reply to " + parent + "\n")
	}
	if message.ThreadID != "" {
		others := []string{}
		for _, other := range m.messages {
			if other.ThreadID == message.ThreadID && other.ID != message.ID {
				first, _, _ := strings.Cut(terminalText(other.Body), "\n")
				others = append(others, "  "+firstNonempty(terminalText(other.Sender), "unknown sender")+": "+clip(first, 60))
			}
		}
		if len(others) > 0 {
			b.WriteString("\nIn this thread\n" + strings.Join(others, "\n") + "\n")
		}
	}
	b.WriteString(mutedStyle.Render("\nMessage " + terminalText(message.ID)))
	return b.String()
}

func renderInbox(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenInbox))
	b.WriteString("\n\n")
	if m.inboxOpen {
		if message, ok := m.SelectedMessage(); ok {
			return b.String() + renderMessage(m, message)
		}
	}
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		// Not an error: a person's profile has no inbox (U36).
		b.WriteString("Inboxes belong to agent sessions, and this profile has none.\n" +
			"To read as a session: clawbridge tui --session NAME, on that session's machine.")
		return b.String()
	}
	if body, ok := bodyState(m, "the inbox", len(m.messages) > 0); ok {
		return b.String() + body
	}
	if len(m.messages) == 0 {
		b.WriteString("No messages for " + terminalText(m.cfg.Session) + ".")
		return b.String()
	}
	for i, message := range m.messages {
		prefix := "  "
		if i == m.selected {
			prefix = accentStyle.Render("› ")
		}
		status := messageStatus(message)
		b.WriteString(prefix)
		b.WriteString(accentStyle.Render(firstNonempty(terminalText(message.Sender), "unknown sender")))
		b.WriteString(" · ")
		b.WriteString(firstNonempty(terminalText(message.Kind), "message"))
		b.WriteString(" · ")
		b.WriteString(status)
		b.WriteString(" · ")
		b.WriteString(clip(firstNonempty(terminalText(clirender.When(m.now(), message.CreatedAt)), terminalText(message.ID)), 36))
		b.WriteString("\n    ")
		b.WriteString(terminalText(message.Body))
		b.WriteString("\n")
		if terminalText(message.ReplyTo) != "" {
			// U28: the parent by who sent it; its ID is in the opened
			// message.
			parent := "an earlier message"
			for _, other := range m.messages {
				if other.ID == message.ReplyTo {
					parent = firstNonempty(terminalText(other.Sender), "unknown sender")
				}
			}
			b.WriteString(mutedStyle.Render("    reply to " + parent))
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
	if body, ok := bodyState(m, "the setup plan", m.plan != nil); ok {
		return b.String() + body
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

func renderInput(m Model) string {
	if m.inputMode == inputLinearName {
		return "Connection label: " + terminalText(m.draft)
	}
	if m.inputMode == inputLinearUnlink {
		return warnStyle.Render("Unlinking the board affects everyone in this project. Type "+terminalText(fmt.Sprintf("%q", m.linearUnlinkSlug()))+" to unlink; Esc cancels") + "\n> " + terminalText(m.draft) + "█"
	}
	if m.inputMode == inputLinearClosesName && len(m.linear.typing) > 0 {
		return warnStyle.Render("Type "+terminalText(fmt.Sprintf("%q", m.linear.typing[0].Name))+" to let agents close its issues; Esc cancels") + "\n> " + terminalText(m.draft) + "█"
	}
	if m.inputMode == inputLinearCancelKinds && m.cancel.ask != nil {
		return warnStyle.Render("Type "+terminalText(fmt.Sprintf("%q", strings.Join(m.cancel.ask.kinds, ", ")))+" to let agents cancel in those kinds; Esc cancels") + "\n> " + terminalText(m.draft) + "█"
	}
	if m.confirming && m.account.pending != nil {
		return warnStyle.Render("Type "+terminalText(m.account.pending.phrase)+" to confirm; Esc cancels") + "\n> " + terminalText(m.confirm) + "█"
	}
	switch m.inputMode {
	case inputLinearBudget:
		return mutedStyle.Render("This project's Linear calls an hour (Enter, then y; empty goes back to the server's; Esc cancels)") + "\n> " + terminalText(m.draft) + "█"
	case inputAccountInvite:
		return mutedStyle.Render("Invite: their email (Enter, then choose projects and roles; Esc cancels)") + "\n> " + terminalText(m.draft) + "█"
	case inputAccountLabel:
		return mutedStyle.Render("Device label (Enter saves, Esc cancels)") + "\n> " + terminalText(m.draft) + "█"
	case inputAccountTransfer:
		return mutedStyle.Render("Offer ownership to (a member's email; Enter, then confirm)") + "\n> " + terminalText(m.draft) + "█"
	}
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
		// Two lines, the cursor in the field being edited (U27).
		to, body := "To: "+terminalText(m.sendTo), "Message: "+terminalText(m.draft)
		if m.sendBody {
			return to + "\n" + body + "█  " + mutedStyle.Render("Enter sends · Tab back to To · Esc cancels")
		}
		return to + "█  " + mutedStyle.Render("Tab: next session · Enter: the message · Esc cancels") + "\n" + mutedStyle.Render(body)
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
	case ScreenLinear:
		return "Linear"
	case ScreenAccount:
		return "Account"
	case ScreenModel:
		return "Dispatcher"
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
