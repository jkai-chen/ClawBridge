package runner

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

type env struct {
	t      *testing.T
	r      *Runner
	origin string
	repo   string
	opens  []opened
	now    time.Time
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
	e.r = &Runner{StateDir: filepath.Join(home, ".clawbridge", "runner"), Home: home, Git: execGit, Now: func() time.Time { return e.now }, Terminal: func(owner, title, command string) error {
		e.opens = append(e.opens, opened{owner, title, command})
		return nil
	}, ClaudeVersion: func() (string, error) { return TestedClaude + " (Claude Code)", nil }, ManagedSettings: filepath.Join(root, "no-managed-settings.json")}
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
	if !strings.HasPrefix(p.Fetch, "git fetch '"+p.Clone+"' agent/fix-12:agent/fix-12") {
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
	// F1, F3, F4: the command changes into the clone, loads no user,
	// project or local settings, and starts in the default mode.
	for _, want := range []string{"cd '" + p.Clone + "' && exec claude --setting-sources '' --permission-mode default --settings '" + p.SettingsFile + "'", "--mcp-config '" + p.MCPFile + "'", "--strict-mcp-config", issueIdent, issueUUID, "untrusted data"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--model") || strings.Contains(cmd, "bypass") || strings.Contains(cmd, "dangerously") {
		t.Fatal("command:", cmd)
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
		Filesystem                                           struct{ DenyRead, AllowWrite []string }
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
	if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"model":"claude-opus-5-5","allowed_domains":["proxy.golang.org"]}`)); err != nil {
		t.Fatal(err)
	}
	p := e.launch("checkout-engineer-1", issueIdent)
	if !strings.Contains(p.Command, "--model 'claude-opus-5-5'") {
		t.Fatal(p.Command)
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
	for _, k := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "npm_config_cache", "XDG_CACHE_HOME"} {
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
	if len(s.Sandbox.Filesystem.DenyRead) == 0 {
		t.Fatal("no denied paths")
	}
	for _, path := range s.Sandbox.Filesystem.DenyRead {
		if !contains(s.Permissions.Deny, "Read("+path+"/**)") || !contains(s.Permissions.Deny, "Edit("+path+"/**)") {
			t.Fatal("the file tools can reach", path)
		}
	}
	for _, path := range []string{"~/.ssh", "~/.config/gh", "~/.clawbridge", "~/Library/Keychains"} {
		if !contains(s.Sandbox.Filesystem.DenyRead, path) {
			t.Fatal("not denied:", path)
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
	e.r.Terminal = func(string, string, string) error { return errors.New("orca is not running") }
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
