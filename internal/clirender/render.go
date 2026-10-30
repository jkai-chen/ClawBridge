package clirender

import (
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

const (
	minWidth     = 60
	defaultWidth = 100
)

// style is how a Doc is coloured: only status words and notes, and only
// when the output allows colour (NO_COLOR, CLICOLOR=0 and a non-terminal
// turn it off, through lipgloss's own detection).
type style struct {
	pass, warn, fail, muted, title lipgloss.Style
}

func newStyle(w io.Writer) style {
	r := lipgloss.NewRenderer(w)
	return style{
		pass:  r.NewStyle().Foreground(lipgloss.Color("2")),
		warn:  r.NewStyle().Foreground(lipgloss.Color("3")),
		fail:  r.NewStyle().Foreground(lipgloss.Color("1")),
		muted: r.NewStyle().Faint(true),
		title: r.NewStyle().Bold(true),
	}
}

// width is the terminal's, at least minWidth; defaultWidth when unknown.
func width(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if cols, _, err := term.GetSize(f.Fd()); err == nil && cols > 0 {
			return max(cols, minWidth)
		}
	}
	return defaultWidth
}

// Render lays out d at width w.
func Render(d Doc, w int, st style) string {
	w = max(w, minWidth)
	var b strings.Builder
	for i := 0; i < len(d.blocks); i++ {
		bl := d.blocks[i]
		pad := strings.Repeat("  ", bl.indent)
		switch bl.kind {
		case bTitle:
			b.WriteString(st.title.Render(bl.text) + "\n")
		case bSection:
			b.WriteString("\n" + pad + st.title.Render(bl.text) + "\n")
		case bKV:
			// Align a run of KVs at the same indent.
			j := i
			labelW := 0
			for ; j < len(d.blocks) && d.blocks[j].kind == bKV && d.blocks[j].indent == bl.indent; j++ {
				labelW = max(labelW, ansi.StringWidth(d.blocks[j].label))
			}
			for _, kv := range d.blocks[i:j] {
				lead := pad + kv.label + strings.Repeat(" ", labelW-ansi.StringWidth(kv.label)) + "  "
				writeWrapped(&b, lead, kv.text, w)
			}
			i = j - 1
		case bTable:
			if bl.records && tableWidth(pad, bl.cols, bl.rows) > w {
				writeRecords(&b, pad, bl.cols, bl.rows, w)
				continue
			}
			writeTable(&b, pad, bl.cols, bl.rows, w, st)
		case bStatus:
			word := levelWords[bl.level]
			styled := word
			switch bl.level {
			case Pass, OK:
				styled = st.pass.Render(word)
			case Warn:
				styled = st.warn.Render(word)
			case Fail, Error:
				styled = st.fail.Render(word)
			case Info, Busy:
				styled = st.muted.Render(word)
			}
			lead := pad + styled + strings.Repeat(" ", max(1, 5-ansi.StringWidth(word)))
			writeWrapped(&b, lead, bl.text, w)
		case bNote:
			b.WriteString(pad + st.muted.Render(ansi.Wordwrap(bl.text, w-len(pad), " ")) + "\n")
		case bText:
			writeWrapped(&b, pad, bl.text, w)
		case bWhole:
			b.WriteString(pad + bl.text + "\n")
		case bDetail:
			writeWrapped(&b, pad+strings.Repeat(" ", 5), bl.text, w)
		case bSecret:
			// Alone on its line and whole, whatever the width.
			if bl.label != "" {
				b.WriteString(pad + bl.label + "\n")
			}
			b.WriteString(pad + "  " + bl.text + "\n")
		}
	}
	return b.String()
}

// writeWrapped writes lead then text, wrapping text under itself.
func writeWrapped(b *strings.Builder, lead, text string, w int) {
	leadW := ansi.StringWidth(lead)
	avail := max(w-leadW, 20)
	lines := strings.Split(ansi.Wrap(text, avail, " "), "\n")
	for k, line := range lines {
		if k == 0 {
			b.WriteString(lead + line + "\n")
		} else {
			b.WriteString(strings.Repeat(" ", leadW) + line + "\n")
		}
	}
}

// writeTable writes aligned columns, two spaces apart, with a muted header.
// When it is too wide, the columns that aren't kept are cut, widest first,
// with "…"; a kept column (an ID) is never cut, even if the line overflows.
func writeTable(b *strings.Builder, pad string, cols []Col, rows [][]string, w int, st style) {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = ansi.StringWidth(c.Name)
	}
	for _, row := range rows {
		for i := range cols {
			if i < len(row) {
				widths[i] = max(widths[i], ansi.StringWidth(row[i]))
			}
		}
	}
	total := func() int {
		t := len(pad) + 2*(len(cols)-1)
		for _, x := range widths {
			t += x
		}
		return t
	}
	for total() > w {
		widest := -1
		for i, c := range cols {
			if !c.Keep && widths[i] > 4 && (widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest] = max(4, widths[widest]-(total()-w))
	}
	cell := func(s string, i int) string {
		if !cols[i].Keep && ansi.StringWidth(s) > widths[i] {
			s = ansi.Truncate(s, widths[i], "…")
		}
		if i == len(cols)-1 {
			return s
		}
		return s + strings.Repeat(" ", max(0, widths[i]-ansi.StringWidth(s)))
	}
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = cell(c.Name, i)
	}
	b.WriteString(pad + st.muted.Render(strings.TrimRight(strings.Join(head, "  "), " ")) + "\n")
	for _, row := range rows {
		line := make([]string, len(cols))
		for i := range cols {
			v := ""
			if i < len(row) {
				v = row[i]
			}
			line[i] = cell(v, i)
		}
		b.WriteString(pad + strings.TrimRight(strings.Join(line, "  "), " ") + "\n")
	}
}

// tableWidth is a table's natural width.
func tableWidth(pad string, cols []Col, rows [][]string) int {
	t := len(pad) + 2*(len(cols)-1)
	for i, c := range cols {
		width := ansi.StringWidth(c.Name)
		for _, row := range rows {
			if i < len(row) {
				width = max(width, ansi.StringWidth(row[i]))
			}
		}
		t += width
	}
	return t
}

// writeRecords writes each row as its own block of aligned fields, numbered,
// blank fields ("-") left out.
func writeRecords(b *strings.Builder, pad string, cols []Col, rows [][]string, w int) {
	labelW := 0
	for _, c := range cols {
		labelW = max(labelW, ansi.StringWidth(c.Name))
	}
	for r, row := range rows {
		if r > 0 {
			b.WriteString("\n")
		}
		b.WriteString(pad + "#" + strconv.Itoa(r+1) + "\n")
		for i, c := range cols {
			if i >= len(row) || row[i] == "-" || row[i] == "" {
				continue
			}
			lead := pad + "  " + c.Name + strings.Repeat(" ", labelW-ansi.StringWidth(c.Name)) + "  "
			writeWrapped(b, lead, row[i], w)
		}
	}
}
