package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
)

func TestPostgresCloudCLITrustedRoutingAndOffline(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	f.send("p", "alice", "bob", "CLI-cloud-mail", "cli-cloud-mail")
	// The trusted route signs with bob's device key (the only sign-in since
	// v1.0); the endpoint records every Authorization it is sent.
	var handler http.Handler
	var authorizations []string
	var authMu sync.Mutex
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		authMu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	handler = server.NewHandlerWithOptions(f.store, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: endpoint.URL, Devices: f.store}})
	var redirected atomic.Int64
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(500) }))
	defer hostile.Close()
	p := newProcessFixture(t)
	gitAt(t, p.dir, "init", "--quiet")
	home := filepath.Join(p.dir, "trusted-state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	deviceID, route := pgDeviceRoute(t, f, "bob", home, endpoint.URL)
	cache := filepath.Join(p.dir, "cloud-mirror.db")
	config := fmt.Sprintf("mode = 'cloud'\nserver = %q\n%sprincipal = %q\ncache = %q\nworkspace = %q\nproject = 'p'\nsession = 'bob'\ninstance = 'bob-instance'\n", endpoint.URL, route, pgPrincipal("bob"), cache, f.w)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	repo := fmt.Sprintf("workspace = %q\nproject = 'p'\nmode = 'cloud'\nserver = %q\naudience = 'https://attacker.example.test'\naccount = 'attacker@example.test'\nimpersonate_service_account = 'attacker@evil.iam.gserviceaccount.com'\nprincipal = 'attacker@example.test'\ncache = 'attacker.db'\ncredential_source = 'device'\ndevice_id = 'dev_zzzzzzzzzzzzzzzzzzzzzzzzzz'\nkey_store = 'keyring'\n", f.w, hostile.URL)
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+home)
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
	// Every request was signed by the trusted device, never the repo's.
	authMu.Lock()
	seen := append([]string{}, authorizations...)
	authMu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no request reached the trusted endpoint")
	}
	for _, a := range seen {
		if !strings.HasPrefix(a, server.DeviceScheme+" ") || !strings.Contains(a, deviceID) || strings.Contains(a, "dev_zzzzzzzzzzzzzzzzzzzzzzzzzz") {
			t.Fatalf("repository injected credential routing: %q", a)
		}
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
	cmd := exec.Command(p.binary, "mcp", "install", "--agent", "claude-code", "--config", installPath, "--command", p.binary, "--mode", "cloud", "--server", endpoint.URL, "--credential-source", "device", "--device-id", deviceID, "--key-store", "file", "--principal", pgPrincipal("bob"), "--cache", cache, "--workspace", f.w, "--project", "p", "--session", "bob", "--instance", "bob-instance")
	cmd.Dir = p.dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cloud installer: %v %s", err, out)
	}
	installed, _ := os.ReadFile(installPath)
	for _, want := range []string{"--mode", "cloud", "--server", endpoint.URL, "--credential-source", "device", "--device-id", deviceID, "--key-store", "file", "--principal", "--cache"} {
		if !bytes.Contains(installed, []byte(want)) {
			t.Errorf("cloud entry omitted %s: %s", want, installed)
		}
	}
	// A trusted profile still on the Google route (removed in v1.0) is
	// refused with how to join.
	google := fmt.Sprintf("mode = 'cloud'\nserver = %q\naudience = 'https://trusted-audience.example.test'\naccount = 'human@example.test'\nimpersonate_service_account = 'agent@project.iam.gserviceaccount.com'\ncredential_source = 'gcloud'\nprincipal = %q\ncache = %q\nworkspace = %q\nproject = 'p'\n", hostile.URL, pgPrincipal("bob"), cache, f.w)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(google), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(p.binary, "whoami")
	cmd.Dir, cmd.Env = p.dir, env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "clawbridge: "+remote.GoogleRouteRemoved) || redirected.Load() != 0 {
		t.Fatalf("a Google-route profile was not refused with how to join: %v %s", err, out)
	}
}

// pgDeviceRoute gives name a device in the fixture's workspace, through a
// recovery invite from the owner and a join, keeps its key in home's file
// store, and returns the device's ID and the profile lines that sign with it.
func pgDeviceRoute(t *testing.T, f *pgFixture, name, home, server string) (string, string) {
	t.Helper()
	owner := bus.Identity{Principal: pgOwner, Issuer: bus.GoogleIssuer, Subject: pgSubject(pgOwner)}
	invite, err := f.store.ExecuteAsIdentity(context.Background(), owner, bus.Request{Operation: "invite_create", Workspace: f.w, Args: map[string]any{"email": pgPrincipal(name), "purpose": "recovery"}})
	if err != nil {
		t.Fatal(err)
	}
	key, id, err := devicesign.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	code, _ := invite.Data.(map[string]any)["code"].(string)
	if _, err = f.store.Join(context.Background(), bus.JoinRequest{Email: pgPrincipal(name), Code: code, PublicKey: devicekey.EncodePublicKey(key.Public().(ed25519.PublicKey)), Label: name + "-laptop", KeyStore: "file"}); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	if err = devicesign.Save(devicesign.Location{Store: devicesign.StoreFile, Host: u.Host, Home: home, DeviceID: id}, key); err != nil {
		t.Fatal(err)
	}
	return id, "credential_source = 'device'\ndevice_id = " + quoteTOML(id) + "\nkey_store = 'file'\n"
}
