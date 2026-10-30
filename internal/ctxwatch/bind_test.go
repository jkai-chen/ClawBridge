package ctxwatch

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Input the hook can't vouch for binds nothing (CW-L2).
func TestReadHookInput(t *testing.T) {
	good := `{"session_id":"s-1","transcript_path":"/x/t.jsonl","cwd":"/x","hook_event_name":"SessionStart","source":"compact"}`
	if in, ok := ReadHookInput(strings.NewReader(good)); !ok || in.SessionID != "s-1" || in.TranscriptPath != "/x/t.jsonl" || in.Source != "compact" {
		t.Fatalf("%+v %v", in, ok)
	}
	if in, ok := ReadHookInput(strings.NewReader(`{"session_id":"s","transcript_path":"/t","trigger":"auto","source":7}`)); !ok ||
		in.Trigger != "auto" || in.Source != "" {
		t.Fatalf("%+v %v", in, ok)
	}
	for name, input := range map[string]string{
		"empty":              "",
		"not JSON":           "hello",
		"a list":             `[1]`,
		"no transcript":      `{"session_id":"s"}`,
		"no session":         `{"transcript_path":"/t"}`,
		"transcript number":  `{"session_id":"s","transcript_path":7}`,
		"session object":     `{"session_id":{},"transcript_path":"/t"}`,
		"empty transcript":   `{"session_id":"s","transcript_path":""}`,
		"transcript is null": `{"session_id":"s","transcript_path":null}`,
	} {
		if in, ok := ReadHookInput(strings.NewReader(input)); ok {
			t.Fatalf("%s accepted: %+v", name, in)
		}
	}
	// Valid JSON one byte over the bound is refused, not cut short.
	pad := `{"session_id":"s","transcript_path":"/t","x":"`
	over := pad + strings.Repeat("a", maxHookInput+1-len(pad)-2) + `"}`
	if len(over) != maxHookInput+1 {
		t.Fatal(len(over))
	}
	if _, ok := ReadHookInput(strings.NewReader(over)); ok {
		t.Fatal("input over the bound was accepted")
	}
	if _, ok := ReadHookInput(strings.NewReader(over[:len(pad)] + over[len(pad)+1:])); !ok {
		t.Fatal("input at the bound was refused")
	}
}

func TestClaudeTranscriptStaysUnderProjects(t *testing.T) {
	config := t.TempDir()
	projects := filepath.Join(config, "projects", "p")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(projects, "s.jsonl")
	outside := filepath.Join(t.TempDir(), "s.jsonl")
	for _, p := range []string{inside, outside} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ClaudeTranscript(inside, config)
	want, _ := filepath.EvalSymlinks(inside)
	if err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}
	// A configuration whose projects directory is a link elsewhere, as
	// some profile managers set up, still binds its own transcripts.
	linked := t.TempDir()
	symlink(t, filepath.Join(config, "projects"), filepath.Join(linked, "projects"))
	if got, err := ClaudeTranscript(filepath.Join(linked, "projects", "p", "s.jsonl"), linked); err != nil || got != want {
		t.Fatalf("through a linked projects directory: %q %v", got, err)
	}
	escape := filepath.Join(projects, "escape.jsonl")
	symlink(t, outside, escape)
	notJSONL := filepath.Join(projects, "s.txt")
	if err := os.WriteFile(notJSONL, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"outside":      outside,
		"link outside": escape,
		"dot-dot":      filepath.Join(projects, "..", "..", "..", filepath.Base(filepath.Dir(outside)), "s.jsonl"),
		"relative":     "projects/p/s.jsonl",
		"not jsonl":    notJSONL,
		"missing out":  filepath.Join(filepath.Dir(outside), "none.jsonl"),
		"the root":     filepath.Join(config, "projects"),
	} {
		if got, err := ClaudeTranscript(path, config); err == nil {
			t.Fatalf("%s accepted: %q", name, got)
		}
	}
}

// A new session's transcript is bound before Claude Code writes it, at the
// path resolving the file gives once it is written.
func TestClaudeTranscriptNotWrittenYet(t *testing.T) {
	config := t.TempDir()
	projects := filepath.Join(config, "projects", "p")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := t.TempDir()
	symlink(t, filepath.Join(config, "projects"), filepath.Join(linked, "projects"))
	for name, path := range map[string]string{
		"in its project":          filepath.Join(projects, "new.jsonl"),
		"in a new project":        filepath.Join(config, "projects", "q", "new.jsonl"),
		"through a linked config": filepath.Join(linked, "projects", "r", "new.jsonl"),
	} {
		got, err := ClaudeTranscript(path, config)
		if name == "through a linked config" {
			got, err = ClaudeTranscript(path, linked)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if want, _ := filepath.EvalSymlinks(path); got != want {
			t.Fatalf("%s: bound %q, written at %q", name, got, want)
		}
	}
	outside := t.TempDir()
	symlink(t, filepath.Join(outside, "none.jsonl"), filepath.Join(projects, "dangling.jsonl"))
	symlink(t, filepath.Join(outside, "none"), filepath.Join(projects, "dangling"))
	symlink(t, outside, filepath.Join(projects, "out"))
	if err := os.WriteFile(filepath.Join(projects, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A project that is a file, not a directory: the walk up meets ENOTDIR.
	if err := os.WriteFile(filepath.Join(config, "projects", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"in a project that is a file": filepath.Join(config, "projects", "f", "new.jsonl"),
		"outside":                     filepath.Join(outside, "new.jsonl"),
		"through a link out":          filepath.Join(projects, "out", "new.jsonl"),
		"a link to nowhere":           filepath.Join(projects, "dangling.jsonl"),
		"under a link to nowhere":     filepath.Join(projects, "dangling", "new.jsonl"),
		"under a file":                filepath.Join(projects, "file", "new.jsonl"),
		"not jsonl":                   filepath.Join(projects, "new.txt"),
		"dot-dot":                     filepath.Join(projects, "q") + string(filepath.Separator) + ".." + string(filepath.Separator) + "new.jsonl",
		"no projects":                 filepath.Join(t.TempDir(), "projects", "p", "new.jsonl"),
	} {
		if got, err := ClaudeTranscript(path, config); err == nil {
			t.Fatalf("%s accepted: %q", name, got)
		}
	}
}

const thread = "019a0000-0000-7000-8000-00000000abcd"

func TestCodexRollout(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "01", "02")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := func(id string) []byte {
		return []byte(`{"timestamp":"2026-01-02T09:00:00Z","type":"session_meta","payload":{"id":"` + id + `"}}` + "\n")
	}
	older := filepath.Join(day, "rollout-2026-01-02T09-00-00-"+thread+".jsonl")
	newer := filepath.Join(day, "rollout-2026-01-02T10-00-00-"+thread+".jsonl")
	wrong := filepath.Join(day, "rollout-2026-01-02T11-00-00-"+thread+".jsonl")
	for path, data := range map[string][]byte{older: meta(thread), newer: meta(thread), wrong: meta("019a0000-0000-7000-8000-000000000000")} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	_ = os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour))
	_ = os.Chtimes(wrong, now.Add(time.Hour), now.Add(time.Hour))
	if got, err := CodexRollout(home, thread); err != nil || filepath.Base(got) != filepath.Base(newer) {
		t.Fatalf("%q %v", got, err)
	}
	// A rollout reached through a link out of the sessions directory is
	// not searched.
	elsewhere := t.TempDir()
	other := "019a0000-0000-7000-8000-0000000000ff"
	if err := os.WriteFile(filepath.Join(elsewhere, "rollout-x-"+other+".jsonl"), meta(other), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink(t, elsewhere, filepath.Join(home, "sessions", "linked"))
	for name, id := range map[string]string{"linked": other, "not an ID": "../" + thread, "unknown": "019a0000-0000-7000-8000-000000000001"} {
		if got, err := CodexRollout(home, id); err == nil {
			t.Fatalf("%s found %q", name, got)
		}
	}
	if _, err := CodexRollout(t.TempDir(), thread); err == nil {
		t.Fatal("found without a sessions directory")
	}
}

func TestSaveBinding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	b := Binding{Agent: ClaudeCode, Transcript: absPath("x", "t.jsonl"), BoundAt: "2026-01-01T10:00:00Z", CompactTrigger: "auto"}
	if err := SaveBinding(dir, "worker-1", b); err != nil {
		t.Fatal(err)
	}
	got, bound, err := LoadBinding(dir, "worker-1")
	b.Version = 1
	if err != nil || !bound || got != b {
		t.Fatalf("%+v %v %v", got, bound, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, "worker-1.json"))
		dirInfo, _ := os.Stat(dir)
		if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("%v %v", info.Mode(), dirInfo.Mode())
		}
	}
	// A link where the record goes is refused, not replaced.
	symlink(t, filepath.Join(dir, "worker-1.json"), filepath.Join(dir, "worker-2.json"))
	if err := SaveBinding(dir, "worker-2", b); err == nil {
		t.Fatal("wrote through a link")
	}
	if err := SaveBinding(dir, "../escape", b); err == nil {
		t.Fatal("a path-escaping session name was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("left %d entries", len(entries))
	}
}

// A binding the hook made before its transcript was written reads as no
// level yet, and is checked again at each read: what is at the path, or a
// directory on it, may since have become a link. The reader's own Claude
// Code configuration doesn't matter.
func TestReadBoundChecksAgain(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	projects := filepath.Join(config, "projects")
	project := filepath.Join(projects, "p")
	sibling := filepath.Join(projects, "q")
	for _, dir := range []string{project, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(project, "s.jsonl")
	canonical, err := ClaudeTranscript(path, config)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{Agent: ClaudeCode, Transcript: canonical, ClaudeSessionID: "s"}
	if r, err := ReadBound(b); err != nil || r != (Reading{Agent: ClaudeCode, Status: StatusOK, NotWritten: true}) {
		t.Fatalf("%+v %v", r, err)
	}

	usage := []byte(claudeAssistant(5000, "") + "\n")
	outside := filepath.Join(t.TempDir(), "s.jsonl")
	for _, p := range []string{outside, filepath.Join(sibling, "s.jsonl"), filepath.Join(sibling, "other.jsonl")} {
		if err := os.WriteFile(p, usage, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(name string) {
		t.Helper()
		if r, err := ReadBound(b); err == nil {
			t.Fatalf("%s read: %+v", name, r)
		}
	}
	// A link at the path, out of the projects directory or within it.
	symlink(t, outside, canonical)
	refused("a link out")
	os.Remove(canonical)
	symlink(t, filepath.Join(sibling, "other.jsonl"), canonical)
	refused("a link within")
	os.Remove(canonical)
	// The project's directory replaced by a link, out or to another project.
	moved := project + ".moved"
	if err := os.Rename(project, moved); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Dir(outside), project)
	refused("a linked directory out")
	os.Remove(project)
	symlink(t, sibling, project)
	refused("a linked directory within")
	os.Remove(project)
	if err := os.Rename(moved, project); err != nil {
		t.Fatal(err)
	}

	// Read from a process with another Claude Code configuration (doctor
	// in a plain shell, say), it still reads: the hook vouched for the path.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if r, err := ReadBound(b); err != nil || !r.NotWritten {
		t.Fatalf("under another configuration, not written: %+v %v", r, err)
	}
	if err := os.WriteFile(canonical, usage, 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadBound(b); err != nil || r.Status != StatusOK || r.NotWritten || r.Tokens != 6010 {
		t.Fatalf("under another configuration: %+v %v", r, err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	if r, err := ReadBound(b); err != nil || r.Status != StatusOK || r.NotWritten || r.Tokens != 6010 {
		t.Fatalf("%+v %v", r, err)
	}
	// A binding made by hand is read as before, wherever it points.
	if r, err := ReadBound(Binding{Agent: ClaudeCode, Transcript: outside}); err != nil || r.Tokens != 6010 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ReadBound(Binding{Agent: ClaudeCode, Transcript: filepath.Join(filepath.Dir(outside), "gone.jsonl")}); err == nil {
		t.Fatal("a missing transcript bound by hand read")
	}
}
