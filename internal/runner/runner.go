// Package runner is slice 1 of the session runner (live/RUNNER-DESIGN.md r2
// §6): a manual launcher. A person runs `clawbridge runner launch ISSUE`,
// and the runner prepares what they would by hand: a separate clone of the
// repository on an agent/* branch that cannot push, the agent's Claude
// Code settings and MCP config outside it, and a visible Orca terminal
// running Claude with an IDs-only start prompt.
//
// The isolation layers of §2.1 are in place from the first launch: the
// permission mode is never bypass, the Bash sandbox denies the owner's
// credentials and limits the network to hosts that are not the git host,
// and the clone has no push URL and no credential helper. The agent's
// clone is untrusted from then on: the runner never runs git inside it
// again, since its configuration and hooks are the agent's to write.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Defaults decided by the owner (RUNNER-DESIGN §8).
const (
	DefaultMaxRetained  = 10
	DefaultSelfTestWait = 3 * time.Minute
	retainNoCommits     = 7 * 24 * time.Hour
)

// Settings are the runner's own, in StateDir/config.json, mode 0600.
type Settings struct {
	// Model is the Claude model for launched sessions; empty uses the
	// owner's own default (the owner has not chosen one yet).
	Model string `json:"model,omitempty"`
	// AllowedDomains is the Bash sandbox's network allow-list. It may not
	// cover the git host the agent's work would be pushed to.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// MaxRetained caps the runner's retained clones.
	MaxRetained int `json:"max_retained,omitempty"`
	// WorkDir holds the agents' clones. The sandbox must be able to read
	// it, so it may not be under any path the sandbox denies.
	WorkDir string `json:"work_dir,omitempty"`
	// DeniedExtra are more paths the file tools are denied, such as the
	// owner's private repositories; Bash already cannot read them.
	DeniedExtra []string `json:"denied_extra,omitempty"`
}

// defaultDomains let an agent fetch the usual package registries.
var defaultDomains = []string{"proxy.golang.org", "sum.golang.org", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org"}

// DeniedPaths are the known credential stores and ClawBridge's own state.
// Bash cannot read anything in the home directory except what the session
// needs (the sandbox denies ~ and re-opens the clone, its cache and the
// Go module cache); these are also denied to the file tools by name
// (RL-M1), and the launch self-test proves the sandbox denies them.
var DeniedPaths = []string{
	"~/.ssh", "~/.gnupg", "~/.config/gh", "~/.clawbridge", "~/.config/clawbridge", "~/Library/Keychains",
	"~/.git-credentials", "~/.netrc", "~/.aws", "~/.azure", "~/.config/gcloud", "~/.docker", "~/.kube",
	"~/.npmrc", "~/.pypirc", "~/.config/op", "~/.claude", "~/Library/Application Support", "~/.cache",
}

// Launch is one recorded launch, in StateDir/launches.json.
type Launch struct {
	Session    string    `json:"session"`
	Instance   string    `json:"instance"`
	Repo       string    `json:"repo"`
	Clone      string    `json:"clone"`
	Cache      string    `json:"cache"`
	Branch     string    `json:"branch"`
	Base       string    `json:"base"`
	BaseCommit string    `json:"base_commit"`
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"issue_identifier"`
	CreatedAt  time.Time `json:"created_at"`
}

// Git runs git in a directory and returns its trimmed output. The runner
// calls it only in the owner's own repository and, before the agent
// starts, in the new clone.
type Git func(dir string, args ...string) (string, error)

// Terminal opens a visible terminal that runs command and returns its
// handle. Orca opens it in a checkout it manages, owner (the person's
// own); the command starts by changing into the agent's clone.
type Terminal func(owner, title, command string) (string, error)

// CloseTerminal closes a terminal Terminal opened.
type CloseTerminal func(handle string) error

// Claude Code versions: the sandbox's strict allow-list needs MinClaude,
// and the launcher's settings were proven live on TestedClaude.
const (
	MinClaude    = "2.1.219"
	TestedClaude = "2.1.310"
)

// DefaultManagedSettings is Claude Code's managed settings file on macOS,
// which loads whatever --setting-sources says.
const DefaultManagedSettings = "/Library/Application Support/ClaudeCode/managed-settings.json"

// Runner prepares and records launches under StateDir.
type Runner struct {
	StateDir string
	// Home expands ~ in paths; the user's home directory when empty.
	Home     string
	Git      Git
	Terminal Terminal
	Close    CloseTerminal
	// SelfTestWait is how long a launch waits for the session's self-test;
	// DefaultSelfTestWait when zero.
	SelfTestWait time.Duration
	// ClaudeVersion reports the installed claude's version.
	ClaudeVersion func() (string, error)
	// ManagedSettings is the managed settings file; DefaultManagedSettings
	// when empty.
	ManagedSettings string
	Now             func() time.Time
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

func (r *Runner) home() (string, error) {
	if r.Home != "" {
		return r.Home, nil
	}
	return os.UserHomeDir()
}

// expand resolves a leading ~ to the home directory.
func (r *Runner) expand(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return filepath.Clean(p), nil
	}
	home, err := r.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// within reports whether p is base or inside it.
func within(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// LoadSettings reads the runner's settings; a missing file is the defaults.
// A work directory under a denied path is refused: the sandbox could not
// read the agent's own clone.
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
	if s.WorkDir == "" {
		s.WorkDir = "~/clawbridge-runs"
	}
	work, err := r.expand(s.WorkDir)
	if err != nil {
		return s, err
	}
	if !filepath.IsAbs(work) {
		return s, errors.New("runner work_dir must be absolute or start with ~/")
	}
	for _, d := range DeniedPaths {
		denied, err := r.expand(d)
		if err != nil {
			return s, err
		}
		if within(work, denied) || within(denied, work) {
			return s, fmt.Errorf("runner work_dir %s overlaps %s, which the sandbox denies; choose another directory", work, d)
		}
	}
	s.WorkDir = work
	return s, nil
}

// pushHosts are the hosts the repository's remotes push to.
func pushHosts(urls []string) []string {
	var hosts []string
	for _, raw := range urls {
		host := ""
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Hostname()
		} else if at := strings.Index(raw, "@"); at >= 0 {
			// scp-like: git@github.com:owner/repo.git
			rest := raw[at+1:]
			if colon := strings.Index(rest, ":"); colon > 0 {
				host = rest[:colon]
			}
		}
		if host != "" {
			hosts = append(hosts, strings.ToLower(host))
		}
	}
	return hosts
}

// coversHost reports whether a sandbox domain pattern lets traffic reach
// host: an exact name, a *. wildcard over it or a parent, or *.
func coversHost(pattern, host string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	switch {
	case p == "*" || p == host:
		return true
	case strings.HasPrefix(p, "*."):
		suffix := p[1:]
		return strings.HasSuffix(host, suffix) || host == p[2:]
	}
	return false
}

// CheckDomains refuses an allow-list that reaches a push host (RL-M2): a
// push URL and a deny rule can be talked around, so the barrier is that
// the agent has no credentials and no route to the git host.
func CheckDomains(domains, remoteURLs []string) error {
	for _, host := range pushHosts(remoteURLs) {
		for _, d := range domains {
			if coversHost(d, host) {
				return fmt.Errorf("the sandbox's allowed domain %q reaches %s, where this repository pushes; remove it", d, host)
			}
		}
	}
	return nil
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

// Retained is the number of recorded launches whose clone still exists.
func (r *Runner) Retained() (int, error) {
	all, err := r.Launches()
	n := 0
	for _, l := range all {
		if _, e := os.Stat(l.Clone); e == nil {
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
