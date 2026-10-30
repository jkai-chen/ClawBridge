package ctxwatch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SaveBinding writes session's binding record under dir, atomically and
// private to the user. The directory is created 0700 when missing; an
// existing record that is a link or not private is refused, not replaced.
func SaveBinding(dir, session string, b Binding) error {
	path, err := BindingPath(dir, session)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the context directory %s could not be created", dir)
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("the context directory %s is not a directory", dir)
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("the binding record %s is not a regular file", path)
		}
		if err := privateToUser(info); err != nil {
			return fmt.Errorf("the binding record %s %s", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the binding record %s is not readable", path)
	}
	b.Version = 1
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".binding-*")
	if err != nil {
		return fmt.Errorf("the binding record %s could not be written", path)
	}
	name := tmp.Name()
	defer os.Remove(name)
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("the binding record %s could not be written", path)
	}
	return nil
}

// Now is the binding time, in the record's form.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// HookInput is the part of a Claude Code hook's JSON input the binding
// reads. Every other field is ignored.
type HookInput struct {
	SessionID      string
	TranscriptPath string
	// Source is SessionStart's (startup, resume, clear or compact);
	// Trigger is PreCompact's (manual or auto).
	Source, Trigger string
	// StopHookActive is Stop's: the agent is already continuing because a
	// Stop hook blocked its stop.
	StopHookActive bool
}

// maxHookInput bounds what is read from a hook's stdin.
const maxHookInput = 1 << 20

// ReadHookInput reads a hook's JSON input. It reports false, never an
// error, when the input is not usable: not JSON, too long, or without
// session_id and transcript_path as non-empty strings (CW-L2). source and
// trigger are kept only when they are strings.
func ReadHookInput(r io.Reader) (HookInput, bool) {
	data, err := io.ReadAll(io.LimitReader(r, maxHookInput+1))
	if err != nil || len(data) > maxHookInput {
		return HookInput{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return HookInput{}, false
	}
	str := func(key string) string {
		var s string
		if json.Unmarshal(raw[key], &s) != nil {
			return ""
		}
		return s
	}
	in := HookInput{SessionID: str("session_id"), TranscriptPath: str("transcript_path"), Source: str("source"), Trigger: str("trigger")}
	// Anything but false counts as active, so a Stop hook never blocks twice.
	var active bool
	if raw["stop_hook_active"] != nil && (json.Unmarshal(raw["stop_hook_active"], &active) != nil || active) {
		in.StopHookActive = true
	}
	if in.SessionID == "" || in.TranscriptPath == "" {
		return HookInput{}, false
	}
	return in, true
}

// ReadTurnInput reads a UserPromptSubmit hook's JSON input and says which
// push of our channel started the turn: its seq, or 0 for any other prompt
// (FR26). It reports false when the input is not usable, as ReadHookInput
// does. The prompt is read only to be matched by ChannelTurnSeq: it is
// never returned, kept or written anywhere.
func ReadTurnInput(r io.Reader) (int64, bool) {
	data, err := io.ReadAll(io.LimitReader(r, maxHookInput+1))
	if err != nil || len(data) > maxHookInput {
		return 0, false
	}
	var raw struct {
		SessionID      any `json:"session_id"`
		TranscriptPath any `json:"transcript_path"`
		Prompt         any `json:"prompt"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return 0, false
	}
	if id, _ := raw.SessionID.(string); id == "" {
		return 0, false
	}
	if path, _ := raw.TranscriptPath.(string); path == "" {
		return 0, false
	}
	prompt, _ := raw.Prompt.(string)
	seq, _ := ChannelTurnSeq(prompt)
	return seq, true
}

// ClaudeConfigDir is CLAUDE_CONFIG_DIR, or ~/.claude.
func ClaudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no home directory for Claude Code's configuration")
	}
	return filepath.Join(home, ".claude"), nil
}

// ClaudeTranscript canonicalises a transcript path a hook named (absolute,
// links resolved) and accepts it only as a .jsonl file under configDir's
// projects directory (CW-L2). A transcript not written yet is accepted too:
// Claude Code runs a new session's SessionStart hook before it creates the
// file.
func ClaudeTranscript(path, configDir string) (string, error) {
	refused := errors.New("the hook's transcript is not under Claude Code's projects directory")
	if !filepath.IsAbs(path) {
		return "", refused
	}
	resolved, err := canonicalTranscript(path)
	if err != nil {
		return "", refused
	}
	root, err := filepath.EvalSymlinks(filepath.Join(configDir, "projects"))
	if err != nil {
		return "", refused
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		filepath.Ext(resolved) != ".jsonl" {
		return "", refused
	}
	return resolved, nil
}

// canonicalTranscript is path with its links resolved, or, for a file not
// written yet, the path it will have (unwritten).
func canonicalTranscript(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return unwritten(path)
	}
	return resolved, nil
}

// unwritten canonicalises the path of a file that doesn't exist yet: the
// nearest directory above it that exists, links resolved, and the names
// below that kept, which is what resolving the file gives once it is
// written under plain directories. A new project's directory may not exist
// yet either. Only a missing file qualifies: a link to nowhere, or any
// other error, is refused.
func unwritten(path string) (string, error) {
	missing := errors.New("not a file still to be written")
	if filepath.Clean(path) != filepath.FromSlash(path) || filepath.Ext(path) != ".jsonl" {
		return "", missing
	}
	path = filepath.Clean(path)
	dir, rest := filepath.Dir(path), filepath.Base(path)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return "", missing
	}
	for {
		_, err := os.Lstat(dir)
		if err == nil {
			// The nearest one that exists must be a directory (or a link to
			// one): on Windows a path under a file reads as missing, not as
			// ENOTDIR, so the walk can reach that file.
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return "", missing
			}
			break
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, os.ErrNotExist) || parent == dir {
			return "", missing
		}
		dir, rest = parent, filepath.Join(filepath.Base(dir), rest)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", missing
	}
	return filepath.Join(resolved, rest), nil
}

// CodexHome is CODEX_HOME, or ~/.codex.
func CodexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no home directory for Codex")
	}
	return filepath.Join(home, ".codex"), nil
}

var threadPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CodexRollout finds the rollout of a Codex thread under codexHome's
// sessions directory: a regular .jsonl file named for the thread whose
// first line is a session_meta carrying it. When several match, the most
// recently written wins.
func CodexRollout(codexHome, thread string) (string, error) {
	if !threadPattern.MatchString(thread) {
		return "", fmt.Errorf("%q is not a Codex thread ID", thread)
	}
	root, err := filepath.EvalSymlinks(filepath.Join(codexHome, "sessions"))
	if err != nil {
		return "", fmt.Errorf("no Codex sessions directory under %s", codexHome)
	}
	var found string
	var newest time.Time
	// WalkDir does not follow links, so the search stays under root.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), "-"+thread+".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !rolloutOf(path, thread) {
			return nil
		}
		if found == "" || info.ModTime().After(newest) {
			found, newest = path, info.ModTime()
		}
		return nil
	})
	if err != nil || found == "" {
		return "", fmt.Errorf("no Codex rollout for thread %s under %s", thread, codexHome)
	}
	return found, nil
}

// rolloutOf says whether path's first line is a session_meta for thread.
func rolloutOf(path, thread string) bool {
	f, _, err := openOwned(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, err := bufio.NewReader(io.LimitReader(f, maxLine)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	return json.Unmarshal(line, &meta) == nil && meta.Type == "session_meta" && meta.Payload.ID == thread
}
