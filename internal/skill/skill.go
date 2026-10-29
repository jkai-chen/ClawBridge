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
	"slices"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/buildinfo"
	"clawbridge/skills"
)

// Name is the skill's directory name inside an agent's skills directory.
const Name = "clawbridge"

// Playbooks are the provisioning playbooks (live/ONBOARDING-DESIGN.md §3),
// each its own skill, installed with --playbook and versioned with the
// binary like the main skill.
var Playbooks = map[string]string{"gcp": "clawbridge-provision-gcp", "aws": "clawbridge-provision-aws", "local": "clawbridge-provision-local"}

// bundled are the skills this binary carries, by directory name.
var bundled = map[string]fs.FS{
	Name:                         skills.ClawBridge,
	"clawbridge-provision-gcp":   skills.ProvisionGCP,
	"clawbridge-provision-aws":   skills.ProvisionAWS,
	"clawbridge-provision-local": skills.ProvisionLocal,
}

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
	// Unsafe lists symbolic links and other non-regular entries; nothing is
	// read through or removed from them.
	Unsafe []string `json:"unsafe,omitempty"`
	// Corrupt says why the marker cannot be trusted; it is then ignored.
	Corrupt string `json:"corrupt,omitempty"`
}

// Result is the outcome of an install or uninstall for one target.
type Result struct {
	State
	Changed bool   `json:"changed"`
	Refused string `json:"refused,omitempty"`
	// Backup is where --force moved a folder holding files we did not install.
	Backup string `json:"backup,omitempty"`
}

// NoAgentFound is the default install's answer when neither agent is set up.
const NoAgentFound = "No Claude Code or Codex installation found; install one, then run `clawbridge skill install` (or pass --dir PATH for a custom agent folder)"

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
func Targets(agent, dir string) ([]Target, error) { return TargetsFor(Name, agent, dir) }

// TargetsFor resolves the directories for the named bundled skill.
func TargetsFor(name, agent, dir string) ([]Target, error) {
	if _, ok := bundled[name]; !ok {
		return nil, fmt.Errorf("no bundled skill %q", name)
	}
	if dir != "" {
		if agent == "" || agent == "all" {
			agent = "custom"
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		return []Target{{Agent: agent, Dir: filepath.Join(abs, name)}}, nil
	}
	names := []string{agent}
	if agent == "" || agent == "all" {
		names = Agents
	}
	out := []Target{}
	for _, agentName := range names {
		config, err := agentHome(agentName)
		if err != nil {
			return nil, err
		}
		if agent == "" {
			if info, e := os.Stat(config); e != nil || !info.IsDir() {
				continue
			}
		}
		out = append(out, Target{Agent: agentName, Dir: filepath.Join(config, "skills", name)})
	}
	if len(out) == 0 {
		// Naming an agent here would create its folder for an agent that is not
		// installed, so the advice is to install one first.
		return nil, errors.New(NoAgentFound)
	}
	return out, nil
}

// embedded returns the named skill's files by slash path, and their digest.
func embedded(name string) (map[string][]byte, string, error) {
	fsys, ok := bundled[name]
	if !ok {
		return nil, "", fmt.Errorf("no bundled skill %q", name)
	}
	files := map[string][]byte{}
	err := fs.WalkDir(fsys, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		files[strings.TrimPrefix(p, name+"/")] = b
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

var errUnsafe = errors.New("symbolic link or non-regular file")

// regularUnder checks every component of rel below dir with Lstat: none may
// be a symbolic link, inner ones must be directories, and the leaf a regular
// file (or, when wantDir, a directory).
func regularUnder(dir, rel string, wantDir bool) error {
	parts := strings.Split(rel, "/")
	current := dir
	for n, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		last := n == len(parts)-1
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return errUnsafe
		case !last || wantDir:
			if !info.IsDir() {
				return errUnsafe
			}
		case !info.Mode().IsRegular():
			return errUnsafe
		}
	}
	return nil
}

// maxMarkerFiles bounds a marker; the skill has far fewer files.
const maxMarkerFiles = 512

// validMarkerPath accepts only a clean relative slash path inside the skill
// folder. A path this binary no longer embeds is a stale entry from an older
// version: trusted only while its file is regular and its hash matches.
func validMarkerPath(p string) bool {
	return p != "" && p != Marker && p == path.Clean(p) && !path.IsAbs(p) && !strings.Contains(p, "\\") && filepath.IsLocal(filepath.FromSlash(p))
}

// inspect reads a target without changing it and without following links.
func inspect(t Target) (State, *marker, error) {
	_, want, err := embedded(filepath.Base(t.Dir))
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
		s.Unsafe = []string{"."}
		return s, nil, nil
	}
	present, err := tree(t.Dir)
	if err != nil {
		return s, nil, err
	}
	markerPresent := false
	for _, p := range present {
		if err := regularUnder(t.Dir, p, false); err != nil {
			s.Unsafe = append(s.Unsafe, p)
		} else if p == Marker {
			markerPresent = true
		}
	}
	var m *marker
	if markerPresent {
		raw, err := os.ReadFile(filepath.Join(t.Dir, Marker))
		if err != nil {
			return s, nil, err
		}
		m = &marker{}
		if json.Unmarshal(raw, m) != nil || len(m.Files) == 0 || len(m.Files) > maxMarkerFiles {
			s.Corrupt = "the installation marker is unreadable"
		}
		for p, h := range m.Files {
			if b, e := hex.DecodeString(h); !validMarkerPath(p) || e != nil || len(b) != sha256.Size {
				s.Corrupt = "the installation marker lists an invalid path or hash"
			}
		}
		if s.Corrupt != "" {
			m = nil
		}
	}
	for _, p := range present {
		if p != Marker && (m == nil || m.Files[p] == "") && !slices.Contains(s.Unsafe, p) {
			s.Unmanaged = append(s.Unmanaged, p)
		}
	}
	if m == nil {
		return s, nil, nil
	}
	s.Installed, s.InstalledVersion = true, m.Version
	for p, h := range m.Files {
		if err := regularUnder(t.Dir, p, false); err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !slices.Contains(s.Unsafe, p) {
				s.Unsafe = append(s.Unsafe, p)
			}
			s.Modified = append(s.Modified, p)
			continue
		}
		b, err := os.ReadFile(filepath.Join(t.Dir, filepath.FromSlash(p)))
		if err != nil || sum(b) != h {
			s.Modified = append(s.Modified, p)
		}
	}
	sort.Strings(s.Modified)
	sort.Strings(s.Unsafe)
	s.Current = m.Digest == want && m.Version == buildinfo.Version && len(s.Modified) == 0 && len(s.Unsafe) == 0
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

// foreign reports whether the target holds anything we did not install.
func (s State) foreign() bool {
	return len(s.Unmanaged) > 0 || len(s.Unsafe) > 0 || s.Corrupt != ""
}

// Install writes the embedded skill to each target. A current installation
// is left alone. Without force, a target with changed files or anything we
// did not install is refused. With force, a folder holding changed files or
// anything else is moved to a backup, never deleted.
func Install(targets []Target, force bool) ([]Result, error) {
	out := []Result{}
	for _, t := range targets {
		files, want, err := embedded(filepath.Base(t.Dir))
		if err != nil {
			return out, err
		}
		s, _, err := inspect(t)
		if err != nil {
			return out, err
		}
		r := Result{State: s}
		switch {
		case s.Current && !s.foreign():
		case !force && s.foreign():
			r.Refused = "the folder holds files ClawBridge did not install, links, or an untrusted marker; rerun with --force to move it to a backup"
		case !force && len(s.Modified) > 0:
			r.Refused = "installed files were changed since installation; rerun with --force to replace them"
		default:
			// Changed files are the user's work too: keep them in a backup.
			if r.Backup, err = replace(t.Dir, files, want, s.foreign() || len(s.Modified) > 0); err != nil {
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

// backupPath names a backup beside the agent's skills directory, where the
// agent does not load it as a second skill.
func backupPath(dir string) (string, error) {
	p := filepath.Join(filepath.Dir(filepath.Dir(dir)), filepath.Base(dir)+"-skill-backup-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("backup path %s already exists", p)
	}
	return p, nil
}

// replace stages the new skill beside the target and swaps it in, so an
// agent never reads a half-written skill; the folder is absent only between
// the two renames. A previous folder holding anything we did not install
// (keep) is moved to a backup, whose path is returned; one holding only our
// files is deleted.
func replace(dir string, files map[string][]byte, want string, keep bool) (string, error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	staged, err := os.MkdirTemp(parent, ".clawbridge-skill-new-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staged)
	hashes := map[string]string{}
	for p, b := range files {
		target := filepath.Join(staged, filepath.FromSlash(p))
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err = writeFile(target, b); err != nil {
			return "", err
		}
		hashes[p] = sum(b)
	}
	raw, err := json.MarshalIndent(marker{Version: buildinfo.Version, Commit: buildinfo.Commit, Digest: want, Files: hashes}, "", "  ")
	if err != nil {
		return "", err
	}
	if err = writeFile(filepath.Join(staged, Marker), append(raw, '\n')); err != nil {
		return "", err
	}
	if err = os.Chmod(staged, 0o755); err != nil {
		return "", err
	}
	if _, err = os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return "", os.Rename(staged, dir)
	} else if err != nil {
		return "", err
	}
	if keep {
		backup, err := backupPath(dir)
		if err != nil {
			return "", err
		}
		if err = os.Rename(dir, backup); err != nil {
			return "", err
		}
		if err = os.Rename(staged, dir); err != nil {
			return backup, fmt.Errorf("%w; the previous folder is kept at %s", err, backup)
		}
		return backup, nil
	}
	old, err := os.MkdirTemp(parent, ".clawbridge-skill-old-")
	if err != nil {
		return "", err
	}
	retired := filepath.Join(old, filepath.Base(dir))
	if err = os.Rename(dir, retired); err != nil {
		os.Remove(old)
		return "", err
	}
	if err = os.Rename(staged, dir); err != nil {
		if back := os.Rename(retired, dir); back != nil {
			return "", fmt.Errorf("%w; the previous skill is kept at %s", err, retired)
		}
		os.Remove(old)
		return "", err
	}
	return "", os.RemoveAll(old)
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

// Uninstall removes only the regular files our trusted marker lists. A
// changed file refuses the uninstall unless force is set, which moves it to a
// backup instead of deleting it. Files we did not install are always kept,
// and a link or an untrusted marker refuses the uninstall.
func Uninstall(targets []Target, force bool) ([]Result, error) {
	out := []Result{}
	for _, t := range targets {
		s, m, err := inspect(t)
		if err != nil {
			return out, err
		}
		r := Result{State: s}
		switch {
		case s.Corrupt != "":
			r.Refused = s.Corrupt + "; nothing was removed"
		case len(s.Unsafe) > 0:
			r.Refused = "the folder holds symbolic links or non-regular files; nothing was removed"
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
			if r.Backup, err = backupModified(t.Dir, s.Modified); err != nil {
				return out, err
			}
			for _, p := range names {
				if regularUnder(t.Dir, p, false) != nil {
					continue
				}
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

// backupModified moves the changed files that still exist into a new backup
// beside the skills directory, keeping their paths, and returns the backup.
func backupModified(dir string, modified []string) (string, error) {
	backup := ""
	for _, p := range modified {
		if regularUnder(dir, p, false) != nil {
			continue
		}
		if backup == "" {
			var err error
			if backup, err = backupPath(dir); err != nil {
				return "", err
			}
		}
		target := filepath.Join(backup, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return backup, err
		}
		if err := os.Rename(filepath.Join(dir, filepath.FromSlash(p)), target); err != nil {
			return backup, err
		}
	}
	return backup, nil
}

// removeEmptyDirs removes the directories our files lived in, deepest first,
// only while they are real, link-free directories and empty.
func removeEmptyDirs(dir string, names []string) {
	dirs := map[string]bool{}
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
	// deepest first.
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		if regularUnder(dir, d, true) == nil {
			os.Remove(filepath.Join(dir, filepath.FromSlash(d)))
		}
	}
	if info, err := os.Lstat(dir); err == nil && info.IsDir() {
		os.Remove(dir)
	}
}
