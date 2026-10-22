// Package skill installs the embedded ClawBridge agent skill into per-user
// agent skill directories, and never touches files it did not install.
package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"clawbridge/internal/buildinfo"
	"clawbridge/skills"
)

// Name is the skill's directory name inside an agent's skills directory.
const Name = "clawbridge"

// Marker records what an installation wrote. It lives inside the skill
// directory, so an uninstall can tell our files from the user's.
const Marker = ".clawbridge-skill.json"

// Agents lists the supported agents in a stable order.
var Agents = []string{"claude-code", "codex"}

// Target is one skill directory and the agent it serves.
type Target struct {
	Agent string `json:"agent"`
	Dir   string `json:"dir"`
}

type marker struct {
	Version string            `json:"version"`
	Commit  string            `json:"commit"`
	Digest  string            `json:"digest"`
	Files   map[string]string `json:"files"`
}

// State describes one target. Installed means our marker is present.
type State struct {
	Target
	Installed        bool     `json:"installed"`
	InstalledVersion string   `json:"installed_version,omitempty"`
	BinaryVersion    string   `json:"binary_version"`
	Current          bool     `json:"current"`
	Modified         []string `json:"modified,omitempty"`
	Unmanaged        []string `json:"unmanaged,omitempty"`
}

// Result is the outcome of an install or uninstall for one target.
type Result struct {
	State
	Changed bool   `json:"changed"`
	Refused string `json:"refused,omitempty"`
}

// agentHome is the agent's own configuration home, which also holds its
// user skills: Claude Code reads $CLAUDE_CONFIG_DIR/skills (default
// ~/.claude/skills) and Codex reads $CODEX_HOME/skills (default
// ~/.codex/skills, where its own skill installer writes).
func agentHome(agent string) (string, error) {
	place, ok := map[string][2]string{"claude-code": {"CLAUDE_CONFIG_DIR", ".claude"}, "codex": {"CODEX_HOME", ".codex"}}[agent]
	env, dir := place[0], place[1]
	if !ok {
		return "", fmt.Errorf("unsupported agent %q: use claude-code or codex", agent)
	}
	if v := os.Getenv(env); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s must be an absolute path", env)
		}
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dir), nil
}

// Targets resolves the skill directories. dir, when set, is a skills
// directory chosen by the user and the skill goes in dir/clawbridge. Agent
// "" picks every supported agent whose configuration home exists; "all"
// picks every supported agent.
func Targets(agent, dir string) ([]Target, error) {
	if dir != "" {
		if agent == "" || agent == "all" {
			agent = "custom"
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		return []Target{{Agent: agent, Dir: filepath.Join(abs, Name)}}, nil
	}
	names := []string{agent}
	if agent == "" || agent == "all" {
		names = Agents
	}
	out := []Target{}
	for _, name := range names {
		config, err := agentHome(name)
		if err != nil {
			return nil, err
		}
		if agent == "" {
			if info, e := os.Stat(config); e != nil || !info.IsDir() {
				continue
			}
		}
		out = append(out, Target{Agent: name, Dir: filepath.Join(config, "skills", Name)})
	}
	if len(out) == 0 {
		return nil, errors.New("no Claude Code or Codex configuration found; pass --agent claude-code|codex|all or --dir PATH")
	}
	return out, nil
}

// embedded returns the skill's files by slash path, and their digest.
func embedded() (map[string][]byte, string, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(skills.ClawBridge, Name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := skills.ClawBridge.ReadFile(p)
		files[strings.TrimPrefix(p, Name+"/")] = b
		return err
	})
	if err != nil {
		return nil, "", err
	}
	hashes := map[string]string{}
	for p, b := range files {
		hashes[p] = sum(b)
	}
	return files, digest(hashes), nil
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func digest(hashes map[string]string) string {
	names := make([]string, 0, len(hashes))
	for p := range hashes {
		names = append(names, p)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, p := range names {
		fmt.Fprintf(h, "%s\x00%s\n", p, hashes[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// inspect reads a target without changing it.
func inspect(t Target) (State, *marker, error) {
	_, want, err := embedded()
	if err != nil {
		return State{}, nil, err
	}
	s := State{Target: t, BinaryVersion: buildinfo.Version}
	info, err := os.Lstat(t.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil, nil
	}
	if err != nil {
		return s, nil, err
	}
	if !info.IsDir() {
		s.Unmanaged = []string{"."}
		return s, nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(t.Dir, Marker))
	var m *marker
	if err == nil {
		m = &marker{}
		if json.Unmarshal(raw, m) != nil || m.Files == nil {
			m = nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return s, nil, err
	}
	present, err := tree(t.Dir)
	if err != nil {
		return s, nil, err
	}
	for _, p := range present {
		if p == Marker {
			continue
		}
		if m == nil || m.Files[p] == "" {
			s.Unmanaged = append(s.Unmanaged, p)
		}
	}
	if m == nil {
		if len(present) > 0 && s.Unmanaged == nil {
			s.Unmanaged = []string{Marker}
		}
		return s, nil, nil
	}
	s.Installed, s.InstalledVersion = true, m.Version
	for p, h := range m.Files {
		b, err := os.ReadFile(filepath.Join(t.Dir, filepath.FromSlash(p)))
		if err != nil || sum(b) != h {
			s.Modified = append(s.Modified, p)
		}
	}
	sort.Strings(s.Modified)
	s.Current = m.Digest == want && m.Version == buildinfo.Version && len(s.Modified) == 0
	return s, m, nil
}

// tree lists every non-directory entry under dir by slash path. Symbolic
// links are listed, never followed.
func tree(dir string) ([]string, error) {
	out := []string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	sort.Strings(out)
	return out, err
}

// Status reports each target without changing anything.
func Status(targets []Target) ([]State, error) {
	out := []State{}
	for _, t := range targets {
		s, _, err := inspect(t)
		if err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Install writes the embedded skill to each target. A current installation
// is left alone. A target holding files we did not install, or files we
// installed that were since changed, is refused unless force is set.
func Install(targets []Target, force bool) ([]Result, error) {
	files, want, err := embedded()
	if err != nil {
		return nil, err
	}
	out := []Result{}
	for _, t := range targets {
		s, _, err := inspect(t)
		if err != nil {
			return out, err
		}
		r := Result{State: s}
		switch {
		case s.Current:
		case !force && len(s.Modified) > 0:
			r.Refused = "installed files were changed since installation; rerun with --force to replace them"
		case !force && len(s.Unmanaged) > 0:
			r.Refused = "the directory holds files ClawBridge did not install; rerun with --force to replace it"
		default:
			if err = replace(t.Dir, files, want); err != nil {
				return out, err
			}
			r.Changed = true
			if r.State, _, err = inspect(t); err != nil {
				return out, err
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// replace builds the new skill beside the target and swaps it in, so readers
// see the old or the new skill, never a partial one.
func replace(dir string, files map[string][]byte, want string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staged, err := os.MkdirTemp(parent, ".clawbridge-skill-new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	hashes := map[string]string{}
	for p, b := range files {
		target := filepath.Join(staged, filepath.FromSlash(p))
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err = writeFile(target, b); err != nil {
			return err
		}
		hashes[p] = sum(b)
	}
	raw, err := json.MarshalIndent(marker{Version: buildinfo.Version, Commit: buildinfo.Commit, Digest: want, Files: hashes}, "", "  ")
	if err != nil {
		return err
	}
	if err = writeFile(filepath.Join(staged, Marker), append(raw, '\n')); err != nil {
		return err
	}
	if err = os.Chmod(staged, 0o755); err != nil {
		return err
	}
	if _, err = os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return os.Rename(staged, dir)
	} else if err != nil {
		return err
	}
	old, err := os.MkdirTemp(parent, ".clawbridge-skill-old-")
	if err != nil {
		return err
	}
	retired := filepath.Join(old, Name)
	if err = os.Rename(dir, retired); err != nil {
		os.Remove(old)
		return err
	}
	if err = os.Rename(staged, dir); err != nil {
		if back := os.Rename(retired, dir); back != nil {
			return fmt.Errorf("%w; the previous skill is kept at %s", err, retired)
		}
		os.Remove(old)
		return err
	}
	return os.RemoveAll(old)
}

func writeFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Uninstall removes only the files our marker lists. A changed file is kept
// unless force is set; files we did not install are always kept.
func Uninstall(targets []Target, force bool) ([]Result, error) {
	out := []Result{}
	for _, t := range targets {
		s, m, err := inspect(t)
		if err != nil {
			return out, err
		}
		r := Result{State: s}
		switch {
		case m == nil:
			if len(s.Unmanaged) > 0 {
				r.Refused = "no ClawBridge installation marker; nothing was removed"
			}
		case !force && len(s.Modified) > 0:
			r.Refused = "installed files were changed since installation; rerun with --force to remove them"
		default:
			names := make([]string, 0, len(m.Files)+1)
			for p := range m.Files {
				names = append(names, p)
			}
			names = append(names, Marker)
			for _, p := range names {
				if err = os.Remove(filepath.Join(t.Dir, filepath.FromSlash(p))); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return out, err
				}
			}
			removeEmptyDirs(t.Dir, names)
			r.Changed = true
			if r.State, _, err = inspect(t); err != nil {
				return out, err
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// removeEmptyDirs removes the directories our files lived in, deepest first,
// and only while they are empty.
func removeEmptyDirs(dir string, names []string) {
	dirs := map[string]bool{".": true}
	for _, p := range names {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	// A directory's path is longer than its parent's, so longest first is
	// deepest first, and "." comes last.
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		os.Remove(filepath.Join(dir, filepath.FromSlash(d)))
	}
}
