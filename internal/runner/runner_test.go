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

// newEnv makes an origin repository with one commit on main and a clone
// of it, the owner's checkout.
func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "checkout"), now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	e.git(root, "init", "--bare", "-b", "main", e.origin)
	e.git(root, "clone", e.origin, e.repo)
	if err := os.WriteFile(filepath.Join(e.repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git(e.repo, "add", "README.md")
	e.git(e.repo, "commit", "-m", "first")
	e.git(e.repo, "push", "origin", "main")
	e.git(e.repo, "remote", "set-head", "origin", "main")
	e.r = &Runner{StateDir: filepath.Join(root, "state"), Git: execGit, Now: func() time.Time { return e.now }, Terminal: func(dir, title, command string) error {
		e.opens = append(e.opens, opened{dir, title, command})
		return nil
	}}
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

func (e *env) spec(session, ident string) Spec {
	s, _ := e.r.LoadSettings()
	return Spec{Session: session, Instance: "inst-" + session, Repo: e.repo, IssueID: issueUUID, Identifier: ident, MCPEntry: map[string]any{"command": "/usr/local/bin/clawbridge", "args": []string{"mcp"}, "type": "stdio"}, Settings: s}
}

// A launch makes an agent/* worktree that cannot push, writes the
// session's files outside it, privately, and opens the terminal with the
// IDs-only prompt; the owner's checkout keeps pushing.
func TestLaunchPreparesAnUnpushableWorktree(t *testing.T) {
	e := newEnv(t)
	p, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent))
	if err != nil || !p.Opened || p.Branch != "agent/fix-12" || p.Base != "origin/main" {
		t.Fatal(p, err)
	}
	if got := e.git(p.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); got != "agent/fix-12" {
		t.Fatal(got)
	}
	// The agent's checkout has no push URL and no credential helper; the
	// owner's has its own.
	if got := e.git(p.Worktree, "config", "--get", "remote.origin.pushurl"); got != "DISABLED" {
		t.Fatal("push URL", got)
	}
	if _, err = execGit(e.repo, "config", "--get", "remote.origin.pushurl"); err == nil {
		t.Fatal("the owner's checkout lost its push URL")
	}
	if got, err := execGit(p.Worktree, "config", "--worktree", "--get", "credential.helper"); err != nil || got != "" {
		t.Fatal("the worktree does not reset the credential helpers", got, err)
	}
	if err = os.WriteFile(filepath.Join(p.Worktree, "work.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git(p.Worktree, "add", "work.txt")
	e.git(p.Worktree, "commit", "-m", "agent work")
	if _, err = execGit(p.Worktree, "push", "origin", "agent/fix-12"); err == nil {
		t.Fatal("the agent's worktree pushed")
	}
	if _, err = execGit(e.repo, "push", "--dry-run", "origin", "main"); err != nil {
		t.Fatal("the owner's checkout cannot push:", err)
	}
	// The session's files are outside the worktree, readable by the owner
	// only.
	for _, f := range []string{p.SettingsFile, p.MCPFile} {
		info, err := os.Stat(f)
		if err != nil || info.Mode().Perm() != 0o600 || strings.HasPrefix(f, p.Worktree) {
			t.Fatal(f, info, err)
		}
		dir, _ := os.Stat(filepath.Dir(f))
		if dir.Mode().Perm() != 0o700 {
			t.Fatal("session directory", dir.Mode())
		}
	}
	var mcp map[string]map[string]map[string]any
	raw, _ := os.ReadFile(p.MCPFile)
	if json.Unmarshal(raw, &mcp) != nil || mcp["mcpServers"]["clawbridge"]["command"] != "/usr/local/bin/clawbridge" {
		t.Fatal(string(raw))
	}
	if len(e.opens) != 1 || e.opens[0].dir != p.Worktree || e.opens[0].title != "checkout-engineer-1" {
		t.Fatal(e.opens)
	}
	cmd := e.opens[0].command
	for _, want := range []string{"claude --settings '" + p.SettingsFile + "'", "--mcp-config '" + p.MCPFile + "'", "--strict-mcp-config", issueIdent, issueUUID, "untrusted data"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--model") || strings.Contains(cmd, "bypass") || strings.Contains(cmd, "dangerously") {
		t.Fatal("command:", cmd)
	}
	launches, _ := e.r.Launches()
	if len(launches) != 1 || launches[0].Instance != "inst-checkout-engineer-1" || launches[0].Repo == "" {
		t.Fatal(launches)
	}
}

// The model is a setting; the settings never allow bypass, and the Bash
// sandbox denies the owner's credentials and limits the network.
func TestLaunchSettingsAndModel(t *testing.T) {
	e := newEnv(t)
	if err := writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"model":"claude-opus-5-5","allowed_domains":["proxy.golang.org"]}`)); err != nil {
		t.Fatal(err)
	}
	p, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent))
	if err != nil || !strings.Contains(p.Command, "--model 'claude-opus-5-5'") {
		t.Fatal(p, err)
	}
	var s struct {
		Permissions struct {
			DefaultMode, DisableBypassPermissionsMode string
			Deny                                      []string
		}
		Sandbox struct {
			Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
			Filesystem                                           struct{ DenyRead []string }
			Network                                              struct{ AllowedDomains []string }
		}
	}
	raw, _ := os.ReadFile(p.SettingsFile)
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Permissions.DefaultMode != "default" || s.Permissions.DisableBypassPermissionsMode != "disable" || !contains(s.Permissions.Deny, "Bash(git push *)") || !contains(s.Permissions.Deny, "Bash(gh *)") {
		t.Fatal("permissions", s.Permissions)
	}
	if !s.Sandbox.Enabled || !s.Sandbox.FailIfUnavailable || s.Sandbox.AllowUnsandboxedCommands {
		t.Fatal("sandbox", s.Sandbox)
	}
	for _, path := range []string{"~/.ssh", "~/.config/gh", "~/.clawbridge", "~/Library/Keychains"} {
		if !contains(s.Sandbox.Filesystem.DenyRead, path) {
			t.Fatal("not denied:", path)
		}
	}
	if len(s.Sandbox.Network.AllowedDomains) != 1 || s.Sandbox.Network.AllowedDomains[0] != "proxy.golang.org" {
		t.Fatal(s.Sandbox.Network)
	}
	if err = writePrivate(filepath.Join(e.r.StateDir, "config.json"), []byte(`{"modle":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = e.r.LoadSettings(); err == nil {
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

// A dry run changes nothing; a taken branch, the retained cap and a bad
// identifier refuse; a terminal Orca cannot open is reported with the
// command, and the launch stays recorded.
func TestLaunchRefusalsAndDryRun(t *testing.T) {
	e := newEnv(t)
	dry := e.spec("checkout-engineer-1", issueIdent)
	dry.DryRun = true
	if p, err := e.r.Launch(dry); err != nil || !p.DryRun || p.Command == "" {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(filepath.Join(e.r.StateDir, "worktrees")); err == nil || len(e.opens) != 0 {
		t.Fatal("a dry run made a worktree or opened a terminal")
	}
	if _, err := e.r.Launch(e.spec("checkout-engineer-1", "not an id")); err == nil {
		t.Fatal("a bad identifier accepted")
	}
	e.r.Terminal = func(string, string, string) error { return errors.New("orca is not running") }
	p, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent))
	if err != nil || p.Opened || !strings.Contains(p.Note, "orca is not running") {
		t.Fatal(p, err)
	}
	if _, err = e.r.Launch(e.spec("checkout-engineer-2", issueIdent)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("a taken branch accepted:", err)
	}
	capped := e.spec("checkout-engineer-3", "FIX-13")
	capped.Settings.MaxRetained = 1
	if _, err = e.r.Launch(capped); err == nil || !strings.Contains(err.Error(), "runner gc") {
		t.Fatal("the cap did not hold:", err)
	}
}

// gc keeps work in progress and removes what is finished: a branch merged
// by rebase, or one a week old with no commits. It previews first.
func TestGCRemovesOnlyFinishedWorktrees(t *testing.T) {
	e := newEnv(t)
	launch := func(session, ident string) Prepared {
		p, err := e.r.Launch(e.spec(session, ident))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	commit := func(p Prepared, file string) {
		if err := os.WriteFile(filepath.Join(p.Worktree, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.git(p.Worktree, "add", file)
		e.git(p.Worktree, "commit", "-m", file)
	}
	merged := launch("checkout-engineer-1", "FIX-1")
	commit(merged, "merged.txt")
	pending := launch("checkout-engineer-2", "FIX-2")
	commit(pending, "pending.txt")
	dirty := launch("checkout-engineer-3", "FIX-3")
	if err := os.WriteFile(filepath.Join(dirty.Worktree, "draft.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	idle := launch("checkout-engineer-4", "FIX-4")
	// The reviewer merges by rebase: the same patch lands under a new ID.
	e.git(e.repo, "cherry-pick", merged.Branch)
	e.git(e.repo, "commit", "--amend", "-m", "merged.txt (#1)")
	e.git(e.repo, "push", "origin", "main")
	e.git(e.repo, "fetch", "origin")
	byBranch := func(cs []Collected) map[string]Collected {
		m := map[string]Collected{}
		for _, c := range cs {
			m[c.Branch] = c
		}
		return m
	}
	preview, err := e.r.GC(false)
	got := byBranch(preview)
	if err != nil || got["agent/fix-1"].Action != "remove" || got["agent/fix-2"].Action != "keep" || got["agent/fix-3"].Action != "keep" || got["agent/fix-4"].Action != "keep" {
		t.Fatal(preview, err)
	}
	if got["agent/fix-3"].Reason != "has uncommitted changes" {
		t.Fatal("dirty", got["agent/fix-3"])
	}
	if _, err = os.Stat(merged.Worktree); err != nil {
		t.Fatal("a preview removed a worktree")
	}
	e.now = e.now.Add(retainNoCommits)
	done, err := e.r.GC(true)
	got = byBranch(done)
	if err != nil || !got["agent/fix-1"].Removed || !got["agent/fix-4"].Removed || got["agent/fix-2"].Removed || got["agent/fix-3"].Removed {
		t.Fatal(done, err)
	}
	if got["agent/fix-3"].Action != "keep" || got["agent/fix-3"].Reason != "has uncommitted changes" {
		t.Fatal("a dirty worktree was taken for finished", got["agent/fix-3"])
	}
	for _, p := range []Prepared{merged, idle} {
		if _, err = os.Stat(p.Worktree); err == nil {
			t.Fatal("not removed:", p.Worktree)
		}
		if _, err = execGit(e.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+p.Branch); err == nil {
			t.Fatal("branch kept:", p.Branch)
		}
		if _, err = os.Stat(filepath.Dir(p.SettingsFile)); err == nil {
			t.Fatal("session files kept:", p.Session)
		}
	}
	for _, p := range []Prepared{pending, dirty} {
		if _, err = os.Stat(p.Worktree); err != nil {
			t.Fatal("removed work in progress:", p.Worktree)
		}
	}
	if n, _ := e.r.Retained(); n != 2 {
		t.Fatal("retained", n)
	}
	if left, _ := e.r.Launches(); len(left) != 2 {
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

// A branch merged by fast-forward has nothing ahead of the base, but its
// tip moved since the launch: it is merged, not idle.
func TestGCSeesAFastForwardMerge(t *testing.T) {
	e := newEnv(t)
	p, err := e.r.Launch(e.spec("checkout-engineer-1", "FIX-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.Worktree, "ff.txt"), []byte("ff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git(p.Worktree, "add", "ff.txt")
	e.git(p.Worktree, "commit", "-m", "ff")
	e.git(e.repo, "merge", "--ff-only", p.Branch)
	e.git(e.repo, "push", "origin", "main")
	e.git(e.repo, "fetch", "origin")
	got, err := e.r.GC(true)
	if err != nil || len(got) != 1 || !got[0].Removed || got[0].Reason != "merged into origin/main" {
		t.Fatal(got, err)
	}
}

// A worktree that turns out dirty after the launch's own check is never
// started on; a removal is never forced.
func TestLaunchRefusesADirtyWorktreeAndRemovalIsNotForced(t *testing.T) {
	e := newEnv(t)
	plain := e.r.Git
	e.r.Git = func(dir string, args ...string) (string, error) {
		out, err := plain(dir, args...)
		if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			_ = os.WriteFile(filepath.Join(args[len(args)-2], "stray.txt"), []byte("x"), 0o644)
		}
		return out, err
	}
	if _, err := e.r.Launch(e.spec("checkout-engineer-1", issueIdent)); err == nil || !strings.Contains(err.Error(), "not clean") || len(e.opens) != 0 {
		t.Fatal("a dirty worktree was started on:", err)
	}
	launches, _ := e.r.Launches()
	if ok, msg := e.r.remove(launches[0], false, true); ok || msg == "" {
		t.Fatal("a dirty worktree was removed")
	}
}
