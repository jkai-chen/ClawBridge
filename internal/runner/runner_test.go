package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	issueUUID  = "11111111-1111-4111-8111-111111111111"
	issueIdent = "FIX-12"
)

func execGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), errors.New(strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

type opened struct{ dir, title, command string }

// launchScriptPath is the launch script a session's command runs.
var launchScriptPath = regexp.MustCompile(`exec /bin/sh '([^']+launch\.sh)'`)

type env struct {
	t      *testing.T
	closed []string
	// denyBin holds ls and head that always fail, as the sandbox refuses
	// those reads; leak runs the self-test without them, silent not at all.
	denyBin string
	// claudeEnv is where the fake claude writes the environment it got.
	claudeEnv    string
	leak, silent bool
	// envLeak runs the launch script with its environment kept, as a
	// script that failed to clear it would.
	envLeak bool
	// namedOnly refuses every read but of the planted files, as a sandbox
	// that denies only the paths its rules name would.
	namedOnly bool
	r         *Runner
	origin    string
	repo      string
	opens     []opened
	now       time.Time
}

// newEnv makes an origin repository with one commit on main, the owner's
// clone of it, and a home directory for the runner's work.
func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "checkout"), now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	e.git(root, "init", "--bare", "-b", "main", e.origin)
	e.git(root, "clone", e.origin, e.repo)
	e.write(e.repo, "README.md")
	e.git(e.repo, "add", "README.md")
	e.git(e.repo, "commit", "-m", "first")
	e.git(e.repo, "push", "origin", "main")
	e.git(e.repo, "remote", "set-head", "origin", "main")
	home := filepath.Join(root, "home")
	// A fake claude: it records the environment the launch script gave it,
	// then runs the self-test its prompt names, as the session's first step.
	fake := filepath.Join(root, "fake-claude")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	e.claudeEnv = filepath.Join(root, "claude.env")
	claude := "#!/bin/sh\nenv > '" + e.claudeEnv + "'\n" +
		`for a in "$@"; do p=$(printf '%s' "$a" | sed -n "s/.*sh '\([^']*selftest\.sh\)'.*/\1/p"); if [ -n "$p" ]; then exec sh "$p"; fi; done` + "\n"
	if err := os.WriteFile(filepath.Join(fake, "claude"), []byte(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	e.denyBin = filepath.Join(root, "deny-bin")
	if err := os.MkdirAll(e.denyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(root, "named-bin")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"ls", "head"} {
		if err := os.WriteFile(filepath.Join(e.denyBin, tool), []byte("#!/bin/sh\necho 'Operation not permitted' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\ncase \"$*\" in *clawbridge-selftest-*) exec " + real + " \"$@\";; esac\necho 'Operation not permitted' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(named, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.r = &Runner{StateDir: filepath.Join(home, ".clawbridge", "runner"), Home: home, Git: execGit, Now: func() time.Time { return e.now }, Terminal: func(owner, title, command string) (string, error) {
		e.opens = append(e.opens, opened{owner, title, command})
		// The terminal runs the launch script, whose claude (the fake) runs
		// the self-test the prompt names, as the sandbox would (every read
		// refused) unless e.leak. The terminal's own environment carries a
		// token, as an owner's shell may (G9-5).
		if m := launchScriptPath.FindStringSubmatch(command); m != nil && !e.silent {
			script := m[1]
			if e.envLeak {
				raw, _ := os.ReadFile(script)
				script = filepath.Join(t.TempDir(), "leaky.sh")
				_ = os.WriteFile(script, []byte(strings.Replace(string(raw), "exec env -i ", "exec env ", 1)), 0o600)
			}
			cmd := exec.Command("/bin/sh", script)
			tail := e.denyBin + ":/usr/bin:/bin"
			switch {
			case e.namedOnly:
				tail = filepath.Join(filepath.Dir(e.denyBin), "named-bin") + ":/usr/bin:/bin"
			case e.leak:
				tail = "/usr/bin:/bin"
			}
			cmd.Env = append(os.Environ(), "PATH="+fake+":"+tail, "GITHUB_TOKEN=ghp_planted-in-the-owners-shell", "CLAUDE_CODE_MESSAGING_TOKEN=parent-session-credential", "CLAUDE_CODE_MESSAGING_SOCKET=/tmp/parent.sock", "ANTHROPIC_API_KEY=sk-ant-claudes-own")
			_ = cmd.Run()
		}
		return "term-" + title, nil
	}, Close: func(handle string) error {
		e.closed = append(e.closed, handle)
		return nil
	}, SelfTestWait: 5 * time.Second, ClaudeVersion: func() (string, error) { return TestedClaude + " (Claude Code)", nil }, ManagedSettings: filepath.Join(root, "no-managed-settings.json")}
	return e
}

func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := execGit(dir, args...)
	if err != nil {
		e.t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func (e *env) write(dir, name string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) commit(dir, name string) {
	e.t.Helper()
	e.write(dir, name)
	e.git(dir, "add", name)
	e.git(dir, "commit", "-m", name)
}

func (e *env) spec(session, ident string) Spec {
	e.t.Helper()
	s, err := e.r.LoadSettings()
	if err != nil {
		e.t.Fatal(err)
	}
	return Spec{Session: session, Instance: "inst-" + session, Repo: e.repo, IssueID: issueUUID, Identifier: ident, MCPEntry: map[string]any{"command": "/usr/local/bin/clawbridge", "args": []string{"mcp"}, "type": "stdio"}, Settings: s}
}

func (e *env) launch(session, ident string) Prepared {
	e.t.Helper()
	p, err := e.r.Launch(e.spec(session, ident))
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// A launch makes a separate clone on agent/<id> that fetches from the
// owner's upstream but cannot push, writes the session's files privately
// outside it, and opens the terminal with the IDs-only prompt. The agent's
// commits stay in its clone: the owner's .git is never written.
func TestLaunchMakesAnUnpushableClone(t *testing.T) {
	e := newEnv(t)
	ownerConfig, _ := os.ReadFile(filepath.Join(e.repo, ".git", "config"))
	p := e.launch("checkout-engineer-1", issueIdent)
	if !p.Opened || p.Branch != "agent/fix-12" || p.Base != "refs/remotes/origin/main" || p.Clone != filepath.Join(e.r.Home, "clawbridge-runs", "checkout-engineer-1") {
		t.Fatal(p)
	}
	if got := e.git(p.Clone, "rev-parse", "--abbrev-ref", "HEAD"); got != "agent/fix-12" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(p.Clone, ".git", "objects")); err != nil {
		t.Fatal("the clone has no object store of its own:", err)
	}
	// Not a shared clone: it never reads the owner's object store.
	if _, err := os.Stat(filepath.Join(p.Clone, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("the clone borrows the owner's objects")
	}
	if got := e.git(p.Clone, "remote", "get-url", "origin"); got != e.origin {
		t.Fatal("origin", got)
	}
	if got := e.git(p.Clone, "config", "--get", "remote.origin.pushurl"); got != "DISABLED" {
		t.Fatal("push URL", got)
	}
	if got, err := execGit(p.Clone, "config", "--local", "--get", "credential.helper"); err != nil || got != "" {
		t.Fatal("credential helper", got, err)
	}
	e.commit(p.Clone, "work.txt")
	if _, err := execGit(p.Clone, "push", "origin", "agent/fix-12"); err == nil {
		t.Fatal("the clone pushed through origin")
	}
	// The owner's repository is untouched: same config, no agent branch,
	// and it still pushes.
	if after, _ := os.ReadFile(filepath.Join(e.repo, ".git", "config")); string(after) != string(ownerConfig) {
		t.Fatal("the owner's git config changed:\n" + string(after))
	}
	if _, err := execGit(e.repo, "rev-parse", "--verify", "--quiet", "refs/heads/agent/fix-12"); err == nil {
		t.Fatal("the agent's branch appeared in the owner's repository")
	}
	if _, err := execGit(e.repo, "push", "--dry-run", "origin", "main"); err != nil {
		t.Fatal("the owner cannot push:", err)
	}
	// The owner brings the work in with the printed fetch.
	if p.Fetch != "clawbridge runner fetch checkout-engineer-1" {
		t.Fatal(p.Fetch)
	}
	e.git(e.repo, "fetch", p.Clone, "agent/fix-12:agent/fix-12")
	if got := e.git(e.repo, "log", "-1", "--format=%s", "agent/fix-12"); got != "work.txt" {
		t.Fatal(got)
	}
	for _, f := range []string{p.SettingsFile, p.MCPFile} {
		info, err := os.Stat(f)
		if err != nil || info.Mode().Perm() != 0o600 || strings.HasPrefix(f, p.Clone) {
			t.Fatal(f, info, err)
		}
		if dir, _ := os.Stat(filepath.Dir(f)); dir.Mode().Perm() != 0o700 {
			t.Fatal("session directory", dir.Mode())
		}
	}
	var mcp map[string]map[string]map[string]any
	raw, _ := os.ReadFile(p.MCPFile)
	if json.Unmarshal(raw, &mcp) != nil || mcp["mcpServers"]["clawbridge"]["command"] != "/usr/local/bin/clawbridge" {
		t.Fatal(string(raw))
	}
	owner, _ := filepath.EvalSymlinks(e.repo)
	if len(e.opens) != 1 || e.opens[0].dir != owner || e.opens[0].title != "checkout-engineer-1" {
		t.Fatal(e.opens)
	}
	cmd := e.opens[0].command
	// The command changes into the clone and runs the launch script, kept
	// beside the settings where the agent can't read or change it.
	script := filepath.Join(filepath.Dir(p.SettingsFile), "launch.sh")
	if cmd != "cd '"+p.Clone+"' && exec /bin/sh '"+script+"'" || p.Command != cmd {
		t.Fatal("command:", cmd)
	}
	raw, _ = os.ReadFile(script)
	launch := string(raw)
	// F1, F3, F4: claude loads no user, project or local settings, and
	// starts in the default mode.
	for _, want := range []string{"exec env -i ", " claude --setting-sources '' --permission-mode default --settings '" + p.SettingsFile + "'", "--mcp-config '" + p.MCPFile + "'", "--strict-mcp-config", issueIdent, issueUUID, "untrusted data"} {
		if !strings.Contains(launch, want) {
			t.Fatalf("launch script lacks %q:\n%s", want, launch)
		}
	}
	if strings.Contains(launch, "--model") || strings.Contains(launch, "bypass") || strings.Contains(launch, "dangerously") {
		t.Fatal("launch script:", launch)
	}
	// G9-5: claude got the allow-list only; the token the terminal's
	// environment carried never reached it, and the self-test (which
	// passed) found the launch script's canary unset.
	got, _ := os.ReadFile(e.claudeEnv)
	if seen := string(got); strings.Contains(seen, "GITHUB_TOKEN") || strings.Contains(seen, "ghp_") || strings.Contains(seen, canaryVar) || strings.Contains(seen, "CLAUDE_CODE_MESSAGING") || strings.Contains(seen, "parent-session-credential") || !strings.Contains(seen, "PATH=") || !strings.Contains(seen, "HOME=") || !strings.Contains(seen, "ANTHROPIC_API_KEY=sk-ant-claudes-own") {
		t.Fatal("claude's environment:", seen)
	}
	launches, _ := e.r.Launches()
	if len(launches) != 1 || launches[0].Instance != "inst-checkout-engineer-1" || launches[0].BaseCommit == "" || launches[0].Cache != p.Cache {
		t.Fatal(launches)
	}
	// F2: the session's caches live beside the clone, privately.
	if info, err := os.Stat(p.Cache); err != nil || info.Mode().Perm() != 0o700 || p.Cache != p.Clone+".cache" || p.ClaudeVersion != TestedClaude {
		t.Fatal("cache", info, err, p)
	}
}

type agentSettings struct {
	Permissions struct {
		DefaultMode, DisableBypassPermissionsMode, DisableAutoMode string
		Deny                                                       []string
	}
	Env     map[string]string
	Sandbox struct {
		Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
		Filesystem                                           struct{ DenyRead, AllowRead, AllowWrite []string }
		Network                                              struct {
			AllowedDomains  []string
			StrictAllowlist bool
		}
	}
}

// The model is a setting; the settings never allow bypass; the Bash
// sandbox and the file tools deny the same credential paths (RL-M1); the
// owner's checkout cannot be edited and WebFetch is off.
func TestLaunchSettingsAndModel(t *testing.T) {
	e := newEnv(t)
	if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"agents":{"claude":{"model":"claude-opus-5-5"},"codex":{"model":"other"}},"allowed_domains":["proxy.golang.org"]}`)); err != nil {
		t.Fatal(err)
	}
	p := e.launch("checkout-engineer-1", issueIdent)
	if launch, _ := os.ReadFile(filepath.Join(filepath.Dir(p.SettingsFile), "launch.sh")); !strings.Contains(string(launch), "--model 'claude-opus-5-5'") {
		t.Fatal(string(launch))
	}
	var s agentSettings
	raw, _ := os.ReadFile(p.SettingsFile)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Permissions.DefaultMode != "default" || s.Permissions.DisableBypassPermissionsMode != "disable" || s.Permissions.DisableAutoMode != "disable" {
		t.Fatal("permissions", s.Permissions)
	}
	if !s.Sandbox.Enabled || !s.Sandbox.FailIfUnavailable || s.Sandbox.AllowUnsandboxedCommands || !s.Sandbox.Network.StrictAllowlist {
		t.Fatal("sandbox", s.Sandbox)
	}
	if len(s.Sandbox.Filesystem.AllowWrite) != 1 || s.Sandbox.Filesystem.AllowWrite[0] != p.Cache {
		t.Fatal("writable", s.Sandbox.Filesystem.AllowWrite)
	}
	if s.Env["GOTOOLCHAIN"] != "local" || s.Env["GOPROXY"] != "" {
		t.Fatal("go network settings without an owner cache", s.Env)
	}
	if s.Env["GIT_CONFIG_GLOBAL"] != "/dev/null" {
		t.Fatal("git would read the denied global config", s.Env)
	}
	for _, k := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "npm_config_cache", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if !strings.HasPrefix(s.Env[k], p.Cache+string(filepath.Separator)) {
			t.Fatal("cache variable", k, s.Env[k])
		}
	}
	repo, _ := filepath.EvalSymlinks(e.repo)
	for _, want := range []string{"Bash(git push *)", "Bash(gh *)", "WebFetch", "Edit(/" + repo + "/**)"} {
		if !contains(s.Permissions.Deny, want) {
			t.Fatal("not denied:", want)
		}
	}
	// Bash reads nothing in the home directory but the clone and its
	// cache; the file tools are denied every known credential location.
	if len(s.Sandbox.Filesystem.DenyRead) != 1 || s.Sandbox.Filesystem.DenyRead[0] != "~/" || !contains(s.Sandbox.Filesystem.AllowRead, p.Clone) || !contains(s.Sandbox.Filesystem.AllowRead, p.Cache) || len(s.Sandbox.Filesystem.AllowRead) != 2 {
		t.Fatal("filesystem", s.Sandbox.Filesystem)
	}
	for _, path := range DeniedPaths {
		if !contains(s.Permissions.Deny, "Read("+path+"/**)") || !contains(s.Permissions.Deny, "Edit("+path+"/**)") {
			t.Fatal("the file tools can reach", path)
		}
	}
	for _, path := range []string{"~/.config/gh", "~/.claude", "~/Library/Application Support", "~/.aws", "~/.kube"} {
		if !contains(DeniedPaths, path) {
			t.Fatal("not a known credential location:", path)
		}
	}
	if len(s.Sandbox.Network.AllowedDomains) != 1 || s.Sandbox.Network.AllowedDomains[0] != "proxy.golang.org" {
		t.Fatal(s.Sandbox.Network)
	}
	if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"modle":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.LoadSettings(); err == nil {
		t.Fatal("a misspelt setting was accepted")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RL-H1: a work directory the sandbox cannot read, or one holding a denied
// path, is refused.
func TestWorkDirOutsideDeniedPaths(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"~/.clawbridge/runs", "~", "~/.ssh", "relative/dir"} {
		if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"work_dir":"`+bad+`"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.r.LoadSettings(); err == nil {
			t.Fatal("work_dir accepted:", bad)
		}
	}
	other := filepath.Join(t.TempDir(), "runs")
	if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"work_dir":"`+other+`"}`)); err != nil {
		t.Fatal(err)
	}
	if s, err := e.r.LoadSettings(); err != nil || s.WorkDir != other {
		t.Fatal(s, err)
	}
}

// RL-M2: the network allow-list may not reach the host the repository
// pushes to; the barrier is no route and no credentials, not a push URL.
func TestAllowListMayNotReachThePushHost(t *testing.T) {
	for _, c := range []struct {
		domains []string
		urls    []string
		ok      bool
	}{
		{[]string{"github.com"}, []string{"git@github.com:o/r.git"}, false},
		{[]string{"*.github.com"}, []string{"https://github.com/o/r.git"}, false},
		{[]string{"*"}, []string{"https://gitlab.example.test/o/r"}, false},
		{[]string{"*.example.test"}, []string{"https://git.example.test/o/r"}, false},
		{[]string{"proxy.golang.org", "api.github.com"}, []string{"https://github.com/o/r.git"}, true},
		{[]string{"github.com"}, []string{"ssh://git@code.example.test:2222/o/r.git"}, true},
		{[]string{"code.example.test"}, []string{"ssh://git@code.example.test:2222/o/r.git"}, false},
	} {
		if err := CheckDomains(c.domains, c.urls); (err == nil) != c.ok {
			t.Fatal(c, err)
		}
	}
	e := newEnv(t)
	e.git(e.repo, "remote", "set-url", "--push", "origin", "https://github.com/o/r.git")
	spec := e.spec("checkout-engineer-1", issueIdent)
	spec.Settings.AllowedDomains = []string{"github.com"}
	if _, err := e.r.Launch(spec); err == nil || !strings.Contains(err.Error(), "github.com") {
		t.Fatal("a launch reached the push host:", err)
	}
}

// A dry run changes nothing; a taken clone, the retained cap and a bad
// identifier refuse; a terminal Orca cannot open is reported with the
// command; a clone that turns out dirty is never started on.
func TestLaunchRefusalsAndDryRun(t *testing.T) {
	e := newEnv(t)
	dry := e.spec("checkout-engineer-1", issueIdent)
	dry.DryRun = true
	if p, err := e.r.Launch(dry); err != nil || !p.DryRun || p.Command == "" {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(dry.Settings.WorkDir); err == nil || len(e.opens) != 0 {
		t.Fatal("a dry run made a clone or opened a terminal")
	}
	if _, err := e.r.Launch(e.spec("checkout-engineer-1", "not an id")); err == nil {
		t.Fatal("a bad identifier accepted")
	}
	e.r.Terminal = func(string, string, string) (string, error) { return "", errors.New("orca is not running") }
	p, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent))
	if err != nil || p.Opened || !strings.Contains(p.Note, "orca is not running") {
		t.Fatal(p, err)
	}
	if _, err = e.r.Launch(e.spec("checkout-engineer-1", issueIdent)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("a taken clone accepted:", err)
	}
	capped := e.spec("checkout-engineer-3", "FIX-13")
	capped.Settings.MaxRetained = 1
	if _, err = e.r.Launch(capped); err == nil || !strings.Contains(err.Error(), "runner gc") {
		t.Fatal("the cap did not hold:", err)
	}
	plain := e.r.Git
	e.r.Git = func(dir string, args ...string) (string, error) {
		out, err := plain(dir, args...)
		if len(args) > 0 && args[0] == "checkout" {
			_ = os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644)
		}
		return out, err
	}
	if _, err = e.r.Launch(e.spec("checkout-engineer-4", "FIX-14")); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatal("a dirty clone was started on:", err)
	}
}

// guardGit fails the test if git runs inside a clone: its config and hooks
// are the agent's.
func (e *env) guardGit(clones ...string) {
	plain := e.r.Git
	e.r.Git = func(dir string, args ...string) (string, error) {
		for _, c := range clones {
			if within(dir, c) {
				e.t.Fatalf("git %v ran inside the agent's clone %s", args, c)
			}
		}
		return plain(dir, args...)
	}
}

func byBranch(cs []Collected) map[string]Collected {
	m := map[string]Collected{}
	for _, c := range cs {
		m[c.Branch] = c
	}
	return m
}

// gc judges the work in the owner's repository and never runs git in a
// clone: merged work (rebase or fast-forward) is removed with its fetched
// branch; unfetched, unmerged or newer work is kept; an untouched clone
// goes after a week without changes; a preview removes nothing.
func TestGCRemovesOnlyFinishedClones(t *testing.T) {
	e := newEnv(t)
	rebased := e.launch("checkout-engineer-1", "FIX-1")
	e.commit(rebased.Clone, "rebased.txt")
	unfetched := e.launch("checkout-engineer-2", "FIX-2")
	e.commit(unfetched.Clone, "unfetched.txt")
	review := e.launch("checkout-engineer-3", "FIX-3")
	e.commit(review.Clone, "review.txt")
	newer := e.launch("checkout-engineer-4", "FIX-4")
	e.commit(newer.Clone, "newer.txt")
	idle := e.launch("checkout-engineer-5", "FIX-5")
	touched := e.launch("checkout-engineer-6", "FIX-6")
	ff := e.launch("checkout-engineer-7", "FIX-7")
	e.commit(ff.Clone, "ff.txt")
	for _, p := range []Prepared{rebased, review, newer, ff} {
		e.git(e.repo, "fetch", p.Clone, p.Branch+":"+p.Branch)
	}
	e.commit(newer.Clone, "newer-2.txt")
	// The reviewer merges one by rebase (a new ID, the same patch) and
	// one by fast-forward.
	e.git(e.repo, "merge", "--ff-only", ff.Branch)
	e.git(e.repo, "cherry-pick", rebased.Branch)
	e.git(e.repo, "commit", "--amend", "-m", "rebased.txt (#1)")
	e.git(e.repo, "push", "origin", "main")
	e.git(e.repo, "fetch", "origin")
	// The idle clone's files are a week old; the touched one changed.
	old := e.now.Add(-8 * 24 * time.Hour)
	for _, p := range []Prepared{idle, touched} {
		_ = filepath.WalkDir(p.Clone, func(path string, d os.DirEntry, err error) error {
			return os.Chtimes(path, old, old)
		})
	}
	e.write(touched.Clone, "draft.txt")
	recent := e.now.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(touched.Clone, "draft.txt"), recent, recent); err != nil {
		t.Fatal(err)
	}
	e.git(rebased.Clone, "pack-refs", "--all")
	e.guardGit(rebased.Clone, unfetched.Clone, review.Clone, newer.Clone, idle.Clone, touched.Clone, ff.Clone)
	preview, err := e.r.GC(false)
	got := byBranch(preview)
	if err != nil || got["agent/fix-1"].Action != "remove" || got["agent/fix-7"].Action != "remove" || got["agent/fix-5"].Action != "keep" {
		t.Fatal(preview, err)
	}
	if !strings.Contains(got["agent/fix-2"].Reason, "not fetched") || !strings.Contains(got["agent/fix-3"].Reason, "waiting for review") || !strings.Contains(got["agent/fix-4"].Reason, "not fetched") {
		t.Fatal(got)
	}
	if _, err = os.Stat(rebased.Clone); err != nil {
		t.Fatal("a preview removed a clone")
	}
	e.now = e.now.Add(retainNoCommits)
	done, err := e.r.GC(true)
	removed := map[string]bool{"agent/fix-1": true, "agent/fix-5": true, "agent/fix-7": true}
	for _, c := range done {
		if c.Removed != removed[c.Branch] || c.Error != "" {
			t.Fatal(c)
		}
	}
	for _, p := range []Prepared{rebased, idle, ff} {
		if _, err = os.Stat(p.Clone); err == nil {
			t.Fatal("not removed:", p.Clone)
		}
		if _, err = os.Stat(filepath.Dir(p.SettingsFile)); err == nil {
			t.Fatal("session files kept:", p.Session)
		}
	}
	for _, branch := range []string{"agent/fix-1", "agent/fix-7"} {
		if _, err = execGit(e.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			t.Fatal("the merged branch was kept:", branch)
		}
	}
	if _, err = execGit(e.repo, "rev-parse", "--verify", "--quiet", "refs/heads/agent/fix-3"); err != nil {
		t.Fatal("an unmerged branch was deleted")
	}
	for _, p := range []Prepared{unfetched, review, newer, touched} {
		if _, err = os.Stat(p.Clone); err != nil {
			t.Fatal("removed work in progress:", p.Clone)
		}
	}
	if n, _ := e.r.Retained(); n != 4 {
		t.Fatal("retained", n)
	}
	if left, _ := e.r.Launches(); len(left) != 4 {
		t.Fatal("records", left)
	}
}

func TestSessionNameAndBranch(t *testing.T) {
	if got := SessionName("ClawBridge", "Engineer", 2); got != "clawbridge-engineer-2" {
		t.Fatal(got)
	}
	if got := SessionName("my.repo_x", "code review", 1); got != "my-repo-x-code-review-1" {
		t.Fatal(got)
	}
	if b, err := Branch("FJA-7"); err != nil || b != "agent/fja-7" {
		t.Fatal(b, err)
	}
	for _, bad := range []string{"", "FJA", "fja-7; rm", "../x-1"} {
		if _, err := Branch(bad); err == nil {
			t.Fatal(bad, "accepted")
		}
	}
}

// The push host is denied in WebFetch's domain form too, which reaches the
// sandbox's proxy.
func TestPushHostDeniedForFetches(t *testing.T) {
	e := newEnv(t)
	e.git(e.repo, "remote", "set-url", "--push", "origin", "https://github.com/o/r.git")
	p := e.launch("checkout-engineer-1", issueIdent)
	var s agentSettings
	raw, _ := os.ReadFile(p.SettingsFile)
	if err := json.Unmarshal(raw, &s); err != nil || !contains(s.Permissions.Deny, "WebFetch(domain:github.com)") {
		t.Fatal(s.Permissions.Deny, err)
	}
}

// F1: claude must be new enough for the strict allow-list and of the major
// version the launcher was proven on.
func TestCheckClaude(t *testing.T) {
	for line, ok := range map[string]bool{TestedClaude + " (Claude Code)": true, "2.1.219": true, "2.9.0 (Claude Code)": true, "2.1.218": false, "3.0.0": false, "1.9.9": false, "garbage": false} {
		if _, err := CheckClaude(line); (err == nil) != ok {
			t.Fatal(line, err)
		}
	}
	if note, _ := CheckClaude("2.9.0"); !strings.Contains(note, "newer") {
		t.Fatal("no note for a newer claude", note)
	}
	e := newEnv(t)
	e.r.ClaudeVersion = func() (string, error) { return "2.1.100 (Claude Code)", nil }
	if _, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent)); err == nil || !strings.Contains(err.Error(), MinClaude) {
		t.Fatal("an old claude was started:", err)
	}
}

// F1: managed settings load whatever --setting-sources says, so one that
// widens the network is refused before anything starts.
func TestManagedSettingsMayNotWidenTheNetwork(t *testing.T) {
	urls := []string{"https://github.com/o/r.git"}
	for name, c := range map[string]struct {
		raw string
		ok  bool
	}{
		"none":            {`{}`, true},
		"same domain":     {`{"sandbox":{"network":{"allowedDomains":["proxy.golang.org"]}}}`, true},
		"extra domain":    {`{"sandbox":{"network":{"allowedDomains":["example.com"]}}}`, false},
		"webfetch allow":  {`{"permissions":{"allow":["WebFetch(domain:example.com)"]}}`, false},
		"push host":       {`{"permissions":{"allow":["WebFetch(domain:github.com)"]}}`, false},
		"other allow":     {`{"permissions":{"allow":["Bash(ls *)"]}}`, true},
		"unreadable json": {`{`, false},
	} {
		if err := CheckManaged([]byte(c.raw), []string{"proxy.golang.org"}, urls); (err == nil) != c.ok {
			t.Fatal(name, err)
		}
	}
	e := newEnv(t)
	if err := os.WriteFile(e.r.ManagedSettings, []byte(`{"permissions":{"allow":["WebFetch(domain:example.com)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent)); err == nil || !strings.Contains(err.Error(), "example.com") {
		t.Fatal("widening managed settings were accepted:", err)
	}
}

// Go modules come read-only from the owner's module cache, never the
// network: Go cannot verify TLS inside the sandbox on macOS (re-spike).
func TestGoModulesComeFromTheOwnersCache(t *testing.T) {
	e := newEnv(t)
	spec := e.spec("checkout-engineer-1", issueIdent)
	spec.GoModCache = "/home/owner/go/pkg/mod"
	p, err := e.r.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	var s agentSettings
	raw, _ := os.ReadFile(p.SettingsFile)
	if err = json.Unmarshal(raw, &s); err != nil || s.Env["GOPROXY"] != "file:///home/owner/go/pkg/mod/cache/download,off" || !strings.HasPrefix(s.Env["GOMODCACHE"], p.Cache) {
		t.Fatal(s.Env, err)
	}
}

// runner fetch brings a clean branch in as agent/<id>; a branch that adds a
// token anywhere in its history is refused and leaves nothing behind, and
// the report names the file and pattern, never the text.
func TestFetchScansForTokens(t *testing.T) {
	e := newEnv(t)
	clean := e.launch("checkout-engineer-1", "FIX-1")
	e.commit(clean.Clone, "work.txt")
	leaky := e.launch("checkout-engineer-2", "FIX-2")
	secret := "ghp_" + strings.Repeat("A1b2", 9)
	if err := os.WriteFile(filepath.Join(leaky.Clone, "notes.txt"), []byte("token: "+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git(leaky.Clone, "add", "notes.txt")
	e.git(leaky.Clone, "commit", "-m", "notes")
	// Removed again: it is still in the history a push would carry.
	e.git(leaky.Clone, "rm", "-q", "notes.txt")
	e.git(leaky.Clone, "commit", "-m", "tidy")
	e.guardGit(clean.Clone, leaky.Clone)
	got, err := e.r.Fetch("checkout-engineer-1")
	if err != nil || !got.Fetched || got.Commits != 1 {
		t.Fatal(got, err)
	}
	if e.git(e.repo, "log", "-1", "--format=%s", "agent/fix-1") != "work.txt" {
		t.Fatal("not fetched")
	}
	got, err = e.r.Fetch("checkout-engineer-2")
	if err == nil || got.Fetched || len(got.Hits) != 1 || got.Hits[0].File != "notes.txt" || got.Hits[0].Pattern != "github-token" {
		t.Fatal("a token-carrying branch was fetched:", got, err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprint(got), secret) {
		t.Fatal("the token was echoed")
	}
	for _, ref := range []string{"refs/heads/agent/fix-2", "refs/clawbridge-runner/checkout-engineer-2", "refs/clawbridge-runner/checkout-engineer-1"} {
		if _, err := execGit(e.repo, "rev-parse", "--verify", "--quiet", ref); err == nil {
			t.Fatal("left behind:", ref)
		}
	}
	if _, err = e.r.Fetch("nobody"); err == nil {
		t.Fatal("an unknown session was fetched")
	}
}

func TestScanPatchPatterns(t *testing.T) {
	for _, line := range []string{"ghp_" + strings.Repeat("x", 36), "github_pat_" + strings.Repeat("a", 40), "AKIA" + strings.Repeat("A", 16), "xoxb-" + strings.Repeat("1", 12), "-----BEGIN OPENSSH PRIVATE KEY-----", "sk-ant-" + strings.Repeat("a", 30),
		"npm_" + strings.Repeat("b", 36), "//registry.npmjs.org/:_authToken=" + strings.Repeat("c", 36), "1//0" + strings.Repeat("d", 40), "Authorization: Bearer " + strings.Repeat("e", 40)} {
		if hits := ScanPatch("+++ b/f\n+" + line + "\n"); len(hits) != 1 {
			t.Fatal("missed", line)
		}
		if hits := ScanPatch("+++ b/f\n-" + line + "\n " + line + "\n"); len(hits) != 0 {
			t.Fatal("a removed or context line counted", line)
		}
	}
	if hits := ScanPatch("+++ b/f\n+just some text with gh_ and AKIA in it\n+_authToken=${NPM_TOKEN}\n+see Authorization: Bearer <token>\n+npm_install_it\n"); len(hits) != 0 {
		t.Fatal("false positive", hits)
	}
}

// The launch waits for the session's sandbox self-test: a readable
// credential location, or no report in time, closes the terminal and
// fails the launch; a passing one is recorded.
func TestLaunchSelfTest(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.r.Home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := e.launch("checkout-engineer-1", "FIX-1")
	if p.SelfTest != "passed" || len(e.closed) != 0 {
		t.Fatal(p, e.closed)
	}
	script, _ := os.ReadFile(filepath.Join(p.Cache, "selftest.sh"))
	if !strings.Contains(string(script), "ls '"+filepath.Join(e.r.Home, ".ssh")+"'") || strings.Contains(string(script), ".aws") {
		t.Fatal("the self-test does not try exactly the locations that exist:\n" + string(script))
	}
	e.leak = true
	if _, err := e.r.Launch(e.spec("checkout-engineer-2", "FIX-2")); err == nil || !strings.Contains(err.Error(), "~/.ssh") || len(e.closed) != 1 || e.closed[0] != "term-checkout-engineer-2" {
		t.Fatal("a readable credential did not stop the session:", err, e.closed)
	}
	e.leak, e.silent = false, true
	e.r.SelfTestWait = time.Second
	if _, err := e.r.Launch(e.spec("checkout-engineer-3", "FIX-3")); err == nil || !strings.Contains(err.Error(), "in time") || len(e.closed) != 2 {
		t.Fatal("a silent session was not stopped:", err, e.closed)
	}
}

// The self-test also reads files the launch plants where no rule names
// them, so a sandbox that only denies the named paths fails it; the
// planted files are gone afterwards, pass or fail.
func TestSelfTestProvesTheDefaultDeny(t *testing.T) {
	e := newEnv(t)
	// The checkout is under this home, so a file is planted in its .git too.
	e.r.Home = filepath.Dir(e.repo)
	planted := func() []string {
		var all []string
		for _, dir := range []string{e.r.Home, filepath.Join(e.repo, ".git")} {
			m, _ := filepath.Glob(filepath.Join(dir, ".clawbridge-selftest-*"))
			all = append(all, m...)
		}
		return all
	}
	p := e.launch("checkout-engineer-1", "FIX-1")
	script, _ := os.ReadFile(filepath.Join(p.Cache, "selftest.sh"))
	if p.SelfTest != "passed" || strings.Count(string(script), ".clawbridge-selftest-checkout-engineer-1-") != 2 || len(planted()) != 0 {
		t.Fatal("planted files", p.SelfTest, planted(), string(script))
	}
	e.namedOnly = true
	_, err := e.r.Launch(e.spec("checkout-engineer-2", "FIX-2"))
	if err == nil || !strings.Contains(err.Error(), "planted") || len(e.closed) != 1 || len(planted()) != 0 {
		t.Fatal("a sandbox denying only the named paths passed:", err, e.closed, planted())
	}
}

// In a linked worktree, .git is a file: the checkout's sentinel goes in
// the worktree's own git directory.
func TestSelfTestPlantsInALinkedWorktreesGitDir(t *testing.T) {
	e := newEnv(t)
	e.r.Home = filepath.Dir(e.repo)
	linked := filepath.Join(e.r.Home, "linked")
	e.git(e.repo, "worktree", "add", "-q", "-b", "side", linked)
	spec := e.spec("linked-engineer-1", "FIX-1")
	spec.Repo = linked
	p, err := e.r.Launch(spec)
	if err != nil || p.SelfTest != "passed" {
		t.Fatal(p, err)
	}
	script, _ := os.ReadFile(filepath.Join(p.Cache, "selftest.sh"))
	if !strings.Contains(string(script), filepath.Join(".git", "worktrees", "linked", ".clawbridge-selftest-")) {
		t.Fatal("no sentinel in the worktree's git directory:\n" + string(script))
	}
	e.namedOnly = true
	spec = e.spec("linked-engineer-2", "FIX-2")
	spec.Repo = linked
	if _, err = e.r.Launch(spec); err == nil || !strings.Contains(err.Error(), "planted") {
		t.Fatal("a sandbox denying only the named paths passed:", err)
	}
}

func TestCheckSelfTestReport(t *testing.T) {
	for report, ok := range map[string]bool{
		"DENIED 0\nDENIED 3\nDONE\n":        true,
		"DENIED 0\nREAD 3\nDONE\n":          false,
		"DENIED 0\nDONE\n":                  false,
		"DENIED 0\nDENIED 3\n":              false,
		"DENIED 0\nDENIED 3\nDONE\nextra\n": false,
	} {
		if err := checkSelfTest(report, map[string]string{"0": "~/.ssh", "3": "~/.clawbridge"}); (err == nil) != ok {
			t.Fatal(report, err)
		}
	}
}

// The clone carries the owner's commit identity, since the sandbox cannot
// read ~/.gitconfig.
func TestCloneCarriesTheCommitIdentity(t *testing.T) {
	e := newEnv(t)
	e.git(e.repo, "config", "user.name", "Owner Name")
	e.git(e.repo, "config", "user.email", "owner@example.test")
	p := e.launch("checkout-engineer-1", issueIdent)
	if e.git(p.Clone, "config", "--local", "user.name") != "Owner Name" || e.git(p.Clone, "config", "--local", "user.email") != "owner@example.test" {
		t.Fatal("identity not copied")
	}
}

// No model is passed unless one is set (owner decision); a top-level model
// key is refused, and agents are keyed by name.
func TestNoDefaultModel(t *testing.T) {
	e := newEnv(t)
	if p := e.launch("checkout-engineer-1", issueIdent); strings.Contains(p.Command, "--model") {
		t.Fatal("a default model was passed:", p.Command)
	}
	for _, raw := range []string{`{"model":"x"}`, `{"agents":{"Claude Code":{"model":"x"}}}`, `{"agents":{"claude":{"model":"x","effort":"max"}}}`} {
		if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.r.LoadSettings(); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

// Every location read is named, sorted, whatever the map's order.
func TestCheckSelfTestNamesEveryRead(t *testing.T) {
	sentinels := map[string]string{"0": "~/.ssh", "3": "~/.clawbridge", "home": "a file planted in the home directory"}
	for i := 0; i < 20; i++ {
		err := checkSelfTest("READ 0\nREAD 3\nREAD home\nDONE\n", sentinels)
		if err == nil || err.Error() != "the sandbox let the session read a file planted in the home directory, ~/.clawbridge, ~/.ssh" {
			t.Fatal(err)
		}
	}
}

// G9-5: the launch script names the allow-list and claude's own variables,
// one by one, never a value, and nothing else: not a parent Claude Code
// session's own variables, its messaging credential least of all
// (R141-M1).
func TestLaunchScriptAllowListsTheEnvironment(t *testing.T) {
	script := LaunchScript("c0ffee", "/s/settings.json", "/s/mcp.json", "", "go")
	for _, want := range []string{`${PATH+"PATH=$PATH"}`, `${HOME+"HOME=$HOME"}`, `${ANTHROPIC_API_KEY+"ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY"}`, `${CLAUDE_CONFIG_DIR+"CLAUDE_CONFIG_DIR=$CLAUDE_CONFIG_DIR"}`, canaryVar + "='c0ffee'", "exec env -i "} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	for _, never := range []string{"GITHUB_TOKEN", "AWS_", "SSH_AUTH_SOCK", "CLAWBRIDGE_DEVICE_KEY", "CLAUDE_CODE_MESSAGING", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_PID", "CLAUDE_CODE_ENTRYPOINT"} {
		if strings.Contains(script, never) {
			t.Fatalf("script has %q:\n%s", never, script)
		}
	}
	// The MCP child gets the owner's ClawBridge settings and the session
	// bus, never a secret or the canary.
	env := mcpEnv([]string{"CLAWBRIDGE_HOME=/h", "CLAWBRIDGE_DEVICE_KEY=k", "CLAWBRIDGE_OAUTH_CLIENT_SECRET=s", canaryVar + "=c", "DBUS_SESSION_BUS_ADDRESS=unix:path=/b", "GITHUB_TOKEN=t"})
	if len(env) != 2 || env["CLAWBRIDGE_HOME"] != "/h" || env["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/b" {
		t.Fatal(env)
	}
}

// G9-5: a launch whose session could see the canary fails its self-test.
func TestSelfTestFailsWhenTheEnvironmentLeaks(t *testing.T) {
	if err := checkSelfTest("DENIED home\nREAD env\nDONE\n", map[string]string{"home": "a file planted in the home directory", "env": "a variable from the shell that started it"}); err == nil || !strings.Contains(err.Error(), "a variable from the shell that started it") {
		t.Fatal(err)
	}
}

// G9-6: the clone's origin never carries a user or password, and an
// unknown transport or a remote helper is refused.
func TestCleanOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:ghp_secret@github.com/acme/app.git": "https://github.com/acme/app.git",
		"https://x-access-token@github.com/acme/app.git":  "https://github.com/acme/app.git",
		"ssh://git@github.com/acme/app.git":               "ssh://github.com/acme/app.git",
		"git@github.com:acme/app.git":                     "github.com:acme/app.git",
		"user:pass@host.example:repo.git":                 "host.example:repo.git",
		"github.com:acme/app.git":                         "github.com:acme/app.git",
		"/srv/git/app.git":                                "/srv/git/app.git",
		"./relative/app.git":                              "./relative/app.git",
		"file:///srv/git/app.git":                         "file:///srv/git/app.git",
	} {
		if got, err := cleanOrigin(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"ext::sh -c evil", "persistent-https://host/app.git", "s3://bucket/app.git"} {
		if _, err := cleanOrigin(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// G9-6: through a launch, an origin with a token lands in the clone without
// it.
func TestLaunchStripsCredentialsFromTheClonesOrigin(t *testing.T) {
	e := newEnv(t)
	e.git(e.repo, "remote", "set-url", "origin", "https://user:ghp_planted0123456789012345678901234@example.invalid/acme/app.git")
	e.git(e.repo, "config", "remote.origin.pushurl", e.origin)
	p := e.launch("checkout-engineer-1", issueIdent)
	if got := e.git(p.Clone, "remote", "get-url", "origin"); got != "https://example.invalid/acme/app.git" {
		t.Fatal("origin:", got)
	}
	config, _ := os.ReadFile(filepath.Join(p.Clone, ".git", "config"))
	if strings.Contains(string(config), "ghp_") || strings.Contains(string(config), "user:") {
		t.Fatal("the clone's config carries the credential")
	}
}

// G9-9: the agent's MCP tools are its session's own. The task-board writes
// (as the owner's Linear profile) are denied unless the launch opts in;
// confirming is never the agent's.
func TestAgentSettingsDenyTheOwnersWriteTools(t *testing.T) {
	deny := func(writes bool) string {
		s := AgentSettings(Settings{}, "/owner", "/clone", "/cache", "", nil, writes)
		return strings.Join(s["permissions"].(map[string]any)["deny"].([]string), " ")
	}
	off, on := deny(false), deny(true)
	for _, tool := range AgentLinearWriteTools {
		if !strings.Contains(off, "mcp__clawbridge__"+tool+" ") && !strings.HasSuffix(off, "mcp__clawbridge__"+tool) {
			t.Fatal("not denied:", tool)
		}
		if strings.Contains(on, "mcp__clawbridge__"+tool) {
			t.Fatal("denied with --linear-writes:", tool)
		}
	}
	for _, d := range []string{off, on} {
		if !strings.Contains(d, "mcp__clawbridge__assignment_confirm") {
			t.Fatal("assignment_confirm allowed")
		}
	}
}

// G9-7, G9-14: before the fetch's upload-pack runs in the clone, a clone
// whose .git the agent made unsafe is refused, with nothing fetched: a
// hooks path, a file-system monitor or an include in its config (however
// it's cased), borrowed objects, or a .git that isn't a plain directory.
func TestFetchRefusesAnUnsafeClone(t *testing.T) {
	e := newEnv(t)
	p := e.launch("checkout-engineer-1", "FIX-1")
	e.commit(p.Clone, "work.txt")
	gitDir := filepath.Join(p.Clone, ".git")
	config := filepath.Join(gitDir, "config")
	clean, _ := os.ReadFile(config)
	appendConfig := func(text string) func() {
		return func() {
			if err := os.WriteFile(config, append(append([]byte{}, clean...), text...), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, c := range map[string]struct {
		break_, restore func()
		why             string
	}{
		"hooks path": {appendConfig("[core]\n\thooksPath = /tmp/evil\n"), appendConfig(""), "core.hookspath"},
		"fsmonitor":  {appendConfig("[Core]\n\tFSMonitor = /tmp/evil\n"), appendConfig(""), "core.fsmonitor"},
		"include":    {appendConfig("[include]\n\tpath = /tmp/evil\n"), appendConfig(""), "include.path"},
		"include if": {appendConfig("[includeIf \"gitdir:/\"]\n\tpath = /tmp/evil\n"), appendConfig(""), "includeif."},
		"alternates": {func() {
			_ = os.WriteFile(filepath.Join(gitDir, "objects", "info", "alternates"), []byte(e.repo+"/.git/objects\n"), 0o644)
		}, func() { _ = os.Remove(filepath.Join(gitDir, "objects", "info", "alternates")) }, "alternates"},
		"commondir": {func() { _ = os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(e.repo+"/.git\n"), 0o644) }, func() { _ = os.Remove(filepath.Join(gitDir, "commondir")) }, "commondir"},
		"linked refs": {func() {
			_ = os.Rename(filepath.Join(gitDir, "refs"), filepath.Join(gitDir, "refs-real"))
			_ = os.Symlink(filepath.Join(gitDir, "refs-real"), filepath.Join(gitDir, "refs"))
		}, func() {
			_ = os.Remove(filepath.Join(gitDir, "refs"))
			_ = os.Rename(filepath.Join(gitDir, "refs-real"), filepath.Join(gitDir, "refs"))
		}, "has an refs entry"},
		"linked objects": {func() {
			_ = os.Rename(filepath.Join(gitDir, "objects"), filepath.Join(gitDir, "objects-real"))
			_ = os.Symlink(filepath.Join(gitDir, "objects-real"), filepath.Join(gitDir, "objects"))
		}, func() {
			_ = os.Remove(filepath.Join(gitDir, "objects"))
			_ = os.Rename(filepath.Join(gitDir, "objects-real"), filepath.Join(gitDir, "objects"))
		}, "has an objects entry"},
		"gitfile": {func() {
			_ = os.Rename(gitDir, gitDir+"-real")
			_ = os.WriteFile(gitDir, []byte("gitdir: "+gitDir+"-real\n"), 0o644)
		}, func() {
			_ = os.Remove(gitDir)
			_ = os.Rename(gitDir+"-real", gitDir)
		}, ".git is not a plain directory"},
	} {
		c.break_()
		got, err := e.r.Fetch("checkout-engineer-1")
		c.restore()
		if err == nil || got.Fetched || !strings.Contains(err.Error(), c.why) {
			t.Fatalf("%s: %v %v", name, got, err)
		}
		if _, err := execGit(e.repo, "rev-parse", "--verify", "--quiet", "refs/clawbridge-runner/checkout-engineer-1"); err == nil {
			t.Fatal(name, "left a quarantine ref")
		}
	}
	// Restored, the same clone fetches.
	if got, err := e.r.Fetch("checkout-engineer-1"); err != nil || !got.Fetched {
		t.Fatal(got, err)
	}
}

// G9-8: a token that only a merge commit adds, in neither parent, is
// caught.
func TestFetchScansAMergesOwnChanges(t *testing.T) {
	e := newEnv(t)
	p := e.launch("checkout-engineer-1", "FIX-1")
	e.commit(p.Clone, "work.txt")
	e.git(p.Clone, "checkout", "-q", "-b", "side", "HEAD~1")
	e.commit(p.Clone, "side.txt")
	e.git(p.Clone, "checkout", "-q", "agent/fix-1")
	e.git(p.Clone, "merge", "-q", "--no-ff", "--no-commit", "side")
	secret := "ghp_" + strings.Repeat("M3r6", 9)
	if err := os.WriteFile(filepath.Join(p.Clone, "merged.txt"), []byte(secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git(p.Clone, "add", "merged.txt")
	e.git(p.Clone, "commit", "-q", "-m", "merge side")
	got, err := e.r.Fetch("checkout-engineer-1")
	if err == nil || got.Fetched || len(got.Hits) != 1 || got.Hits[0].File != "merged.txt" {
		t.Fatal("a token only a merge adds was fetched:", got, err)
	}
}

// G9-14: the clone's .git is never offered to the file tools or Bash.
func TestAgentSettingsDenyTheClonesGitDir(t *testing.T) {
	deny := AgentSettings(Settings{}, "/owner", "/runs/s", "/runs/s.cache", "", nil, false)["permissions"].(map[string]any)["deny"].([]string)
	joined := strings.Join(deny, "\n")
	if !strings.Contains(joined, "Edit(//runs/s/.git/**)") || !strings.Contains(joined, "Bash(* .git/*)") {
		t.Fatal(deny)
	}
}

// G9-5: a launch whose session sees the shell's environment fails its
// self-test, and is stopped; the MCP child gets the owner's ClawBridge
// settings through its config.
func TestLaunchProvesTheEnvironmentIsCleared(t *testing.T) {
	e := newEnv(t)
	e.r.Environ = func() []string { return append(os.Environ(), "CLAWBRIDGE_HOME=/owner/home", "CLAWBRIDGE_DEVICE_KEY=k") }
	p := e.launch("checkout-engineer-1", "FIX-1")
	var mcp struct {
		MCPServers map[string]struct{ Env map[string]string } `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(p.MCPFile)
	if err := json.Unmarshal(raw, &mcp); err != nil || mcp.MCPServers["clawbridge"].Env["CLAWBRIDGE_HOME"] != "/owner/home" || mcp.MCPServers["clawbridge"].Env["CLAWBRIDGE_DEVICE_KEY"] != "" {
		t.Fatal("the MCP child's environment:", string(raw), err)
	}
	e.envLeak = true
	_, err := e.r.Launch(e.spec("checkout-engineer-2", "FIX-2"))
	if err == nil || !strings.Contains(err.Error(), "a variable from the shell that started it") {
		t.Fatal("a session that saw the shell's environment was not stopped:", err)
	}
}

// G9-7: the fetch checks every object it takes from the clone.
func TestFetchChecksObjects(t *testing.T) {
	e := newEnv(t)
	p := e.launch("checkout-engineer-1", "FIX-1")
	e.commit(p.Clone, "work.txt")
	plain, fsck := e.r.Git, false
	e.r.Git = func(dir string, args ...string) (string, error) {
		for i, a := range args {
			if a == "fetch" && i >= 2 && args[i-2] == "-c" && args[i-1] == "transfer.fsckObjects=true" {
				fsck = true
			}
		}
		return plain(dir, args...)
	}
	if got, err := e.r.Fetch("checkout-engineer-1"); err != nil || !got.Fetched || !fsck {
		t.Fatal(got, err, fsck)
	}
}
