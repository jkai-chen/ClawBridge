// Package runner is slice 1 of the session runner (live/RUNNER-DESIGN.md r2
// §6): a manual launcher. A person runs `clawbridge runner launch ISSUE`,
// and the runner prepares what they would by hand: a git worktree on an
// agent/* branch that cannot push, the agent's Claude Code settings and
// MCP config outside the worktree, and a visible Orca terminal running
// Claude with an IDs-only start prompt.
//
// The isolation layers of §2.1 are in place from the first launch: the
// permission mode is never bypass, the Bash sandbox denies the owner's
// credentials and restricts the network, and the worktree's remote has no
// push URL and no credential helper. They are a second line behind the
// person who started the session; slice 3 (auto) requires them proven.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Defaults decided by the owner (RUNNER-DESIGN §8).
const (
	DefaultMaxRetained = 10
	retainNoCommits    = 7 * 24 * time.Hour
)

// Settings are the runner's own, in StateDir/config.json, mode 0600.
type Settings struct {
	// Model is the Claude model for launched sessions; empty uses the
	// owner's own default (the owner has not chosen one yet).
	Model string `json:"model,omitempty"`
	// AllowedDomains is the Bash sandbox's network allow-list.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// MaxRetained caps the runner's retained worktrees.
	MaxRetained int `json:"max_retained,omitempty"`
}

// defaultDomains let an agent fetch the usual package registries.
var defaultDomains = []string{"proxy.golang.org", "sum.golang.org", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org"}

// Launch is one recorded launch, in StateDir/launches.json.
type Launch struct {
	Session    string    `json:"session"`
	Instance   string    `json:"instance"`
	Repo       string    `json:"repo"`
	Worktree   string    `json:"worktree"`
	Branch     string    `json:"branch"`
	Base       string    `json:"base"`
	BaseCommit string    `json:"base_commit"`
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"issue_identifier"`
	CreatedAt  time.Time `json:"created_at"`
}

// Git runs git in a directory and returns its trimmed output.
type Git func(dir string, args ...string) (string, error)

// Terminal opens a visible terminal in dir that runs command.
type Terminal func(dir, title, command string) error

// Runner prepares and records launches under StateDir.
type Runner struct {
	StateDir string
	Git      Git
	Terminal Terminal
	Now      func() time.Time
}

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,9}-[0-9]{1,9}$`)
	namePart          = regexp.MustCompile(`[^a-z0-9]+`)
)

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// LoadSettings reads the runner's settings; a missing file is the defaults.
func (r *Runner) LoadSettings() (Settings, error) {
	var s Settings
	raw, err := os.ReadFile(filepath.Join(r.StateDir, "config.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&s); err != nil {
			return s, fmt.Errorf("runner config.json: %w", err)
		}
	}
	if s.MaxRetained <= 0 {
		s.MaxRetained = DefaultMaxRetained
	}
	if s.AllowedDomains == nil {
		s.AllowedDomains = defaultDomains
	}
	return s, nil
}

// SessionName is <repo>-<role>-<n>: no personal data, since it shows in
// Linear markers.
func SessionName(repo, role string, n int) string {
	clean := func(s string) string { return strings.Trim(namePart.ReplaceAllString(strings.ToLower(s), "-"), "-") }
	return fmt.Sprintf("%s-%s-%d", clean(repo), clean(role), n)
}

// Branch is the agent's branch for an issue.
func Branch(identifier string) (string, error) {
	if !identifierPattern.MatchString(identifier) {
		return "", errors.New("the issue identifier must look like ABC-12")
	}
	return "agent/" + strings.ToLower(identifier), nil
}

// Launches reads the recorded launches.
func (r *Runner) Launches() ([]Launch, error) {
	raw, err := os.ReadFile(filepath.Join(r.StateDir, "launches.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Launch
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("runner launches.json is unreadable")
	}
	return out, nil
}

func (r *Runner) saveLaunches(all []Launch) error {
	raw, _ := json.MarshalIndent(all, "", " ")
	return writePrivate(filepath.Join(r.StateDir, "launches.json"), append(raw, '\n'))
}

// Retained is the number of recorded launches whose worktree still exists.
func (r *Runner) Retained() (int, error) {
	all, err := r.Launches()
	n := 0
	for _, l := range all {
		if _, e := os.Stat(l.Worktree); e == nil {
			n++
		}
	}
	return n, err
}

// writePrivate writes a file only its owner can read, in a directory only
// its owner can open.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
