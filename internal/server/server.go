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
	"clawbridge/internal/principal"
	"google.golang.org/api/idtoken"
)

type Identity struct{ Principal, Subject, HostedDomain string }
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
	return Identity{Principal: email, Subject: p.Subject, HostedDomain: hostedDomain}, nil
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
	// Auth chooses the sign-in methods; the zero value is Google only.
	Auth AuthOptions
	// Now is the clock device sign-in reads; nil is time.Now.
	Now func() time.Time
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
	var intake, tick http.Handler
	if s, ok := store.(IntakeStore); ok {
		intake = newIntakeHandler(s, o.IntakeProver, l)
	}
	if o.TickVerifier != nil && o.Ticker != nil {
		tick = newTickHandler(o.TickVerifier, o.Ticker)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	methods := []string{AuthGoogle}
	if len(o.Auth.Methods) > 0 {
		methods = o.Auth.Methods
	}
	failures := &failureLog{now: now, last: map[string]time.Time{}}
	failedAuth := newCounter(failedAuthPerMin, time.Minute, now)
	cache := newDeviceCache(now)
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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
			intake.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == TickPath && tick != nil {
			tick.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/v1/join" && join != nil {
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
		address := clientAddress(r)
		if failedAuth.full(address) {
			fail(w, 429, "rate_limited", "too many failed sign-ins from this address; try again later")
			return
		}
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			fail(w, 401, "unauthenticated", required)
			return
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
			return
		}
		// The body is read raw before authentication: a device signs its
		// hash.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if err != nil {
			fail(w, 400, "invalid", "invalid or oversized request JSON")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		verified, refused := authenticator.Authenticate(ctx, r, strings.TrimSpace(credentials), body)
		if refused != nil {
			if refused.status == 401 {
				failedAuth.add(address)
				failures.record(address, refused.kind)
			}
			if refused.serverTime {
				failWithTime(w, refused.status, refused.code, refused.message, now())
			} else {
				fail(w, refused.status, refused.code, refused.message)
			}
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
		if err = decoder.Decode(&trailing); err != io.EOF {
			fail(w, 400, "invalid", "exactly one request object is required")
			return
		}
		if request.Operation == "" || store == nil {
			fail(w, 400, "invalid", "an operation is required")
			return
		}
		var result bus.Result
		if request.Operation == "linear_write" || request.Operation == "linear_app_status" || request.Operation == "linear_app_states" {
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
		if revokesDevices(request.Operation) {
			cache.clear()
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
	writer, err := appWriter(store)
	if err != nil {
		return err
	}
	auth.Devices = store
	options := startWatcher(ctx, store, watch, HandlerOptions{OAuth: metadata, Writer: writer, Auth: auth})
	// A Linear write the server runs as the app may take up to a minute.
	srv := &http.Server{Addr: settings.Listen, Handler: NewHandlerWithOptions(store, verifier, options), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 80 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 24 << 10}
	result := make(chan error, 1)
	go func() { result <- srv.ListenAndServe() }()
	select {
	case err = <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// openServingStore opens the hosted authority the server serves. Every request
// must pass the account binding, so the store refuses any path that would
// reach it without the verified account.
func openServingStore(ctx context.Context, dsn, owner string) (*bus.Store, error) {
	store, err := bus.OpenPostgres(ctx, dsn, strings.ToLower(owner))
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
	const usage = "usage: clawbridge operator principal-unbind EMAIL --confirm [--allow-previous] | workspace-clear-domains WORKSPACE --confirm | linear-app-probe --confirm [--project UUID] [--p8 [--dry-run]] | linear-app-doc-probe --confirm | bootstrap-invite --workspace NAME [--force-stdout] [--database-url-env NAME]"
	if len(args) > 0 && args[0] == "bootstrap-invite" {
		return runBootstrapInvite(ctx, args[1:], stdout)
	}
	if len(args) == 0 || (args[0] != "principal-unbind" && args[0] != "workspace-clear-domains" && args[0] != "linear-app-probe" && args[0] != "linear-app-doc-probe") {
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
	p8, dryRun := false, false
	if command == "linear-app-probe" {
		f.StringVar(&project, "project", "", "a bound Linear project UUID whose health inputs are read; with --p8, the empty throwaway project")
		f.BoolVar(&p8, "p8", false, "post one real status update to the empty --project as the app, read it back, delete it and read again (P8)")
		f.BoolVar(&dryRun, "dry-run", false, "with --p8: check the project and the app's tools, and send nothing")
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
	probe := command == "linear-app-probe" || command == "linear-app-doc-probe"
	if probe != (len(positional) == 0) || len(positional) > 1 {
		return errors.New(usage)
	}
	if p8 && project == "" || dryRun && !p8 {
		return errors.New("--p8 needs --project, and --dry-run needs --p8")
	}
	if !*confirm {
		if command == "linear-app-probe" && p8 {
			return errors.New("linear-app-probe --p8 posts one real status update to the empty project as the Linear app, then deletes it; repeat with --confirm")
		}
		if command == "linear-app-probe" {
			return errors.New("linear-app-probe sends one project update for a project that does not exist, as the Linear app; repeat with --confirm")
		}
		if command == "linear-app-doc-probe" {
			return errors.New("linear-app-doc-probe lists the workspace's documents of the last 30 days as the Linear app, one read, and prints counts only; repeat with --confirm")
		}
		if command == "workspace-clear-domains" {
			return errors.New("workspace-clear-domains lets any Google account that is a member in; repeat with --confirm")
		}
		return errors.New("principal-unbind releases an address from its Google account; repeat with --confirm")
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
	if command == "linear-app-probe" && p8 {
		return linearAppP8(ctx, store, project, dryRun, stdout)
	}
	if command == "linear-app-probe" {
		return linearAppProbe(ctx, store, project, stdout)
	}
	if command == "linear-app-doc-probe" {
		return linearAppDocProbe(ctx, store, stdout)
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
	out := ServeSettings{Listen: *listen, BootstrapOwner: *owner, DatabaseURLEnv: *dsnEnv, Auth: AuthOptions{Methods: methods}}
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
