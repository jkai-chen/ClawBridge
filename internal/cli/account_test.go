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
	// answerID, when set, is the device the join's answer names.
	answerID string
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
	if m.answerID != "" {
		id = m.answerID
	}
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
	if r.Operation == "whoami" {
		return bus.Result{Source: "server", Data: map[string]any{"principal": id.Principal}}, nil
	}
	if r.Operation == "list_projects" {
		return bus.Result{Source: "server", Data: map[string]any{"projects": []any{map[string]any{"id": "p1", "slug": "api", "role": "member"}}}}, nil
	}
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

// loseJoinAnswer delivers the join to the server and then loses its answer.
type loseJoinAnswer struct{}

func (loseJoinAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && r.URL.Path == "/v1/join" {
		response.Body.Close()
		return nil, errors.New("connection reset")
	}
	return response, err
}

func TestALostJoinAnswerKeepsTheKeyAndSaysWhere(t *testing.T) {
	f := newJoinFixture(t)
	_, err := JoinDevice(context.Background(), JoinParams{Server: f.url, Email: "Ada@Example.com", Code: "CBJ-4K7P-WQ9D-2MXH-T6RB", HTTPClient: &http.Client{Transport: loseJoinAnswer{}}})
	var lost *JoinUncertainError
	if !errors.As(err, &lost) || !errors.Is(err, remote.ErrJoinUncertain) || len(f.keys) != 1 {
		t.Fatal(err)
	}
	home, _ := stateHome()
	want := JoinUncertainError{DeviceID: f.keys[0], KeyStore: devicesign.StoreKeyring, KeyHome: home, Server: f.url, Principal: "ada@example.com"}
	if *lost != want || !strings.Contains(err.Error(), "revoke "+f.keys[0]) {
		t.Fatalf("%+v: %v", *lost, err)
	}
	// The key is where the error says, and it derives the device.
	u := strings.TrimPrefix(f.url, "http://")
	if _, e := devicesign.Load(devicesign.Location{Store: lost.KeyStore, Host: u, Home: lost.KeyHome, DeviceID: lost.DeviceID}); e != nil {
		t.Fatal("a lost join answer lost the key:", e)
	}
	if _, e := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a lost join answer wrote a profile")
	}
}

func TestJoinWithAFileKey(t *testing.T) {
	f := newJoinFixture(t)
	// Through Run, where --key-store is also a global flag (R85-H1).
	out, err := run("--server", f.url, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB", "--key-store", "file")
	if err != nil || !strings.Contains(out, devicesign.FileWarning) || !strings.Contains(out, `"key_store":"file"`) {
		t.Fatal(err, out)
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
	// failOp fails only that operation.
	failOp string
}

func (r *accountRecorder) Execute(_ context.Context, req bus.Request) (bus.Result, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return bus.Result{}, r.err
	}
	if req.Operation == r.failOp {
		return bus.Result{}, &remote.HTTPError{Status: 401, Code: "unauthenticated", Message: "unknown device"}
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
	onTerminal(t)
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
		{[]string{"device", "add", "--expires", "12h"}, "invite_create", `{"email":"ada@example.com","expires_minutes":720,"purpose":"device"}`},
		{[]string{"device", "add", "--expires", "15m"}, "invite_create", `{"email":"ada@example.com","expires_minutes":15,"purpose":"device"}`},
		{[]string{"device", "events"}, "device_events", `{"all":false}`},
		{[]string{"device", "events", "--all"}, "device_events", `{"all":true}`},
		{[]string{"device", "ack", "dve_1", "dve_2"}, "device_ack", `{"event_ids":["dve_1","dve_2"]}`},
		{[]string{"device", "ack", "--all"}, "device_ack", `{"all":true}`},
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
		{[]string{"device", "ack"}, "or --all"},
		{[]string{"device", "ack", "--all", "dve_1"}, "or --all"},
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
	onTerminal(t)
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
	serverOK(t)
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
	// R85-M1: one signed read with the new key before the profile uses it.
	if len(r.requests) != 2 || r.requests[1].Operation != "device_list" {
		t.Fatal("the profile switched without a signed request:", r.requests)
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

// serverOK answers enroll's server check as a server taking device keys at
// the profile's origin.
func serverOK(t *testing.T) {
	t.Helper()
	old := enrollServer
	enrollServer = func(context.Context, string, *http.Client) (string, error) { return "https://cb.example.test", nil }
	t.Cleanup(func() { enrollServer = old })
}

// R85-M1, R85-H1, R85-L2: enroll checks the server before making a key,
// keeps the Google route when a signed request fails, takes --key-store
// through Run, and drops a key a failed Google credential never sent.
func TestEnrollChecksTheServerAndTheKeyFirst(t *testing.T) {
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
	oldServer := enrollServer
	enrollServer = func(context.Context, string, *http.Client) (string, error) {
		return "", errors.New("this server doesn't accept device keys; ask its owner, or sign in with clawbridge login")
	}
	r := &accountRecorder{}
	withAccountRecorder(t, r)
	if _, err := run("device", "enroll"); err == nil || !strings.Contains(err.Error(), "doesn't accept device keys") || len(made) != 0 || len(r.requests) != 0 {
		t.Fatal("enrolled on a server without device keys:", err, made, r.requests)
	}
	enrollServer = oldServer
	serverOK(t)
	r.failOp = "device_list"
	if _, err := run("device", "enroll"); err == nil || !strings.Contains(err.Error(), "still signs with Google") || profile(t, path)["credential_source"] != "oauth" {
		t.Fatal("the profile switched to a key whose request failed:", err)
	}
	google := &accountRecorder{err: remote.CredentialFailure(errors.New("token expired"))}
	withAccountRecorder(t, google)
	if _, err := run("device", "enroll"); err == nil {
		t.Fatal("enrolled without a Google credential")
	}
	if _, err := devicesign.Load(devicesign.Location{Store: devicesign.StoreKeyring, Host: "cb.example.test", DeviceID: made[len(made)-1]}); !errors.Is(err, devicesign.ErrNoKey) {
		t.Fatal("a key never sent was kept:", err)
	}
	ok := &accountRecorder{}
	withAccountRecorder(t, ok)
	if out, err := run("device", "enroll", "--key-store", "file"); err != nil || !strings.Contains(out, `"key_store":"file"`) || ok.requests[0].Args["key_store"] != "file" || profile(t, path)["key_store"] != "file" {
		t.Fatal("--key-store file through Run:", err, out)
	}
}

// R85-L3: an answer naming another device is uncertain: the key stays.
func TestAJoinAnswerForAnotherDeviceKeepsTheKey(t *testing.T) {
	f := newJoinFixture(t)
	f.store.answerID = "dev_someoneelse"
	_, err := run("--server", f.url, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB")
	var lost *JoinUncertainError
	if !errors.As(err, &lost) || lost.DeviceID != f.keys[0] {
		t.Fatal(err)
	}
	u := strings.TrimPrefix(f.url, "http://")
	if _, e := devicesign.Load(devicesign.Location{Store: devicesign.StoreKeyring, Host: u, DeviceID: f.keys[0]}); e != nil {
		t.Fatal("an answer for another device deleted the key:", e)
	}
}

// R85-L4: at a terminal the invite code is read without echo.
func TestJoinReadsTheCodeHiddenAtATerminal(t *testing.T) {
	f := newJoinFixture(t)
	oldTerm, oldHidden := stdinTerminal, readHidden
	stdinTerminal = func() bool { return true }
	hidden := false
	readHidden = func() ([]byte, error) { hidden = true; return []byte("CBJ-4K7P-WQ9D-2MXH-T6RB"), nil }
	t.Cleanup(func() { stdinTerminal, readHidden = oldTerm, oldHidden })
	var out, errOut bytes.Buffer
	if err := runDeviceJoin(context.Background(), Config{Server: f.url}, []string{"--email", "ada@example.com"}, os.Stdin, &out, &errOut); err != nil || !hidden || !strings.Contains(out.String(), f.keys[0]) {
		t.Fatal(err, hidden, out.String())
	}
}

// A device label is chosen by whoever adds the device (R1): the alert line
// and every printed answer escape its terminal controls, so it can't erase
// or rewrite the line.
func TestDeviceEventLineEscapesTheLabel(t *testing.T) {
	evil := "\x1b[1A\x1b[2KAll good\u202e\x9b"
	line := deviceEventLine(map[string]any{"event_id": "dve_1", "kind": "added", "label": evil, "device_id": "dev_9k2", "principal": "ada@example.com", "route": "device_add\x1b[2K", "created_at": "2026-10-27T14:02:00.000000000Z"})
	if strings.ContainsAny(line, "\x1b\u202e\u009b") || !strings.Contains(line, "via device add") || !strings.Contains(line, "clawbridge device revoke dev_9k2") || !strings.Contains(line, "[dve_1]") {
		t.Fatalf("%q", line)
	}
	revoked := deviceEventLine(map[string]any{"event_id": "dve_2", "kind": "revoked", "label": evil, "device_id": "dev_9k2", "principal": "ada@example.com", "actor": "ada@example.com"})
	if strings.ContainsAny(revoked, "\x1b\u202e\u009b") || !strings.Contains(revoked, "was revoked") {
		t.Fatalf("%q", revoked)
	}
	// A kind this client doesn't know is never a "new device".
	other := deviceEventLine(map[string]any{"event_id": "dve_3", "kind": "dormant_soon\x1b[2K", "label": evil, "device_id": "dev_9k2", "principal": "ada@example.com"})
	if strings.Contains(other, "New device") || !strings.Contains(other, `of ada@example.com: dormant soon`) || strings.ContainsAny(other, "\x1b\u202e\u009b") || !strings.Contains(other, "[dve_3]") {
		t.Fatalf("%q", other)
	}
	googleProfile(t)
	r := &accountRecorder{data: map[string]any{"devices": []any{map[string]any{"label": evil}}}}
	withAccountRecorder(t, r)
	out, err := run("device", "list")
	if err != nil || strings.Contains(out, "\\u001b[1A") == false {
		t.Fatalf("%v %q", err, out)
	}
	if strings.ContainsAny(out, "\x1b\u202e\u009b") {
		t.Fatalf("%q", out)
	}
	var b bytes.Buffer
	printDeviceEvents(&b)([]map[string]any{{"event_id": "dve_1", "kind": "added", "label": evil}})
	if strings.ContainsAny(b.String(), "\x1b\u202e\u009b") || !strings.Contains(b.String(), "device ack --all") || !strings.Contains(b.String(), `"\u001b[1A`) {
		t.Fatalf("%q", b.String())
	}
	// A flood prints the newest five and a count (M2).
	b.Reset()
	flood := []map[string]any{}
	for i := 0; i < 8; i++ {
		flood = append(flood, map[string]any{"event_id": fmt.Sprintf("dve_%d", i), "kind": "added", "label": "x"})
	}
	printDeviceEvents(&b)(flood)
	if strings.Count(b.String(), "New device") != 5 || !strings.Contains(b.String(), "…and 3 more: clawbridge device events") {
		t.Fatalf("%q", b.String())
	}
	for _, bad := range []string{"tab\there", "rtl\u202e", "c1\u009b", "\xff"} {
		if _, err := deviceLabel(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
