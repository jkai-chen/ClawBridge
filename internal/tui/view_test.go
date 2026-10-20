package tui

import (
	"strings"
	"testing"
	"unicode"
)

func TestTerminalTextEscapesControlsAndPreservesText(t *testing.T) {
	normal := "Hello 世界 café🙂👩‍💻\nnext\tcolumn"
	if terminalText(normal) != normal {
		t.Fatal("normal Unicode, newline or tab changed")
	}
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		if strings.ContainsRune(terminalText(string(r)), r) {
			t.Fatalf("directional control U+%04X survived", r)
		}
	}
	for r := rune(0); r <= 0x9f; r++ {
		if !unicode.IsControl(r) || r == '\n' || r == '\t' {
			continue
		}
		if strings.ContainsRune(terminalText(string(r)), r) {
			t.Fatalf("control U+%04X survived", r)
		}
	}
}

func TestRenderEscapesAllDynamicFieldsWithoutChangingStoredText(t *testing.T) {
	payload := "peer\x1b]52;c;YXVkaXQ=\a\r\x9b31m\u202e\x1b[2J"
	m := Model{
		cfg:     Config{Mode: ModeLocal, Workspace: payload, Project: payload, Session: payload, Principal: payload, Provenance: payload},
		errText: payload, actionErr: payload, source: payload, draft: payload, confirm: payload,
		workspace: WorkspaceView{Name: payload}, project: ProjectView{Slug: payload}, session: SessionView{Name: payload},
		identity: IdentityView{Principal: payload, Provenance: payload}, fresh: Freshness{UpdatedAt: payload},
		projects: []ProjectRow{{Slug: payload, Role: payload, Name: payload}},
		sessions: []SessionRow{{Name: payload, Role: payload, AgentKind: payload, Status: payload, Presence: payload}},
		messages: []MessageRow{{Sender: payload, Kind: payload, Body: payload, ID: payload, CreatedAt: payload, ReplyTo: payload}},
		checks:   []Check{{Title: payload, ID: payload, Status: Status(payload), Detail: payload, Fix: payload}},
		plan:     &SetupPlan{Title: payload, Plan: payload, MonthlyCost: payload, Region: payload, ResidencyNotes: payload, Confirmation: payload, Resources: []Resource{{Action: payload, Name: payload, Kind: payload, Region: payload, Monthly: payload}}},
		apply:    &ApplyResult{Summary: payload, Applied: []string{payload}},
	}
	for _, screen := range orderedScreens {
		for _, confirmation := range []bool{false, true} {
			m.screen, m.confirming, m.inputMode = screen, confirmation, inputReply
			view := Render(m)
			for _, forbidden := range []string{"\x1b]52;", "\a", "\r", "\x9b"} {
				if strings.Contains(view, forbidden) {
					t.Fatalf("dynamic terminal control survived on screen %v", screen)
				}
			}
		}
	}
	if m.messages[0].Body != payload || m.plan.Plan != payload || m.draft != payload {
		t.Fatal("render mutated stored/action input")
	}
}
