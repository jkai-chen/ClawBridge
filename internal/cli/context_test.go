package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/doctor"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "ctxwatch", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func bindForTest(t *testing.T, session, agent, transcript string, levels *ctxwatch.Levels) {
	t.Helper()
	dir, err := contextDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(ctxwatch.Binding{Version: 1, Agent: agent, Transcript: transcript, BoundAt: "2026-10-26T10:00:00Z", Levels: levels})
	if err := os.WriteFile(filepath.Join(dir, session+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runContextCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), append([]string{"context"}, args...), &out, io.Discard)
	return out.String(), err
}

func TestContextShow(t *testing.T) {
	claude, codex := fixture(t, "claude-code.jsonl"), fixture(t, "codex.jsonl")
	isolated(t)
	out, err := runContextCmd(t, "show", "--agent", "claude-code", "--transcript", claude)
	if err != nil || !strings.Contains(out, "context: 43k tokens, 1 compaction") || !strings.Contains(out, "transcript: "+claude) {
		t.Fatalf("%q %v", out, err)
	}
	out, err = runContextCmd(t, "show", "--agent", "codex", "--transcript", codex, "--json")
	var got struct {
		ctxwatch.Reading
		Transcript string `json:"transcript"`
	}
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.Tokens != 30500 || got.Window != 258400 || got.Transcript != codex {
		t.Fatalf("%q %v", out, err)
	}
	// The session's binding record, by name or from CLAWBRIDGE_SESSION.
	if out, err = runContextCmd(t, "show", "--session", "worker-1"); err != nil || out != "context: worker-1 is not bound to a transcript\n" {
		t.Fatalf("%q %v", out, err)
	}
	bindForTest(t, "worker-1", ctxwatch.Codex, codex, nil)
	t.Setenv("CLAWBRIDGE_SESSION", "worker-1")
	if out, err = runContextCmd(t, "show"); err != nil || !strings.Contains(out, "context: 30k tokens (window 258k), 1 compaction") {
		t.Fatalf("%q %v", out, err)
	}
	// A transcript in the other agent's format is unsupported, not guessed.
	if out, err = runContextCmd(t, "show", "--agent", "codex", "--transcript", claude); err != nil || !strings.Contains(out, "unsupported transcript") {
		t.Fatalf("%q %v", out, err)
	}
	t.Setenv("CLAWBRIDGE_SESSION", "")
	for _, args := range [][]string{
		{},
		{"bind"},
		{"show"},
		{"show", "--agent", "codex"},
		{"show", "--transcript", codex},
		{"show", "extra"},
		{"show", "--agent", "cursor", "--transcript", codex},
		{"show", "--session", "../escape"},
	} {
		if _, err := runContextCmd(t, args...); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestDoctorContext(t *testing.T) {
	claude := fixture(t, "claude-code.jsonl")
	isolated(t)
	check := func(session string) doctor.Check {
		t.Helper()
		var checks []doctor.Check
		r := doctor.Run(context.Background(), doctor.Input{Context: func() doctor.ContextState { return doctorContext(session) }}, nil)
		for _, c := range r.Checks {
			if c.ID == "context" {
				checks = append(checks, c)
			}
		}
		if len(checks) != 1 {
			t.Fatalf("%d context checks", len(checks))
		}
		return checks[0]
	}
	if c := check("worker-1"); c.Status != doctor.Pass || !strings.Contains(c.Message, "not bound") {
		t.Fatalf("%+v", c)
	}
	bindForTest(t, "worker-1", ctxwatch.ClaudeCode, claude, nil)
	if c := check("worker-1"); c.Status != doctor.Pass || c.Path != claude || !strings.Contains(c.Message, "43005 tokens in use") {
		t.Fatalf("%+v", c)
	}
	for levels, want := range map[ctxwatch.Levels]string{
		{WarnTokens: 40000, HandoverTokens: 50000}: "Past the warn level",
		{WarnTokens: 30000, HandoverTokens: 40000}: "Past the handover level",
	} {
		bindForTest(t, "worker-1", ctxwatch.ClaudeCode, claude, &levels)
		if c := check("worker-1"); c.Status != doctor.Warn || !strings.Contains(c.Message, want) {
			t.Fatalf("%+v", c)
		}
	}
	// Since a compaction the context is unknown, so no level applies.
	compacted := filepath.Join(t.TempDir(), "compacted.jsonl")
	if err := os.WriteFile(compacted, []byte(`{"type":"assistant","sessionId":"s","message":{"model":"m","usage":{"input_tokens":900000}}}`+"\n"+
		`{"type":"system","subtype":"compact_boundary","sessionId":"s"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bindForTest(t, "worker-1", ctxwatch.ClaudeCode, compacted, &ctxwatch.Levels{WarnTokens: 30000, HandoverTokens: 40000})
	if c := check("worker-1"); c.Status != doctor.Pass || !strings.Contains(c.Message, "unknown until the next model call") {
		t.Fatalf("%+v", c)
	}
	bindForTest(t, "worker-1", ctxwatch.Codex, claude, nil)
	if c := check("worker-1"); c.Status != doctor.Warn || !strings.Contains(c.Message, "unsupported") {
		t.Fatalf("%+v", c)
	}
	bindForTest(t, "worker-1", ctxwatch.ClaudeCode, filepath.Join(t.TempDir(), "gone.jsonl"), nil)
	if c := check("worker-1"); c.Status != doctor.Warn || c.Action == "" {
		t.Fatalf("%+v", c)
	}
}

func TestDescribeReading(t *testing.T) {
	ok := ctxwatch.Reading{Status: ctxwatch.StatusOK, Tokens: 43005, UsageAt: "2026-01-01T11:00:09Z", Compactions: 2}
	unknown := ctxwatch.Reading{Status: ctxwatch.StatusOK, Compactions: 1}
	partial, partialNone := ok, ok
	partial.Partial = true
	partialNone.Partial, partialNone.Compactions = true, 0
	for r, want := range map[ctxwatch.Reading]string{
		ok:                          "context: 43k tokens, 2 compactions",
		unknown:                     "context: unknown until the first model call after the compaction, 1 compaction",
		partial:                     "context: 43k tokens, at least 2 compactions",
		partialNone:                 "context: 43k tokens, no compaction in the last 4 MiB",
		{Status: ctxwatch.StatusOK}: "context: no model call yet, no compaction",
	} {
		if got := describeReading(r); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
