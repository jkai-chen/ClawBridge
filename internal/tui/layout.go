package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// screenLines measures cells after terminal-safe text and trusted styling.
func screenLines(s string, w int) []string {
	return strings.Split(ansi.Wrap(s, max(1, w), ""), "\n")
}
func singleLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// layoutParts reserves controls first, even when notices and editors
// compete. The header is the breadcrumb, the tabs, then the status line.
func layoutParts(m Model) ([]string, []string, int) {
	w, h := max(1, m.width), max(1, m.height)
	header := []string{headerLine(m), renderTabs(m)}
	paging := ""
	if m.screen == ScreenSessions || m.screen == ScreenInbox {
		// U25: what's shown, in words; the caveats are in help.
		paging = fmt.Sprintf("Showing: %s · page %d", strings.ToLower(m.filter.String()), m.pageIndex+1)
		switch {
		case m.source == "offline-mirror":
			paging += " (cached; incomplete)"
		case m.hasMore:
			paging += " (more: ])"
		}
		header = append(header, mutedStyle.Render(paging))
	}
	// The status row is always there, so a load or a message never moves
	// the body (U8).
	status := statusLine(m)
	header = append(header, status)
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
	// At least one body row, two borders and a footer remain. Compact
	// header first; the status line is kept over the rest.
	maxHeader := max(0, h-len(editor)-4)
	if len(header) > maxHeader {
		compact := []string{fmt.Sprintf("ClawBridge · %s · %s", title(m.screen), scopeLine(m))}
		compact = append(compact, status)
		if paging != "" {
			compact = append(compact, paging)
		}
		header = compact[:min(len(compact), maxHeader)]
	}
	for i := range header {
		header[i] = ansi.Truncate(singleLine(header[i]), w, "…")
	}
	body := renderBody(m)
	if (m.screen == ScreenSessions || m.screen == ScreenInbox) && m.source == "offline-mirror" && !m.help {
		body += "\nMirror coverage is incomplete; retained snapshot only, read-only."
	}
	content := screenLines(body, w-4)
	budget := max(1, h-len(header)-len(editor)-3)
	return append(header, editor...), content, budget
}
func bodyOffset(m Model, lines []string, budget int) int {
	offset := min(m.scroll, max(0, len(lines)-budget))
	if !m.scrollManual {
		// The focus is the field being typed into (its █ cursor), else
		// the selected row (› ); either is kept in view (U41).
		focus := -1
		for i, line := range lines {
			plain := strings.TrimRight(ansi.Strip(line), " ")
			if strings.HasSuffix(plain, "█") {
				focus = i
				break
			}
			if focus < 0 && strings.HasPrefix(plain, "› ") {
				focus = i
			}
		}
		if focus >= 0 {
			offset = max(0, focus-budget+1)
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
