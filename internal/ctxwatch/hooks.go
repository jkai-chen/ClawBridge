package ctxwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// HookMarker ends every hook command ClawBridge writes (CW-M1).
const HookMarker = "# clawbridge-context-hook v1"

// Hook scopes (§4.1, §7.2).
const (
	ScopeLocal         = "local"
	ScopeUser          = "user"
	ScopeProjectShared = "project-shared"
)

// agentPID passes the hook's parent, the Claude Code process, to the
// command: a binding remembers which agent process it belongs to.
const agentPID = "CLAWBRIDGE_AGENT_PID=$PPID "

// hookTimeout is each hook's limit in Claude Code, in seconds. The
// commands themselves read and write local files only.
const hookTimeout = 5

// hookEvents are the Claude Code events hooked, with the subcommand each
// runs. Stop is the message queue's (MESSAGE-QUEUE-DESIGN §5): it keeps an
// agent from going idle on unhandled mail. UserPromptSubmit is the channel's
// (FR26): it marks a turn's start, and is installed only when asked for.
var hookEvents = []struct {
	event, sub string
	channel    bool
}{
	{"SessionStart", "bind", false},
	{"PreCompact", "compacting", false},
	{"Stop", "stop", false},
	{"UserPromptSubmit", "turn", true},
}

// hookCommandWords are the clawbridge words each subcommand runs.
var hookCommandWords = map[string]string{"bind": "context bind", "compacting": "context compacting", "stop": "hook stop", "turn": "hook turn"}

// HookCommand is the command text for one hook. In the local and user
// scopes it runs binary, an absolute path, and does nothing when that is
// gone; in the shared scope it runs whatever clawbridge is on PATH, and
// does nothing without one. Every form ends with HookMarker.
func HookCommand(scope, binary, sub string) (string, error) {
	words, ok := hookCommandWords[sub]
	if !ok {
		return "", fmt.Errorf("unknown hook %q", sub)
	}
	switch scope {
	case ScopeProjectShared:
		return "command -v clawbridge >/dev/null 2>&1 || exit 0; " + agentPID + "clawbridge " + words + " --from-hook " + HookMarker, nil
	case ScopeLocal, ScopeUser:
		if !filepath.IsAbs(binary) || strings.ContainsAny(binary, "'\n\r") {
			return "", fmt.Errorf("the binary path %q can't be written into a hook", binary)
		}
		q := "'" + binary + "'"
		return "[ -x " + q + " ] || exit 0; " + agentPID + q + " " + words + " --from-hook " + HookMarker, nil
	}
	return "", fmt.Errorf("unknown scope %q: want %s, %s or %s", scope, ScopeLocal, ScopeUser, ScopeProjectShared)
}

var ownHookPattern = regexp.MustCompile(`^\[ -x '([^'\n\r]+)' \] \|\| exit 0; ` + regexp.QuoteMeta(agentPID) + `'([^'\n\r]+)' (context bind|context compacting|hook stop|hook turn) --from-hook ` +
	regexp.QuoteMeta(HookMarker) + `$`)

// isOwnHook says whether command is exactly a text HookCommand writes, in
// any scope and for any binary path.
func isOwnHook(command string) bool {
	if m := ownHookPattern.FindStringSubmatch(command); m != nil {
		return m[1] == m[2] && filepath.IsAbs(m[1])
	}
	for _, e := range hookEvents {
		if shared, _ := HookCommand(ScopeProjectShared, "", e.sub); command == shared {
			return true
		}
	}
	return false
}

// HookTag is the tag the exec-form hook carries as its last argument, where
// the POSIX text carries HookMarker as a shell comment. The hook commands
// accept it and ignore it.
const HookTag = "clawbridge-context-hook-v1"

// Hook forms, as doctor reports them.
const (
	FormShell = "shell"
	FormExec  = "exec"
)

// hookArgs are the exec-form arguments for one subcommand.
func hookArgs(sub string) ([]string, bool) {
	words, ok := hookCommandWords[sub]
	if !ok {
		return nil, false
	}
	return append(strings.Fields(words), "--from-hook", "--hook-tag", HookTag), true
}

// isWindowsAbs says whether p is an absolute Windows path (a drive path or
// a UNC path), whatever platform this runs on.
func isWindowsAbs(p string) bool {
	if len(p) >= 3 && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z' && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return len(p) > 2 && p[0] == '\\' && p[1] == '\\' && p[2] != '\\'
}

// windowsExe says whether p can be an exec-form hook's command: an absolute
// path to an .exe with nothing in it that would end a JSON string or line.
func windowsExe(p string) bool {
	return isWindowsAbs(p) && strings.HasSuffix(strings.ToLower(p), ".exe") && !strings.ContainsAny(p, "\"\n\r\x00")
}

// HookEntry is the settings entry for one hook on goos. On Windows it is
// Claude Code's exec form, a command and its arguments run with no shell;
// elsewhere it is HookCommand's POSIX text.
func HookEntry(goos, scope, binary, sub string) (map[string]any, error) {
	if goos != "windows" {
		command, err := HookCommand(scope, binary, sub)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "command", "command": command, "timeout": hookTimeout}, nil
	}
	args, ok := hookArgs(sub)
	if !ok {
		return nil, fmt.Errorf("unknown hook %q", sub)
	}
	command := "clawbridge"
	switch scope {
	case ScopeProjectShared:
	case ScopeLocal, ScopeUser:
		if !windowsExe(binary) {
			return nil, fmt.Errorf("the binary path %q can't be written into a hook", binary)
		}
		command = binary
	default:
		return nil, fmt.Errorf("unknown scope %q: want %s, %s or %s", scope, ScopeLocal, ScopeUser, ScopeProjectShared)
	}
	list := make([]any, len(args))
	for i, a := range args {
		list[i] = a
	}
	return map[string]any{"type": "command", "command": command, "args": list, "timeout": hookTimeout}, nil
}

// ownEntryKey identifies a hook entry by what it runs; "" when it is not
// exactly one ClawBridge writes, in either form, for any binary path.
func ownEntryKey(entry map[string]any) string {
	command, ok := entry["command"].(string)
	if !ok {
		return ""
	}
	rawArgs, hasArgs := entry["args"]
	if !hasArgs {
		if isOwnHook(command) {
			return command
		}
		return ""
	}
	list, ok := rawArgs.([]any)
	if !ok || (command != "clawbridge" && !windowsExe(command)) {
		return ""
	}
	got := make([]string, len(list))
	for i, a := range list {
		if got[i], ok = a.(string); !ok {
			return ""
		}
	}
	for _, e := range hookEvents {
		if want, _ := hookArgs(e.sub); strings.Join(want, "\x00") == strings.Join(got, "\x00") {
			return command + "\x00" + strings.Join(got, "\x00")
		}
	}
	return ""
}

// entryForm is the form of a ClawBridge entry (see ownEntryKey).
func entryForm(entry map[string]any) string {
	if _, ok := entry["args"]; ok {
		return FormExec
	}
	return FormShell
}

// HookChange is a settings file before and after an install or uninstall.
type HookChange struct {
	Path          string
	Before, After []byte
	// Exists says whether the file was there before.
	Exists bool
}

// Changed says whether the change would alter the file.
func (c HookChange) Changed() bool { return !bytes.Equal(c.Before, c.After) }

// PlanHooks reads the settings file at path and works out the file with
// ClawBridge's hooks installed (install) or removed (not install). Other
// settings and hooks are kept; only entries that are exactly ClawBridge's
// are removed. Nothing is written.
func PlanHooks(path, scope, binary string, install bool) (HookChange, error) {
	return PlanHooksFor(runtime.GOOS, path, scope, binary, install)
}

// PlanHooksFor is PlanHooks writing the form of goos: Windows gets the exec
// form. Removal recognises both forms whatever goos is.
func PlanHooksFor(goos, path, scope, binary string, install bool) (HookChange, error) {
	return PlanHooksWith(goos, path, scope, binary, install, false)
}

// PlanHooksWith is PlanHooksFor that, with channel, also installs the turn
// hook the channel needs (FR26). An install without channel adds no turn
// hook but keeps one already there, rewritten for binary; an uninstall
// removes it either way.
func PlanHooksWith(goos, path, scope, binary string, install, channel bool) (HookChange, error) {
	c := HookChange{Path: path}
	settings := map[string]any{}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, fmt.Errorf("the settings file %s is not readable", path)
	case !info.Mode().IsRegular():
		return c, fmt.Errorf("the settings file %s is a link or not a regular file; edit it by hand", path)
	default:
		c.Exists = true
		if c.Before, err = os.ReadFile(path); err != nil {
			return c, fmt.Errorf("the settings file %s is not readable", path)
		}
		if len(bytes.TrimSpace(c.Before)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(c.Before))
			dec.UseNumber()
			if dec.Decode(&settings) != nil || dec.More() {
				return c, fmt.Errorf("the settings file %s is not a JSON object; fix it first", path)
			}
		}
	}
	hooks := map[string]any{}
	if v, ok := settings["hooks"]; ok {
		if hooks, ok = v.(map[string]any); !ok {
			return c, fmt.Errorf("the settings file %s has a \"hooks\" that is not an object; fix it first", path)
		}
	}
	for _, e := range hookEvents {
		groups, removed, err := withoutOwnHooks(hooks[e.event])
		if err != nil {
			return c, fmt.Errorf("the settings file %s: %s %v; fix it first", path, e.event, err)
		}
		if install && (!e.channel || channel || len(removed) > 0) {
			entry, err := HookEntry(goos, scope, binary, e.sub)
			if err != nil {
				return c, err
			}
			if len(removed) == 1 && removed[0] == ownEntryKey(entry) {
				// Already installed as it would be: left where it is.
				continue
			}
			groups = append(groups, map[string]any{"hooks": []any{entry}})
		}
		if len(groups) == 0 {
			delete(hooks, e.event)
		} else {
			hooks[e.event] = groups
		}
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	if !c.Exists && len(settings) == 0 {
		return c, nil
	}
	// Hook commands keep their > and & as written, not \u escapes.
	var after bytes.Buffer
	enc := json.NewEncoder(&after)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return c, err
	}
	c.After = after.Bytes()
	// An unchanged meaning leaves the file as it is, layout included.
	if c.Exists && sameJSON(c.Before, c.After) {
		c.After = c.Before
	}
	return c, nil
}

// withoutOwnHooks is an event's hook groups with ClawBridge's entries
// removed, and any group they leave empty, plus the removed entries' keys (ownEntryKey).
func withoutOwnHooks(v any) (kept []any, removed []string, err error) {
	if v == nil {
		return nil, nil, nil
	}
	groups, ok := v.([]any)
	if !ok {
		return nil, nil, errors.New("is not a list")
	}
	for _, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		entries, ok := group["hooks"].([]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		var left []any
		for _, h := range entries {
			if entry, ok := h.(map[string]any); ok {
				if key := ownEntryKey(entry); key != "" {
					removed = append(removed, key)
					continue
				}
			}
			left = append(left, h)
		}
		if len(left) == len(entries) {
			kept = append(kept, g)
			continue
		}
		if len(left) > 0 {
			copied := map[string]any{}
			for k, v := range group {
				copied[k] = v
			}
			copied["hooks"] = left
			kept = append(kept, copied)
		}
	}
	return kept, removed, nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	ja, ea := json.Marshal(x)
	jb, eb := json.Marshal(y)
	return ea == nil && eb == nil && bytes.Equal(ja, jb)
}

// Apply writes the change atomically, keeping an existing file's
// permissions; a new file is 0600. A file that changed since it was read
// is not overwritten.
func (c HookChange) Apply() error {
	if !c.Changed() {
		return nil
	}
	mode := os.FileMode(0o600)
	info, err := os.Lstat(c.Path)
	switch {
	case err == nil && c.Exists && info.Mode().IsRegular():
		mode = info.Mode().Perm()
		if now, err := os.ReadFile(c.Path); err != nil || !bytes.Equal(now, c.Before) {
			return fmt.Errorf("the settings file %s changed while it was being edited; run the command again", c.Path)
		}
	case errors.Is(err, os.ErrNotExist) && !c.Exists:
	default:
		return fmt.Errorf("the settings file %s changed while it was being edited; run the command again", c.Path)
	}
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the directory %s could not be created", dir)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*")
	if err != nil {
		return fmt.Errorf("the settings file %s could not be written", c.Path)
	}
	name := tmp.Name()
	defer os.Remove(name)
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(c.After)
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(name, c.Path)
	}
	if err != nil {
		return fmt.Errorf("the settings file %s could not be written", c.Path)
	}
	return nil
}

// Diff is the change as removed (-) and added (+) lines, with unchanged
// lines indented, for showing before it is applied.
func (c HookChange) Diff() string {
	a, b := lines(c.Before), lines(c.After)
	// Longest common subsequence; settings files are small.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			out.WriteString("  " + a[i] + "\n")
			i, j = i+1, j+1
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			out.WriteString("+ " + b[j] + "\n")
			j++
		default:
			out.WriteString("- " + a[i] + "\n")
			i++
		}
	}
	return out.String()
}

func lines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// InstalledHookForm is the form of the ClawBridge hooks in the settings file
// at path: FormExec, FormShell, "mixed", or "" for none (or no file).
func InstalledHookForm(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("the settings file %s is not readable", path)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return "", fmt.Errorf("the settings file %s is not readable JSON", path)
	}
	forms := map[string]bool{}
	for _, groups := range settings.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if ownEntryKey(h) != "" {
					forms[entryForm(h)] = true
				}
			}
		}
	}
	switch {
	case forms[FormExec] && forms[FormShell]:
		return "mixed", nil
	case forms[FormExec]:
		return FormExec, nil
	case forms[FormShell]:
		return FormShell, nil
	}
	return "", nil
}

// TurnHookInstalled says whether the settings file at path holds
// ClawBridge's UserPromptSubmit turn hook, in either form: the hook
// `mcp install --channel` adds, so a session that has it asked for the
// channel (FR26). A missing file holds none.
func TurnHookInstalled(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("the settings file %s is not readable", path)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return false, fmt.Errorf("the settings file %s is not readable JSON", path)
	}
	for _, g := range settings.Hooks["UserPromptSubmit"] {
		for _, h := range g.Hooks {
			key := ownEntryKey(h)
			if key == "" {
				continue
			}
			if strings.Contains(key, "\x00hook\x00turn\x00") || strings.Contains(key, " hook turn --from-hook ") {
				return true, nil
			}
		}
	}
	return false, nil
}
