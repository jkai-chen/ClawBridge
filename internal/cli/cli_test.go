package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func isolated(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"DB", "WORKSPACE", "PROJECT", "SESSION", "INSTANCE", "CONFIG", "MODE"} {
		t.Setenv("CLAWBRIDGE_"+k, "")
	}
	t.Setenv("CLAWBRIDGE_HOME", filepath.Join(dir, "state"))
	t.Chdir(dir)
	return dir
}
func invoke(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	if e := Run(context.Background(), args, &out, &errOut); e != nil {
		t.Fatalf("%v: %v (%s)", args, e, errOut.String())
	}
	var result struct {
		Source string         `json:"source"`
		Data   map[string]any `json:"data"`
	}
	if e := json.Unmarshal(out.Bytes(), &result); e != nil {
		t.Fatal(e, out.String())
	}
	if result.Source != "local-authority" {
		t.Fatal("missing source")
	}
	return result.Data
}
func rejectCLI(t *testing.T, args ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if e := Run(context.Background(), args, &out, &errOut); e == nil {
		t.Fatalf("%v unexpectedly succeeded", args)
	}
	if out.Len() != 0 {
		t.Fatalf("failure wrote success stdout: %s", out.String())
	}
}
func TestCLIExchangeAndResume(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "a-one")
	invoke(t, "register", "b", "--instance", "b-one")
	sent := invoke(t, "--session", "a", "send", "b", "hello from cli", "--instance", "a-one", "--operation-id", "one")
	inbox := invoke(t, "inbox", "--session=b", "--instance=b-one", "--limit=1")
	if len(inbox["messages"].([]any)) != 1 {
		t.Fatal("missing delivery")
	}
	invoke(t, "--session", "b", "--instance", "b-one", "mark-read", sent["id"].(string))
	reply := invoke(t, "reply", sent["id"].(string), "reply from cli", "--session", "b", "--instance", "b-one", "--operation-id", "reply")
	if reply["thread_id"] != sent["id"] || reply["reply_to"] != sent["id"] {
		t.Fatal("lost reply/thread distinction")
	}
	resumed := invoke(t, "register", "b", "--resume", "--instance", "b-one")
	if resumed["instance"] == "b-one" {
		t.Fatal("resume did not fence")
	}
	rejectCLI(t, "inbox", "--session", "b", "--instance", "b-one")
	invoke(t, "whoami", "--session", "b", "--instance", resumed["instance"].(string))
	rejectCLI(t, "send", "b", "missing retry ID", "--session", "a", "--instance", "a-one")
	rejectCLI(t, "setup")
}
func git(t *testing.T, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	if out, e := c.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
}
func TestCheckoutConfigAndExplicitPrecedence(t *testing.T) {
	dir := isolated(t)
	git(t, "init", "-q")
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "configured")
	invoke(t, "project", "create", "explicit")
	if e := os.WriteFile(filepath.Join(dir, ".clawbridge.toml"), []byte("workspace = 'w'\nproject = 'configured'\nmode = 'local'\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(dir, "nested"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Chdir(filepath.Join(dir, "nested"))
	info := invoke(t, "whoami")
	if info["project"].(map[string]any)["slug"] != "configured" {
		t.Fatal("checkout-root config not found")
	}
	t.Setenv("CLAWBRIDGE_PROJECT", "missing-environment-project")
	info = invoke(t, "whoami", "--project", "explicit")
	if info["project"].(map[string]any)["slug"] != "explicit" {
		t.Fatal("explicit project lost precedence")
	}
}
func TestRemoteResolutionAmbiguityAndCase(t *testing.T) {
	isolated(t)
	git(t, "init", "-q")
	git(t, "remote", "add", "origin", "git@EXAMPLE.com:Org/Repo.git")
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "one", "--git-remote", "https://example.com/Org/Repo.git")
	info := invoke(t, "whoami")
	if info["project"].(map[string]any)["slug"] != "one" {
		t.Fatal("remote did not resolve")
	}
	invoke(t, "project", "create", "two", "--git-remote", "ssh://git@example.com/Org/Repo.git")
	rejectCLI(t, "whoami")
	invoke(t, "whoami", "--project", "two")
	git(t, "remote", "set-url", "origin", "git@example.com:org/repo.git")
	rejectCLI(t, "whoami")
}
func TestExplicitConfigIsolationAndPreservation(t *testing.T) {
	dir := isolated(t)
	path := filepath.Join(dir, "explicit.toml")
	t.Setenv("CLAWBRIDGE_CONFIG", path)
	invoke(t, "setup", "--local", "--workspace", "w")
	b, e := os.ReadFile(path)
	if e != nil || !strings.Contains(string(b), "local") {
		t.Fatal(e, string(b))
	}
	b = append(b, []byte("\n[unrelated]\nkeep = 'untouched'\n")...)
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	invoke(t, "setup", "--local", "--workspace", "w")
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "untouched") {
		t.Fatal("setup lost unrelated keys")
	}
	t.Setenv("CLAWBRIDGE_MODE", "cloud")
	rejectCLI(t, "setup", "--local")
}
func TestFlagLookingBodyTerminator(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "i")
	sent := invoke(t, "--session", "a", "--instance", "i", "send", "--operation-id", "body", "--", "a", "--project")
	if sent["body"] != "--project" {
		t.Fatal("flag-looking body corrupted")
	}
}

func TestLiteralCommandWordsAreMessageArguments(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "i")
	invoke(t, "register", "mcp", "--instance", "m")
	sent := invoke(t, "--session", "a", "--instance", "i", "send", "mcp", "install", "--operation-id", "literal")
	if sent["address"] != "mcp" || sent["body"] != "install" {
		t.Fatal("command words were not preserved")
	}
}
