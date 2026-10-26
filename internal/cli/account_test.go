package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/doctor"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
	"github.com/pelletier/go-toml/v2"
	"github.com/zalando/go-keyring"
)

// joinDevices is the hosted authority's device side, in memory.
type joinDevices struct {
	mu      sync.Mutex
	devices map[string]bus.Device
	nonces  map[string]bool
	joinErr error
}

func (m *joinDevices) DeviceByID(_ context.Context, id string) (bus.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return bus.Device{}, errors.New("not found")
	}
	return d, nil
}

func (m *joinDevices) UseNonce(_ context.Context, device, nonce string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nonces[device+" "+nonce] {
		return bus.ErrNonceReused
	}
	m.nonces[device+" "+nonce] = true
	return nil
}

func (m *joinDevices) Join(_ context.Context, in bus.JoinRequest) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.joinErr != nil {
		return nil, m.joinErr
	}
	pub, _ := devicekey.ParsePublicKey(in.PublicKey)
	id := devicekey.ID(pub)
	m.devices[id] = bus.Device{ID: id, Principal: in.Email, Kind: "device", PublicKey: pub, Live: true}
	return map[string]any{"device_id": id, "principal": in.Email, "workspace": map[string]any{"id": "w1", "name": "acme"}, "purpose": "user", "grants": map[string]any{"p1": "member"}}, nil
}

// signedRecorder answers as the hosted authority, recording each caller
// and request.
type signedRecorder struct {
	mu       sync.Mutex
	ids      []bus.Identity
	requests []bus.Request
}

func (e *signedRecorder) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("unexpected unbound execution")
}

func (e *signedRecorder) ExecuteAsIdentity(_ context.Context, id bus.Identity, r bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, id)
	e.requests = append(e.requests, r)
	return bus.Result{Source: "server", Data: map[string]any{"devices": []any{}}}, nil
}

type joinFixture struct {
	url   string
	store *joinDevices
	exec  *signedRecorder
	keys  []string
}

// newJoinFixture is the real server handler taking device keys, a mock OS
// key store, and an isolated ClawBridge home.
func newJoinFixture(t *testing.T) *joinFixture {
	t.Helper()
	isolated(t)
	keyring.MockInit()
	f := &joinFixture{store: &joinDevices{devices: map[string]bus.Device{}, nonces: map[string]bool{}}, exec: &signedRecorder{}}
	var handler http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	f.url = ts.URL
	handler = server.NewHandlerWithOptions(f.exec, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: ts.URL, Devices: f.store}})
	made := newDeviceKey
	newDeviceKey = func() (ed25519.PrivateKey, string, error) {
		key, id, err := made()
		f.keys = append(f.keys, id)
		return key, id, err
	}
	t.Cleanup(func() { newDeviceKey = made })
	return f
}

func profile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]any{}
	if err = toml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func run(args ...string) (string, error) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), args, &out, &errOut)
	return out.String(), err
}

func TestJoinWritesADeviceProfileThatSigns(t *testing.T) {
	f := newJoinFixture(t)
	out, err := run("--server", f.url, "join", "--email", "Ada@Example.com", "--code", "cbj 4k7p wq9d 2mxh t6rb", "--label", "Ada's laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || !strings.Contains(out, f.keys[0]) || !strings.Contains(out, `"workspace":"acme"`) {
		t.Fatal(out)
	}
	home, _ := stateHome()
	path := filepath.Join(home, "config.toml")
	p := profile(t, path)
	want := map[string]any{"mode": "cloud", "server": f.url, "workspace": "acme", "principal": "ada@example.com", "credential_source": "device", "device_id": f.keys[0], "key_store": "keyring", "cache": filepath.Join(home, "config.cache.db")}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("profile mode", info.Mode())
	}
	if _, ok := p["audience"]; ok {
		t.Fatal("a device profile got an audience")
	}
	// The profile signs: the server sees the device, as its person.
	if _, err = run("device", "list"); err != nil {
		t.Fatal(err)
	}
	if len(f.exec.ids) != 1 || f.exec.ids[0].Issuer != bus.DeviceIssuer || f.exec.ids[0].Subject != f.keys[0] || f.exec.requests[0].Operation != "device_list" || f.exec.requests[0].Workspace != "acme" {
		t.Fatalf("%+v %+v", f.exec.ids, f.exec.requests)
	}
	// A second join into this profile is refused before any key is made.
	if _, err = run("--server", f.url, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB"); err == nil || !strings.Contains(err.Error(), "already signs as device") || len(f.keys) != 1 {
		t.Fatal("joined over a device profile:", err)
	}
	// Another profile may join another workspace with its own key.
	other := filepath.Join(t.TempDir(), "acme2.toml")
	if _, err = run("--server", f.url, "--client-config", other, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB"); err != nil {
		t.Fatal(err)
	}
	if p2 := profile(t, other); len(f.keys) != 2 || p2["device_id"] != f.keys[1] || f.keys[1] == f.keys[0] || p2["cache"] != filepath.Join(filepath.Dir(other), "acme2.cache.db") {
		t.Fatal(p2)
	}
}

func TestJoinReadsTheCodeFromStdin(t *testing.T) {
	f := newJoinFixture(t)
	var out, errOut bytes.Buffer
	err := runDeviceJoin(context.Background(), Config{Server: f.url}, []string{"--email", "ada@example.com"}, strings.NewReader("CBJ-4K7P-WQ9D-2MXH-T6RB\n"), &out, &errOut)
	if err != nil || !strings.Contains(out.String(), f.keys[0]) || !strings.Contains(errOut.String(), "Invite code") {
		t.Fatal(err, out.String(), errOut.String())
	}
}

func TestARefusedJoinLeavesNoKeyAndNoProfile(t *testing.T) {
	f := newJoinFixture(t)
	f.store.joinErr = bus.ErrInvalidInvite
	_, err := run("--server", f.url, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB")
	if err == nil || !strings.Contains(err.Error(), "invalid or expired invite") || len(f.keys) != 1 {
		t.Fatal(err)
	}
	u := strings.TrimPrefix(f.url, "http://")
	if _, e := devicesign.Load(devicesign.Location{Store: devicesign.StoreKeyring, Host: u, DeviceID: f.keys[0]}); !errors.Is(e, devicesign.ErrNoKey) {
		t.Fatal("a refused join kept its key:", e)
	}
	home, _ := stateHome()
	if _, e := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a refused join wrote a profile")
	}
}

func TestJoinWithAFileKey(t *testing.T) {
	f := newJoinFixture(t)
	var out, errOut bytes.Buffer
	err := runDeviceJoin(context.Background(), Config{Server: f.url}, []string{"--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB", "--key-store", "file"}, nil, &out, &errOut)
	if err != nil || !strings.Contains(errOut.String(), devicesign.FileWarning) || !strings.Contains(out.String(), devicesign.FileWarning) {
		t.Fatal(err, errOut.String())
	}
	home, _ := stateHome()
	key := filepath.Join(home, "device", f.keys[0]+".key")
	if runtime.GOOS != "windows" {
		if info, e := os.Stat(key); e != nil || info.Mode().Perm() != 0o600 {
			t.Fatal(e)
		}
		if _, err = run("device", "list"); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(key, 0o644)
		if _, err = run("device", "list"); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatal("signed with a key file open to others:", err)
		}
	}
}

func TestJoinableProfile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	origin := "https://cb.example.test"
	for name, c := range map[string]struct{ body, err string }{
		"local":        {"mode = \"local\"\ndb = \"x.db\"\n", "local workspace"},
		"other server": {"mode = \"cloud\"\nserver = \"https://other.test\"\n", "another server"},
		"device":       {"mode = \"cloud\"\nserver = \"https://cb.example.test\"\ncredential_source = \"device\"\ndevice_id = \"dev_x\"\n", "already signs as device dev_x"},
		"google":       {"mode = \"cloud\"\nserver = \"https://CB.example.test/\"\ncredential_source = \"oauth\"\noauth_client_id = \"c\"\n", ""},
	} {
		_, err := joinableProfile(write(strings.ReplaceAll(name, " ", "_")+".toml", c.body), origin)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if v, err := joinableProfile(filepath.Join(dir, "absent.toml"), origin); err != nil || len(v) != 0 {
		t.Fatal(v, err)
	}
}

// accountRecorder is the account commands' executor, recording requests.
type accountRecorder struct {
	requests []bus.Request
	data     map[string]any
	err      error
}

func (r *accountRecorder) Execute(_ context.Context, req bus.Request) (bus.Result, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return bus.Result{}, r.err
	}
	data := map[string]any{}
	for k, v := range r.data {
		data[k] = v
	}
	if pub, ok := req.Args["public_key"].(string); ok {
		key, _ := devicekey.ParsePublicKey(pub)
		data["device_id"] = devicekey.ID(key)
	}
	return bus.Result{Source: "server", Data: data}, nil
}

func googleProfile(t *testing.T) string {
	t.Helper()
	isolated(t)
	home, _ := stateHome()
	path := filepath.Join(home, "config.toml")
	_ = os.MkdirAll(home, 0o700)
	body := "mode = \"cloud\"\nserver = \"https://cb.example.test\"\naudience = \"c.apps\"\nworkspace = \"acme\"\nprincipal = \"ada@example.com\"\ncache = \"cache.db\"\ncredential_source = \"oauth\"\noauth_client_id = \"c.apps\"\noauth_client_secret = \"s\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withAccountRecorder(t *testing.T, r *accountRecorder) {
	t.Helper()
	old := newAccountExecutor
	newAccountExecutor = func(Config) (accountExecutor, error) { return r, nil }
	t.Cleanup(func() { newAccountExecutor = old })
}

func TestAccountCommandsMapToTheBus(t *testing.T) {
	googleProfile(t)
	for _, c := range []struct {
		args []string
		op   string
		want string
	}{
		{[]string{"invite", "--email", "bob@example.com", "--grant", "api=member", "--grant", "web=lead", "--expires", "48h"}, "invite_create", `{"email":"bob@example.com","expires_hours":48,"grants":{"api":"member","web":"lead"},"purpose":"user"}`},
		{[]string{"invite", "--email", "bob@example.com", "--recovery", "--revoke-others", "--expires", "3d"}, "invite_create", `{"email":"bob@example.com","expires_hours":72,"purpose":"recovery","revoke_others":true}`},
		{[]string{"invite", "list"}, "invite_list", `{}`},
		{[]string{"invite", "revoke", "inv_abc"}, "invite_revoke", `{"invite_id":"inv_abc"}`},
		{[]string{"device", "list", "--member", "bob@example.com"}, "device_list", `{"principal":"bob@example.com"}`},
		{[]string{"device", "revoke", "dev_x", "--reason", "lost"}, "device_revoke", `{"device_id":"dev_x","reason":"lost"}`},
		{[]string{"device", "label", "dev_x", "Ada's desktop"}, "device_label", `{"device_id":"dev_x","label":"Ada's desktop"}`},
		{[]string{"device", "add"}, "invite_create", `{"email":"ada@example.com","purpose":"device"}`},
		{[]string{"device", "add", "--expires", "12h"}, "invite_create", `{"email":"ada@example.com","expires_hours":12,"purpose":"device"}`},
		{[]string{"user", "list"}, "user_list", `{}`},
		{[]string{"user", "revoke", "bob@example.com", "--confirm"}, "user_revoke", `{"principal":"bob@example.com"}`},
		{[]string{"owner", "show"}, "owner_transfer_get", `{}`},
		{[]string{"owner", "transfer", "--to", "bob@example.com"}, "owner_transfer_offer", `{"to":"bob@example.com"}`},
		{[]string{"owner", "accept"}, "owner_transfer_respond", `{"action":"accept"}`},
		{[]string{"owner", "decline"}, "owner_transfer_respond", `{"action":"decline"}`},
		{[]string{"owner", "cancel"}, "owner_transfer_respond", `{"action":"cancel"}`},
	} {
		r := &accountRecorder{}
		withAccountRecorder(t, r)
		if _, err := run(c.args...); err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		got, _ := json.Marshal(r.requests[0].Args)
		if len(r.requests) != 1 || r.requests[0].Operation != c.op || r.requests[0].Workspace != "acme" || r.requests[0].Project != "" || string(got) != c.want {
			t.Errorf("%v: %s %s", c.args, r.requests[0].Operation, got)
		}
	}
	for _, c := range []struct {
		args []string
		err  string
	}{
		{[]string{"user", "revoke", "bob@example.com"}, "--confirm"},
		{[]string{"invite", "--email", "b@example.com", "--grant", "api"}, "PROJECT=ROLE"},
		{[]string{"invite", "--email", "b@example.com", "--grant", "api=member", "--grant", "api=lead"}, "twice"},
		{[]string{"invite", "--email", "b@example.com", "--expires", "90m"}, "whole hours"},
		{[]string{"device", "add", "--grant", "api=member"}, "go with --ci"},
		{[]string{"device", "add", "--ci", "deploy", "--grant", "api=member"}, "needs --expires"},
		{[]string{"device", "add", "--ci", "deploy", "--grant", "api=member", "--expires", "36h"}, "in days"},
		{[]string{"device", "frobnicate"}, "unknown device command"},
	} {
		r := &accountRecorder{}
		withAccountRecorder(t, r)
		if _, err := run(c.args...); err == nil || !strings.Contains(err.Error(), c.err) || len(r.requests) != 0 {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

func TestCIDeviceKeyIsShownOnceAndOnlyItsPublicHalfSent(t *testing.T) {
	googleProfile(t)
	r := &accountRecorder{data: map[string]any{"principal": "ci:deploy@acme"}}
	withAccountRecorder(t, r)
	out, err := run("device", "add", "--ci", "deploy", "--grant", "api=member", "--expires", "30d")
	if err != nil {
		t.Fatal(err)
	}
	a := r.requests[0].Args
	if r.requests[0].Operation != "device_ci_create" || a["name"] != "deploy" || a["expires_days"] != int64(30) || fmt.Sprint(a["grants"]) != "map[api:member]" {
		t.Fatalf("%+v", a)
	}
	var result struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &result)
	shown, _ := result.Data[devicesign.EnvKey].(string)
	key, err := devicesign.ParseKey(shown)
	if err != nil || devicekey.EncodePublicKey(key.Public().(ed25519.PublicKey)) != a["public_key"] || strings.Contains(fmt.Sprint(a), shown) {
		t.Fatal("the CI key shown isn't the one registered, or its private half was sent:", err)
	}
	// CI signs with the key from its environment alone.
	t.Setenv(devicesign.EnvKey, shown)
	id := devicekey.ID(key.Public().(ed25519.PublicKey))
	if got, err := devicesign.Load(devicesign.Location{Store: devicesign.StoreEnv, DeviceID: id}); err != nil || !got.Equal(key) {
		t.Fatal(err)
	}
}

func TestEnrollSwitchesTheProfileToTheKey(t *testing.T) {
	path := googleProfile(t)
	keyring.MockInit()
	var made []string
	old := newDeviceKey
	newDeviceKey = func() (ed25519.PrivateKey, string, error) {
		k, id, err := old()
		made = append(made, id)
		return k, id, err
	}
	t.Cleanup(func() { newDeviceKey = old })
	refused := &accountRecorder{err: &remote.HTTPError{Status: 403, Code: "forbidden", Message: "enrolling a device needs a Google sign-in"}}
	withAccountRecorder(t, refused)
	if _, err := run("device", "enroll", "--label", "laptop"); err == nil {
		t.Fatal("a refused enrolment passed")
	}
	if _, err := devicesign.Load(devicesign.Location{Store: devicesign.StoreKeyring, Host: "cb.example.test", DeviceID: made[0]}); !errors.Is(err, devicesign.ErrNoKey) {
		t.Fatal("a refused enrolment kept its key:", err)
	}
	if profile(t, path)["credential_source"] != "oauth" {
		t.Fatal("a refused enrolment changed the profile")
	}
	r := &accountRecorder{}
	withAccountRecorder(t, r)
	out, err := run("device", "enroll", "--label", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	a := r.requests[0].Args
	if r.requests[0].Operation != "device_enroll" || a["label"] != "laptop" || a["key_store"] != "keyring" || !strings.Contains(out, made[1]) {
		t.Fatal(a, out)
	}
	p := profile(t, path)
	if p["credential_source"] != "device" || p["device_id"] != made[1] || p["key_store"] != "keyring" || p["oauth_client_id"] != "c.apps" {
		t.Fatal("the profile after enrolment:", p)
	}
	if _, err = devicesign.Load(devicesign.Location{Store: devicesign.StoreKeyring, Host: "cb.example.test", DeviceID: made[1]}); err != nil {
		t.Fatal(err)
	}
	// The device profile resolves without an audience, refuses Google
	// login, and enrolling again is refused.
	c, _, err := resolveConfig(context.Background(), Config{})
	if err != nil || c.CredentialSource != "device" || c.DeviceID != made[1] {
		t.Fatal(c, err)
	}
	if _, _, err = resolveLoginConfig(context.Background(), Config{}, false); err == nil || !strings.Contains(err.Error(), "--credential-source oauth") {
		t.Fatal("logged in with Google over a device profile:", err)
	}
	if _, err = run("device", "enroll"); err == nil || !strings.Contains(err.Error(), "already signs") {
		t.Fatal(err)
	}
	// Switching back to Google is one flag: the old settings were kept.
	if c, _, err = resolveConfig(context.Background(), Config{CredentialSource: "oauth"}); err != nil || c.OAuthClientID != "c.apps" {
		t.Fatal(c, err)
	}
}

func TestClockOffsetIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.toml")
	if err := os.WriteFile(path, []byte("mode = \"cloud\"\ncredential_source = \"device\"\nworkspace = \"acme\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	if err := saveClockOffset(path, 125*time.Second, at); err != nil {
		t.Fatal(err)
	}
	p := profile(t, path)
	if p["clock_offset_seconds"] != int64(125) || p["clock_offset_at"] != "2026-10-26T22:00:00Z" || p["workspace"] != "acme" {
		t.Fatal(p)
	}
	r := remoteConfig(Config{CredentialSource: "device", ClockOffsetSeconds: 125, ClockOffsetAt: "2026-10-26T22:00:00Z", ClientConfig: path})
	if r.ClockOffset != 125*time.Second || !r.ClockOffsetAt.Equal(at) || r.SaveClockOffset == nil {
		t.Fatal(r)
	}
	// Another route's profile is never given one.
	google := filepath.Join(t.TempDir(), "g.toml")
	_ = os.WriteFile(google, []byte("mode = \"cloud\"\ncredential_source = \"oauth\"\n"), 0o600)
	if err := saveClockOffset(google, time.Minute, at); err != nil || profile(t, google)["clock_offset_seconds"] != nil {
		t.Fatal(err)
	}
}

func TestDoctorChecksTheDeviceKey(t *testing.T) {
	f := newJoinFixture(t)
	if _, err := run("--server", f.url, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB"); err != nil {
		t.Fatal(err)
	}
	status := func() map[string]doctor.Check {
		config, path, err := resolveConfig(context.Background(), Config{})
		if err != nil {
			t.Fatal(err)
		}
		report, err := collectDoctor(context.Background(), config, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]doctor.Check{}
		for _, c := range report.Checks {
			out[c.ID] = c
		}
		return out
	}
	checks := status()
	if checks["auth.device"].Status != doctor.Pass || !strings.Contains(checks["auth.device"].Message, f.keys[0]) || checks["auth.clock"].Status != doctor.Pass || checks["config.trusted"].Status != doctor.Pass {
		t.Fatalf("%+v", checks)
	}
	if _, ok := checks["auth.token-selection"]; ok {
		t.Fatal("a device route ran the Google token check")
	}
	if len(f.exec.ids) == 0 || f.exec.ids[0].Subject != f.keys[0] {
		t.Fatal("doctor didn't reach the server as the device")
	}
	u := strings.TrimPrefix(f.url, "http://")
	_ = devicesign.Delete(devicesign.Location{Store: devicesign.StoreKeyring, Host: u, DeviceID: f.keys[0]})
	if c := status()["auth.device"]; c.Status != doctor.Fail || !strings.Contains(c.Message, "not in its store") {
		t.Fatalf("a missing key: %+v", c)
	}
}
