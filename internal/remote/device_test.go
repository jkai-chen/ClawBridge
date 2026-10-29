package remote

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/server"
)

// memDevices is the hosted authority's device side, in memory.
type memDevices struct {
	mu      sync.Mutex
	devices map[string]bus.Device
	nonces  map[string]bool
	joins   []bus.JoinRequest
	joinErr error
	// answer rewrites a join's answer, to test the client's checks.
	answer func(map[string]any)
}

func (m *memDevices) DeviceByID(_ context.Context, id string) (bus.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return bus.Device{}, errors.New("not found")
	}
	return d, nil
}

func (m *memDevices) UseNonce(_ context.Context, device, nonce string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nonces[device+" "+nonce] {
		return bus.ErrNonceReused
	}
	m.nonces[device+" "+nonce] = true
	return nil
}

func (m *memDevices) Join(_ context.Context, in bus.JoinRequest) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.joins = append(m.joins, in)
	if m.joinErr != nil {
		return nil, m.joinErr
	}
	pub, err := devicekey.ParsePublicKey(in.PublicKey)
	if err != nil {
		return nil, bus.ErrInvalidInvite
	}
	id := devicekey.ID(pub)
	m.devices[id] = bus.Device{ID: id, Principal: in.Email, Kind: "device", PublicKey: pub, Live: true}
	out := map[string]any{"device_id": id, "principal": in.Email, "workspace": map[string]any{"id": "w1", "name": "acme"}, "purpose": "user", "grants": map[string]any{"p1": "member"}}
	if m.answer != nil {
		m.answer(out)
	}
	return out, nil
}

// whoamiExecutor answers as the hosted authority, recording who called.
type whoamiExecutor struct {
	mu  sync.Mutex
	ids []bus.Identity
}

func (e *whoamiExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("unexpected unbound execution")
}

func (e *whoamiExecutor) ExecuteAsIdentity(_ context.Context, id bus.Identity, r bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, id)
	return bus.Result{Source: "server", Data: map[string]any{"operation": r.Operation}}, nil
}

type deviceServer struct {
	t      *testing.T
	ts     *httptest.Server
	store  *memDevices
	exec   *whoamiExecutor
	mu     sync.Mutex
	offset time.Duration
}

func (d *deviceServer) setOffset(o time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.offset = o
}

// newDeviceServer is the real server handler, taking device keys at its
// own origin, over an in-memory device store.
func newDeviceServer(t *testing.T, methods ...string) *deviceServer {
	t.Helper()
	d := &deviceServer{t: t, store: &memDevices{devices: map[string]bus.Device{}, nonces: map[string]bool{}}, exec: &whoamiExecutor{}}
	if len(methods) == 0 {
		methods = []string{server.AuthDevice}
	}
	var handler http.Handler
	d.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(d.ts.Close)
	now := func() time.Time {
		d.mu.Lock()
		defer d.mu.Unlock()
		return time.Now().Add(d.offset)
	}
	handler = server.NewHandlerWithOptions(d.exec, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: methods, PublicOrigin: d.ts.URL, Devices: d.store}, Now: now})
	return d
}

func (d *deviceServer) join(t *testing.T, key ed25519.PrivateKey) Joined {
	t.Helper()
	origin, err := JoinServer(context.Background(), d.ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := Join(context.Background(), origin, key, JoinRequest{Email: " Ada@Example.com ", Code: "cbj-4k7p-wq9d-2mxh-t6rb", Label: "laptop", KeyStore: "keyring"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return joined
}

func (d *deviceServer) client(t *testing.T, key ed25519.PrivateKey, save func(time.Duration, time.Time)) *Client {
	t.Helper()
	u, _ := url.Parse(d.ts.URL)
	signer := devicesign.NewSigner(key, Origin(u), 0, time.Time{})
	c, err := NewWithSigner(Config{URL: d.ts.URL, Principal: "ada@example.com", CachePath: t.TempDir() + "/cache.db", SaveClockOffset: save}, signer, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDeviceJoinThenSignedRequests(t *testing.T) {
	d := newDeviceServer(t)
	key, id, _ := devicesign.NewKey()
	joined := d.join(t, key)
	if joined.DeviceID != id || joined.Principal != "ada@example.com" || joined.Workspace.Name != "acme" || joined.Grants["p1"] != "member" || !joined.ClockOffsetAt.IsZero() {
		t.Fatalf("%+v", joined)
	}
	in := d.store.joins[0]
	if in.Email != "ada@example.com" || in.Code != "4K7PWQ9D2MXHT6RB" || in.KeyStore != "keyring" || in.Label != "laptop" {
		t.Fatalf("sent %+v", in)
	}
	c := d.client(t, key, nil)
	for i := 0; i < 2; i++ {
		if _, err := c.Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "acme", Project: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.exec.ids) != 2 || d.exec.ids[0].Issuer != bus.DeviceIssuer || d.exec.ids[0].Subject != id || d.exec.ids[0].Principal != "ada@example.com" {
		t.Fatalf("%+v", d.exec.ids)
	}
	// A revoked device hears why. (The server caches a device for 30 s;
	// the bus re-checks it in every request, so this one starts revoked.)
	revoked, revokedID, _ := devicesign.NewKey()
	d.store.mu.Lock()
	d.store.devices[revokedID] = bus.Device{ID: revokedID, Principal: "ada@example.com", Kind: "device", PublicKey: revoked.Public().(ed25519.PublicKey), Live: false}
	d.store.mu.Unlock()
	_, err := d.client(t, revoked, nil).Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "acme", Project: "p"})
	var refused *HTTPError
	if !errors.As(err, &refused) || refused.Status != 401 || !strings.Contains(refused.Message, "revoked") || len(d.exec.ids) != 2 {
		t.Fatal("a revoked device's refusal:", err)
	}
}

func TestDeviceClockSkewIsCorrectedOnce(t *testing.T) {
	d := newDeviceServer(t)
	key, _, _ := devicesign.NewKey()
	d.join(t, key)
	var saved []time.Duration
	c := d.client(t, key, func(o time.Duration, _ time.Time) { saved = append(saved, o) })
	d.setOffset(10 * time.Minute)
	if _, err := c.Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "acme", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0] < 9*time.Minute || saved[0] > 11*time.Minute || len(d.exec.ids) != 1 {
		t.Fatal("one corrected retry, saved:", saved, len(d.exec.ids))
	}
	// The correction stays in use: no further refusal.
	if _, err := c.Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "acme", Project: "p"}); err != nil || len(saved) != 1 {
		t.Fatal(err, saved)
	}
	// Beyond an hour the clock is wrong: no retry.
	d.setOffset(2 * time.Hour)
	_, err := c.Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "acme", Project: "p"})
	var clock *devicesign.ClockError
	if !errors.As(err, &clock) || len(d.exec.ids) != 2 {
		t.Fatal("a 2 h skew:", err)
	}
}

// The client sends the request target exactly as it signed it, an escaped
// space included (AU-M1).
func TestDeviceRequestSendsTheTargetItSigned(t *testing.T) {
	key, _, _ := devicesign.NewKey()
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		h := strings.TrimPrefix(r.Header.Get("Authorization"), devicesign.Scheme+" v1 ")
		f := map[string]string{}
		for _, part := range strings.Split(h, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			f[k] = v
		}
		sig, _ := base64.RawURLEncoding.DecodeString(f["sig"])
		body := []byte(`{}`)
		if !ed25519.Verify(key.Public().(ed25519.PublicKey), devicekey.RequestBase(r.Method, "http://"+r.Host, r.RequestURI, f["ts"], f["nonce"], body), sig) {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	c, err := NewWithSigner(Config{URL: ts.URL, Principal: "ada@example.com", CachePath: "/tmp/x"}, devicesign.NewSigner(key, Origin(u), 0, time.Time{}), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/v1/a%20b", "/v1/execute?note=a%20b&x=%7E", "/v1/execute"} {
		status, _, _, err := c.send(context.Background(), c.http, http.MethodPost, target, []byte(`{}`), "")
		if err != nil || status != 204 || got != target {
			t.Fatalf("%s: sent %q, status %d, %v", target, got, status, err)
		}
	}
}

func TestJoinRefusals(t *testing.T) {
	key, _, _ := devicesign.NewKey()
	google := newDeviceServer(t, server.AuthGoogle)
	// A Google-only server names no methods (so v0.3.3 clients parse it),
	// which reads as no invites.
	if _, err := JoinServer(context.Background(), google.ts.URL, nil); !errors.Is(err, ErrInvitesUnsupported) {
		t.Fatal("a Google-only server:", err)
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"browser_client_id":"b","browser_client_secret":"bs","device_client_id":"d","device_client_secret":"ds"}`))
	}))
	defer old.Close()
	if _, err := JoinServer(context.Background(), old.URL, nil); !errors.Is(err, ErrInvitesUnsupported) {
		t.Fatal("an older server:", err)
	}
	if _, err := JoinServer(context.Background(), old.URL+"/prefix", nil); err == nil {
		t.Fatal("joined a URL with a path")
	}
	// The server's public origin differs from the URL given.
	d := newDeviceServer(t)
	alias := strings.Replace(d.ts.URL, "127.0.0.1", "[::1]", 1)
	if _, err := JoinServer(context.Background(), alias, nil); err == nil {
		t.Log("no IPv6 loopback here")
	} else if !strings.Contains(err.Error(), "public origin is "+d.ts.URL) && !strings.Contains(err.Error(), "cannot read") && !strings.Contains(err.Error(), "cannot reach") {
		t.Fatal(err)
	}
	origin, err := JoinServer(context.Background(), d.ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Join(context.Background(), origin, key, JoinRequest{Email: "ada@example.com", Code: "nope"}, nil); err == nil {
		t.Fatal("sent a malformed code")
	}
	d.store.joinErr = bus.ErrInvalidInvite
	_, err = Join(context.Background(), origin, key, JoinRequest{Email: "ada@example.com", Code: "CBJ-4K7P-WQ9D-2MXH-T6RB", Label: "l", KeyStore: "keyring"}, nil)
	var refused *JoinError
	if !errors.As(err, &refused) || refused.Status != 403 || !strings.Contains(refused.Error(), "invalid or expired invite") {
		t.Fatal(err)
	}
	d.store.joinErr = nil
	d.store.answer = func(out map[string]any) { out["device_id"] = "dev_aaaaaaaaaaaaaaaaaaaaaaaaaa" }
	if _, err = Join(context.Background(), origin, key, JoinRequest{Email: "ada@example.com", Code: "CBJ-4K7P-WQ9D-2MXH-T6RB", Label: "l", KeyStore: "keyring"}, nil); err == nil || !strings.Contains(err.Error(), "another device") {
		t.Fatal("took an answer for another device:", err)
	}
	d.store.answer = func(out map[string]any) { out["principal"] = "eve@example.com" }
	if _, err = Join(context.Background(), origin, key, JoinRequest{Email: "ada@example.com", Code: "CBJ-4K7P-WQ9D-2MXH-T6RB", Label: "l", KeyStore: "keyring"}, nil); err == nil {
		t.Fatal("took an answer for another email")
	}
}

func TestJoinClockSkewIsCorrectedOnce(t *testing.T) {
	d := newDeviceServer(t)
	d.setOffset(-8 * time.Minute)
	key, _, _ := devicesign.NewKey()
	joined := d.join(t, key)
	if joined.ClockOffsetAt.IsZero() || joined.ClockOffset > -7*time.Minute || joined.ClockOffset < -9*time.Minute || len(d.store.joins) != 1 {
		t.Fatalf("one corrected retry: %+v, %d joins", joined, len(d.store.joins))
	}
	d.setOffset(3 * time.Hour)
	origin, _ := JoinServer(context.Background(), d.ts.URL, nil)
	_, err := Join(context.Background(), origin, key, JoinRequest{Email: "ada@example.com", Code: "CBJ-4K7P-WQ9D-2MXH-T6RB", Label: "l", KeyStore: "keyring"}, nil)
	var clock *devicesign.ClockError
	if !errors.As(err, &clock) {
		t.Fatal(err)
	}
}

// A server that keeps refusing for skew gets exactly one corrected retry.
func TestDeviceSkewRetriesOnlyOnce(t *testing.T) {
	key, _, _ := devicesign.NewKey()
	var calls int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"clock_skew","message":"skewed","server_time":` + strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10) + `}}`))
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	c, err := NewWithSigner(Config{URL: ts.URL, Principal: "ada@example.com", CachePath: "/tmp/x"}, devicesign.NewSigner(key, Origin(u), 0, time.Time{}), ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "acme", Project: "p"})
	var refused *HTTPError
	if !errors.As(err, &refused) || refused.Code != "clock_skew" || calls != 2 {
		t.Fatal(calls, err)
	}
}

func TestJoinServerRefusesAnotherOrigin(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"browser_client_id":"","browser_client_secret":"","device_client_id":"","device_client_secret":"","auth_methods":["device"],"public_origin":"https://cb.example.test"}`))
	}))
	defer ts.Close()
	if _, err := JoinServer(context.Background(), ts.URL, nil); err == nil || !strings.Contains(err.Error(), "join with --server https://cb.example.test") {
		t.Fatal(err)
	}
}

// A signer for one server can't be used with another.
func TestDeviceSignerIsBoundToItsServer(t *testing.T) {
	key, _, _ := devicesign.NewKey()
	signer := devicesign.NewSigner(key, "https://cb.example.test", 0, time.Time{})
	if _, err := NewWithSigner(Config{URL: "https://other.example.test", Principal: "ada@example.com"}, signer, http.DefaultClient); err == nil {
		t.Fatal("a signer for another origin was accepted")
	}
	if _, err := NewWithSigner(Config{URL: "https://cb.example.test/prefix", Principal: "ada@example.com"}, signer, http.DefaultClient); err == nil {
		t.Fatal("a server URL with a path was accepted")
	}
	if _, err := NewWithSigner(Config{URL: "https://cb.example.test", Principal: "ada@example.com"}, signer, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
}

// R85-M2: a device route has no audience, so its mirror is keyed by the
// device: sync publishes a snapshot, an outage reads it, and another
// device's route never does.
func TestDeviceRouteSyncsAndReadsItsMirror(t *testing.T) {
	ctx := context.Background()
	var down atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		var request bus.Request
		_ = json.NewDecoder(r.Body).Decode(&request)
		data := map[string]any{}
		switch request.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws_1", "project_id": "prj_1"}
		case "snapshot":
			data = map[string]any{"workspace": map[string]any{"id": "ws_1"}, "project": map[string]any{"id": "prj_1"}, "session": map[string]any{"id": "ses_1", "name": "worker", "instance": "instance-1"}, "principal": "ada@example.com", "verified": true, "snapshot_at": time.Now().UTC().Format(time.RFC3339Nano), "sessions": []any{}, "messages": []any{map[string]any{"sequence": 1, "id": "msg_1", "body": "cached", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}}, "sent_messages": []any{}, "events": []any{}}
		}
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: data})
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	cache := filepath.Join(t.TempDir(), "cache.db")
	device := func() *Client {
		key, _, _ := devicesign.NewKey()
		c, err := NewWithSigner(Config{URL: ts.URL, Principal: "ada@example.com", CachePath: cache}, devicesign.NewSigner(key, Origin(u), 0, time.Time{}), ts.Client())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := device()
	if _, err := c.Sync(ctx, "team", "app", "worker", "instance-1"); err != nil {
		t.Fatal("a device route could not sync:", err)
	}
	down.Store(true)
	inbox := bus.Request{Operation: "inbox", Workspace: "ws_1", Project: "prj_1", Session: "worker", Args: map[string]any{"instance": "instance-1"}}
	if r, err := c.Execute(ctx, inbox); err != nil || r.Source != "offline-mirror" {
		t.Fatalf("offline read on a device route: %+v %v", r, err)
	}
	if r, err := device().Execute(ctx, inbox); err == nil {
		t.Fatalf("another device read this device's mirror: %+v", r)
	}
}

// A transport failure to the invite's server names the host, not OAuth; a
// refused answer keeps the sign-in-settings wording.
func TestJoinServerUnreachableNamesTheHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = JoinServer(context.Background(), "http://"+addr, nil)
	want := "cannot reach " + addr + ": check the invite link's address and your network"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if _, err := JoinServer(context.Background(), bad.URL, nil); err == nil || !strings.Contains(err.Error(), "cannot read the server's sign-in settings") {
		t.Fatal(err)
	}
}
