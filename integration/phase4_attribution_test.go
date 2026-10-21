package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type attributionExecutor struct {
	row map[string]any
	ops []string
}

func (e *attributionExecutor) Execute(_ context.Context, req bus.Request) (bus.Result, error) {
	e.ops = append(e.ops, req.Operation)
	if req.Operation != "list_sessions" {
		return bus.Result{}, fmt.Errorf("unexpected operation %s", req.Operation)
	}
	return bus.Result{Source: "server", Data: map[string]any{"sessions": []map[string]any{e.row}}}, nil
}

func TestPhase4SessionsDisplayExistingAttributionWithoutPeerInstance(t *testing.T) {
	owner := strings.Repeat("long-owner", 7) + "@example.test"
	host := "界e\u0301👩‍💻-" + strings.Repeat("long-host", 7) + "\x1b[2J\r\u202e"
	account := "local-account-not-google\n\t\x1b]52;c;payload\a"
	for _, mode := range []tui.Mode{tui.ModeLocal, tui.ModeCloud} {
		for _, width := range []int{8, 12, 20, 24, 32, 56, 80, 140} {
			t.Run(fmt.Sprintf("%s/%d", mode, width), func(t *testing.T) {
				e := &attributionExecutor{row: map[string]any{"name": "worker", "owner_principal": owner, "host": host, "account": account, "instance": "PEER-INSTANCE-MUST-NOT-LEAK", "agent_kind": "codex", "presence": "online"}}
				m, err := tui.New(tui.Config{Executor: e, Mode: mode, Workspace: "workspace", Project: "project"})
				if err != nil {
					t.Fatal(err)
				}
				m, _ = m.UpdateModel(tea.WindowSizeMsg{Width: width, Height: 45})
				m, cmd := m.Show(tui.Sessions)
				m = applyTUIMsg(t, m, runTUICmd(t, cmd))
				if len(e.ops) != 1 || e.ops[0] != "list_sessions" {
					t.Fatal("display triggered extra authority operations")
				}
				rows := m.State().Sessions
				if len(rows) != 1 || rows[0].Owner != owner || rows[0].Host != host || rows[0].Account != account {
					t.Fatal("display changed stored attribution")
				}
				output := ansi.Strip(tui.Render(m))
				if strings.Contains(output, "PEER-INSTANCE-MUST-NOT-LEAK") || strings.ContainsAny(output, "\x1b\r\a\u202e") {
					t.Fatal("peer instance or executable control leaked")
				}
				if width < 24 {
					if !strings.Contains(output, "Resize") {
						t.Fatal("tiny view lacks resize hint")
					}
					m = m.SetSize(32, 45)
					output = ansi.Strip(m.View())
				}
				// Gather overlapping viewports so full attribution remains inspectable.
				for i := 0; i < 120; i++ {
					m = m.Scroll(1)
					output += "\n" + ansi.Strip(m.View())
				}
				// Remove only presentation whitespace and borders to check wrapped details.
				flat := strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) || strings.ContainsRune("│╭╮╰╯─", r) {
						return -1
					}
					return r
				}, output)
				safeHost := strings.NewReplacer("\x1b", `\x1b`, "\r", `\x0d`, "\u202e", `\u202e`).Replace(host)
				for _, want := range []string{owner, safeHost, "Hostlabel(self-reported)", "Accountlabel(self-reported,notverifiedidentity)", "local-account-not-google"} {
					if !strings.Contains(flat, want) {
						t.Errorf("wrapped details lost attribution %q", want)
					}
				}
				if mode == tui.ModeLocal && !strings.Contains(flat, "noGoogleverificationclaimed") {
					t.Error("local attribution lacks authority qualification")
				}
			})
		}
	}
}
