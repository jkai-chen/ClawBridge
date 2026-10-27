package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
)

const testOrigin = "https://cb.example.com"

// fakeDevices is the hosted authority's device side, in memory.
type fakeDevices struct {
	mu      sync.Mutex
	devices map[string]bus.Device
	nonces  map[string]bool
	lookups int
	joins   []bus.JoinRequest
	joinErr error
	// dormant devices are refused at the nonce, as the bus does (§17.4).
	dormant map[string]bool
}

func (f *fakeDevices) DeviceByID(_ context.Context, id string) (bus.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	d, ok := f.devices[id]
	if !ok {
		return bus.Device{}, errors.New("not found")
	}
	return d, nil
}

func (f *fakeDevices) UseNonce(_ context.Context, device, nonce string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dormant[device] {
		return &bus.DormantError{Device: device, Days: 90}
	}
	if f.nonces[device+" "+nonce] {
		return bus.ErrNonceReused
	}
	f.nonces[device+" "+nonce] = true
	return nil
}

func (f *fakeDevices) Join(_ context.Context, in bus.JoinRequest) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, in)
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	return map[string]any{"device_id": "dev_joined", "principal": in.Email}, nil
}

// identityRecorder is a hosted executor that records who called.
type identityRecorder struct {
	mu  sync.Mutex
	ids []bus.Identity
}

func (e *identityRecorder) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("unexpected unbound execution")
}

func (e *identityRecorder) ExecuteAsIdentity(_ context.Context, id bus.Identity, _ bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, id)
	return bus.Result{Data: map[string]any{}}, nil
}

type deviceFixture struct {
	t       *testing.T
	store   *fakeDevices
	exec    *identityRecorder
	handler http.Handler
	priv    ed25519.PrivateKey
	id      string
	now     time.Time
}

func newDeviceFixture(t *testing.T, methods ...string) *deviceFixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	f := &deviceFixture{t: t, priv: priv, id: devicekey.ID(pub), now: time.Unix(1761498000, 0)}
	f.store = &fakeDevices{devices: map[string]bus.Device{f.id: {ID: f.id, Principal: "ada@example.com", Kind: "device", PublicKey: pub, Live: true}}, nonces: map[string]bool{}}
	f.exec = &identityRecorder{}
	if len(methods) == 0 {
		methods = []string{AuthGoogle, AuthDevice}
	}
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		if token != "good-google-token" {
			return Identity{}, errors.New("bad token")
		}
		return Identity{Principal: "bob@example.com", Subject: "g-bob"}, nil
	})
	f.handler = NewHandlerWithOptions(f.exec, verifier, HandlerOptions{Auth: AuthOptions{Methods: methods, PublicOrigin: testOrigin, Devices: f.store}, Now: func() time.Time { return f.now }})
	return f
}

func nonce(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// signed is a request signed with a base that may differ from what is sent.
type signed struct {
	method, origin, target, body string
	ts                           int64
	nonce                        string
}

func (f *deviceFixture) header(s signed) string {
	if s.method == "" {
		s.method = http.MethodPost
	}
	if s.origin == "" {
		s.origin = testOrigin
	}
	if s.ts == 0 {
		s.ts = f.now.Unix()
	}
	if s.nonce == "" {
		s.nonce = nonce(16)
	}
	ts := strconv.FormatInt(s.ts, 10)
	sig := ed25519.Sign(f.priv, devicekey.RequestBase(s.method, s.origin, s.target, ts, s.nonce, []byte(s.body)))
	return fmt.Sprintf("%s v1 device=%s, ts=%s, nonce=%s, sig=%s", DeviceScheme, f.id, ts, s.nonce, base64.RawURLEncoding.EncodeToString(sig))
}

func (f *deviceFixture) send(method, target, body, authorization string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.7:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	for _, e := range edit {
		e(r)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

const whoami = `{"operation":"whoami","workspace":"acme"}`

// do signs and sends a request exactly as signed.
func (f *deviceFixture) do(target, body string) *httptest.ResponseRecorder {
	return f.send(http.MethodPost, target, body, f.header(signed{target: target, body: body}))
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Error.Code
}

// A signed request reaches the bus as the device's identity, and whoami
// says so. A target with an escaped space is signed and sent verbatim.
func TestDeviceSignedRequestIsTheDevicesIdentity(t *testing.T) {
	f := newDeviceFixture(t)
	w := f.do("/v1/execute?note=a%20b", whoami)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	got := f.exec.ids[0]
	if got.Issuer != bus.DeviceIssuer || got.Subject != f.id || got.Principal != "ada@example.com" {
		t.Fatalf("%+v", got)
	}
	var out struct{ Data map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Data["identity_provenance"] != "device-key" || out.Data["device_id"] != f.id {
		t.Fatal(out.Data)
	}
	// Google still signs in alongside.
	if w = f.send(http.MethodPost, "/v1/execute", whoami, "Bearer good-google-token"); w.Code != 200 || f.exec.ids[1].Issuer != bus.GoogleIssuer {
		t.Fatal(w.Code, w.Body.String())
	}
}

// Every signed part is checked: body, path, query, method and origin; the
// origin is the configured one, never Host or X-Forwarded-Host.
func TestDeviceSignatureCoversTheRequest(t *testing.T) {
	f := newDeviceFixture(t)
	for name, c := range map[string]struct {
		sent, signed signed
	}{
		"body":   {signed{target: "/v1/execute", body: whoami}, signed{target: "/v1/execute", body: `{"operation":"whoami","workspace":"evil"}`}},
		"query":  {signed{target: "/v1/execute?a=1", body: whoami}, signed{target: "/v1/execute?a=2", body: whoami}},
		"path":   {signed{target: "/v1/execute", body: whoami}, signed{target: "/v1/join", body: whoami}},
		"method": {signed{target: "/v1/execute", body: whoami}, signed{method: http.MethodPut, target: "/v1/execute", body: whoami}},
		"origin": {signed{target: "/v1/execute", body: whoami}, signed{origin: "https://other.example.com", target: "/v1/execute", body: whoami}},
	} {
		if w := f.send(http.MethodPost, c.sent.target, c.sent.body, f.header(c.signed)); w.Code != 401 {
			t.Errorf("%s: tampered request answered %d", name, w.Code)
		}
	}
	w := f.send(http.MethodPost, "/v1/execute", whoami, f.header(signed{target: "/v1/execute", body: whoami}), func(r *http.Request) {
		r.Host = "evil.example.com"
		r.Header.Set("X-Forwarded-Host", "evil.example.com")
		r.Header.Set("X-Forwarded-Proto", "http")
	})
	if w.Code != 200 {
		t.Fatal("a Host change altered the origin:", w.Code, w.Body.String())
	}
}

// A target has one spelling (AU-M1): anything else is refused before the
// signature is even checked.
func TestCanonicalTarget(t *testing.T) {
	for target, want := range map[string]bool{
		"/v1/execute": true, "/": true, "/v1/execute?a=%20b&c=%C3%A9": true, "/v1/execute?": true,
		"": false, "v1/execute": false, "http://cb.example.com/v1/execute": false, "//v1/execute": false,
		"/v1//execute": false, "/v1/./execute": false, "/v1/../v1/execute": false, "/v1/execute/": false,
		"/v1/execute?a=%2F": false, "/v1/execute?a=%2f": false, "/v1/execute?a=%e2": false, "/v1/execute?a=%zz": false,
		"/v1/execute?a=%2": false, "/v1/%2E%2E/x": false, "/v1/execute?a=%5C": false, "/v1/execute?a=%0A": false,
		"/v1/execute#x": false, "/v1\\execute": false, "/v1/execute?a=b c": false, "/v1/exécute": false,
	} {
		if canonicalTarget(target) != want {
			t.Errorf("%q: want %v", target, want)
		}
	}
	f := newDeviceFixture(t)
	for _, target := range []string{"/v1/execute?a=%2f", "/v1/execute?a=%2F"} {
		if w := f.do(target, whoami); w.Code != 401 {
			t.Errorf("%s answered %d", target, w.Code)
		}
	}
}

// The clock, the nonce and the device's state each refuse.
func TestDeviceRequestRefusals(t *testing.T) {
	f := newDeviceFixture(t)
	for _, skew := range []int64{-301, 301} {
		w := f.send(http.MethodPost, "/v1/execute", whoami, f.header(signed{target: "/v1/execute", body: whoami, ts: f.now.Unix() + skew}))
		var b struct {
			Error struct {
				Code       string
				ServerTime int64 `json:"server_time"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != 401 || b.Error.Code != "clock_skew" || b.Error.ServerTime != f.now.Unix() {
			t.Fatal("skew", skew, w.Code, w.Body.String())
		}
	}
	if w := f.send(http.MethodPost, "/v1/execute", whoami, f.header(signed{target: "/v1/execute", body: whoami, ts: f.now.Unix() - 299})); w.Code != 200 {
		t.Fatal("inside the window:", w.Code)
	}
	n := nonce(16)
	if w := f.send(http.MethodPost, "/v1/execute", whoami, f.header(signed{target: "/v1/execute", body: whoami, nonce: n})); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := f.send(http.MethodPost, "/v1/execute", whoami, f.header(signed{target: "/v1/execute", body: whoami, nonce: n})); w.Code != 401 || !strings.Contains(w.Body.String(), "nonce") {
		t.Fatal("a nonce was reused:", w.Code, w.Body.String())
	}
	for _, size := range []int{15, 65} {
		if w := f.send(http.MethodPost, "/v1/execute", whoami, f.header(signed{target: "/v1/execute", body: whoami, nonce: nonce(size)})); w.Code != 401 {
			t.Fatal("nonce of", size, "bytes:", w.Code)
		}
	}
	// A header with a missing, doubled or extra field.
	good := f.header(signed{target: "/v1/execute", body: whoami})
	for _, bad := range []string{
		strings.Replace(good, " v1 ", " v2 ", 1),
		strings.Replace(good, ", ts=", ", nonce=AAAAAAAAAAAAAAAAAAAAAA, ts=", 1),
		good + ", extra=1",
		strings.Replace(good, "ts=", "ts=0", 1),
	} {
		if w := f.send(http.MethodPost, "/v1/execute", whoami, bad); w.Code != 401 {
			t.Fatal("header accepted:", bad, w.Code)
		}
	}
	// An unknown device, and a revoked one (said only once the key is proven).
	other, otherPriv, _ := ed25519.GenerateKey(nil)
	g := *f
	g.priv, g.id = otherPriv, devicekey.ID(other)
	if w := g.do("/v1/execute", whoami); w.Code != 401 || strings.Contains(w.Body.String(), "revoked") {
		t.Fatal("unknown device:", w.Code, w.Body.String())
	}
	f.store.devices[f.id] = bus.Device{ID: f.id, Principal: "ada@example.com", PublicKey: f.store.devices[f.id].PublicKey, Live: false}
	f.now = f.now.Add(deviceCacheTTL)
	if w := f.do("/v1/execute", whoami); w.Code != 401 || !strings.Contains(w.Body.String(), "revoked") {
		t.Fatal("revoked device:", w.Code, w.Body.String())
	}
}

// §17.4: a dormant device is told so, once its key is proven, and how to
// wake it.
func TestDormantDeviceIsTold(t *testing.T) {
	f := newDeviceFixture(t)
	f.store.dormant = map[string]bool{f.id: true}
	w := f.do("/v1/execute", whoami)
	var b struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if w.Code != 401 || b.Error.Code != "unauthenticated" || !strings.HasPrefix(b.Error.Message, "device_dormant: this device hasn't been used for 90 days") || !strings.Contains(b.Error.Message, "clawbridge device confirm "+f.id) {
		t.Fatal(w.Code, w.Body.String())
	}
	other, otherPriv, _ := ed25519.GenerateKey(nil)
	g := *f
	g.priv, g.id = otherPriv, devicekey.ID(other)
	f.store.dormant[g.id] = true
	if w = g.do("/v1/execute", whoami); strings.Contains(w.Body.String(), "dormant") {
		t.Fatal("an unproven key learned of dormancy:", w.Body.String())
	}
}

// A device's key is cached for 30 s, and a revocation through this server
// clears the cache at once.
func TestDeviceCache(t *testing.T) {
	f := newDeviceFixture(t)
	f.do("/v1/execute", whoami)
	f.do("/v1/execute", whoami)
	if f.store.lookups != 1 {
		t.Fatal("lookups", f.store.lookups)
	}
	f.now = f.now.Add(deviceCacheTTL)
	f.do("/v1/execute", whoami)
	if f.store.lookups != 2 {
		t.Fatal("the cache outlived 30 s", f.store.lookups)
	}
	if w := f.do("/v1/execute", `{"operation":"device_revoke","workspace":"acme","args":{"device_id":"dev_x"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.do("/v1/execute", whoami)
	if f.store.lookups != 3 {
		t.Fatal("a revocation left the cache", f.store.lookups)
	}
}

// --auth chooses what is accepted, and /v1/config says so.
func TestAuthMethods(t *testing.T) {
	device := newDeviceFixture(t, AuthDevice)
	if w := device.send(http.MethodPost, "/v1/execute", whoami, "Bearer good-google-token"); w.Code != 401 || !strings.Contains(w.Body.String(), "device keys only") {
		t.Fatal("--auth device took a Google token:", w.Code, w.Body.String())
	}
	if w := device.do("/v1/execute", whoami); w.Code != 200 {
		t.Fatal(w.Code)
	}
	google := newDeviceFixture(t, AuthGoogle)
	if w := google.do("/v1/execute", whoami); w.Code != 401 {
		t.Fatal("--auth google took a device signature:", w.Code)
	}
	if w := google.send(http.MethodPost, "/v1/join", "{}", ""); w.Code != 404 {
		t.Fatal("--auth google serves join:", w.Code)
	}
	w := device.send(http.MethodGet, "/v1/config", "", "")
	var config map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &config)
	if fmt.Sprint(config["auth_methods"]) != "[device]" || config["public_origin"] != testOrigin {
		t.Fatal(config)
	}
	// A Google-only server names no methods and no origin (R78-H1).
	w = google.send(http.MethodGet, "/v1/config", "", "")
	config = nil
	_ = json.Unmarshal(w.Body.Bytes(), &config)
	if _, ok := config["auth_methods"]; ok || len(config) != 4 {
		t.Fatal("a Google-only server advertised sign-in methods:", config)
	}
	both := newDeviceFixture(t, AuthGoogle, AuthDevice)
	w = both.send(http.MethodGet, "/v1/config", "", "")
	config = nil
	_ = json.Unmarshal(w.Body.Bytes(), &config)
	if fmt.Sprint(config["auth_methods"]) != "[google device]" || config["public_origin"] != testOrigin {
		t.Fatal(config)
	}
	for in, want := range map[string]string{"": "error", "google": "[google]", "device": "[device]", "google,device": "[google device]", " device , google ": "[device google]", "google,google": "error", "saml": "error"} {
		methods, err := authMethods(in, false, false)
		if got := fmt.Sprint(methods); (err != nil && want != "error") || (err == nil && got != want) {
			t.Errorf("--auth %q: %v %v", in, methods, err)
		}
	}
	for _, c := range []struct {
		audience, origin bool
		want             string
	}{{true, false, "[google]"}, {false, true, "[device]"}, {true, true, "[google device]"}} {
		if methods, _ := authMethods("", c.audience, c.origin); fmt.Sprint(methods) != c.want {
			t.Errorf("default with audience=%v origin=%v: %v", c.audience, c.origin, methods)
		}
	}
	for in, want := range map[string]string{"https://CB.example.com": "https://cb.example.com", "https://cb.example.com:8443/": "https://cb.example.com:8443", "http://127.0.0.1:8080": "http://127.0.0.1:8080", "http://cb.example.com": "", "https://cb.example.com/x": "", "https://u@cb.example.com": "", "https://cb.example.com?a": "", "cb.example.com": ""} {
		got, err := ParsePublicOrigin(in)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
}

// Sixty failed sign-ins from one address in a minute lock it out until the
// minute passes, even with a good signature.
func TestFailedAuthLimiter(t *testing.T) {
	f := newDeviceFixture(t)
	for i := 0; i < failedAuthPerMin; i++ {
		f.send(http.MethodPost, "/v1/execute", whoami, "Bearer wrong")
	}
	if w := f.do("/v1/execute", whoami); w.Code != 429 {
		t.Fatal("not limited:", w.Code)
	}
	f.now = f.now.Add(time.Minute)
	if w := f.do("/v1/execute", whoami); w.Code != 200 {
		t.Fatal("still limited:", w.Code)
	}
}

func (f *deviceFixture) joinBody(email, code string, ts int64, priv ed25519.PrivateKey) string {
	pub := priv.Public().(ed25519.PublicKey)
	normal, _ := devicekey.NormalCode(code)
	tsText := strconv.FormatInt(ts, 10)
	proof := ed25519.Sign(priv, devicekey.JoinProofBase(testOrigin, email, normal, tsText))
	b, _ := json.Marshal(map[string]string{"email": email, "code": code, "public_key": devicekey.EncodePublicKey(pub), "label": "laptop", "key_store": "keyring", "ts": tsText, "proof": base64.RawURLEncoding.EncodeToString(proof)})
	return string(b)
}

func (f *deviceFixture) postJoin(body, address string) *httptest.ResponseRecorder {
	return f.send(http.MethodPost, "/v1/join", body, "", func(r *http.Request) { r.RemoteAddr = address + ":5000" })
}

// Join checks the proof before the store, answers every failure alike after
// the time floor, and is limited per address and per email.
func TestJoinEndpoint(t *testing.T) {
	joinFloor = 40 * time.Millisecond
	t.Cleanup(func() { joinFloor = 250 * time.Millisecond })
	f := newDeviceFixture(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	code := devicekey.NewCode()
	start := time.Now()
	w := f.postJoin(f.joinBody("ada@example.com", code, f.now.Unix(), priv), "198.51.100.1")
	if w.Code != 200 || time.Since(start) < joinFloor || len(f.store.joins) != 1 || f.store.joins[0].Code != code {
		t.Fatal(w.Code, w.Body.String())
	}
	var ok map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &ok)
	if ok["server_time"] != float64(f.now.Unix()) {
		t.Fatal(ok)
	}
	// A proof over another origin, email or code never reaches the store.
	_, other, _ := ed25519.GenerateKey(nil)
	forged := map[string]any{}
	_ = json.Unmarshal([]byte(f.joinBody("ada@example.com", code, f.now.Unix(), priv)), &forged)
	forged["public_key"] = devicekey.EncodePublicKey(other.Public().(ed25519.PublicKey))
	b, _ := json.Marshal(forged)
	invalid := f.postJoin(string(b), "198.51.100.2")
	if invalid.Code != 403 || !strings.Contains(invalid.Body.String(), "invalid or expired invite") || len(f.store.joins) != 1 {
		t.Fatal("a forged proof:", invalid.Code, invalid.Body.String())
	}
	f.store.joinErr = bus.ErrInvalidInvite
	start = time.Now()
	if w = f.postJoin(f.joinBody("bob@example.com", code, f.now.Unix(), priv), "198.51.100.3"); w.Body.String() != invalid.Body.String() || w.Code != 403 || time.Since(start) < joinFloor {
		t.Fatal("the store's refusal differs:", w.Code, w.Body.String())
	}
	if w = f.postJoin(f.joinBody("cara@example.com", code, f.now.Unix()-301, priv), "198.51.100.4"); w.Code != 401 || errorCode(t, w) != "clock_skew" || !strings.Contains(w.Body.String(), "server_time") {
		t.Fatal("join skew:", w.Code, w.Body.String())
	}
	if w = f.postJoin(`{"email":"x@example.com","extra":1}`, "198.51.100.5"); w.Code != 400 {
		t.Fatal("unknown field:", w.Code)
	}
	if w = f.send(http.MethodGet, "/v1/join", "", ""); w.Code != 405 {
		t.Fatal("GET join:", w.Code)
	}
	// Ten per address an hour, five per email.
	for i := 0; i < joinPerAddress; i++ {
		f.postJoin(f.joinBody(fmt.Sprintf("u%d@example.com", i), code, f.now.Unix(), priv), "198.51.100.9")
	}
	if w = f.postJoin(f.joinBody("dan@example.com", code, f.now.Unix(), priv), "198.51.100.9"); w.Code != 429 {
		t.Fatal("per-address limit:", w.Code)
	}
	for i := 0; i < joinPerEmail; i++ {
		f.postJoin(f.joinBody("eve@example.com", code, f.now.Unix(), priv), fmt.Sprintf("192.0.2.%d", i+1))
	}
	if w = f.postJoin(f.joinBody("EVE@example.com", code, f.now.Unix(), priv), "192.0.2.99"); w.Code != 429 {
		t.Fatal("per-email limit:", w.Code)
	}
	f.now = f.now.Add(time.Hour)
	if w = f.postJoin(f.joinBody("eve@example.com", code, f.now.Unix(), priv), "192.0.2.99"); w.Code == 429 {
		t.Fatal("the limit outlived its hour")
	}
}

// End to end on Postgres: an invite joins through /v1/join, the device signs
// a request whose target has an escaped space, and a revocation through the
// server refuses the device at once.
func TestPostgresDeviceJoinAndSignedRequest(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	joinFloor = time.Millisecond
	t.Cleanup(func() { joinFloor = 250 * time.Millisecond })
	ctx := context.Background()
	owner := awUnique("owner") + "@device-test.example"
	store, err := bus.OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	google := bus.Identity{Principal: owner, Issuer: bus.GoogleIssuer, Subject: "sub-" + owner}
	ws := awUnique("devws")
	for _, r := range []bus.Request{{Operation: "workspace_init", Workspace: ws, Args: map[string]any{"mode": "server"}}, {Operation: "project_create", Workspace: ws, Args: map[string]any{"slug": "p"}}} {
		if _, err = store.ExecuteAsIdentity(ctx, google, r); err != nil {
			t.Fatal(err)
		}
	}
	ada := awUnique("ada") + "@device-test.example"
	invite, err := store.ExecuteAsIdentity(ctx, google, bus.Request{Operation: "invite_create", Workspace: ws, Args: map[string]any{"email": ada, "grants": map[string]any{"p": "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := newPGDeviceClient(t, store)
	w := f.postJoin(f.joinBody(ada, invite.Data.(map[string]any)["code"].(string), f.now.Unix(), f.priv), "198.51.100.20")
	if w.Code != 200 || !strings.Contains(w.Body.String(), f.id) {
		t.Fatal(w.Code, w.Body.String())
	}
	list := fmt.Sprintf(`{"operation":"list_projects","workspace":%q}`, ws)
	if w = f.do("/v1/execute?note=a%20b", list); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = f.do("/v1/execute", fmt.Sprintf(`{"operation":"whoami","workspace":%q,"project":"p"}`, ws)); w.Code != 200 || !strings.Contains(w.Body.String(), `"identity_provenance":"device-key"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = f.do("/v1/execute", fmt.Sprintf(`{"operation":"device_revoke","workspace":%q,"args":{"device_id":%q}}`, ws, f.id)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = f.do("/v1/execute", list); w.Code != 401 || !strings.Contains(w.Body.String(), "revoked") {
		t.Fatal("a revoked device signed in:", w.Code, w.Body.String())
	}
}

// newPGDeviceClient is a fresh device key against a device-only handler
// over a real store.
func newPGDeviceClient(t *testing.T, store *bus.Store) *deviceFixture {
	f := &deviceFixture{t: t, now: time.Now()}
	pub, priv, _ := ed25519.GenerateKey(nil)
	f.priv, f.id = priv, devicekey.ID(pub)
	f.handler = NewHandlerWithOptions(store, nil, HandlerOptions{Auth: AuthOptions{Methods: []string{AuthDevice}, PublicOrigin: testOrigin, Devices: store}, Now: time.Now})
	return f
}

// countingRecorder is a hosted store with pending device events.
type countingRecorder struct{ identityRecorder }

func (e *countingRecorder) PendingDeviceEvents(_ context.Context, device string) (int, error) {
	return len(device) % 7, nil
}

// Every answer to a device-signed request carries the device's pending
// device events (§17.3, R7); a Google sign-in's carries none.
func TestDeviceAnswerCarriesPendingEvents(t *testing.T) {
	f := newDeviceFixture(t)
	exec := &countingRecorder{}
	f.handler = NewHandlerWithOptions(exec, VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: "bob@example.com", Subject: "g-bob"}, nil
	}), HandlerOptions{Auth: AuthOptions{Methods: []string{AuthGoogle, AuthDevice}, PublicOrigin: testOrigin, Devices: f.store}, Now: func() time.Time { return f.now }})
	w := f.do("/v1/execute", whoami)
	var out struct {
		Pending *int `json:"pending_device_events"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Pending == nil || *out.Pending != len(f.id)%7 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = f.send(http.MethodPost, "/v1/execute", whoami, "Bearer good-google-token"); w.Code != 200 || strings.Contains(w.Body.String(), "pending_device_events") {
		t.Fatal(w.Code, w.Body.String())
	}
}

// An invite link opened in a browser gets a static page: no script, a CSP
// that allows none, no referrer; the fragment never reaches the server.
func TestJoinPageIsStatic(t *testing.T) {
	f := newDeviceFixture(t)
	w := f.send(http.MethodGet, "/join", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "clawbridge join") || strings.Contains(strings.ToLower(w.Body.String()), "<script") {
		t.Fatal(w.Code, w.Body.String())
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Fatal(csp)
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Header())
	}
	if w = f.send(http.MethodPost, "/join", "x", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
}
