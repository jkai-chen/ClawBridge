package ctxwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// hookEvents are the Claude Code events hooked, with the context
// subcommand each runs.
var hookEvents = []struct{ event, sub string }{
	{"SessionStart", "bind"},
	{"PreCompact", "compacting"},
}

// HookCommand is the command text for one hook. In the local and user
// scopes it runs binary, an absolute path, and does nothing when that is
// gone; in the shared scope it runs whatever clawbridge is on PATH, and
// does nothing without one. Every form ends with HookMarker.
func HookCommand(scope, binary, sub string) (string, error) {
	switch scope {
	case ScopeProjectShared:
		return "command -v clawbridge >/dev/null 2>&1 || exit 0; " + agentPID + "clawbridge context " + sub + " --from-hook " + HookMarker, nil
	case ScopeLocal, ScopeUser:
		if !filepath.IsAbs(binary) || strings.ContainsAny(binary, "'\n\r") {
			return "", fmt.Errorf("the binary path %q can't be written into a hook", binary)
		}
		q := "'" + binary + "'"
		return "[ -x " + q + " ] || exit 0; " + agentPID + q + " context " + sub + " --from-hook " + HookMarker, nil
	}
	return "", fmt.Errorf("unknown scope %q: want %s, %s or %s", scope, ScopeLocal, ScopeUser, ScopeProjectShared)
}

var ownHookPattern = regexp.MustCompile(`^\[ -x '([^'\n\r]+)' \] \|\| exit 0; ` + regexp.QuoteMeta(agentPID) + `'([^'\n\r]+)' context (bind|compacting) --from-hook ` +
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
		if install {
			command, err := HookCommand(scope, binary, e.sub)
			if err != nil {
				return c, err
			}
			if len(removed) == 1 && removed[0] == command {
				// Already installed as it would be: left where it is.
				continue
			}
			groups = append(groups, map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": command, "timeout": hookTimeout},
			}})
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
// removed, and any group they leave empty, plus the removed commands.
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
				if command, ok := entry["command"].(string); ok && isOwnHook(command) {
					removed = append(removed, command)
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
