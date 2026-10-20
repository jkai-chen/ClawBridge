package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitAt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func writeScope(t *testing.T, path, workspace, project, db, mode string) {
	t.Helper()
	text := fmt.Sprintf("workspace = %q\nproject = %q\ndb = %q\nmode = %q\n", workspace, project, db, mode)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func testResolution(t *testing.T, parent *processFixture) {
	p := *parent
	p.t = t
	p.run("project", "create", "registered-remote", "--git-remote", "git@Example.invalid:Org/Repo.git")
	root := t.TempDir()
	gitAt(t, root, "init", "--quiet")
	gitAt(t, root, "remote", "add", "origin", "https://example.invalid/Org/Repo.git/")
	sub := filepath.Join(root, "sub", "directory")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	resolved := p
	resolved.scopeFromConfig = true
	resolved.dir = sub
	resolved.env = append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+filepath.Join(t.TempDir(), "isolated-state"))
	who := resolved.run("whoami")
	if stringField(t, object(t, who)["project"], "slug") != "registered-remote" {
		t.Fatalf("remote resolution chose wrong project: %v", who)
	}
	config := filepath.Join(root, ".clawbridge.toml")
	// With no trusted route, a repository-only cloud mode cannot manufacture one.
	// The permitted scope still resolves through the normal local default. A
	// non-loopback HTTP URL would fail if cloud validation were wrongly triggered.
	repoOnlyCloud := fmt.Sprintf("workspace = %q\nproject = %q\ndb = %q\nmode = \"cloud\"\nserver = \"http://attacker.example.test\"\naudience = \"attacker-audience\"\ncredential_source = \"oauth\"\noauth_client_id = \"attacker-client\"\n", "w", "p", p.db)
	if err := os.WriteFile(config, []byte(repoOnlyCloud), 0600); err != nil {
		t.Fatal(err)
	}
	who = resolved.run("whoami")
	if stringField(t, object(t, who)["project"], "slug") != "p" {
		t.Fatalf("repository-only cloud mode replaced the trusted local default: %v", who)
	}
	who = resolved.run("--workspace", "w", "--project", "other", "whoami")
	if stringField(t, object(t, who)["project"], "slug") != "other" {
		t.Fatalf("explicit flags did not override config")
	}
	withEnv := resolved
	withEnv.env = append(append([]string{}, resolved.env...), "CLAWBRIDGE_PROJECT=other")
	who = withEnv.run("whoami")
	if stringField(t, object(t, who)["project"], "slug") != "other" {
		t.Fatalf("environment did not override root config")
	}
	who = withEnv.run("--project", "p", "whoami")
	if stringField(t, object(t, who)["project"], "slug") != "p" {
		t.Fatalf("flags did not override environment")
	}
	// Repository mode and routing fields are untrusted and ignored. They cannot
	// switch the trusted local authority to cloud.
	repoCloud := "workspace = \"w\"\nproject = \"p\"\nmode = \"cloud\"\nserver = \"https://attacker.example.test\"\naudience = \"attacker-audience\"\naccount = \"attacker@example.test\"\nimpersonate_service_account = \"attacker@evil.example.test\"\nprincipal = \"attacker@example.test\"\ncache = \"attacker-cache.db\"\ncredential_source = \"oauth\"\noauth_client_id = \"attacker-client\"\n"
	if err := os.WriteFile(config, []byte(repoCloud), 0600); err != nil {
		t.Fatal(err)
	}
	who = resolved.run("whoami")
	if stringField(t, object(t, who)["project"], "slug") != "p" {
		t.Fatalf("repository mode switched trusted local authority: %v", who)
	}
	resolved.run("--workspace", "w", "--project", "p", "setup", "--local")

	// The same authority rule applies to an explicit trusted-home config. An
	// unsupported home mode must fail before mirror or local fallback.
	stateDir := strings.TrimPrefix(resolved.env[len(resolved.env)-1], "CLAWBRIDGE_HOME=")
	homeConfig := filepath.Join(stateDir, "config.toml")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeConfig, []byte("mode = \"offline-mirror\"\nworkspace = \"w\"\nproject = \"p\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved.fail("whoami")
	if err := os.Remove(homeConfig); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	gitAt(t, root, "remote", "set-url", "origin", "git@example.invalid:Org/repo.git")
	resolved.fail("whoami") // path case must not collapse into Org/Repo.
	gitAt(t, root, "remote", "set-url", "origin", "ssh://git@example.invalid/Org/Repo.git")
	p.run("--workspace", "another-workspace", "setup", "--local")
	p.run("--workspace", "another-workspace", "project", "create", "same-remote", "--git-remote", "https://example.invalid/Org/Repo")
	resolved.fail("whoami") // two owned workspaces match the normalized remote.
	who = resolved.run("--workspace", "w", "whoami")
	if stringField(t, object(t, who)["project"], "slug") != "registered-remote" {
		t.Fatal("explicit workspace failed to disambiguate")
	}
	gitAt(t, root, "remote", "remove", "origin")
	resolved.fail("whoami")
	// An unborn linked worktree needs no commits or index operations.
	worktree := filepath.Join(t.TempDir(), "linked")
	gitAt(t, root, "worktree", "add", "--orphan", "-b", "integration-only", worktree)
	writeScope(t, filepath.Join(worktree, ".clawbridge.toml"), "w", "p", p.db, "local")
	nested := filepath.Join(worktree, "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	linked := resolved
	linked.dir = nested
	who = linked.run("whoami")
	if stringField(t, object(t, who)["project"], "slug") != "p" {
		t.Fatal("linked checkout root config not found")
	}
}
