package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/principal"
)

// ClawBridge's own sign-in on the server (live/AUTH-INVITE-DESIGN.md §4, §5,
// §7, §8): requests signed by a device key, and the join that binds one.

// The sign-in methods a server accepts (§10).
const (
	AuthGoogle = "google"
	AuthDevice = "device"
)

// DeviceScheme is the Authorization scheme of a device-signed request.
const DeviceScheme = "ClawBridge-Device"

const (
	clockSkew         = 300 * time.Second
	deviceCacheTTL    = 30 * time.Second
	deviceCacheMax    = 10000
	nonceMinBytes     = 16
	nonceMaxBytes     = 64
	maxTargetLength   = 2048
	maxDeviceHeader   = 1024
	failedAuthPerMin  = 60
	joinPerAddress    = 10
	joinPerEmail      = 5
	maxJoinBody       = 8 << 10
	authFailureLogged = time.Hour
)

// joinFloor is the least time any join answer takes, so a failure's reason
// can't be told by its timing (§4). Tests shorten it.
var joinFloor = 250 * time.Millisecond

// DeviceStore is what device sign-in needs from the hosted authority.
type DeviceStore interface {
	DeviceByID(context.Context, string) (bus.Device, error)
	UseNonce(context.Context, string, string) error
	Join(context.Context, bus.JoinRequest) (map[string]any, error)
}

var _ DeviceStore = (*bus.Store)(nil)

// AuthOptions choose how /v1/execute proves its caller.
type AuthOptions struct {
	// Methods are the accepted sign-in methods; empty means Google only.
	Methods []string
	// PublicOrigin is the server's configured origin (serve --public-origin),
	// which device signatures and join proofs cover. It is never read from
	// Host or X-Forwarded-*.
	PublicOrigin string
	// Devices is the authority for device keys; device sign-in needs it.
	Devices DeviceStore
}

func (o AuthOptions) accepts(method string) bool {
	if len(o.Methods) == 0 {
		return method == AuthGoogle
	}
	for _, m := range o.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// authFailure is a refused authentication, answered as is.
type authFailure struct {
	status        int
	code, message string
	// kind names the failure in the rate-limited log; never the caller's
	// input.
	kind       string
	serverTime bool
}

// Authenticator proves a request's caller from its Authorization
// credentials (after the scheme) and its raw body. The handler asks only
// for a bus.Identity, so a method can be removed by deleting its
// Authenticator (§10.1).
type Authenticator interface {
	Scheme() string
	Authenticate(ctx context.Context, r *http.Request, credentials string, body []byte) (bus.Identity, *authFailure)
}

// googleAuthenticator accepts a Google ID token as a Bearer credential.
type googleAuthenticator struct{ verifier Verifier }

func (googleAuthenticator) Scheme() string { return "Bearer" }

func (g googleAuthenticator) Authenticate(ctx context.Context, _ *http.Request, credentials string, _ []byte) (bus.Identity, *authFailure) {
	if g.verifier == nil || credentials == "" || strings.ContainsAny(credentials, " \t") || len(credentials) > 16384 {
		return bus.Identity{}, &authFailure{status: 401, code: "unauthenticated", message: "a valid Google ID token is required", kind: "google_token"}
	}
	identity, err := g.verifier.Verify(ctx, credentials)
	if err != nil || !principal.Valid(identity.Principal) || identity.Subject == "" {
		return bus.Identity{}, &authFailure{status: 401, code: "unauthenticated", message: "Google identity verification failed", kind: "google_token"}
	}
	return bus.Identity{Principal: identity.Principal, Issuer: bus.GoogleIssuer, Subject: identity.Subject, HostedDomain: identity.HostedDomain, Expires: identity.Expires}, nil
}

// deviceAuthenticator verifies a request signed by a device key (§5).
type deviceAuthenticator struct {
	origin string
	store  DeviceStore
	cache  *deviceCache
	now    func() time.Time
}

func (deviceAuthenticator) Scheme() string { return DeviceScheme }

func deviceRefused(message, kind string) *authFailure {
	return &authFailure{status: 401, code: "unauthenticated", message: message, kind: kind}
}

func (d deviceAuthenticator) Authenticate(ctx context.Context, r *http.Request, credentials string, body []byte) (bus.Identity, *authFailure) {
	// 1. The header parses, and the target is the one canonical spelling.
	h, ok := parseDeviceHeader(credentials)
	if !ok {
		return bus.Identity{}, deviceRefused("a valid ClawBridge-Device v1 header is required", "device_header")
	}
	if !canonicalTarget(r.RequestURI) {
		return bus.Identity{}, deviceRefused("the request target must be written in its one canonical form", "device_target")
	}
	// 2. The device exists.
	device, err := d.cache.get(ctx, d.store, h.device)
	if err != nil {
		return bus.Identity{}, deviceRefused("device signature verification failed", "device_unknown")
	}
	// 3. The clock: a refusal carries the server's time for one corrected
	// retry (§7).
	if skew := d.now().Sub(time.Unix(h.ts, 0)); skew > clockSkew || skew < -clockSkew {
		return bus.Identity{}, &authFailure{status: 401, code: "clock_skew", message: "the request time is more than 300 s from the server's; correct by server_time and retry once", kind: "clock_skew", serverTime: true}
	}
	// 4. The signature covers the method, the configured origin, the raw
	// target, the timestamp, the nonce and the body.
	if !ed25519.Verify(device.PublicKey, devicekey.RequestBase(r.Method, d.origin, r.RequestURI, h.tsText, h.nonce, body), h.sig) {
		return bus.Identity{}, deviceRefused("device signature verification failed", "device_signature")
	}
	// The key is proven, so its holder may learn it was revoked.
	if !device.Live {
		return bus.Identity{}, deviceRefused("this device was revoked or has expired; ask an owner for an invite", "device_revoked")
	}
	// 5. The nonce is unused for this device, and the device isn't dormant.
	if err = d.store.UseNonce(ctx, device.ID, h.nonce); err != nil {
		if errors.Is(err, bus.ErrNonceReused) {
			return bus.Identity{}, deviceRefused("request nonce already used", "device_nonce")
		}
		// A dormant device (§17.4) is told how to wake it; it holds
		// everything it had.
		var dormant *bus.DormantError
		if errors.As(err, &dormant) {
			return bus.Identity{}, deviceRefused(dormant.Error(), "device_dormant")
		}
		return bus.Identity{}, &authFailure{status: 503, code: "unavailable", message: publicMessage("unavailable"), kind: "device_store"}
	}
	// 6. The bus re-checks, in the request's own transaction, that the
	// device is live, the principal's, and of the request's workspace.
	return bus.Identity{Principal: device.Principal, Issuer: bus.DeviceIssuer, Subject: device.ID}, nil
}

type deviceHeader struct {
	device, tsText, nonce string
	ts                    int64
	sig                   []byte
}

// parseDeviceHeader reads "v1 device=…, ts=…, nonce=…, sig=…": each field
// exactly once, in any order, and nothing else.
func parseDeviceHeader(credentials string) (deviceHeader, bool) {
	var h deviceHeader
	if len(credentials) > maxDeviceHeader {
		return h, false
	}
	version, rest, ok := strings.Cut(credentials, " ")
	if !ok || version != "v1" {
		return h, false
	}
	seen := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || v == "" || seen[k] != "" {
			return h, false
		}
		seen[k] = v
	}
	if len(seen) != 4 {
		return h, false
	}
	h.device, h.tsText, h.nonce = seen["device"], seen["ts"], seen["nonce"]
	if !strings.HasPrefix(h.device, "dev_") || len(h.device) != len("dev_")+26 {
		return h, false
	}
	if h.tsText == "" || len(h.tsText) > 12 || strings.TrimLeft(h.tsText, "0123456789") != "" || (h.tsText[0] == '0' && h.tsText != "0") {
		return h, false
	}
	var err error
	if h.ts, err = strconv.ParseInt(h.tsText, 10, 64); err != nil {
		return h, false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(h.nonce)
	if err != nil || len(nonce) < nonceMinBytes || len(nonce) > nonceMaxBytes {
		return h, false
	}
	if h.sig, err = base64.RawURLEncoding.DecodeString(seen["sig"]); err != nil || len(h.sig) != ed25519.SignatureSize {
		return h, false
	}
	return h, true
}

// canonicalTarget admits a request target with exactly one spelling (AU-M1):
// origin-form, printable ASCII, uppercase percent-escapes of nothing that
// changes the path (no %2F, %5C or %2E), and no empty, "." or ".." path
// segment.
func canonicalTarget(target string) bool {
	if target == "" || target[0] != '/' || len(target) > maxTargetLength {
		return false
	}
	for i := 0; i < len(target); i++ {
		c := target[i]
		if c <= 0x20 || c >= 0x7f || c == '#' || c == '\\' {
			return false
		}
		if c != '%' {
			continue
		}
		if i+2 >= len(target) || !upperHex(target[i+1]) || !upperHex(target[i+2]) {
			return false
		}
		switch escaped := target[i+1 : i+3]; {
		case escaped == "2F", escaped == "5C", escaped == "2E", escaped < "20", escaped == "7F":
			return false
		}
	}
	path, _, _ := strings.Cut(target, "?")
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "." || segment == ".." || (segment == "" && path != "/") {
			return false
		}
	}
	return true
}

func upperHex(c byte) bool { return ('0' <= c && c <= '9') || ('A' <= c && c <= 'F') }

// deviceCache keeps a device's key and status for 30 s (§5). The bus
// re-checks the device in every request's transaction, so a revocation
// binds at once on every replica; the cache only saves the key lookup.
type deviceCache struct {
	mu      sync.Mutex
	entries map[string]cachedDevice
	now     func() time.Time
}

type cachedDevice struct {
	device bus.Device
	at     time.Time
}

func newDeviceCache(now func() time.Time) *deviceCache {
	return &deviceCache{entries: map[string]cachedDevice{}, now: now}
}

func (c *deviceCache) get(ctx context.Context, store DeviceStore, id string) (bus.Device, error) {
	c.mu.Lock()
	entry, ok := c.entries[id]
	c.mu.Unlock()
	if ok && c.now().Sub(entry.at) < deviceCacheTTL {
		return entry.device, nil
	}
	device, err := store.DeviceByID(ctx, id)
	if err != nil {
		return bus.Device{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= deviceCacheMax {
		c.entries = map[string]cachedDevice{}
	}
	c.entries[id] = cachedDevice{device: device, at: c.now()}
	return device, nil
}

// clear forgets every device, after a request on this server revoked any.
func (c *deviceCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cachedDevice{}
}

// revokesDevices reports whether a successful operation may have revoked a
// device, so this server's cache forgets them.
func revokesDevices(operation string) bool {
	return operation == "device_revoke" || operation == "user_revoke"
}

// counter limits events per key within a fixed window, in memory per
// process (§8).
type counter struct {
	mu     sync.Mutex
	limit  int
	period time.Duration
	now    func() time.Time
	seen   map[string]window
}

func newCounter(limit int, period time.Duration, now func() time.Time) *counter {
	return &counter{limit: limit, period: period, now: now, seen: map[string]window{}}
}

// full reports whether key has reached the limit in its current window.
func (c *counter) full(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.seen[key]
	return ok && c.now().Sub(v.start) < c.period && v.count >= c.limit
}

// add counts one event for key, and reports false when the limit was
// already reached. A full table makes room by evicting, never by refusing
// keys it doesn't hold: refusing would let rotating addresses lock
// everyone out (G9-1).
func (c *counter) add(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	v, ok := c.seen[key]
	if !ok || now.Sub(v.start) >= c.period {
		v = window{start: now}
	}
	if v.count >= c.limit {
		return false
	}
	if _, held := c.seen[key]; !held {
		evictOldest(c.seen, deviceCacheMax, now, c.period)
	}
	v.count++
	c.seen[key] = v
	return true
}

// failureLog logs a refused sign-in at most once an hour per (address,
// kind), so a flood can't fill the log (§8).
type failureLog struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]time.Time
}

func (l *failureLog) record(address, kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := address + " " + kind
	now := l.now()
	if at, ok := l.last[key]; ok && now.Sub(at) < authFailureLogged {
		return
	}
	if len(l.last) >= deviceCacheMax {
		l.last = map[string]time.Time{}
	}
	l.last[key] = now
	log.Printf("clawbridge: refused sign-in from %s: %s (logged at most hourly per address and kind)", address, kind)
}

// failWithTime answers a refusal that carries the server's clock.
func failWithTime(w http.ResponseWriter, status int, code, message string, now time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	var b struct {
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			ServerTime int64  `json:"server_time"`
		} `json:"error"`
	}
	b.Error.Code, b.Error.Message, b.Error.ServerTime = code, message, now.Unix()
	_ = json.NewEncoder(w).Encode(b)
}

// ParsePublicOrigin reads serve --public-origin: https://host[:port], or
// http for a loopback host, with no path, query or credentials.
func ParsePublicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Host == "" {
		return "", errors.New("--public-origin must be https://HOST[:PORT], with no path")
	}
	host := strings.ToLower(u.Host)
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("--public-origin must use https (http only for a loopback host)")
	}
	return u.Scheme + "://" + host, nil
}

// joinHandler serves POST /v1/join (§4): unauthenticated, rate-limited per
// address and email, and every failure answers alike after a time floor.
type joinHandler struct {
	origin     string
	store      DeviceStore
	byAddress  *counter
	byEmail    *counter
	failures   *failureLog
	now        func() time.Time
	invalidMsg string
}

type joinBody struct {
	Email     string `json:"email"`
	Code      string `json:"code"`
	PublicKey string `json:"public_key"`
	Label     string `json:"label"`
	KeyStore  string `json:"key_store"`
	TS        string `json:"ts"`
	Proof     string `json:"proof"`
}

func (j *joinHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	start := time.Now()
	status, answer := j.join(w, r)
	// The floor applies to every answer, success included.
	if wait := joinFloor - time.Since(start); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		}
	}
	if answer.err != nil {
		if answer.err.serverTime {
			failWithTime(w, answer.err.status, answer.err.code, answer.err.message, j.now())
		} else {
			fail(w, answer.err.status, answer.err.code, answer.err.message)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(answer.data)
}

type joinAnswer struct {
	data map[string]any
	err  *authFailure
}

func (j *joinHandler) join(w http.ResponseWriter, r *http.Request) (int, joinAnswer) {
	address := addressKey(r)
	invalid := joinAnswer{err: &authFailure{status: 403, code: "forbidden", message: j.invalidMsg}}
	if !j.byAddress.add(address) {
		return 429, joinAnswer{err: &authFailure{status: 429, code: "rate_limited", message: "too many join attempts; try again later"}}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJoinBody))
	if err != nil {
		return 400, joinAnswer{err: &authFailure{status: 400, code: "invalid", message: "invalid or oversized join request"}}
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var in joinBody
	if decoder.Decode(&in) != nil || decoder.More() {
		return 400, joinAnswer{err: &authFailure{status: 400, code: "invalid", message: "invalid join request JSON"}}
	}
	if !j.byEmail.add(strings.ToLower(strings.TrimSpace(in.Email))) {
		return 429, joinAnswer{err: &authFailure{status: 429, code: "rate_limited", message: "too many join attempts; try again later"}}
	}
	ts, err := strconv.ParseInt(in.TS, 10, 64)
	if err != nil {
		j.failures.record(address, "join_request")
		return 403, invalid
	}
	if skew := j.now().Sub(time.Unix(ts, 0)); skew > clockSkew || skew < -clockSkew {
		return 401, joinAnswer{err: &authFailure{status: 401, code: "clock_skew", message: "the join time is more than 300 s from the server's; correct by server_time and retry once", serverTime: true}}
	}
	// The proof binds the key to this server, the email and the code.
	pub, keyErr := devicekey.ParsePublicKey(in.PublicKey)
	code, codeOK := devicekey.NormalCode(in.Code)
	proof, proofErr := base64.RawURLEncoding.DecodeString(in.Proof)
	if keyErr != nil || !codeOK || proofErr != nil || !ed25519.Verify(pub, devicekey.JoinProofBase(j.origin, in.Email, code, in.TS), proof) {
		j.failures.record(address, "join_proof")
		return 403, invalid
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := j.store.Join(ctx, bus.JoinRequest{Email: in.Email, Code: in.Code, PublicKey: in.PublicKey, Label: in.Label, KeyStore: in.KeyStore})
	if errors.Is(err, bus.ErrInvalidInvite) {
		j.failures.record(address, "join_invite")
		return 403, invalid
	}
	if err != nil {
		code, status := serviceError(err)
		return status, joinAnswer{err: &authFailure{status: status, code: code, message: publicMessage(code)}}
	}
	out["server_time"] = j.now().Unix()
	return 200, joinAnswer{data: out}
}
