package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"clawbridge/internal/ctxwatch"
)

// hookRun runs a context hook command with input on stdin.
func hookRun(t *testing.T, input string, args ...string) string {
	t.Helper()
	contextStdin = strings.NewReader(input)
	t.Cleanup(func() { contextStdin = os.Stdin })
	var out bytes.Buffer
	if err := Run(context.Background(), append([]string{"context"}, args...), &out, io.Discard); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String()
}

func loadTestBinding(t *testing.T, session string) (ctxwatch.Binding, bool) {
	t.Helper()
	dir, err := contextDir()
	if err != nil {
		t.Fatal(err)
	}
	b, bound, err := ctxwatch.LoadBinding(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	return b, bound
}

// jsonString is s as a JSON string: Windows paths carry backslashes.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func hookInput(transcript, extra string) string {
	return `{"session_id":"00000000-0000-4000-8000-000000000001","transcript_path":` + jsonString(transcript) +
		`,"cwd":"/work","hook_event_name":"SessionStart"` + extra + `}`
}

func TestBindFromHook(t *testing.T) {
	dir := isolated(t)
	config := filepath.Join(dir, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	projects := filepath.Join(config, "projects", "p")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projects, "s.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","sessionId":"s"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(transcript)
	outside := filepath.Join(dir, "elsewhere.jsonl")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Without a session, or with input it can't trust, nothing is bound
	// and nothing is printed, and the hook still succeeds.
	t.Setenv("CLAWBRIDGE_SESSION", "")
	if out := hookRun(t, hookInput(transcript, `,"source":"compact"`), "bind", "--from-hook"); out != "" {
		t.Fatalf("printed %q", out)
	}
	t.Setenv("CLAWBRIDGE_SESSION", "worker-1")
	for name, input := range map[string]string{
		"not JSON":     "{",
		"no path":      `{"session_id":"s","source":"compact"}`,
		"path number":  `{"session_id":"s","transcript_path":1,"source":"compact"}`,
		"outside":      hookInput(outside, `,"source":"compact"`),
		"dot-dot":      hookInput(filepath.Join(projects, "..", "..", "..", "elsewhere.jsonl"), `,"source":"compact"`),
		"empty stdin":  "",
		"session list": `{"session_id":[],"transcript_path":` + jsonString(transcript) + `}`,
	} {
		if out := hookRun(t, input, "bind", "--from-hook"); out != "" {
			t.Fatalf("%s printed %q", name, out)
		}
		if _, bound := loadTestBinding(t, "worker-1"); bound {
			t.Fatalf("%s bound", name)
		}
	}

	if out := hookRun(t, hookInput(transcript, `,"source":"startup"`), "bind", "--from-hook"); out != "" {
		t.Fatalf("startup printed %q", out)
	}
	b, bound := loadTestBinding(t, "worker-1")
	if !bound || b.Agent != ctxwatch.ClaudeCode || b.Transcript != canonical || b.BoundAt == "" {
		t.Fatalf("%+v %v", b, bound)
	}

	// PreCompact notes the compaction and its trigger on that binding.
	hookRun(t, hookInput(transcript, `,"hook_event_name":"PreCompact","trigger":"auto"`), "compacting", "--from-hook")
	if b, _ = loadTestBinding(t, "worker-1"); b.CompactingAt == "" || b.CompactTrigger != "auto" {
		t.Fatalf("%+v", b)
	}
	hookRun(t, hookInput(transcript, `,"trigger":"sideways"`), "compacting", "--from-hook")
	if b, _ = loadTestBinding(t, "worker-1"); b.CompactTrigger != "" {
		t.Fatalf("an unknown trigger was kept: %+v", b)
	}
	// Another transcript's compaction is not this binding's.
	other := filepath.Join(projects, "other.jsonl")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := loadTestBinding(t, "worker-1")
	hookRun(t, hookInput(other, `,"trigger":"manual"`), "compacting", "--from-hook")
	if b, _ = loadTestBinding(t, "worker-1"); b != before {
		t.Fatalf("%+v", b)
	}

	// After a compaction the reminder is printed, with the cached hint;
	// the cached levels and hint survive the rebind.
	cached := b
	cached.Levels, cached.HandoverHint = &ctxwatch.Levels{WarnTokens: 1, HandoverTokens: 2}, "Write live/NOTES.md."
	cdir, _ := contextDir()
	if err := ctxwatch.SaveBinding(cdir, "worker-1", cached); err != nil {
		t.Fatal(err)
	}
	out := hookRun(t, hookInput(transcript, `,"source":"compact"`), "bind", "--from-hook")
	if out != compactReminder+" Write live/NOTES.md.\n" {
		t.Fatalf("%q", out)
	}
	if b, _ = loadTestBinding(t, "worker-1"); b.Levels == nil || b.HandoverHint == "" || b.CompactingAt != cached.CompactingAt {
		t.Fatalf("%+v", b)
	}
	// A new transcript drops the old one's compaction, not the cache.
	hookRun(t, hookInput(other, `,"source":"clear"`), "bind", "--from-hook")
	if b, _ = loadTestBinding(t, "worker-1"); b.CompactingAt != "" || b.HandoverHint == "" {
		t.Fatalf("%+v", b)
	}
}

func TestBindExplicitly(t *testing.T) {
	claude := fixture(t, "claude-code.jsonl")
	dir := isolated(t)
	codexHome := filepath.Join(dir, "codex")
	t.Setenv("CODEX_HOME", codexHome)
	thread := "019a0000-0000-7000-8000-00000000abcd"
	day := filepath.Join(codexHome, "sessions", "2026", "01", "02")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(day, "rollout-2026-01-02T09-00-00-"+thread+".jsonl")
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta","payload":{"id":"`+thread+`"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runContextCmd(t, "bind", "--session", "worker-1", "--codex-thread", strings.ToUpper(thread))
	if err != nil || !strings.Contains(out, "bound worker-1 to its codex transcript") {
		t.Fatalf("%q %v", out, err)
	}
	if b, _ := loadTestBinding(t, "worker-1"); b.Agent != ctxwatch.Codex || filepath.Base(b.Transcript) != filepath.Base(rollout) {
		t.Fatalf("%+v", b)
	}
	if out, err = runContextCmd(t, "show", "--session", "worker-1"); err != nil || !strings.Contains(out, "no model call yet") {
		t.Fatalf("%q %v", out, err)
	}

	if _, err := runContextCmd(t, "bind", "--session", "worker-2", "--agent", "claude-code", "--transcript", claude); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no session":     {"bind", "--agent", "claude-code", "--transcript", claude},
		"unknown thread": {"bind", "--session", "w", "--codex-thread", "019a0000-0000-7000-8000-000000000001"},
		"both":           {"bind", "--session", "w", "--codex-thread", thread, "--agent", "codex", "--transcript", rollout},
		"agent only":     {"bind", "--session", "w", "--agent", "codex"},
		"unknown agent":  {"bind", "--session", "w", "--agent", "cursor", "--transcript", claude},
		"missing file":   {"bind", "--session", "w", "--agent", "codex", "--transcript", filepath.Join(dir, "none.jsonl")},
		"directory":      {"bind", "--session", "w", "--agent", "codex", "--transcript", dir},
		"hook and flags": {"bind", "--from-hook", "--agent", "codex"},
		"compacting":     {"compacting"},
		"stray argument": {"bind", "--session", "w", "--codex-thread", thread, "x"},
	} {
		if _, err := runContextCmd(t, args...); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestContextHooksCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := isolated(t)
	contextGOOS = "linux"
	t.Cleanup(func() { contextGOOS = runtime.GOOS })
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	local := filepath.Join(dir, ".claude", "settings.local.json")
	shared := filepath.Join(dir, ".claude", "settings.json")

	out, err := runContextCmd(t, "hooks", "install", "--dry-run")
	if err != nil || !strings.Contains(out, "+ ") || !strings.Contains(out, "dry run") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(local); err == nil {
		t.Fatal("a dry run wrote the file")
	}
	if out, err = runContextCmd(t, "hooks", "install"); err != nil || !strings.Contains(out, "written") {
		t.Fatalf("%q %v", out, err)
	}
	data, _ := os.ReadFile(local)
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	// The settings file is JSON: compare the command as JSON writes it.
	guard := jsonString("[ -x '" + exe + "' ] || exit 0")
	if strings.Count(string(data), ctxwatch.HookMarker) != 3 || !strings.Contains(string(data), guard[1:len(guard)-1]) || !strings.Contains(string(data), "hook stop --from-hook") {
		t.Fatalf("%s", data)
	}
	if out, err = runContextCmd(t, "hooks", "install"); err != nil || !strings.Contains(out, "no change") {
		t.Fatalf("%q %v", out, err)
	}
	if out, err = runContextCmd(t, "hooks", "uninstall"); err != nil || !strings.Contains(out, "written") {
		t.Fatalf("%q %v", out, err)
	}
	if data, _ = os.ReadFile(local); strings.Contains(string(data), ctxwatch.HookMarker) {
		t.Fatalf("%s", data)
	}

	// A tracked local file is not private: the install refuses.
	git("add", "-f", ".claude/settings.local.json")
	if _, err = runContextCmd(t, "hooks", "install"); err == nil || !strings.Contains(err.Error(), "tracked by git") {
		t.Fatalf("%v", err)
	}

	// The committed file needs the explicit confirmation.
	out, err = runContextCmd(t, "hooks", "install", "--scope", "project-shared")
	if err == nil || !strings.Contains(out, "every teammate") || !strings.Contains(out, "first on their PATH") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(shared); err == nil {
		t.Fatal("wrote the shared file unconfirmed")
	}
	if _, err = runContextCmd(t, "hooks", "install", "--scope", "project-shared", "--confirm-shared"); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(shared); !strings.Contains(string(data), "command -v clawbridge") {
		t.Fatalf("%s", data)
	}

	if _, err = runContextCmd(t, "hooks", "install", "--scope", "user"); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(filepath.Join(dir, "claude", "settings.json")); strings.Count(string(data), ctxwatch.HookMarker) != 3 {
		t.Fatalf("%s", data)
	}
	contextGOOS = "windows"
	if _, err = runContextCmd(t, "hooks", "install", "--scope", "user"); err == nil || !strings.Contains(err.Error(), "POSIX shell") {
		t.Fatalf("%v", err)
	}
	if _, err = runContextCmd(t, "hooks", "uninstall", "--scope", "user"); err != nil {
		t.Fatalf("uninstall on Windows: %v", err)
	}
	contextGOOS = "linux"
	for name, args := range map[string][]string{
		"scope":  {"hooks", "install", "--scope", "project"},
		"agent":  {"hooks", "install", "--agent", "codex"},
		"action": {"hooks", "status"},
		"stray":  {"hooks", "install", "x"},
	} {
		if _, err := runContextCmd(t, args...); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The hook fixtures have the shape of Claude Code's real hook input
// (testdata/README.md); each runs through the command its hook runs.
func TestHookFixtures(t *testing.T) {
	read := func(name string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(fixture(t, filepath.Join("hooks", name)))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	fixtures := map[string]map[string]any{}
	for _, name := range []string{"session-start-startup.json", "session-start-resume.json", "pre-compact-manual.json", "session-start-compact.json"} {
		fixtures[name] = read(name)
	}
	dir := isolated(t)
	config := filepath.Join(dir, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("CLAWBRIDGE_SESSION", "worker-1")
	transcript := filepath.Join(config, "projects", "-work-synthetic", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	input := func(name string) string {
		m := fixtures[name]
		m["transcript_path"] = transcript
		data, _ := json.Marshal(m)
		return string(data)
	}
	for _, step := range []struct{ fixture, sub, out string }{
		{"session-start-startup.json", "bind", ""},
		{"session-start-resume.json", "bind", ""},
		{"pre-compact-manual.json", "compacting", ""},
		{"session-start-compact.json", "bind", compactReminder + "\n"},
	} {
		if out := hookRun(t, input(step.fixture), step.sub, "--from-hook"); out != step.out {
			t.Fatalf("%s: %q", step.fixture, out)
		}
	}
	canonical, _ := filepath.EvalSymlinks(transcript)
	if b, _ := loadTestBinding(t, "worker-1"); b.Transcript != canonical || b.CompactTrigger != "manual" || b.CompactingAt == "" {
		t.Fatalf("%+v", b)
	}
}

// A hook from another Claude Code session takes the binding over only when
// the bound agent process is gone, or it is that process under a new
// session (/clear, /resume) [R102-M1, R102-M2]:
// a nested `claude -p` shares CLAWBRIDGE_SESSION but not the parent's
// transcript.
func TestBindFromHookKeepsALiveAgentsBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The hooks are POSIX shell and never installed on Windows, where
		// ProcessAlive is false: there is no live agent to keep.
		t.Skip("the context hooks are not installed on Windows")
	}
	dir := isolated(t)
	config := filepath.Join(dir, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("CLAWBRIDGE_SESSION", "worker-1")
	projects := filepath.Join(config, "projects", "p")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := func(name string) string {
		path := filepath.Join(projects, name+".jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		canonical, _ := filepath.EvalSymlinks(path)
		return canonical
	}
	parent, child, later := transcript("parent"), transcript("child"), transcript("later")
	input := func(session, path, source string) string {
		return `{"session_id":"` + session + `","transcript_path":` + jsonString(path) + `,"hook_event_name":"SessionStart","source":"` + source + `"}`
	}
	bound := func() ctxwatch.Binding {
		t.Helper()
		b, _ := loadTestBinding(t, "worker-1")
		return b
	}
	live := strconv.Itoa(os.Getpid())
	t.Setenv("CLAWBRIDGE_AGENT_PID", live)
	hookRun(t, input("s-parent", parent, "startup"), "bind", "--from-hook")
	if b := bound(); b.Transcript != parent || b.ClaudeSessionID != "s-parent" || b.AgentPID != os.Getpid() {
		t.Fatalf("%+v", b)
	}
	// The holding session rebinds itself (a resume to another file).
	moved := transcript("moved")
	hookRun(t, input("s-parent", moved, "resume"), "bind", "--from-hook")
	if b := bound(); b.Transcript != moved {
		t.Fatalf("%+v", b)
	}
	hookRun(t, input("s-parent", parent, "resume"), "bind", "--from-hook")
	// A nested agent, another live process, is refused, but still gets its
	// compaction reminder, without the parent's hint.
	cached := bound()
	cached.HandoverHint = "Parent's hint."
	cdir0, _ := contextDir()
	if err := ctxwatch.SaveBinding(cdir0, "worker-1", cached); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_AGENT_PID", strconv.Itoa(os.Getppid()))
	hookRun(t, input("s-child", child, "startup"), "bind", "--from-hook")
	if out := hookRun(t, input("s-child", child, "compact"), "bind", "--from-hook"); out != compactReminder+"\n" {
		t.Fatalf("%q", out)
	}
	hookRun(t, `{"session_id":"s-child","transcript_path":`+jsonString(parent)+`,"trigger":"auto"}`, "compacting", "--from-hook")
	if b := bound(); b.Transcript != parent || b.CompactTrigger != "" {
		t.Fatalf("the nested agent took the binding: %+v", b)
	}
	// The same process under a new session rebinds: /resume of another
	// session [R102-M2], and /clear.
	t.Setenv("CLAWBRIDGE_AGENT_PID", live)
	hookRun(t, input("s-resumed", child, "resume"), "bind", "--from-hook")
	if b := bound(); b.Transcript != child || b.ClaudeSessionID != "s-resumed" {
		t.Fatalf("%+v", b)
	}
	hookRun(t, input("s-cleared", later, "clear"), "bind", "--from-hook")
	if b := bound(); b.Transcript != later || b.ClaudeSessionID != "s-cleared" {
		t.Fatalf("%+v", b)
	}
	// Once the bound process is gone, a new agent binds.
	cdir, _ := contextDir()
	gone := bound()
	gone.AgentPID = 1 << 30
	if err := ctxwatch.SaveBinding(cdir, "worker-1", gone); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_AGENT_PID", strconv.Itoa(os.Getppid()))
	hookRun(t, input("s-new", child, "startup"), "bind", "--from-hook")
	if b := bound(); b.Transcript != child || b.ClaudeSessionID != "s-new" {
		t.Fatalf("%+v", b)
	}
}

func TestHooksInstallWarnsWhenNotIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := isolated(t)
	// The user's own global ignore file must not decide the test.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "no-gitconfig"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "no-xdg"))
	contextGOOS = "linux"
	t.Cleanup(func() { contextGOOS = runtime.GOOS })
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	run := func() string {
		var out, errs bytes.Buffer
		if err := Run(context.Background(), []string{"context", "hooks", "install", "--dry-run"}, &out, &errs); err != nil {
			t.Fatal(err)
		}
		return errs.String()
	}
	if !strings.Contains(run(), "doesn't ignore") {
		t.Fatal("no warning for a settings file git would add")
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".claude/settings.local.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(run(), "doesn't ignore") {
		t.Fatal("warned about an ignored file")
	}
}
