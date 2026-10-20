package integration

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"clawbridge/internal/bus"
	"clawbridge/internal/setup"
	"clawbridge/internal/tui"
)

func TestAuditSetupRunnerUsesReviewedCommandWithoutDuplicateExecutable(t *testing.T) {
	plan, err := setup.BuildPlan(setupCreateInput())
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "fake-gcloud")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s' \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := (setup.GCloudRunner{Path: fake}).Run(context.Background(), plan.Operations[0].Command)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "projects" {
		t.Fatalf("first argument=%q, want projects (executable must not also be an argument)", output)
	}
}

func TestAuditTUIEscapesPeerTerminalControls(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	// OSC 52 controls a terminal clipboard. Never print the actual sequence in
	// failure output; only inspect the returned renderer string in memory.
	payload := "peer text \x1b]52;c;YXVkaXQ=\a"
	f.send("w", "p", "lead", "worker", payload, "audit-terminal-controls")
	model, err := tui.New(tui.Config{
		Executor: f.store, Mode: tui.ModeLocal, Workspace: "w", Project: "p",
		Session: "worker", Instance: "worker-instance",
	})
	if err != nil {
		t.Fatal(err)
	}
	model, command := model.Show(tui.ScreenInbox)
	model = applyTUIMsg(t, model, runTUICmd(t, command))
	if strings.Contains(tui.Render(model), "\x1b]52;") {
		t.Fatal("TUI emitted a peer-controlled OSC clipboard sequence")
	}
	if got := model.State().Messages; len(got) != 1 || got[0].Body != payload {
		t.Fatal("display escaping changed the message data")
	}
}

type auditControlExecutor struct{ payload string }

func (e auditControlExecutor) Execute(ctx context.Context, req bus.Request) (bus.Result, error) {
	result, err := (&recordingTUIExecutor{source: "server"}).Execute(ctx, req)
	var inject func(any)
	inject = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				switch key {
				case "name", "slug", "principal", "sender", "body", "agent_kind":
					value[key] = e.payload
				default:
					inject(child)
				}
			}
		case []any:
			for _, child := range value {
				inject(child)
			}
		}
	}
	inject(result.Data)
	return result, err
}

func TestAuditTUIDynamicSurfacesEscapeControls(t *testing.T) {
	const payload = "界é👩‍💻\x1b]52;c;YWJj\a\r\x9b31m\x1b[2J"
	model, err := tui.New(tui.Config{
		Executor: auditControlExecutor{payload}, Mode: tui.ModeCloud,
		Workspace: payload, Project: payload, Session: payload, Instance: "bound-instance",
		Principal: payload,
		Doctor: tui.DoctorFunc(func(context.Context) ([]tui.Check, error) {
			return []tui.Check{{ID: payload, Title: payload, Detail: payload, Fix: payload, Status: tui.Warn}}, nil
		}),
		Setup: tui.SetupFuncs{PlanFunc: func(context.Context) (tui.SetupPlan, error) {
			return tui.SetupPlan{Title: payload, Plan: payload, MonthlyCost: payload, Confirmation: payload}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, screen := range []tui.Screen{tui.ScreenOverview, tui.ScreenProjects, tui.ScreenSessions, tui.ScreenInbox, tui.ScreenDoctor, tui.ScreenCloud} {
		t.Run(string(screen), func(t *testing.T) {
			current, command := model.Show(screen)
			current = applyTUIMsg(t, current, runTUICmd(t, command))
			rendered := tui.Render(current)
			// Render legitimately adds trusted SGR styling. Remove only SGR,
			// leaving OSC, non-SGR CSI, C0/C1 and directional controls detectable.
			plain := regexp.MustCompile("\x1b\\[[0-9;:]*m").ReplaceAllString(rendered, "")
			for _, r := range plain {
				if r != '\n' && r != '\t' && unicode.IsControl(r) {
					t.Fatalf("dynamic surface emitted raw terminal control U+%04X", r)
				}
			}
			if !strings.Contains(rendered, "界é👩‍💻") || !strings.Contains(rendered, `\x1b`) {
				t.Fatal("Unicode text or visible escape representation missing")
			}
		})
	}
}
