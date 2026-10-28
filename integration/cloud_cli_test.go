package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

func TestPostgresCloudCLITrustedRoutingAndOffline(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	f.send("p", "alice", "bob", "CLI-cloud-mail", "cli-cloud-mail")
	endpoint := httptest.NewServer(server.NewHandler(f.store, fixtureVerifier()))
	defer endpoint.Close()
	var redirected atomic.Int64
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(500) }))
	defer hostile.Close()
	p := newProcessFixture(t)
	gitAt(t, p.dir, "init", "--quiet")
	home := filepath.Join(p.dir, "trusted-state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(p.dir, "cloud-mirror.db")
	config := fmt.Sprintf("mode = 'cloud'\nserver = %q\naudience = 'https://trusted-audience.example.test'\naccount = 'human@example.test'\nimpersonate_service_account = 'agent@project.iam.gserviceaccount.com'\nprincipal = %q\ncache = %q\nworkspace = %q\nproject = 'p'\nsession = 'bob'\ninstance = 'bob-instance'\n", endpoint.URL, pgPrincipal("bob"), cache, f.w)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	repo := fmt.Sprintf("workspace = %q\nproject = 'p'\nmode = 'cloud'\nserver = %q\naudience = 'https://attacker.example.test'\naccount = 'attacker@example.test'\nimpersonate_service_account = 'attacker@evil.iam.gserviceaccount.com'\nprincipal = 'attacker@example.test'\ncache = 'attacker.db'\n", f.w, hostile.URL)
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	fakeDir := filepath.Join(p.dir, "fake-bin")
	if err := os.MkdirAll(fakeDir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(p.dir, "gcloud-args.log")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$CLAWBRIDGE_TEST_GCLOUD_LOG\"\nprintf 'synthetic-bob\\n'\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "gcloud"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+home, "PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"), "CLAWBRIDGE_TEST_GCLOUD_LOG="+log)
	run := func(wantError bool, args ...string) bus.Result {
		t.Helper()
		cmd := exec.Command(p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = env
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		out, err := cmd.Output()
		if wantError {
			if err == nil {
				t.Fatalf("CLI cloud %v unexpectedly succeeded: %s", args, out)
			}
			return bus.Result{}
		}
		if err != nil {
			t.Fatalf("CLI cloud %v: %v stderr=%s stdout=%s", args, err, errOut.String(), out)
		}
		var result bus.Result
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("CLI cloud output not Result: %s", out)
		}
		return result
	}
	who := run(false, "whoami")
	if who.Source != "server" || object(t, jsonData(t, who.Data))["principal"] != pgPrincipal("bob") {
		t.Fatalf("CLI trusted identity mismatch: %+v", who)
	}
	if redirected.Load() != 0 {
		t.Fatal("repository redirected authenticated request")
	}
	args, _ := os.ReadFile(log)
	if strings.Contains(string(args), "attacker") || !strings.Contains(string(args), "https://trusted-audience.example.test") {
		t.Fatalf("repository injected credential routing: %s", args)
	}
	synced := run(false, "sync")
	if synced.Source != "server" {
		t.Fatal("CLI sync source missing")
	}
	endpoint.Close()
	offline := run(false, "inbox")
	if offline.Source != "offline-mirror" {
		t.Fatalf("CLI outage source=%q", offline.Source)
	}
	inbox := pageItems(t, "messages", jsonData(t, offline.Data))
	if len(inbox) != 1 || object(t, inbox[0])["body"] != "CLI-cloud-mail" {
		t.Fatalf("CLI mirror inbox %v", inbox)
	}
	// With the client outbox off, a write fails fast and never falls back
	// to a local authority. (With it on, the send would wait in the outbox:
	// internal/cli's TestOutboxCommands.)
	env = append(env, "CLAWBRIDGE_CLIENT_OUTBOX=off")
	run(true, "send", "alice", "do-not-queue", "--operation-id", "cli-offline-write")
	if _, err := os.Stat(filepath.Join(home, "local.db")); !os.IsNotExist(err) {
		t.Fatal("cloud CLI silently opened writable local authority")
	}
	// Installer cloud flags must preserve every trusted route and never infer local.
	installPath := filepath.Join(p.dir, "isolated-mcp.json")
	cmd := exec.Command(p.binary, "mcp", "install", "--agent", "claude-code", "--config", installPath, "--command", p.binary, "--mode", "cloud", "--server", endpoint.URL, "--audience", "https://trusted-audience.example.test", "--credential-source", "gcloud", "--account", "human@example.test", "--impersonate-service-account", "agent@project.iam.gserviceaccount.com", "--principal", pgPrincipal("bob"), "--cache", cache, "--workspace", f.w, "--project", "p", "--session", "bob", "--instance", "bob-instance")
	cmd.Dir = p.dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cloud installer: %v %s", err, out)
	}
	installed, _ := os.ReadFile(installPath)
	for _, want := range []string{"--mode", "cloud", "--server", endpoint.URL, "--audience", "--credential-source", "gcloud", "--account", "--impersonate-service-account", "--principal", "--cache"} {
		if !bytes.Contains(installed, []byte(want)) {
			t.Errorf("cloud entry omitted %s: %s", want, installed)
		}
	}
}
