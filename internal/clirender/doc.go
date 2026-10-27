package clirender

import (
	"fmt"
	"strings"
	"unicode"
)

// Safe makes a string from data safe for a terminal (the R1 rule): control
// characters, ESC among them, and bidi controls are escaped, so data can
// never move the cursor, colour the screen or reorder text. A newline and
// a tab show as ↵ and ⇥, so a value stays on its own line. The primitives
// below call it on everything they print; a view can't skip it.
func Safe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("↵")
		case r == '\t':
			b.WriteString("⇥")
		case unicode.IsControl(r):
			if r <= 0xff {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		case bidi(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func bidi(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// Level is a status line's meaning, shown as a word (O-glyphs).
type Level int

const (
	OK Level = iota
	Pass
	Warn
	Fail
	Info
	// Error is an action that failed; Busy one still running. The TUI's
	// status line uses them (TUI-UX-AUDIT §3.3).
	Error
	Busy
)

var levelWords = map[Level]string{OK: "OK", Pass: "PASS", Warn: "WARN", Fail: "FAIL", Info: "NOTE", Error: "ERROR", Busy: "…"}

// Word is a level's word, which carries its meaning without colour.
func Word(l Level) string { return levelWords[l] }

// Col is a table column. Keep marks one the next command needs (an ID, a
// revision, a link): it is never cut (L4).
type Col struct {
	Name string
	Keep bool
}

type blockKind int

const (
	bTitle blockKind = iota
	bKV
	bTable
	bStatus
	bNote
	bSecret
	bSection
	bText
	bDetail
)

type block struct {
	kind   blockKind
	text   string
	label  string
	indent int
	level  Level
	cols   []Col
	rows   [][]string
	// records: a table that doesn't fit is shown as one block per row
	// instead of being cut (the generic layout, L1).
	records bool
}

// Doc is a command's human output, built from a few primitives; Render
// lays it out. Every string is escaped by Safe as it is added.
type Doc struct {
	blocks []block
	indent int
}

// Title is the heading line.
func (d *Doc) Title(s string) { d.blocks = append(d.blocks, block{kind: bTitle, text: Safe(s)}) }

// Section starts a titled group; what follows is indented under it until
// End.
func (d *Doc) Section(title string) {
	d.blocks = append(d.blocks, block{kind: bSection, text: Safe(title), indent: d.indent})
	d.indent++
}

// End closes the innermost Section.
func (d *Doc) End() {
	if d.indent > 0 {
		d.indent--
	}
}

// KV is one aligned label and value; consecutive KVs align together. A long
// value wraps under itself, never cut.
func (d *Doc) KV(label, value string) {
	d.blocks = append(d.blocks, block{kind: bKV, label: Safe(label), text: Safe(value), indent: d.indent})
}

// Table is a table with a header; an empty one prints empty instead, such
// as "No sessions.".
func (d *Doc) Table(cols []Col, rows [][]string, empty string) {
	if len(rows) == 0 {
		d.Text(empty)
		return
	}
	safeRows := make([][]string, len(rows))
	for i, row := range rows {
		safeRows[i] = make([]string, len(row))
		for j, cell := range row {
			safeRows[i][j] = Safe(cell)
		}
	}
	safeCols := make([]Col, len(cols))
	for i, c := range cols {
		safeCols[i] = Col{Name: Safe(c.Name), Keep: c.Keep}
	}
	d.blocks = append(d.blocks, block{kind: bTable, cols: safeCols, rows: safeRows, indent: d.indent})
}

// Records is a table that, when it doesn't fit the width, is shown as one
// labelled block per row, every field whole, instead of cutting columns.
func (d *Doc) Records(cols []Col, rows [][]string, empty string) {
	d.Table(cols, rows, empty)
	if n := len(d.blocks); n > 0 && d.blocks[n-1].kind == bTable {
		d.blocks[n-1].records = true
	}
}

// Status is a line led by its level's word: PASS, WARN, FAIL, OK, NOTE.
func (d *Doc) Status(level Level, text string) {
	d.blocks = append(d.blocks, block{kind: bStatus, level: level, text: Safe(text), indent: d.indent})
}

// Detail is a line under the Status before it, aligned with its text,
// such as the action a failed check asks for.
func (d *Doc) Detail(s string) {
	d.blocks = append(d.blocks, block{kind: bDetail, text: Safe(s), indent: d.indent})
}

// Note is a muted line of advice.
func (d *Doc) Note(s string) {
	d.blocks = append(d.blocks, block{kind: bNote, text: Safe(s), indent: d.indent})
}

// Text is a plain line.
func (d *Doc) Text(s string) {
	d.blocks = append(d.blocks, block{kind: bText, text: Safe(s), indent: d.indent})
}

// Secret is a secret printed once, alone on its line and whole: never
// wrapped or cut, so it copies in one piece (M3).
func (d *Doc) Secret(label, value string) {
	d.blocks = append(d.blocks, block{kind: bSecret, label: Safe(label), text: Safe(value), indent: d.indent})
}
