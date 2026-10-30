package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
)

const turnHead = `{"session_id":"00000000-0000-4000-8000-000000000001","transcript_path":"/t.jsonl","hook_event_name":"UserPromptSubmit","prompt":`

// turnRun runs the UserPromptSubmit hook with input, and returns what it
// printed on stdout and stderr.
func turnRun(t *testing.T, input string, args ...string) (string, string) {
	t.Helper()
	contextStdin = strings.NewReader(input)
	t.Cleanup(func() { contextStdin = os.Stdin })
	if len(args) == 0 {
		args = []string{"hook", "turn", "--from-hook"}
	}
	var out, errs bytes.Buffer
	if err := Run(context.Background(), args, &out, &errs); err != nil {
		t.Fatal(err)
	}
	return out.String(), errs.String()
}

// noSecret fails when any file under dir holds SECRET: the prompt is never
// written.
func noSecret(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if data, _ := os.ReadFile(path); bytes.Contains(data, []byte("SECRET")) {
				t.Fatalf("%s holds the prompt: %s", path, data)
			}
		}
		return nil
	})
}

// FR26: the turn hook marks a turn's start, and the push that started a
// channel turn; it prints nothing (Claude Code would give it to the model)
// and never keeps the prompt. Input it can't read still marks a start.
func TestTurnHookMarksTheTurn(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(0), "latest_seq": float64(0)}}
	dir := queueHookSetup(t, server)
	home := filepath.Dir(dir)
	channelPrompt := `"<channel source=\"plugin:clawbridge:wake\" kind=\"wake\" count=\"2\" seq=\"41\">2 ClawBridge messages are waiting; call inbox</channel>"}`
	for name, c := range map[string]struct {
		input string
		seq   int64
		args  []string
	}{
		"plain":     {turnHead + `"SECRET run the tests"}`, 0, nil},
		"channel":   {turnHead + channelPrompt, 41, nil},
		"exec form": {turnHead + channelPrompt, 41, []string{"hook", "turn", "--from-hook", "--hook-tag", ctxwatch.HookTag}},
		"lookalike": {turnHead + `"SECRET <channel source=\"plugin:clawbridge:wake\" kind=\"wake\" count=\"2\" seq=\"41\">"}`, 0, nil},
		"other src": {turnHead + `"<channel source=\"plugin:evil:wake\" kind=\"wake\" count=\"2\" seq=\"41\">SECRET</channel>"}`, 0, nil},
		"not json":  {`SECRET`, 0, nil},
		"oversized": {turnHead + `"<channel source=\"plugin:clawbridge:wake\" kind=\"wake\" count=\"2\" seq=\"41\">SECRET` + strings.Repeat("x", 1<<20) + `"}`, 0, nil},
	} {
		for _, env := range []string{"", "agent"} {
			t.Setenv("CLAWBRIDGE_SESSION", env)
			_ = os.Remove(filepath.Join(dir, "agent.turn.json"))
			out, errs := turnRun(t, c.input, c.args...)
			if out != "" || errs != "" {
				t.Fatalf("%s: printed %q %q", name, out, errs)
			}
			rec, ok := ctxwatch.LoadTurn(dir, "agent")
			if !ok || rec.ChannelSeq != c.seq || rec.StartedAt == "" || rec.EndedAt != "" {
				t.Fatalf("%s %q: %+v %v", name, env, rec, ok)
			}
			if c.seq != 0 && rec.ChannelAt != rec.StartedAt {
				t.Fatalf("%s: %+v", name, rec)
			}
		}
	}
	noSecret(t, home)
	var errs bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "turn"}, &bytes.Buffer{}, &errs); err == nil || !strings.Contains(err.Error(), "hook turn --from-hook") {
		t.Fatal(err)
	}
}

// The marks are the agent's own: a session with no MCP server record of
// this agent's, or a record of another agent (a nested claude that
// inherited CLAWBRIDGE_SESSION), gets no mark from either hook.
func TestTurnMarksNeedTheAgentsRecord(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(0), "latest_seq": float64(0)}}
	dir := queueHookSetup(t, server)
	for name, set := range map[string]func(){
		"no record": func() { t.Setenv("CLAWBRIDGE_SESSION", "solo") },
		"other agent": func() {
			t.Setenv("CLAWBRIDGE_SESSION", "agent")
			t.Setenv("CLAWBRIDGE_AGENT_PID", strconv.Itoa(os.Getppid()+1))
		},
	} {
		set()
		turnRun(t, turnHead+`"x"}`)
		stopRun(t, stopInput)
		if turns, _ := filepath.Glob(filepath.Join(dir, "*.turn.json")); len(turns) != 0 {
			t.Fatalf("%s: %v", name, turns)
		}
	}
}

// The Stop hook marks the turn's end only when it lets the agent stop: a
// block continues the turn, whose own later Stop (stop_hook_active) ends
// it. Unreadable input marks nothing.
func TestStopHookMarksTheEnd(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(2), "latest_seq": float64(7), "unread_count": float64(2)}}
	dir := queueHookSetup(t, server)
	start := func() {
		t.Helper()
		_ = os.Remove(filepath.Join(dir, "agent.turn.json"))
		if err := ctxwatch.MarkTurnStarted(dir, "agent", time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	ended := func() bool {
		rec, ok := ctxwatch.LoadTurn(dir, "agent")
		return ok && rec.EndedAt != ""
	}
	start()
	if out := stopRun(t, stopInput); out != blocked || ended() {
		t.Fatalf("a block ended the turn: %q", out)
	}
	if out := stopRun(t, strings.Replace(stopInput, `"stop_hook_active":false`, `"stop_hook_active":true`, 1)); out != "" || !ended() {
		t.Fatalf("the continuation's stop didn't end it: %q", out)
	}
	rec, ok := ctxwatch.LoadTurn(dir, "agent")
	if !ctxwatch.Idle(rec, ok, time.Now()) {
		t.Fatal(rec)
	}
	// Nothing new to block for: the turn ends.
	start()
	if out := stopRun(t, stopInput); out != "" || !ended() {
		t.Fatalf("%q", out)
	}
	for _, input := range []string{"", "SECRET", `{"session_id":""}`} {
		start()
		if stopRun(t, input); ended() {
			t.Fatalf("unreadable input %q ended the turn", input)
		}
	}
}

// A panic in either hook ends it quietly with status 0 and nothing on
// stdout: for UserPromptSubmit a crash would block and erase the prompt.
func TestHooksSurviveAPanic(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(2), "latest_seq": float64(7)}}
	queueHookSetup(t, server)
	hookFault = func() { panic("boom") }
	t.Cleanup(func() { hookFault = nil })
	if out, errs := turnRun(t, turnHead+`"x"}`); out != "" || errs != "" {
		t.Fatal(out, errs)
	}
	if out := stopRun(t, stopInput); out != "" {
		t.Fatal(out)
	}
}
