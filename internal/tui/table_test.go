package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTableColumnsUseDisplayCellsAndSharedGutter(t *testing.T) {
	for _, width := range []int{20, 32, 56, 80, 140} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := Model{width: width, selected: 0}
			headers := []string{"SLUG", "ROLE", "NAME"}
			// Include wide glyphs, combining accents, joined emoji and table-breaking controls.
			rows := [][]string{{"界界e\u0301👩‍💻long", "owner", "name\nline\t\x1b[2J"}, {"ascii", "member", "other"}}
			lines := strings.Split(ansi.Strip(renderTable(m, headers, []int{28, 10, 42}, rows)), "\n")
			if !strings.HasPrefix(lines[0], "  ") || !strings.HasPrefix(lines[1], "› ") || !strings.HasPrefix(lines[2], "  ") {
				t.Fatalf("gutter mismatch: %q", lines)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > tableBudget(m) {
					t.Fatalf("row exceeds budget: %q", line)
				}
				if strings.ContainsAny(line, "\x1b\t") {
					t.Fatalf("raw control: %q", line)
				}
			}
			// Equal total widths establish padding includes display cells even for wide/combined text.
			if ansi.StringWidth(lines[0]) != ansi.StringWidth(lines[1]) || ansi.StringWidth(lines[1]) != ansi.StringWidth(lines[2]) {
				t.Fatalf("unequal column widths: %q", lines)
			}
		})
	}
	m := Model{width: 140}
	lines := strings.Split(ansi.Strip(renderTable(m, []string{"SLUG", "ROLE", "NAME"}, []int{28, 10, 42}, [][]string{{"界e\u0301", "owner", "sample"}})), "\n")
	if ansi.StringWidth(lines[0][:strings.Index(lines[0], "ROLE")]) != ansi.StringWidth(lines[1][:strings.Index(lines[1], "owner")]) {
		t.Fatal("role header and value have different cell offsets")
	}
}

func TestSessionsAttributionDetailsPreserveFullSanitizedMetadata(t *testing.T) {
	rows := sessionRows([]map[string]any{{"name": "worker", "owner_principal": "long-owner@example.test", "host": "界host\x1b[2J-long-name", "account": "not-google", "instance": "MUST-NOT-APPEAR"}})
	if rows[0].Host == "" || rows[0].Account != "not-google" {
		t.Fatal("metadata not decoded")
	}
	for _, width := range []int{32, 80, 140} {
		m := Model{width: width, sessions: rows, selected: 0, cfg: Config{Mode: ModeLocal}}
		output := ansi.Strip(renderSessions(m))
		details := output[strings.Index(output, "Selected session"):]
		flat := strings.ReplaceAll(details, "\n", "")
		for _, want := range []string{"long-owner@example.test", `界host\x1b[2J-long-name`, "self-reported", "not verified identity", "not-google", "no Google verification claimed"} {
			if !strings.Contains(flat, want) {
				t.Fatalf("width %d missing %q: %s", width, want, details)
			}
		}
		if strings.Contains(output, "MUST-NOT-APPEAR") || strings.Contains(output, "\x1b") {
			t.Fatal("instance or raw control leaked")
		}
		for _, line := range strings.Split(details, "\n") {
			if ansi.StringWidth(line) > tableBudget(m) {
				t.Fatalf("detail overflow: %q", line)
			}
		}
	}
	m := Model{width: 80, sessions: []SessionRow{{Name: "legacy"}}}
	if !strings.Contains(renderSessions(m), "Not recorded") {
		t.Fatal("missing host must be explicit")
	}
}

func TestSessionsDetailsShowTheLiveAssignment(t *testing.T) {
	rows := sessionRows([]map[string]any{
		{"name": "eng", "assignment": map[string]any{"state": "confirmed", "issue_uuid": "0e0e0e0e-0e0e-4e0e-8e0e-0e0e0e0e0e0e", "issue_identifier": "SYN-5", "task_role": "engineer", "scope_paths": []any{"internal/tui/**", "docs"}, "assignment_stale": true}},
		{"name": "idle", "assignment": nil},
	})
	if rows[0].Assignment != "confirmed SYN-5 (engineer) internal/tui/**, docs [owner stale]" || rows[1].Assignment != "" {
		t.Fatalf("%q %q", rows[0].Assignment, rows[1].Assignment)
	}
	for selected, want := range []string{"Assignment: confirmed SYN-5", "Assignment: None"} {
		m := Model{width: 140, sessions: rows, selected: selected}
		if out := ansi.Strip(renderSessions(m)); !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestSessionsShowTheLastContextReport(t *testing.T) {
	rows := sessionRows([]map[string]any{
		{"name": "eng", "context_at": "2026-01-01T10:00:00Z", "context_tokens": float64(612345), "context_window": float64(1000000), "context_level": "warn", "context_compactions": float64(2)},
		{"name": "quiet", "context_at": "2026-01-01T10:00:00Z", "context_tokens": float64(5000), "context_level": "ok", "context_backstop_at": "2026-01-01T09:00:00Z"},
		{"name": "none"},
	})
	if rows[0].Context != "612k warn [1000k]" || rows[1].Context != "5k" || rows[2].Context != "" {
		t.Fatalf("%q %q %q", rows[0].Context, rows[1].Context, rows[2].Context)
	}
	if rows[0].ContextDetail != "612k tokens, level warn, window 1000k, 2 compaction(s), reported 2026-01-01T10:00:00Z" ||
		!strings.Contains(rows[1].ContextDetail, "last compacted before its handover level at 2026-01-01T09:00:00Z") {
		t.Fatalf("%q %q", rows[0].ContextDetail, rows[1].ContextDetail)
	}
	for selected, want := range []string{"Context: 612k tokens, level warn", "Context: 5k tokens, level ok", "Context: Not reported"} {
		m := Model{width: 160, sessions: rows, selected: selected}
		out := ansi.Strip(renderSessions(m))
		if !strings.Contains(out, want) || !strings.Contains(out, "CONTEXT") {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
