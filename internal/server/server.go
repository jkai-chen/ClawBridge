// Package server exposes the shared application behind verified Google identity.
package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/principal"
	"google.golang.org/api/idtoken"
)

type Identity struct {
	Principal, Subject, HostedDomain string
	// Expires is when the token stops proving the identity; zero when the
	// verifier doesn't say.
	Expires time.Time
}
type Verifier interface {
	Verify(context.Context, string) (Identity, error)
}
type VerifierFunc func(context.Context, string) (Identity, error)

func (f VerifierFunc) Verify(ctx context.Context, t string) (Identity, error) { return f(ctx, t) }

type Executor interface {
	ExecuteAs(context.Context, string, bus.Request) (bus.Result, error)
}

// IdentityExecutor also receives the verified account, so the authority can
// bind the principal to it. The hosted store implements it, and the server
// always uses it when available.
type IdentityExecutor interface {
	ExecuteAsIdentity(context.Context, bus.Identity, bus.Request) (bus.Result, error)
}

var _ IdentityExecutor = (*bus.Store)(nil)

// OAuthMetadata contains the installed-app OAuth credentials distributed to
// clients. Google treats native-app client secrets as application identifiers,
// not confidential server secrets.
type OAuthMetadata struct {
	BrowserClientID     string
	BrowserClientSecret string
	DeviceClientID      string
	DeviceClientSecret  string
}

// GoogleVerifier uses Google's certificate-backed validator. Email assertions are
// accepted only after signature, audience, issuer and expiry validation.
type GoogleVerifier struct {
	audiences []string
	validator *idtoken.Validator
}

func NewGoogleVerifier(ctx context.Context, audience string) (*GoogleVerifier, error) {
	return NewGoogleVerifierForAudiences(ctx, []string{audience})
}

// NewGoogleVerifierForAudiences accepts an ordered set of exact Google ID-token
// audiences. The slice is copied so callers cannot change the trusted set after
// construction.
func NewGoogleVerifierForAudiences(ctx context.Context, audiences []string) (*GoogleVerifier, error) {
	normalized, err := normalizeAudiences(audiences)
	if err != nil {
		return nil, err
	}
	v, err := idtoken.NewValidator(ctx, idtoken.WithHTTPClient(&http.Client{Timeout: 8 * time.Second}))
	if err != nil {
		return nil, errors.New("cannot initialize Google ID-token verifier")
	}
	return &GoogleVerifier{audiences: normalized, validator: v}, nil
}
func (v *GoogleVerifier) Verify(ctx context.Context, token string) (identity Identity, err error) {
	// Treat malformed external cryptographic input as authentication failure even
	// if a dependency panics while parsing it. No application operation has run.
	defer func() {
		if recover() != nil {
			identity = Identity{}
			err = errors.New("invalid identity token")
		}
	}()
	if len(token) == 0 || len(token) > 16384 {
		return Identity{}, errors.New("invalid identity token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("invalid identity token")
	}
	headerRaw, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return Identity{}, errors.New("invalid identity token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(headerRaw, &header) != nil || header.Kid == "" || len(header.Kid) > 256 {
		return Identity{}, errors.New("invalid identity token")
	}
	signature, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil {
		return Identity{}, errors.New("invalid identity token")
	}
	if (header.Alg == "ES256" && len(signature) != 64) || (header.Alg == "RS256" && (len(signature) < 256 || len(signature) > 1024)) || (header.Alg != "RS256" && header.Alg != "ES256") {
		return Identity{}, errors.New("invalid identity token")
	}
	for _, audience := range v.audiences {
		p, validateErr := v.validator.Validate(ctx, token, audience)
		if validateErr != nil {
			continue
		}
		return verifiedIdentity(p, audience, time.Now())
	}
	return Identity{}, errors.New("invalid identity token")
}

const (
	maxAudienceEntries     = 16
	maxAudienceEntryLength = 2048
)

func parseAudiences(raw string) ([]string, error) {
	if len(raw) > maxAudienceEntries*(maxAudienceEntryLength+1) {
		return nil, errors.New("invalid Google ID-token audience set")
	}
	entries := strings.Split(raw, ",")
	for i := range entries {
		entries[i] = strings.TrimSpace(entries[i])
	}
	return normalizeAudiences(entries)
}

func normalizeAudiences(audiences []string) ([]string, error) {
	if len(audiences) == 0 {
		return nil, errors.New("an explicit Google ID-token audience is required")
	}
	if len(audiences) > maxAudienceEntries {
		return nil, fmt.Errorf("an audience set may contain at most %d entries", maxAudienceEntries)
	}
	normalized := make([]string, 0, len(audiences))
	seen := make(map[string]struct{}, len(audiences))
	for i, audience := range audiences {
		if audience == "" {
			return nil, fmt.Errorf("audience entry %d is empty", i+1)
		}
		if len(audience) > maxAudienceEntryLength {
			return nil, fmt.Errorf("audience entry %d is too long", i+1)
		}
		for _, r := range audience {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return nil, fmt.Errorf("audience entry %d contains whitespace or control characters", i+1)
			}
		}
		if _, exists := seen[audience]; exists {
			return nil, fmt.Errorf("audience entry %d is duplicated", i+1)
		}
		seen[audience] = struct{}{}
		normalized = append(normalized, audience)
	}
	return normalized, nil
}
func verifiedIdentity(p *idtoken.Payload, audience string, now time.Time) (Identity, error) {
	if p == nil || p.Audience != audience || audience == "" || p.Subject == "" || p.Expires <= now.Unix() ||
		(p.Issuer != "https://accounts.google.com" && p.Issuer != "accounts.google.com") {
		return Identity{}, errors.New("invalid identity token claims")
	}
	email, _ := p.Claims["email"].(string)
	verified, _ := p.Claims["email_verified"].(bool)
	email = strings.ToLower(email)
	if !verified || !principal.Valid(email) {
		return Identity{}, errors.New("verified Google email is required")
	}
	// An hd claim that is not a domain name counts as none: any allow-list
	// refuses the account.
	hostedDomain, _ := p.Claims["hd"].(string)
	hostedDomain, _ = principal.NormalDomain(hostedDomain)
	return Identity{Principal: email, Subject: p.Subject, HostedDomain: hostedDomain, Expires: time.Unix(p.Expires, 0)}, nil
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	b := errorBody{}
	b.Error.Code = code
	b.Error.Message = message
	_ = json.NewEncoder(w).Encode(b)
}

type window struct {
	start time.Time
	count int
}
type limiter struct {
	mu    sync.Mutex
	users map[string]window
	// perMinute is each principal's limit; zero is 1200.
	perMinute int
}

func (l *limiter) allow(principal string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	v := l.users[principal]
	if now.Sub(v.start) >= time.Minute {
		v = window{start: now}
	}
	if v.count >= cmp.Or(l.perMinute, 1200) {
		return false
	}
	if len(l.users) >= 10000 {
		for k, w := range l.users {
			if now.Sub(w.start) >= time.Minute {
				delete(l.users, k)
			}
		}
		if _, ok := l.users[principal]; !ok && len(l.users) >= 10000 {
			return false
		}
	}
	v.count++
	l.users[principal] = v
	return true
}

// NewHandler has no authentication bypass. An injected verifier exists for tests
// and embedded hosts; Run always supplies the production Google verifier.
func NewHandler(store Executor, verifier Verifier) http.Handler {
	return NewHandlerWithOAuthMetadata(store, verifier, OAuthMetadata{})
}

// NewHandlerWithOAuthClientIDs retains the two-ID compatibility constructor.
// Production Run uses NewHandlerWithOAuthMetadata with the complete bundle.
func NewHandlerWithOAuthClientIDs(store Executor, verifier Verifier, browserClientID, deviceClientID string) http.Handler {
	return NewHandlerWithOAuthMetadata(store, verifier, OAuthMetadata{BrowserClientID: browserClientID, DeviceClientID: deviceClientID})
}

// NewHandlerWithOAuthMetadata adds the public installed-app OAuth discovery
// endpoint while retaining the same authenticated execution boundary.
func NewHandlerWithOAuthMetadata(store Executor, verifier Verifier, metadata OAuthMetadata) http.Handler {
	return NewHandlerWithApp(store, verifier, metadata, nil)
}

// NewHandlerWithApp also serves linear_write and linear_app_status through
// writer, the server's Linear app identity; nil leaves them off.
func NewHandlerWithApp(store Executor, verifier Verifier, metadata OAuthMetadata, writer *AppWriter) http.Handler {
	return NewHandlerWithOptions(store, verifier, HandlerOptions{OAuth: metadata, Writer: writer})
}

// HandlerOptions are the server's optional endpoints.
type HandlerOptions struct {
	OAuth OAuthMetadata
	// Writer serves linear_write and linear_app_status as the server's
	// Linear app identity; nil leaves them off.
	Writer *AppWriter
	// IntakeProver is the Linear app's in-binding proof for /mcp/intake.
	// Without one, every intake request is refused (app_unavailable).
	IntakeProver IssueProver
	// TickVerifier and Ticker serve the watcher's tick endpoint; it exists
	// only when both are set.
	TickVerifier TickVerifier
	Ticker       Ticker
	// Nudge asks the watcher for an early marker pass of a request's
	// project after its confirm or release commits; nil without a watcher
	// clock; it never blocks.
	Nudge func(workspace, project string)
	// NudgeProject asks the watcher for an early pass of a project by its
	// ID, which runs its dispatcher; nil without a watcher clock; it never
	// blocks.
	NudgeProject func(projectID string)
	// Auth chooses the sign-in methods; the zero value is Google only.
	Auth AuthOptions
	// Now is the clock device sign-in reads; nil is time.Now.
	Now func() time.Time
	// ModelMaxAgents is dispatcher_model_max_agents, what launcher/wanted
	// reports; zero is defaultMaxAgents.
	ModelMaxAgents int
	// Streams serves GET /v1/stream, woken by the hub; nil leaves it off
	// (404, which a client reads as an older server).
	Streams *StreamHub
	// StreamLimits are the stream's intervals and caps; zero fields take
	// the defaults.
	StreamLimits StreamLimits
	// Drain, when set, refuses what a draining server must not start
	// (maintenance.go) and counts each request in flight; nil never
	// drains.
	Drain *Drain
	// Webhook serves POST /linear/webhook (FR14); nil, or one without a
	// secret or the app's OrgID, leaves it a 404 like an unknown path.
	Webhook *WebhookOptions
	// TrustedProxies are the peers whose forwarded headers name the
	// client and its scheme (S2); nil trusts none.
	TrustedProxies *TrustedProxies
}

// NewHandlerWithOptions also serves the send-only /mcp/intake endpoint when
// the store has intake, and the watcher's tick endpoint when configured.
func NewHandlerWithOptions(store Executor, verifier Verifier, o HandlerOptions) http.Handler {
	metadata, writer := o.OAuth, o.Writer
	l := &limiter{users: map[string]window{}}
	// Heartbeats have their own budget: liveness never spends, or waits on,
	// the principal's request limit. 240 a minute is 120 sessions renewing
	// every 30 s.
	beats := &limiter{users: map[string]window{}, perMinute: 240}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	// Intake and the tick check their address before any token, each in
	// its own table: never l, which holds only principals (G9-1).
	var intake, tick, model http.Handler
	if s, ok := store.(IntakeStore); ok {
		intake = newIntakeHandler(s, o.IntakeProver, newPreauthLimiter(1200, now))
	}
	if o.TickVerifier != nil && o.Ticker != nil {
		tick = newTickHandler(o.TickVerifier, o.Ticker, newPreauthLimiter(60, now))
	}
	var webhook http.Handler
	if s, ok := store.(WebhookStore); ok && o.Webhook != nil && len(o.Webhook.Secrets) > 0 && o.Webhook.OrgID != nil {
		webhook = newWebhookHandler(s, o.Webhook, now, o.Drain)
	}
	if s, ok := store.(ModelStore); ok {
		model = newModelHandler(s, o.Streams, cmp.Or(o.ModelMaxAgents, defaultMaxAgents), now, o.NudgeProject, o.Drain)
	}
	methods := []string{AuthGoogle}
	if len(o.Auth.Methods) > 0 {
		methods = o.Auth.Methods
	}
	failures := &failureLog{now: now, last: map[string]time.Time{}}
	failedAuth := newCounter(failedAuthPerMin, time.Minute, now)
	cache := newDeviceCache(now)
	if o.Streams != nil {
		o.Streams.onAccess(cache.clear)
	}
	var authenticators []Authenticator
	var join http.Handler
	if o.Auth.accepts(AuthGoogle) {
		authenticators = append(authenticators, googleAuthenticator{verifier: verifier})
	}
	if o.Auth.accepts(AuthDevice) && o.Auth.Devices != nil && o.Auth.PublicOrigin != "" {
		authenticators = append(authenticators, deviceAuthenticator{origin: o.Auth.PublicOrigin, store: o.Auth.Devices, cache: cache, now: now})
		join = &joinHandler{origin: o.Auth.PublicOrigin, store: o.Auth.Devices, byAddress: newCounter(joinPerAddress, time.Hour, now), byEmail: newCounter(joinPerEmail, time.Hour, now), failures: failures, now: now, invalidMsg: bus.ErrInvalidInvite.Error()}
	}
	// /v1/config names the sign-in methods only when device sign-in is on:
	// a Google-only server answers exactly the four OAuth fields, which is
	// all a v0.3.3 client's strict parser accepts.
	var advertised []string
	advertisedOrigin := ""
	if join != nil {
		advertised, advertisedOrigin = methods, o.Auth.PublicOrigin
	}
	required := "a Google ID token is required"
	if join != nil {
		required = "a Google ID token or a ClawBridge-Device signature is required"
		if !o.Auth.accepts(AuthGoogle) {
			required = "a ClawBridge-Device signature is required; this server accepts device keys only"
		}
	}
	// authenticate proves a request's caller from its one Authorization
	// header and its raw body, which it reads first: a device signs its
	// hash. A refusal is answered here.
	authenticate := func(ctx context.Context, w http.ResponseWriter, r *http.Request) (bus.Identity, []byte, bool) {
		address, key := clientAddress(r), addressKey(r)
		if failedAuth.full(key) {
			fail(w, 429, "rate_limited", "too many failed sign-ins from this address; try again later")
			return bus.Identity{}, nil, false
		}
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			fail(w, 401, "unauthenticated", required)
			return bus.Identity{}, nil, false
		}
		scheme, credentials, _ := strings.Cut(strings.TrimSpace(headers[0]), " ")
		var authenticator Authenticator
		for _, a := range authenticators {
			if strings.EqualFold(scheme, a.Scheme()) {
				authenticator = a
			}
		}
		if authenticator == nil {
			fail(w, 401, "unauthenticated", required)
			return bus.Identity{}, nil, false
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if err != nil {
			fail(w, 400, "invalid", "invalid or oversized request JSON")
			return bus.Identity{}, nil, false
		}
		verified, refused := authenticator.Authenticate(ctx, r, strings.TrimSpace(credentials), body)
		if refused != nil {
			if refused.status == 401 {
				failedAuth.add(key)
				failures.record(address, refused.kind)
			}
			if refused.serverTime {
				failWithTime(w, refused.status, refused.code, refused.message, now())
			} else {
				fail(w, refused.status, refused.code, refused.message)
			}
			return bus.Identity{}, nil, false
		}
		return verified, body, true
	}
	var stream http.Handler
	if s, ok := store.(StreamStore); ok && o.Streams != nil {
		stream = &streamHandler{store: s, hub: o.Streams, authenticate: authenticate, limit: l, limits: o.StreamLimits.orDefaults(), now: now, drain: o.Drain}
	}
	// draining is a maintenance the request must not pass, with its
	// Retry-After.
	draining := func(ctx context.Context) (int, bool) {
		if o.Drain == nil {
			return 0, false
		}
		return o.Drain.Check(ctx)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client's address and scheme, read once: every per-IP limit
		// and the plain-HTTP check use these (S2).
		r = o.TrustedProxies.apply(r)
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == StreamPath && stream != nil {
			// A stream is never in flight: a drain closes it.
			stream.ServeHTTP(w, r)
			return
		}
		// Counted from its start, before authentication and the
		// maintenance check (D5).
		if o.Drain != nil {
			defer o.Drain.Begin()()
		}
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				fail(w, 405, "method_not_allowed", "GET required")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": buildinfo.Version})
			return
		}
		if r.URL.Path == LinearCallbackPath {
			linearCallback(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, LinearWebhookPath) && len(r.URL.Path) > len(LinearWebhookPath) && webhook != nil {
			webhook.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/v1/config" {
			if r.Method != http.MethodGet {
				fail(w, 405, "method_not_allowed", "GET required")
				return
			}
			config := struct {
				BrowserClientID     string   `json:"browser_client_id"`
				BrowserClientSecret string   `json:"browser_client_secret"`
				DeviceClientID      string   `json:"device_client_id"`
				DeviceClientSecret  string   `json:"device_client_secret"`
				AuthMethods         []string `json:"auth_methods,omitempty"`
				PublicOrigin        string   `json:"public_origin,omitempty"`
			}{
				BrowserClientID:     metadata.BrowserClientID,
				BrowserClientSecret: metadata.BrowserClientSecret,
				DeviceClientID:      metadata.DeviceClientID,
				DeviceClientSecret:  metadata.DeviceClientSecret,
				AuthMethods:         advertised,
				PublicOrigin:        advertisedOrigin,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config)
			return
		}
		if r.URL.Path == IntakePath && intake != nil {
			if retry, on := draining(r.Context()); on {
				failIntakeMaintenance(w, retry)
				return
			}
			intake.ServeHTTP(w, r)
			return
		}
		if modelPath(r.URL.Path) && model != nil {
			model.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == TickPath && tick != nil {
			tick.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == devicekey.LinkPath {
			// An invite link opened in a browser lands here. The code is in
			// the fragment, which the browser never sends; the page has no
			// script, so nothing reads it (§17.1).
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				fail(w, 405, "method_not_allowed", "GET required")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = io.WriteString(w, joinPage)
			return
		}
		if r.URL.Path == "/v1/join" && join != nil {
			if retry, on := draining(r.Context()); on {
				failMaintenance(w, retry)
				return
			}
			join.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/v1/execute" {
			fail(w, 404, "not_found", "endpoint not found")
			return
		}
		if r.Method != http.MethodPost {
			fail(w, 405, "method_not_allowed", "POST required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		verified, body, ok := authenticate(ctx, w, r)
		if !ok {
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var request bus.Request
		decoded := decoder.Decode(&request)
		// A heartbeat spends its own budget; anything else, a malformed
		// request included, the principal's.
		limit := l
		if decoded == nil && request.Operation == "heartbeat" {
			limit = beats
		}
		if !limit.allow(verified.Principal) {
			fail(w, 429, "rate_limited", "principal request limit reached")
			return
		}
		if decoded != nil {
			fail(w, 400, "invalid", "invalid or oversized request JSON")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			fail(w, 400, "invalid", "exactly one request object is required")
			return
		}
		if request.Operation == "" || store == nil {
			fail(w, 400, "invalid", "an operation is required")
			return
		}
		// After authentication, whose own writes a read needs too; before
		// any transaction or lock. Only a pure read passes a drain.
		if retry, on := draining(ctx); on {
			if !bus.ServedInMaintenance(request) {
				failMaintenance(w, retry)
				return
			}
			// A read is served, and says when the drain ends, for doctor.
			w.Header().Set(MaintenanceUntilHeader, o.Drain.Until().UTC().Format(time.RFC3339))
		}
		var result bus.Result
		var err error
		request = planReserveRequest(r.Context(), writer, request)
		if request.Operation == "linear_write" || request.Operation == "linear_app_status" || request.Operation == "linear_app_states" || request.Operation == "linear_app_project_status" || request.Operation == "linear_app_project_status_set" || request.Operation == "linear_app_subscribers_backfill" || request.Operation == "linear_write_status" || request.Operation == "linear_issue_history" {
			result, err = writer.Execute(r.Context(), verified, request)
		} else if bound, ok := store.(IdentityExecutor); ok {
			result, err = bound.ExecuteAsIdentity(ctx, verified, request)
		} else if verified.Issuer == bus.GoogleIssuer {
			result, err = store.ExecuteAs(ctx, verified.Principal, request)
		} else {
			err = errors.New("device sign-in needs the hosted authority")
		}
		if err != nil {
			code, status := serviceError(err)
			fail(w, status, code, responseMessage(code, err))
			return
		}
		result.Source = "server"
		// Every response a device signed says how many device events it
		// hasn't acknowledged, so a stolen device's additions and revokes
		// reach the user's other devices with no extra request (§17.3).
		if counter, ok := store.(deviceEventCounter); ok && verified.Issuer == bus.DeviceIssuer {
			if n, e := counter.PendingDeviceEvents(ctx, verified.Subject); e == nil {
				result.PendingDeviceEvents = &n
			}
		}
		// A change that took access away: this server forgets its cached
		// devices and wakes its streams now, as every replica does on the
		// change's AccessChannel notify. Each woken stream re-authorizes,
		// so the streams this took access from close now, not at their
		// next re-read.
		if bus.TakesAccess(request.Operation) {
			cache.clear()
			if o.Streams != nil {
				o.Streams.WakeAll()
			}
		}
		// The confirm or release has committed: its marker rows are due now.
		if o.Nudge != nil && (request.Operation == "assignment_confirm" || request.Operation == "assignment_release") {
			o.Nudge(request.Workspace, request.Project)
		}
		if request.Operation == "whoami" {
			if data, ok := result.Data.(map[string]any); ok {
				data["principal"] = verified.Principal
				data["subject"] = verified.Subject
				data["verified"] = true
				data["identity_provenance"] = "google-id-token"
				if verified.Issuer == bus.DeviceIssuer {
					data["identity_provenance"] = "device-key"
					data["device_id"] = verified.Subject
				}
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil || len(encoded) > 16<<20 {
			fail(w, 500, "unavailable", "response unavailable; reduce the requested dataset")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(encoded, '\n'))
	})
}

// joinPage is the static page an invite link opens in a browser.
const joinPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Join ClawBridge</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem}code{background:#eee;padding:.1rem .3rem}</style>
</head><body>
<h1>Join ClawBridge</h1>
<p>This is a ClawBridge invite link. It isn't opened in a browser.</p>
<p>In a terminal, run <code>clawbridge join</code> and paste the whole link when it asks. Or choose
"I have an invite code" in <code>clawbridge</code>'s guided setup and paste it there.</p>
<p>The link works once. Treat it like a password: don't forward it.</p>
</body></html>
`

// deviceEventCounter is the hosted store's count of a device's pending
// device events.
type deviceEventCounter interface {
	PendingDeviceEvents(context.Context, string) (int, error)
}

func serviceError(err error) (string, int) {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		switch c := coded.Code(); c {
		case "forbidden":
			return c, 403
		case "not_found":
			return c, 404
		case "conflict", "unsupported_mapping", "binding_unreadable", "in_progress":
			return c, 409
		case "rate_limited":
			return c, 429
		case "unavailable":
			return c, 503
		case "invalid":
			return c, 400
		}
	}
	var sqlState interface{ SQLState() string }
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &sqlState) || errors.As(err, &ne) {
		return "unavailable", 503
	}
	return "invalid", 400
}

// responseMessage shows a validation refusal's or an unreadable binding's
// own message when the bus marked it as written for the caller; every other failure, including
// database and unknown errors, gets only the generic text for its code.
func responseMessage(code string, err error) string {
	if code == "invalid" || code == "binding_unreadable" || code == "rate_limited" || code == "conflict" || code == "in_progress" {
		if message, ok := bus.PublicMessage(err); ok {
			if safe, ok := bus.SafePublicText(message); ok {
				return safe
			}
		}
	}
	return publicMessage(code)
}
func publicMessage(code string) string {
	switch code {
	case "forbidden":
		return "this Google principal is not authorized for the requested project or operation"
	case "not_found":
		return "requested scope or entity was not found"
	case "binding_unreadable":
		return "this project's task board binding is unreadable and is not used; after a ClawBridge downgrade, upgrade instead of repairing; otherwise a project owner can clear it with linear unlink --repair"
	case "conflict":
		return "operation conflicts with existing state; verify session instance and retry identifier"
	case "in_progress":
		return "this operation is being sent to Linear by another request; retry the same operation_id shortly"
	case "rate_limited":
		return "request rate limit reached; no write queued"
	case "unavailable":
		return "authoritative service unavailable; outcome may be uncertain, no write queued"
	default:
		return "request rejected; verify scope, session instance and operation arguments; no write queued"
	}
}

// Run starts a production-authenticated service. Database credentials come only
// from the explicitly named environment variable and are never logged.
func Run(ctx context.Context, args []string) error {
	settings, err := ParseServe(args, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	auth, metadata := settings.Auth, settings.OAuth
	var verifier Verifier
	if auth.accepts(AuthGoogle) {
		if verifier, err = NewGoogleVerifierForAudiences(ctx, settings.Audiences); err != nil {
			return err
		}
	}
	dsn := os.Getenv(settings.DatabaseURLEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	watch, err := readWatchSettings(os.Getenv)
	if err != nil {
		return err
	}
	store, err := openServingStore(ctx, dsn, settings.BootstrapOwner)
	if err != nil {
		return errors.New("cannot initialize Postgres; check the configured connection and schema version")
	}
	defer store.Close()
	store.SetProjectFlowProven(settings.ProjectFlowProven)
	store.SetIssueSubscribersProven(settings.IssueSubscribersProven)
	store.SetLinearBudgetMax(watch.budgetMax)
	store.SetIssueMoveProven(settings.IssueMoveProven)
	store.SetProjectTeamAddProven(settings.ProjectTeamAddProven)
	store.SetProjectStatusDomainProven(settings.ProjectStatusDomainProven)
	store.SetProjectPlanProven(settings.ProjectPlanProven)
	store.SetIssueFamilyMoveProven(settings.IssueFamilyMoveProven)
	store.SetWebhooksProven(settings.WebhooksProven)
	writer, err := appWriter(store)
	if err != nil {
		return err
	}
	var orgID func(context.Context) (string, error)
	if writer != nil && writer.App != nil {
		app := writer.App
		orgID = cachedOrgID(func(ctx context.Context) (string, error) {
			id, err := app.Identity(ctx)
			return id.LinearOrgID, err
		}, time.Now)
	}
	webhook, err := webhookStartup(ctx, store, settings.WebhookSecrets, settings.WebhooksProven, orgID, time.Now())
	if err != nil {
		return err
	}
	auth.Devices = store
	seal, maxAgents, err := readModelSettings(os.Getenv)
	if err != nil {
		return err
	}
	store.SetModelSeal(seal)
	store.SetModelHost(maxAgents, memAvailable)
	// The drain (maintenance.go): a start ends a maintenance that began
	// before it, since this binary is the swap's new one, and then waits
	// the resume grace; a start that cleared nothing never waits.
	instance, err := newInstanceName()
	if err != nil {
		return err
	}
	drain := NewDrain(store, instance, time.Now)
	clearAtStart(ctx, store, drain)
	store.SetQuiet(drain.Quiet)
	hub := NewStreamHub()
	hub.maintenance = drain.Invalidate
	drain.onStart = hub.StartDrain
	options := startWatcher(ctx, store, watch, HandlerOptions{OAuth: metadata, Writer: writer, Auth: auth, ModelMaxAgents: maxAgents, Drain: drain, Webhook: webhook, TrustedProxies: settings.TrustedProxies})
	// A drain's end also catches up the live webhook teams (FR14 §6 P6);
	// drain.Run starts below, so onEnd is set before it can run.
	drain.onEnd = webhookDrainEnd(hub.EndDrain, store, func() func(string) {
		if webhook == nil {
			return nil
		}
		return webhook.Nudge
	}, time.Now)
	var flushWebhooks func(context.Context)
	if webhook != nil && webhook.flush != nil {
		flushWebhooks = webhook.flush.FlushCounts
	}
	// The message queue: the hub wakes the streams, the sweeper offers run-out
	// leases again and makes dead letters.
	options.Streams = hub
	go hub.ListenAs(ctx, dsn, store.InstanceName())
	go drain.Run(ctx)
	go runSweeper(ctx, store, sweepEvery, drain, flushWebhooks)
	// The Linear write outbox: queued app writes are sent as Linear and the
	// project's cap allow; it sends nothing while the server drains.
	if writer != nil && store.OutboxAvailable() {
		go writer.runOutbox(ctx, outboxEvery, drain)
	}
	// A Linear write the server runs as the app may take up to a minute. A
	// stream clears its own deadlines.
	srv := &http.Server{Addr: settings.Listen, Handler: NewHandlerWithOptions(store, verifier, options), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 80 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 24 << 10}
	srv.RegisterOnShutdown(options.Streams.Close)
	result := make(chan error, 1)
	go func() { result <- srv.ListenAndServe() }()
	select {
	case err = <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		// Long enough for an app write to Linear (LinearWriteTimeout) to
		// finish; a drained server has nothing in flight and stops at once.
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// shutdownGrace is how long serve waits for in-flight requests on SIGTERM;
// compose's stop_grace_period (90s) is above it.
const shutdownGrace = 75 * time.Second

// openServingStore opens the hosted authority the server serves. Every request
// must pass the account binding, so the store refuses any path that would
// reach it without the verified account.
func openServingStore(ctx context.Context, dsn, owner string) (*bus.Store, error) {
	// Only serve names its connections, so doctor counts serving
	// instances, never CLI or operator commands.
	store, err := bus.OpenPostgresServer(ctx, dsn, strings.ToLower(owner))
	if err != nil {
		return nil, err
	}
	store.RequireVerifiedAccount()
	return store, nil
}

// RunOperator runs a host operator's maintenance command against the
// database directly, without a signed-in principal: access to the database
// connection is the authority. These are the ways out when the server owner
// cannot sign in: their own address moved to a new Google account, or a
// workspace allow-list refuses every account.
func RunOperator(ctx context.Context, args []string, stdout io.Writer) error {
	const usage = "usage: clawbridge operator principal-unbind EMAIL --confirm [--allow-previous] | workspace-clear-domains WORKSPACE --confirm | linear-app-probe --confirm [--project UUID] [--p8 [--dry-run] [--allow-member UUID] | --health-order | --project-status [--dry-run] [--allow-member UUID]] | linear-app-probe --rate-headers --confirm | linear-app-probe --confirm --subscribe --issue UUID --team UUID (--email OWNER | --email-stdin) [--user UUID] [--dry-run] | linear-app-doc-probe --confirm | linear-webhook-probe --confirm --team UUID [--forged --public-origin https://HOST] | health-run --project UUID --confirm [--force-new | --delete UPDATE_ID] | bootstrap-invite --workspace NAME [--force-stdout | --code-stdin [--expect-owner EMAIL]] [--database-url-env NAME] | launcher-token create --out PATH [--print] | launcher-token revoke ID | model-seal-key rotate [--confirm [--delete-unreadable]] | maintenance start --reason TEXT [--ttl 15m] (--wait DURATION | --no-wait) --confirm | maintenance end --confirm | maintenance show"
	if len(args) > 0 && args[0] == "bootstrap-invite" {
		return runBootstrapInvite(ctx, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "launcher-token" {
		return runLauncherToken(ctx, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "model-seal-key" {
		return runModelSealKey(ctx, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "maintenance" {
		return runMaintenance(ctx, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "linear-webhook-probe" {
		return runLinearWebhookProbe(ctx, args[1:], stdout)
	}
	if len(args) == 0 || (args[0] != "principal-unbind" && args[0] != "workspace-clear-domains" && args[0] != "linear-app-probe" && args[0] != "linear-app-doc-probe" && args[0] != "health-run") {
		return errors.New(usage)
	}
	command := args[0]
	f := flag.NewFlagSet("operator "+command, flag.ContinueOnError)
	confirm := f.Bool("confirm", false, "confirm the change")
	allowPrevious := false
	if command == "principal-unbind" {
		f.BoolVar(&allowPrevious, "allow-previous", false, "let the released account bind again, for an unbind made by mistake")
	}
	project := ""
	p8, dryRun, healthOrder, projectStatus, rateHeaders := false, false, false, false, false
	allowMember := ""
	subscribe, subIssue, subTeam, subEmail, subUser, emailStdin := false, "", "", "", "", false
	if command == "linear-app-probe" {
		f.BoolVar(&rateHeaders, "rate-headers", false, "read once over GraphQL and once over MCP as the app, and print only Linear's rate-limit headers")
		f.BoolVar(&subscribe, "subscribe", false, "the issue subscribers live check: subscribe --email (and --user) to the fixture --issue in --team as the app, and read back")
		f.StringVar(&subIssue, "issue", "", "with --subscribe: the hand-made fixture issue's UUID")
		f.StringVar(&subTeam, "team", "", "with --subscribe: the fixture's team UUID")
		f.StringVar(&subEmail, "email", "", "with --subscribe: the project owner's email, subscribed by email")
		f.BoolVar(&emailStdin, "email-stdin", false, "with --subscribe: read the owner's email from stdin instead of --email, so it isn't on the command line")
		f.StringVar(&subUser, "user", "", "with --subscribe: a second Linear user's UUID, subscribed by ID")
		f.StringVar(&project, "project", "", "a bound Linear project UUID whose health inputs are read; with --p8, the empty throwaway project")
		f.BoolVar(&p8, "p8", false, "post one real status update to the empty --project as the app, read it back, delete it and read again (P8)")
		f.BoolVar(&dryRun, "dry-run", false, "with --p8 or --project-status: check the project and the app, and send nothing")
		f.BoolVar(&projectStatus, "project-status", false, "move the empty --project, named by "+probeProjectEnv+", to started, completed and back as the app, reading each move back (A4)")
		f.StringVar(&allowMember, "allow-member", "", "with --p8 or --project-status: accept the empty project's members when they are exactly this one Linear user (a UUID, never a name or email)")
		f.BoolVar(&healthOrder, "health-order", false, "read --project's updates as the health judgement does and with last:, two reads, and print their order")
	}
	forceNew, deleteID := false, ""
	if command == "health-run" {
		f.StringVar(&project, "project", "", "the bound Linear project UUID whose health runs now")
		f.BoolVar(&forceNew, "force-new", false, "act and post on this one run, without the two-run confirmation and over the day's settled post; audited")
		f.StringVar(&deleteID, "delete", "", "delete this project update from Linear, only if a forced health run made it on --project")
	}
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	var positional []string
	rest := args[1:]
	for {
		if err := f.Parse(rest); err != nil {
			return err
		}
		if f.NArg() == 0 {
			break
		}
		positional, rest = append(positional, f.Arg(0)), f.Args()[1:]
	}
	probe := command == "linear-app-probe" || command == "linear-app-doc-probe" || command == "health-run"
	if probe != (len(positional) == 0) || len(positional) > 1 {
		return errors.New(usage)
	}
	if rateHeaders && (project != "" || p8 || healthOrder || subscribe) {
		return errors.New("--rate-headers takes no --project, --p8, --health-order or --subscribe")
	}
	if emailStdin && (!subscribe || subEmail != "") {
		return errors.New("--email-stdin goes with --subscribe, in place of --email")
	}
	if subscribe && (p8 || healthOrder || projectStatus || project != "" || allowMember != "") {
		return errors.New("--subscribe takes --issue, --team, --email and --user, not --project, --p8, --health-order, --project-status or --allow-member")
	}
	if p8 && project == "" || dryRun && !p8 && !projectStatus && !subscribe {
		return errors.New("--p8 needs --project, and --dry-run needs --p8, --project-status or --subscribe")
	}
	if projectStatus && (project == "" || p8 || healthOrder) {
		return errors.New("--project-status needs --project, and not --p8 or --health-order")
	}
	if allowMember != "" && (!p8 && !projectStatus || !projectUUID.MatchString(allowMember)) {
		return errors.New("--allow-member takes one Linear user UUID, with --p8 or --project-status")
	}
	if healthOrder && (project == "" || p8) {
		return errors.New("--health-order needs --project, and not --p8")
	}
	if command == "health-run" && (project == "" || forceNew && deleteID != "") {
		return errors.New("health-run needs --project, and takes --force-new or --delete, not both")
	}
	if !*confirm {
		if command == "linear-app-probe" && rateHeaders {
			return errors.New("linear-app-probe --rate-headers reads once over GraphQL (viewer) and once over MCP (list_teams) as the Linear app, never mints, and prints only Linear's rate-limit headers; it writes nothing; repeat with --confirm")
		}
		if command == "linear-app-probe" && healthOrder {
			return errors.New("linear-app-probe --health-order reads the project's latest updates twice as the Linear app and prints their IDs and times; it writes nothing; repeat with --confirm")
		}
		if command == "linear-app-probe" && projectStatus {
			return errors.New("linear-app-probe --project-status moves the empty probe project's status three times as the Linear app (to started, completed and back); repeat with --confirm")
		}
		if command == "linear-app-probe" && subscribe {
			return errors.New("linear-app-probe --subscribe subscribes the owner (and --user) to the fixture issue as the Linear app and reads its subscribers back; it never unsubscribes, so the owner deletes the fixture after; repeat with --confirm")
		}
		if command == "linear-app-probe" && p8 {
			return errors.New("linear-app-probe --p8 posts one real status update to the empty project as the Linear app, then deletes it; repeat with --confirm")
		}
		if command == "linear-app-probe" {
			return errors.New("linear-app-probe sends one project update for a project that does not exist, as the Linear app; repeat with --confirm")
		}
		if command == "health-run" && deleteID != "" {
			return errors.New("health-run --delete deletes that project update from Linear as the Linear app, only if a forced health run made it; repeat with --confirm")
		}
		if command == "health-run" {
			return errors.New("health-run runs the Linear project's health now, as the Linear app, and may post one project update to it; repeat with --confirm")
		}
		if command == "linear-app-doc-probe" {
			return errors.New("linear-app-doc-probe lists the workspace's documents of the last 30 days as the Linear app, one read, and prints counts only; repeat with --confirm")
		}
		if command == "workspace-clear-domains" {
			return errors.New("workspace-clear-domains lets any Google account that is a member in; repeat with --confirm")
		}
		return errors.New("principal-unbind releases an address from its Google account; repeat with --confirm")
	}
	if emailStdin {
		// Read before anything is opened; never echoed.
		var err error
		if subEmail, err = readProbeEmail(operatorStdin); err != nil {
			return err
		}
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	store, err := bus.OpenPostgresOperator(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot open Postgres: %w", err)
	}
	defer store.Close()
	if command == "linear-app-probe" && rateHeaders {
		return linearAppRateHeaders(ctx, store, stdout)
	}
	if command == "linear-app-probe" && healthOrder {
		return linearAppHealthOrder(ctx, store, project, stdout)
	}
	if command == "linear-app-probe" && projectStatus {
		return linearAppStatusProbe(ctx, store, project, allowMember, dryRun, stdout)
	}
	if command == "linear-app-probe" && subscribe {
		return linearAppSubscribeProbe(ctx, store, subIssue, subTeam, subEmail, subUser, dryRun, stdout)
	}
	if command == "linear-app-probe" && p8 {
		return linearAppP8(ctx, store, project, allowMember, dryRun, stdout)
	}
	if command == "linear-app-probe" {
		return linearAppProbe(ctx, store, project, stdout)
	}
	if command == "linear-app-doc-probe" {
		return linearAppDocProbe(ctx, store, stdout)
	}
	if command == "health-run" && deleteID != "" {
		return healthRunDelete(ctx, store, project, deleteID, stdout)
	}
	if command == "health-run" {
		forcer, err := newHealthForcer(store)
		if err != nil {
			return err
		}
		return healthRun(ctx, store, forcer, project, forceNew, stdout)
	}
	var out map[string]any
	if command == "workspace-clear-domains" {
		out, err = store.OperatorClearWorkspaceDomains(ctx, positional[0])
	} else {
		out, err = store.OperatorUnbindPrincipal(ctx, positional[0], allowPrevious)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(out)
}

func validateOAuthMetadata(audiences []string, metadata OAuthMetadata) (OAuthMetadata, error) {
	allowed := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		allowed[audience] = struct{}{}
	}
	pairs := []struct {
		name, clientID, clientSecret, secretEnv string
	}{
		{name: "browser", clientID: metadata.BrowserClientID, clientSecret: metadata.BrowserClientSecret, secretEnv: "CLAWBRIDGE_OAUTH_CLIENT_SECRET"},
		{name: "device", clientID: metadata.DeviceClientID, clientSecret: metadata.DeviceClientSecret, secretEnv: "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET"},
	}
	for _, pair := range pairs {
		clientID := pair.clientID
		if clientID == "" {
			if pair.clientSecret != "" {
				return OAuthMetadata{}, fmt.Errorf("OAuth %s client credential has a secret but no client ID", pair.name)
			}
			continue
		}
		if len(clientID) > maxAudienceEntryLength {
			return OAuthMetadata{}, fmt.Errorf("OAuth %s client ID is too long", pair.name)
		}
		for _, r := range clientID {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return OAuthMetadata{}, fmt.Errorf("OAuth %s client ID contains whitespace or control characters", pair.name)
			}
		}
		if _, ok := allowed[clientID]; !ok {
			return OAuthMetadata{}, fmt.Errorf("OAuth %s client ID must be present in --audience", pair.name)
		}
		if pair.clientSecret == "" {
			return OAuthMetadata{}, fmt.Errorf("OAuth %s client ID requires %s", pair.name, pair.secretEnv)
		}
		if len(pair.clientSecret) > 4096 {
			return OAuthMetadata{}, fmt.Errorf("OAuth %s client credential is too long", pair.name)
		}
		for _, r := range pair.clientSecret {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return OAuthMetadata{}, fmt.Errorf("OAuth %s client credential contains whitespace or control characters", pair.name)
			}
		}
	}
	if metadata.BrowserClientID != "" && metadata.BrowserClientID == metadata.DeviceClientID {
		return OAuthMetadata{}, errors.New("OAuth browser and device client IDs must be distinct")
	}
	return metadata, nil
}

// authMethods reads serve --auth (§10). Without it, Google is on when an
// audience is configured and device keys when a public origin is, so an
// upgrade keeps today's Google sign-in and adds device keys only once the
// operator names the origin.
func authMethods(flagValue string, haveAudience, haveOrigin bool) ([]string, error) {
	if flagValue == "" {
		var methods []string
		if haveAudience {
			methods = append(methods, AuthGoogle)
		}
		if haveOrigin {
			methods = append(methods, AuthDevice)
		}
		if len(methods) == 0 {
			return nil, errors.New("serve needs --audience for Google sign-in or --public-origin for device sign-in")
		}
		return methods, nil
	}
	seen := map[string]bool{}
	var methods []string
	for _, m := range strings.Split(flagValue, ",") {
		m = strings.TrimSpace(m)
		if (m != AuthGoogle && m != AuthDevice) || seen[m] {
			return nil, errors.New("--auth must be google, device or google,device")
		}
		seen[m] = true
		methods = append(methods, m)
	}
	return methods, nil
}

// ServeSettings are serve's flags, checked.
type ServeSettings struct {
	Listen, BootstrapOwner, DatabaseURLEnv string
	// Auth holds the effective sign-in methods and the public origin.
	Auth      AuthOptions
	Audiences []string
	OAuth     OAuthMetadata
	// ProjectFlowProven lets a status policy turn project flow on (A4 L5):
	// serve --project-flow-proven or CLAWBRIDGE_PROJECT_FLOW_PROVEN=1, set
	// by the operator after the deploy probe.
	ProjectFlowProven bool
	// IssueSubscribersProven lets a project's issue subscribers turn on
	// (ISSUE-SUBSCRIBERS-PLAN B2): serve --issue-subscribers-proven or
	// CLAWBRIDGE_ISSUE_SUBSCRIBERS_PROVEN=1, set by the operator after the
	// live check that the app may subscribe other people.
	IssueSubscribersProven bool
	// IssueMoveProven lets an issue's team move be confirmed (FR7): serve
	// --issue-move-proven or CLAWBRIDGE_ISSUE_MOVE_PROVEN=1, set by the
	// operator after the move probe. Previews work without it.
	IssueMoveProven bool
	// ProjectTeamAddProven lets a team be added to a Linear project (FR10):
	// serve --project-team-add-proven or CLAWBRIDGE_PROJECT_TEAM_ADD_PROVEN=1,
	// set by the operator after the team-add probe.
	ProjectTeamAddProven bool
	// ProjectStatusDomainProven lets a dispatcher domain's coordinator set a
	// Linear project's status inside its lane: serve
	// --project-status-domain-proven or CLAWBRIDGE_PROJECT_STATUS_DOMAIN_PROVEN=1,
	// set by the operator after the probe that the app's project.teams lists
	// a private team the app isn't a member of.
	ProjectStatusDomainProven bool
	// ProjectPlanProven lets a Linear project's priority, target date and
	// start date be confirmed (FR13): serve --project-plan-proven or
	// CLAWBRIDGE_PROJECT_PLAN_PROVEN=1, set by the operator after the plan
	// probe. Previews work without it.
	ProjectPlanProven bool
	// IssueFamilyMoveProven lets a family move (a parent with its
	// sub-issues) be confirmed too: serve --issue-family-move-proven or
	// CLAWBRIDGE_ISSUE_FAMILY_MOVE_PROVEN=1, after the family probe. It
	// needs IssueMoveProven as well.
	IssueFamilyMoveProven bool
	// WebhookSecrets are the Linear webhooks' signing secrets by lower-case
	// team UUID, from CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS only (never a flag,
	// never logged).
	WebhookSecrets map[string][]string
	// WebhooksProven lets a verified webhook delivery mark watch cursors
	// due (FR14): serve --webhooks-proven or CLAWBRIDGE_WEBHOOKS_PROVEN=1,
	// after the webhook probe. Off, deliveries are verified and recorded
	// inert.
	WebhooksProven bool
	// TrustedProxies are the peers whose X-Forwarded-For and
	// X-Forwarded-Proto count (S2): serve --trusted-proxies, else
	// CLAWBRIDGE_TRUSTED_PROXIES. Unset trusts no peer.
	TrustedProxies *TrustedProxies
}

// ParseServe reads serve's flags and the OAuth secrets from getenv. The
// sign-in mode is never inferred down: wherever Google sign-in is on, its
// audience and all four OAuth settings must be present, so a Google
// deployment missing one fails at startup instead of serving devices only.
func ParseServe(args []string, getenv func(string) string) (ServeSettings, error) {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:8080", "listen address; use a private/IAP path or TLS proxy for remote access")
	audience := f.String("audience", "", "comma-separated Google ID-token audience set; required for Google sign-in")
	browserClientID := f.String("oauth-client-id", "", "Desktop-app OAuth client ID, in --audience; required for Google sign-in")
	deviceClientID := f.String("oauth-device-client-id", "", "TVs and Limited Input devices OAuth client ID, in --audience; required for Google sign-in")
	owner := f.String("bootstrap-owner", "", "verified principal allowed to initialize a new workspace")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	authFlag := f.String("auth", "", "sign-in methods: google, device or google,device; without it, google with --audience and device with --public-origin")
	publicOrigin := f.String("public-origin", "", "this server's public origin, https://HOST[:PORT]; device signatures and join proofs cover it, so device sign-in requires it")
	flowProven := f.Bool("project-flow-proven", false, "let status policies turn project flow on; set only after the deploy probe proves it (also CLAWBRIDGE_PROJECT_FLOW_PROVEN=1)")
	subscribersProven := f.Bool("issue-subscribers-proven", false, "let projects subscribe their people to agents' issues; set only after the live check proves the app may (also CLAWBRIDGE_ISSUE_SUBSCRIBERS_PROVEN=1)")
	familyProven := f.Bool("issue-family-move-proven", false, "let a family move (a parent with its sub-issues) be confirmed; set only after the family probe proves it (also CLAWBRIDGE_ISSUE_FAMILY_MOVE_PROVEN=1)")
	moveProven := f.Bool("issue-move-proven", false, "let an issue's team move be confirmed; set only after the move probe proves it (also CLAWBRIDGE_ISSUE_MOVE_PROVEN=1)")
	planProven := f.Bool("project-plan-proven", false, "let the project owner, a lead, a team's lead or the coordinator set a Linear project's priority, target date and start date; set only after the plan probe proves it (also CLAWBRIDGE_PROJECT_PLAN_PROVEN=1)")
	webhooksProven := f.Bool("webhooks-proven", false, "let verified Linear webhook deliveries mark watch cursors due; set only after the webhook probe proves them (also CLAWBRIDGE_WEBHOOKS_PROVEN=1)")
	teamAddProven := f.Bool("project-team-add-proven", false, "let the project owner, a lead or the coordinator add a bound team to a Linear project; set only after the team-add probe proves it (also CLAWBRIDGE_PROJECT_TEAM_ADD_PROVEN=1)")
	trustedProxies := f.String("trusted-proxies", "", "comma-separated IPs, CIDRs or host names of the reverse proxies whose X-Forwarded-For and X-Forwarded-Proto count; unset trusts none (also CLAWBRIDGE_TRUSTED_PROXIES)")
	statusDomainProven := f.Bool("project-status-domain-proven", false, "let a dispatcher domain's coordinator set a Linear project's status inside its lane; set only after the probe shows the app's project teams list a private team the app isn't a member of (also CLAWBRIDGE_PROJECT_STATUS_DOMAIN_PROVEN=1)")
	if err := f.Parse(args); err != nil {
		return ServeSettings{}, err
	}
	if f.NArg() != 0 {
		return ServeSettings{}, errors.New("serve accepts flags only")
	}
	if !principal.Valid(*owner) {
		return ServeSettings{}, errors.New("serve requires --bootstrap-owner")
	}
	methods, err := authMethods(*authFlag, *audience != "", *publicOrigin != "")
	if err != nil {
		return ServeSettings{}, err
	}
	out := ServeSettings{Listen: *listen, BootstrapOwner: *owner, DatabaseURLEnv: *dsnEnv, Auth: AuthOptions{Methods: methods}, ProjectFlowProven: *flowProven, IssueSubscribersProven: *subscribersProven, ProjectTeamAddProven: *teamAddProven, ProjectStatusDomainProven: *statusDomainProven, ProjectPlanProven: *planProven}
	switch v := getenv("CLAWBRIDGE_PROJECT_FLOW_PROVEN"); v {
	case "":
	case "1", "true":
		out.ProjectFlowProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_PROJECT_FLOW_PROVEN is 1 or 0")
	}
	switch v := getenv("CLAWBRIDGE_ISSUE_SUBSCRIBERS_PROVEN"); v {
	case "":
	case "1", "true":
		out.IssueSubscribersProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_ISSUE_SUBSCRIBERS_PROVEN is 1 or 0")
	}
	out.IssueMoveProven = *moveProven
	switch v := getenv("CLAWBRIDGE_ISSUE_MOVE_PROVEN"); v {
	case "":
	case "1", "true":
		out.IssueMoveProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_ISSUE_MOVE_PROVEN is 1 or 0")
	}
	switch v := getenv("CLAWBRIDGE_PROJECT_TEAM_ADD_PROVEN"); v {
	case "":
	case "1", "true":
		out.ProjectTeamAddProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_PROJECT_TEAM_ADD_PROVEN is 1 or 0")
	}
	switch v := getenv("CLAWBRIDGE_PROJECT_STATUS_DOMAIN_PROVEN"); v {
	case "":
	case "1", "true":
		out.ProjectStatusDomainProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_PROJECT_STATUS_DOMAIN_PROVEN is 1 or 0")
	}
	switch v := getenv("CLAWBRIDGE_PROJECT_PLAN_PROVEN"); v {
	case "":
	case "1", "true":
		out.ProjectPlanProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_PROJECT_PLAN_PROVEN is 1 or 0")
	}
	out.IssueFamilyMoveProven = *familyProven
	switch v := getenv("CLAWBRIDGE_ISSUE_FAMILY_MOVE_PROVEN"); v {
	case "":
	case "1", "true":
		out.IssueFamilyMoveProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_ISSUE_FAMILY_MOVE_PROVEN is 1 or 0")
	}
	out.WebhooksProven = *webhooksProven
	switch v := getenv("CLAWBRIDGE_WEBHOOKS_PROVEN"); v {
	case "":
	case "1", "true":
		out.WebhooksProven = true
	case "0", "false":
	default:
		return ServeSettings{}, errors.New("CLAWBRIDGE_WEBHOOKS_PROVEN is 1 or 0")
	}
	proxiesSource, proxiesValue := "--trusted-proxies", *trustedProxies
	if strings.TrimSpace(proxiesValue) == "" {
		proxiesSource, proxiesValue = "CLAWBRIDGE_TRUSTED_PROXIES", getenv("CLAWBRIDGE_TRUSTED_PROXIES")
	}
	if out.TrustedProxies, err = ParseTrustedProxies(proxiesValue); err != nil {
		return ServeSettings{}, fmt.Errorf("%s: %w", proxiesSource, err)
	}
	if out.WebhookSecrets, err = parseWebhookSecrets(getenv("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS")); err != nil {
		return ServeSettings{}, err
	}
	if len(out.WebhookSecrets) > 0 && getenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID") == "" && getenv("CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET") == "" && getenv("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY") == "" {
		return ServeSettings{}, errors.New("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS needs the Linear app (CLAWBRIDGE_LINEAR_APP_*); configure it or unset the secrets")
	}
	if out.Auth.accepts(AuthDevice) {
		if out.Auth.PublicOrigin, err = ParsePublicOrigin(*publicOrigin); err != nil {
			return ServeSettings{}, err
		}
	}
	if out.Auth.accepts(AuthGoogle) {
		if out.Audiences, err = parseAudiences(*audience); err != nil {
			return ServeSettings{}, errors.New("Google sign-in requires --audience")
		}
		metadata := OAuthMetadata{
			BrowserClientID:     strings.TrimSpace(*browserClientID),
			BrowserClientSecret: getenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET"),
			DeviceClientID:      strings.TrimSpace(*deviceClientID),
			DeviceClientSecret:  getenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET"),
		}
		if out.OAuth, err = validateOAuthMetadata(out.Audiences, metadata); err != nil {
			return ServeSettings{}, err
		}
		for _, setting := range []struct{ name, value string }{
			{"--oauth-client-id", metadata.BrowserClientID},
			{"CLAWBRIDGE_OAUTH_CLIENT_SECRET", metadata.BrowserClientSecret},
			{"--oauth-device-client-id", metadata.DeviceClientID},
			{"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", metadata.DeviceClientSecret},
		} {
			if setting.value == "" {
				return ServeSettings{}, fmt.Errorf("Google sign-in is on (--auth %s) but %s is empty; set it, or choose --auth device", strings.Join(methods, ","), setting.name)
			}
		}
	}
	return out, nil
}
