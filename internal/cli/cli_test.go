package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/faketool"
)

func isolated(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"DB", "WORKSPACE", "PROJECT", "SESSION", "INSTANCE", "CONFIG", "MODE", "SERVER",
		"AUDIENCE", "ACCOUNT", "IMPERSONATE_SERVICE_ACCOUNT", "PRINCIPAL", "CACHE",
		"CREDENTIAL_SOURCE", "CREDENTIALS_FILE", "OAUTH_CLIENT_ID", "OAUTH_CLIENT_SECRET",
		"OAUTH_DEVICE_CLIENT_ID", "OAUTH_DEVICE_CLIENT_SECRET", "OAUTH_KEYRING_SERVICE",
	} {
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

func TestRepoCannotInjectCloudCredentialRouting(t *testing.T) {
	dir := isolated(t)
	git(t, "init", "-q")
	home := filepath.Join(dir, "state")
	if e := os.MkdirAll(home, 0700); e != nil {
		t.Fatal(e)
	}
	trusted := "mode='cloud'\nserver='https://trusted.example'\naudience='trusted-audience'\naccount='human@example.test'\nimpersonate_service_account='service@example.test'\nprincipal='service@example.test'\ncache='trusted-cache.db'\n"
	if e := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0600); e != nil {
		t.Fatal(e)
	}
	untrusted := "workspace='repo-w'\nproject='repo-p'\nmode='cloud'\nserver='https://evil.example'\naudience='evil-audience'\naccount='evil@example.test'\nimpersonate_service_account='evil@example.test'\nprincipal='evil@example.test'\ncache='evil.db'\ncredential_source='oauth'\noauth_client_id='evil-client'\noauth_client_secret='synthetic-repo-browser-credential'\noauth_device_client_id='evil-device-client'\noauth_device_client_secret='synthetic-repo-device-credential'\n"
	if e := os.WriteFile(filepath.Join(dir, ".clawbridge.toml"), []byte(untrusted), 0600); e != nil {
		t.Fatal(e)
	}
	c, _, e := resolveConfig(context.Background(), Config{})
	if e != nil {
		t.Fatal(e)
	}
	if c.Server != "https://trusted.example" || c.Audience != "trusted-audience" || c.Account != "human@example.test" || c.ImpersonateServiceAccount != "service@example.test" || c.Principal != "service@example.test" || c.Cache != filepath.Join(home, "trusted-cache.db") {
		t.Fatalf("repository redirected credential routing: %#v", c)
	}
	if c.CredentialSource != "" || c.OAuthClientID != "" || c.OAuthClientSecret != "" ||
		c.OAuthDeviceClientID != "" || c.OAuthDeviceClientSecret != "" {
		t.Fatalf("repository injected OAuth routing or installed-app credentials: %#v", c)
	}
	if c.Workspace != "repo-w" || c.Project != "repo-p" {
		t.Fatal("repository scope selection lost")
	}
	t.Setenv("CLAWBRIDGE_AUDIENCE", "env-audience")
	c, _, e = resolveConfig(context.Background(), Config{Audience: "flag-audience"})
	if e != nil || c.Audience != "flag-audience" {
		t.Fatal("flag/env credential precedence failed", e)
	}
}

func TestRepositoryModeCannotSwitchTrustedAuthority(t *testing.T) {
	dir := isolated(t)
	git(t, "init", "-q")
	home := filepath.Join(dir, "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, "local.db")
	trusted := "mode='local'\nworkspace='trusted-workspace'\ndb='" + dbPath + "'\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0600); err != nil {
		t.Fatal(err)
	}
	repo := "mode='cloud'\nserver='https://attacker.example.test'\naudience='attacker-audience'\ncredential_source='oauth'\noauth_client_id='attacker-client'\noauth_client_secret='synthetic-repo-browser-credential'\noauth_device_client_id='attacker-device-client'\noauth_device_client_secret='synthetic-repo-device-credential'\nworkspace='repo-workspace'\nproject='repo-project'\n"
	if err := os.WriteFile(filepath.Join(dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	config, _, err := resolveConfig(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != "local" {
		t.Fatalf("repository switched trusted authority mode to %q", config.Mode)
	}
	if config.Workspace != "repo-workspace" || config.Project != "repo-project" {
		t.Fatalf("repository scope selection was lost: %#v", config)
	}
	if config.Server != "" || config.Audience != "" || config.CredentialSource != "" ||
		config.OAuthClientID != "" || config.OAuthClientSecret != "" ||
		config.OAuthDeviceClientID != "" || config.OAuthDeviceClientSecret != "" {
		t.Fatalf("repository injected trusted cloud routing: %#v", config)
	}

	invoke(t, "setup", "--local", "--workspace", "review-workspace")
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mode = 'local'") && !strings.Contains(string(raw), `mode = "local"`) {
		t.Fatalf("trusted local mode was not preserved: %s", raw)
	}
	for _, injected := range []string{
		"attacker.example.test", "attacker-audience", "attacker-client",
		"synthetic-repo-browser-credential", "attacker-device-client",
		"synthetic-repo-device-credential", "credential_source",
	} {
		if strings.Contains(string(raw), injected) {
			t.Fatalf("repository injected %q into trusted configuration: %s", injected, raw)
		}
	}
}

func TestOAuthDeviceCredentialFlagsAndEnvironmentRemainTrusted(t *testing.T) {
	dir := isolated(t)
	home := filepath.Join(dir, "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	trusted := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_ID", "browser-client.apps.googleusercontent.com")
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "synthetic-env-browser-credential")
	t.Setenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID", "device-client.apps.googleusercontent.com")
	t.Setenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "synthetic-env-device-credential")
	flags, rest, err := extractGlobals([]string{
		"--oauth-device-client-secret", "synthetic-flag-device-credential",
		"login", "--device",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rest, " ") != "login --device" {
		t.Fatalf("credential flag leaked into command arguments: %#v", rest)
	}
	config, _, err := resolveLoginConfig(context.Background(), flags, true)
	if err != nil {
		t.Fatal(err)
	}
	if config.OAuthDeviceClientID != "device-client.apps.googleusercontent.com" ||
		config.OAuthDeviceClientSecret != "synthetic-flag-device-credential" ||
		config.OAuthClientSecret != "synthetic-env-browser-credential" {
		t.Fatal("OAuth credential precedence failed")
	}
}

func TestServeDoesNotReadConfigOrOpenLocalDatabase(t *testing.T) {
	dir := isolated(t)
	path := filepath.Join(dir, "bad.toml")
	if e := os.WriteFile(path, []byte("invalid = ["), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("CLAWBRIDGE_CONFIG", path)
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "synthetic-browser")
	t.Setenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "synthetic-device")
	var out, diagnostics bytes.Buffer
	e := Run(context.Background(), []string{"serve", "--audience", "desktop-client.apps.googleusercontent.com,device-client.apps.googleusercontent.com", "--oauth-client-id", "desktop-client.apps.googleusercontent.com", "--oauth-device-client-id", "device-client.apps.googleusercontent.com", "--bootstrap-owner", "owner@example.test"}, &out, &diagnostics)
	if e == nil || !strings.Contains(e.Error(), "Postgres connection") {
		t.Fatal("serve did not delegate before config resolution", e)
	}
	if _, e = os.Stat(filepath.Join(dir, "state", "local.db")); !os.IsNotExist(e) {
		t.Fatal("serve opened a local authority")
	}
}
func TestCloudCLIAndExplicitSyncUseRemoteOnly(t *testing.T) {
	dir := isolated(t)
	faketool.Install(t, dir, "gcloud", "print:synthetic-cli-token")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	principal := "service@example.test"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-cli-token" {
			t.Error("missing synthetic token")
		}
		var req struct {
			Operation, Workspace, Project, Session string
			Args                                   map[string]any
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		data := map[string]any{}
		switch req.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws-cloud", "project_id": "prj-cloud"}
		case "whoami":
			data = map[string]any{"principal": principal, "verified": true, "storage_mode": "cloud"}
		case "snapshot":
			data = map[string]any{"workspace": map[string]any{"id": "ws-cloud", "name": "w"}, "project": map[string]any{"id": "prj-cloud", "workspace_id": "ws-cloud", "slug": "p"}, "session": map[string]any{"id": "ses-cloud", "name": "a", "project_id": "prj-cloud", "instance": "cloud-inst", "owner_principal": principal}, "principal": principal, "verified": true, "storage_mode": "cloud", "identity_provenance": "google-id-token", "snapshot_at": now, "sessions": []any{}, "messages": []any{}, "sent_messages": []any{}, "events": []any{}}
		default:
			t.Errorf("unexpected cloud operation %s", req.Operation)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"source": "server", "data": data})
	}))
	defer host.Close()
	for k, v := range map[string]string{"MODE": "cloud", "SERVER": host.URL, "AUDIENCE": "test-audience", "ACCOUNT": "human@example.test", "IMPERSONATE_SERVICE_ACCOUNT": principal, "PRINCIPAL": principal, "CACHE": filepath.Join(dir, "mirror.db"), "WORKSPACE": "w", "PROJECT": "p", "SESSION": "a", "INSTANCE": "cloud-inst"} {
		t.Setenv("CLAWBRIDGE_"+k, v)
	}
	run := func(command string) map[string]any {
		t.Helper()
		var out, diag bytes.Buffer
		if e := Run(context.Background(), []string{command}, &out, &diag); e != nil {
			t.Fatal(e)
		}
		var value map[string]any
		if e := json.Unmarshal(out.Bytes(), &value); e != nil {
			t.Fatal(e)
		}
		if value["source"] != "server" {
			t.Fatal("cloud command did not report server")
		}
		return value["data"].(map[string]any)
	}
	if run("whoami")["principal"] != principal {
		t.Fatal("wrong remote identity")
	}
	if run("sync")["cache_updated"] != true {
		t.Fatal("explicit sync did not publish mirror")
	}
	if _, e := os.Stat(filepath.Join(dir, "mirror.db")); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(dir, "state", "local.db")); !os.IsNotExist(e) {
		t.Fatal("cloud command opened writable SQLite authority")
	}
}

func TestCloudInstallerRequiresExplicitSourceAndNeverForwardsSecret(t *testing.T) {
	dir := isolated(t)
	command := faketool.Install(t, dir, "clawbridge", "exit:0")
	configPath := filepath.Join(dir, "mcp.json")
	base := []string{
		"mcp", "install", "--agent", "claude-code", "--config", configPath,
		"--command", command, "--mode", "cloud", "--server", "https://server.example.test",
		"--audience", "desktop-client.apps.googleusercontent.com", "--principal", "alice@example.test",
		"--cache", filepath.Join(dir, "cache.db"), "--workspace", "w",
		"--project", "p", "--session", "alice",
	}
	var out, diagnostics bytes.Buffer
	err := Run(context.Background(), base, &out, &diagnostics)
	if err == nil || !strings.Contains(err.Error(), "explicit credential source") {
		t.Fatalf("cloud installer inferred a credential source: %v", err)
	}
	oauthArgs := append([]string{}, base...)
	oauthArgs = append(oauthArgs,
		"--credential-source", "oauth",
		"--oauth-client-id", "desktop-client.apps.googleusercontent.com",
		"--oauth-client-secret", "synthetic-browser-client-credential",
		"--oauth-device-client-id", "device-client.apps.googleusercontent.com",
		"--oauth-device-client-secret", "synthetic-device-client-credential",
		"--oauth-keyring-service", "review-keyring",
		"--dry-run",
	)
	originalStdout := os.Stdout
	readOutput, writeOutput, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	os.Stdout = writeOutput
	t.Cleanup(func() { os.Stdout = originalStdout })
	if err = Run(context.Background(), oauthArgs, &out, &diagnostics); err != nil {
		writeOutput.Close()
		readOutput.Close()
		t.Fatal(err)
	}
	if err = writeOutput.Close(); err != nil {
		readOutput.Close()
		t.Fatal(err)
	}
	installOutput, err := io.ReadAll(readOutput)
	if err != nil {
		readOutput.Close()
		t.Fatal(err)
	}
	readOutput.Close()
	for _, forbidden := range []string{
		"oauth_client_secret", "oauth-client-secret", "synthetic-browser-client-credential",
		"oauth_device_client_secret", "oauth-device-client-secret", "synthetic-device-client-credential",
		"browser_client_secret", "device_client_secret", "refresh_token", "id_token", "access_token",
	} {
		if bytes.Contains(installOutput, []byte(forbidden)) {
			t.Fatalf("installer output contains forbidden credential field %q: %s", forbidden, installOutput)
		}
	}
	raw, err := os.ReadFile(configPath)
	if err == nil {
		t.Fatalf("default-output installer unexpectedly touched stdout fixture: %s", raw)
	}
	for _, forbidden := range []string{"oauth_client_secret", "oauth-client-secret", "refresh_token", "id_token", "access_token"} {
		if strings.Contains(diagnostics.String(), forbidden) {
			t.Fatalf("installer diagnostics contains forbidden credential field %q", forbidden)
		}
	}
}
func TestCLIProjectRoleRevokeReachesHostedOperation(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	for _, args := range [][]string{{"project", "role-revoke", "bob@example.com"}, {"project", "role", "revoke", "bob@example.com"}} {
		var out, errOut bytes.Buffer
		e := Run(context.Background(), args, &out, &errOut)
		if e == nil || !strings.Contains(e.Error(), "hosted authority") || out.Len() != 0 {
			t.Fatalf("%v did not reach project_role_revoke: %v", args, e)
		}
	}
	rejectCLI(t, "project", "role-revoke")
	rejectCLI(t, "project", "role", "revoke", "bob@example.com", "member")
	rejectCLI(t, "project", "role", "drop", "bob@example.com")
}
