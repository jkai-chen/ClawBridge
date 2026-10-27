package ctxwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fixtures are hand-written in the shape of Claude Code's transcript
// and Codex's rollout, holding the line types the parsers read and a few
// they skip. Nothing in them comes from a real session.
func TestReadsTheFixtures(t *testing.T) {
	for _, c := range []struct {
		agent, file string
		want        Reading
	}{
		{ClaudeCode, "testdata/claude-code.jsonl", Reading{Agent: ClaudeCode, Status: StatusOK, Tokens: 43005, Compactions: 1,
			LastCompactionAt: "2026-01-01T11:00:00Z", UsageAt: "2026-01-01T11:00:09Z"}},
		{Codex, "testdata/codex.jsonl", Reading{Agent: Codex, Status: StatusOK, Tokens: 30500, Window: 258400, Compactions: 1,
			LastCompactionAt: "2026-01-02T10:00:00Z", UsageAt: "2026-01-02T10:00:08Z"}},
	} {
		got, err := Read(c.agent, c.file)
		if err != nil {
			t.Fatal(err)
		}
		got.WrittenAt = ""
		if got != c.want {
			t.Fatalf("%s:\n got %+v\nwant %+v", c.agent, got, c.want)
		}
	}
	// Each parser refuses the other's format rather than guessing.
	for _, c := range []struct{ agent, file string }{{ClaudeCode, "testdata/codex.jsonl"}, {Codex, "testdata/claude-code.jsonl"}} {
		if got, err := Read(c.agent, c.file); err != nil || got.Status != StatusUnsupported || got.Tokens != 0 {
			t.Fatalf("%s on %s: %+v %v", c.agent, c.file, got, err)
		}
	}
}

// The fixtures stay synthetic: no tool-call IDs, addresses or home paths.
func TestFixturesAreSynthetic(t *testing.T) {
	hooks, _ := filepath.Glob("testdata/hooks/*.json")
	if len(hooks) != 4 {
		t.Fatalf("%d hook fixtures", len(hooks))
	}
	for _, name := range append([]string{"testdata/claude-code.jsonl", "testdata/codex.jsonl", "testdata/README.md"}, hooks...) {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"toolu_", "call_", "msg_", "req_", "@", "/Users/", "/home/", "rate_limit"} {
			if strings.Contains(string(data), bad) {
				t.Fatalf("%s contains %q", name, bad)
			}
		}
	}
}

func write(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const secret = "SECRET-CONTENT-7f3a"

func claudeAssistant(tokens int, extra string) string {
	return `{"type":"assistant","sessionId":"s","timestamp":"2026-10-26T10:00:00Z",` + extra +
		`"message":{"model":"m","content":[{"type":"text","text":"` + secret + `"}],"usage":{"input_tokens":` +
		jsonInt(tokens) + `,"cache_read_input_tokens":1000,"cache_creation_input_tokens":10}}}`
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestClaudeCodeEdgeCases(t *testing.T) {
	for name, c := range map[string]struct {
		lines  []string
		status string
		tokens int64
	}{
		"main thread": {[]string{claudeAssistant(5, "")}, StatusOK, 1015},
		// A subagent's sidechain has its own context.
		"sidechain after": {[]string{claudeAssistant(5, ""), claudeAssistant(900000, `"isSidechain":true,`)}, StatusOK, 1015},
		// An API error shown in the transcript carries no real usage.
		"synthetic after": {[]string{claudeAssistant(5, ""),
			`{"type":"assistant","sessionId":"s","message":{"model":"<synthetic>","usage":{"input_tokens":0}}}`}, StatusOK, 1015},
		"no call yet": {[]string{`{"type":"user","sessionId":"s","message":{"content":"` + secret + `"}}`}, StatusOK, 0},
		"usage renamed": {[]string{`{"type":"assistant","sessionId":"s","message":{"model":"m","usage":{"inputTokens":5}}}`},
			StatusUnsupported, 0},
		"not a transcript": {[]string{"hello", `{"a":1}`, `[1,2]`}, StatusUnsupported, 0},
		"empty":            {[]string{""}, StatusUnsupported, 0},
		// The usage before a compaction no longer applies.
		"compacted since": {[]string{claudeAssistant(5, ""), `{"type":"system","subtype":"compact_boundary","sessionId":"s"}`},
			StatusOK, 0},
	} {
		got, err := Read(ClaudeCode, write(t, c.lines...))
		if err != nil || got.Status != c.status || got.Tokens != c.tokens {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
}

func TestCodexEdgeCases(t *testing.T) {
	count := func(tokens, window int) string {
		return `{"type":"event_msg","timestamp":"2026-10-26T10:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":` +
			jsonInt(tokens) + `},"model_context_window":` + jsonInt(window) + `}}}`
	}
	for name, c := range map[string]struct {
		lines          []string
		status         string
		tokens, window int64
	}{
		"last count wins": {[]string{count(5, 100), `{"type":"compacted","timestamp":"2026-10-26T10:01:00Z"}`, count(7, 200)}, StatusOK, 7, 200},
		// A token_count before the first call has no info yet.
		"null info": {[]string{`{"type":"session_meta","payload":{"id":"x"}}`,
			`{"type":"event_msg","payload":{"type":"token_count","info":null}}`}, StatusOK, 0, 0},
		"usage renamed": {[]string{`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"inputTokens":5}}}}`},
			StatusUnsupported, 0, 0},
		"not a rollout":   {[]string{`{"type":"assistant","sessionId":"s"}`}, StatusUnsupported, 0, 0},
		"compacted since": {[]string{count(5, 100), `{"type":"compacted","timestamp":"2026-10-26T10:01:00Z"}`}, StatusOK, 0, 0},
	} {
		got, err := Read(Codex, write(t, c.lines...))
		if err != nil || got.Status != c.status || got.Tokens != c.tokens || got.Window != c.window {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
}

// Only the last 4 MiB are read; the first partial line is dropped, a line
// over 1 MiB is skipped whole, and reading goes on after it.
func TestReadsABoundedTail(t *testing.T) {
	pad := `{"type":"user","sessionId":"s","message":{"content":"` + strings.Repeat("p", 1000) + `"}}`
	lines := []string{claudeAssistant(111, "")}
	for range (5 << 20) / len(pad) {
		lines = append(lines, pad)
	}
	huge := `{"type":"assistant","sessionId":"s","message":{"model":"m","content":"` + strings.Repeat("h", maxLine) +
		`","usage":{"input_tokens":999999}}}`
	lines = append(lines, `{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-26T11:00:00Z"}`, huge, claudeAssistant(222, ""))
	got, err := Read(ClaudeCode, write(t, lines...))
	if err != nil || !got.Partial || got.Tokens != 1232 || got.Compactions != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

// Counts and times only: nothing of a line's content ever comes out.
func TestNoContentComesOut(t *testing.T) {
	got, err := Read(ClaudeCode, write(t, claudeAssistant(5, ""), `{"type":"user","sessionId":"s","message":{"content":"`+secret+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	if strings.Contains(string(out), secret) {
		t.Fatalf("content leaked: %s", out)
	}
}

func TestRefusesWhatIsNotTheUsersFile(t *testing.T) {
	dir := t.TempDir()
	real := write(t, claudeAssistant(5, ""))
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"link": link, "directory": dir, "missing": filepath.Join(dir, "none.jsonl")} {
		if _, err := Read(ClaudeCode, path); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := Read("cursor", real); err == nil {
		t.Fatal("an unknown agent kind was accepted")
	}
}

func TestLoadBinding(t *testing.T) {
	dir := t.TempDir()
	if _, bound, err := LoadBinding(dir, "worker-1"); err != nil || bound {
		t.Fatal("a missing record is simply not bound", err)
	}
	put := func(name, body string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"version":1,"agent":"claude-code","transcript":"/home/u/.claude/projects/p/s.jsonl","bound_at":"2026-10-26T10:00:00Z",` +
		`"levels":{"warn_tokens":600000,"handover_tokens":650000},"handover_hint":"Update RESUME HERE."}`
	put("worker-1", good, 0o600)
	b, bound, err := LoadBinding(dir, "worker-1")
	if err != nil || !bound || b.Agent != ClaudeCode || b.Levels == nil || b.Levels.HandoverTokens != 650000 {
		t.Fatal(b, bound, err)
	}
	bad := map[string]struct {
		body string
		mode os.FileMode
	}{
		"relative":      {`{"version":1,"agent":"codex","transcript":"rollout.jsonl"}`, 0o600},
		"agent":         {`{"version":1,"agent":"cursor","transcript":"/t.jsonl"}`, 0o600},
		"version":       {`{"version":2,"agent":"codex","transcript":"/t.jsonl"}`, 0o600},
		"unknown field": {`{"version":1,"agent":"codex","transcript":"/t.jsonl","extra":true}`, 0o600},
		"not json":      {`version=1`, 0o600},
	}
	if runtime.GOOS != "windows" {
		bad["readable by others"] = struct {
			body string
			mode os.FileMode
		}{good, 0o644}
	}
	for name, c := range bad {
		put("s-"+strings.ReplaceAll(name, " ", "-"), c.body, c.mode)
		if _, _, err := LoadBinding(dir, "s-"+strings.ReplaceAll(name, " ", "-")); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "worker-1.json"), filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadBinding(dir, "linked"); err == nil {
		t.Fatal("a linked record was accepted")
	}
	for _, name := range []string{"", ".", "..", "../worker-1", "a/b", `a\b`, "c:d"} {
		if _, _, err := LoadBinding(dir, name); err == nil {
			t.Fatalf("session name %q accepted", name)
		}
	}
}

func TestLevels(t *testing.T) {
	l := Levels{WarnTokens: 600000, HandoverTokens: 650000}
	for tokens, want := range map[int64]string{0: "ok", 599999: "ok", 600000: "warn", 649999: "warn", 650000: "handover", 900000: "handover"} {
		if got := l.Level(tokens); got != want {
			t.Fatalf("%d: %s", tokens, got)
		}
	}
	if (Levels{}).Level(900000) != "ok" {
		t.Fatal("no levels means no level")
	}
}
