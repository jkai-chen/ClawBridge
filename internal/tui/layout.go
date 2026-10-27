package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// screenLines measures cells after terminal-safe text and trusted styling.
func screenLines(s string, w int) []string {
	return strings.Split(ansi.Hardwrap(s, max(1, w), true), "\n")
}
func singleLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}
func footerText(m Model) string {
	if m.screen == ScreenModel && m.modelSetup.wizard.step >= modelStepProvider && m.modelSetup.wizard.step <= modelStepDisclosure {
		return "Enter next · Esc cancel setup · Ctrl-C quit"
	}
	if m.inputMode != inputNone || m.confirming {
		if m.width < 40 {
			return "Esc cancel · Enter send"
		}
		return "Enter submit · Esc cancel · Ctrl-C quit"
	}
	if m.screen == ScreenSessions || m.screen == ScreenInbox {
		if m.width < 65 {
			return "v filter [ ] pages q quit"
		}
		return "↑↓ select · PgUp/PgDn scroll · v filter · [ ] pages · r reload · q quit"
	}
	if m.width < 65 {
		return "↑↓ row Pg↑↓ scroll r reload q quit"
	}
	return "↑↓ select · PgUp/PgDn scroll · r reload · Tab views · q quit"
}

// layoutParts reserves controls first, even when notices and editors compete.
func layoutParts(m Model) ([]string, []string, int) {
	w, h := max(1, m.width), max(1, m.height)
	header := []string{accentStyle.Render("ClawBridge") + "  " + modeLabel(m), scopeLine(m)}
	tabs := renderTabs(m.screen)
	if ansi.StringWidth(tabs) > w {
		tabs = fmt.Sprintf("%d %s · Tab views", indexOf(m.screen)+1, title(m.screen))
	}
	header = append(header, tabs)
	if m.screen == ScreenSessions || m.screen == ScreenInbox {
		coverage := "end"
		if m.hasMore {
			coverage = "more"
		}
		if m.source == "offline-mirror" {
			coverage = "mirror*"
		}
		header = append(header, fmt.Sprintf("%s p%d %s · v[]", m.filter, m.pageIndex+1, coverage))
	}
	if m.loading {
		header = append(header, "Refreshing…")
	}
	status := sourceLine(m)
	if m.errText != "" {
		status += " Error: " + terminalText(m.errText)
	}
	if m.actionErr != "" {
		status += " Notice: " + terminalText(m.actionErr)
	}
	if status != "" {
		header = append(header, status)
	}
	if m.screen == ScreenCloud && m.plan != nil && m.cfg.Setup != nil {
		header = append(header, "Nothing is applied before Enter; exact confirmation required.")
	}
	editor := []string{}
	if m.inputMode != inputNone || m.confirming {
		editor = screenLines(renderInput(m), w)
		if len(editor) > 2 {
			editor = editor[len(editor)-2:]
		}
	}
	// At least one body row, two borders and a footer remain. Compact header first.
	maxHeader := max(0, h-len(editor)-4)
	if len(header) > maxHeader { // Scope/status remain inspectable in the content.
		compact := []string{fmt.Sprintf("ClawBridge · %s", title(m.screen))}
		if m.screen == ScreenSessions || m.screen == ScreenInbox {
			compact = append(compact, header[3])
		}
		header = compact[:min(len(compact), maxHeader)]
	}
	for i := range header {
		header[i] = ansi.Truncate(singleLine(header[i]), w, "…")
	}
	body := renderBody(m) + "\n\nScope: " + scopeLine(m)
	if m.screen == ScreenSessions || m.screen == ScreenInbox {
		body += "\nPage-local filter only; [ previous visited page, ] next server page. No exhaustive search."
		if m.source == "offline-mirror" {
			body += "\nMirror coverage is incomplete; retained snapshot only, read-only."
		}
	}
	if status != "" {
		body += "\n" + status
	}
	content := screenLines(body, w-4)
	budget := max(1, h-len(header)-len(editor)-3)
	return append(header, editor...), content, budget
}
func bodyOffset(m Model, lines []string, budget int) int {
	offset := min(m.scroll, max(0, len(lines)-budget))
	if !m.scrollManual {
		for i, line := range lines {
			if strings.HasPrefix(ansi.Strip(line), "› ") {
				offset = max(0, i-budget+1)
				break
			}
		}
	}
	return min(offset, max(0, len(lines)-budget))
}
func renderResponsive(m Model) string {
	w, h := max(1, m.width), max(1, m.height)
	if w < 24 || h < 9 {
		return fitScreen("Resize terminal (24×9 min). Ctrl-C quit; Esc cancel", w, h)
	}
	controls, lines, budget := layoutParts(m)
	editorCount := 0
	if m.inputMode != inputNone || m.confirming {
		editorCount = min(2, len(screenLines(renderInput(m), w)))
	}
	split := len(controls) - editorCount
	offset := bodyOffset(m, lines, budget)
	end := min(len(lines), offset+budget)
	frame := boxStyle.Width(w - 2).Render(strings.Join(lines[offset:end], "\n"))
	out := append([]string(nil), controls[:split]...)
	out = append(out, strings.Split(frame, "\n")...)
	out = append(out, controls[split:]...)
	out = append(out, ansi.Truncate(footerText(m), w, "…"))
	return fitScreen(strings.Join(out, "\n"), w, h)
}
func fitScreen(s string, w, h int) string {
	lines := screenLines(s, w)
	if len(lines) > h {
		lines = lines[:h]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], w, "")
	}
	return strings.Join(lines, "\n")
}
