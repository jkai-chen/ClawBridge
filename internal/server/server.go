// Package server exposes the shared application behind verified Google identity.
package server

import (
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

	"clawbridge/internal/bus"
	"google.golang.org/api/idtoken"
)

type Identity struct{ Principal, Subject string }
type Verifier interface {
	Verify(context.Context, string) (Identity, error)
}
type VerifierFunc func(context.Context, string) (Identity, error)

func (f VerifierFunc) Verify(ctx context.Context, t string) (Identity, error) { return f(ctx, t) }

type Executor interface {
	ExecuteAs(context.Context, string, bus.Request) (bus.Result, error)
}

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
	if !verified || !validPrincipal(email) {
		return Identity{}, errors.New("verified Google email is required")
	}
	return Identity{Principal: strings.ToLower(email), Subject: p.Subject}, nil
}
func validPrincipal(p string) bool {
	return len(p) <= 320 && strings.Count(p, "@") == 1 && !strings.ContainsAny(p, " \t\r\n\x00") && !strings.HasPrefix(p, "@") && !strings.HasSuffix(p, "@")
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
}

func (l *limiter) allow(principal string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	v := l.users[principal]
	if now.Sub(v.start) >= time.Minute {
		v = window{start: now}
	}
	if v.count >= 1200 {
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
	l := &limiter{users: map[string]window{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				fail(w, 405, "method_not_allowed", "GET required")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
			return
		}
		if r.URL.Path == "/v1/config" {
			if r.Method != http.MethodGet {
				fail(w, 405, "method_not_allowed", "GET required")
				return
			}
			config := struct {
				BrowserClientID     string `json:"browser_client_id"`
				BrowserClientSecret string `json:"browser_client_secret"`
				DeviceClientID      string `json:"device_client_id"`
				DeviceClientSecret  string `json:"device_client_secret"`
			}{
				BrowserClientID:     metadata.BrowserClientID,
				BrowserClientSecret: metadata.BrowserClientSecret,
				DeviceClientID:      metadata.DeviceClientID,
				DeviceClientSecret:  metadata.DeviceClientSecret,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config)
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
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			fail(w, 401, "unauthenticated", "a Google ID token is required")
			return
		}
		fields := strings.Fields(headers[0])
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 16384 || verifier == nil {
			fail(w, 401, "unauthenticated", "a valid Google ID token is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		identity, err := verifier.Verify(ctx, fields[1])
		if err != nil || !validPrincipal(identity.Principal) || identity.Subject == "" {
			fail(w, 401, "unauthenticated", "Google identity verification failed")
			return
		}
		if !l.allow(identity.Principal) {
			fail(w, 429, "rate_limited", "principal request limit reached")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request bus.Request
		if err = decoder.Decode(&request); err != nil {
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
		result, err := store.ExecuteAs(ctx, identity.Principal, request)
		if err != nil {
			code, status := serviceError(err)
			fail(w, status, code, responseMessage(code, err))
			return
		}
		result.Source = "server"
		if request.Operation == "whoami" {
			if data, ok := result.Data.(map[string]any); ok {
				data["principal"] = identity.Principal
				data["subject"] = identity.Subject
				data["verified"] = true
				data["identity_provenance"] = "google-id-token"
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
		case "conflict", "unsupported_mapping":
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

// responseMessage shows a validation refusal's own message when the bus
// marked it as written for the caller; every other failure, including
// database and unknown errors, gets only the generic text for its code.
func responseMessage(code string, err error) string {
	if code == "invalid" {
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
	case "conflict":
		return "operation conflicts with existing state; verify session instance and retry identifier"
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
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:8080", "listen address; use a private/IAP path or TLS proxy for remote access")
	audience := f.String("audience", "", "required comma-separated Google ID-token audience set")
	browserClientID := f.String("oauth-client-id", "", "optional Desktop-app OAuth client ID; must be in --audience")
	deviceClientID := f.String("oauth-device-client-id", "", "optional TVs and Limited Input devices OAuth client ID; must be in --audience")
	owner := f.String("bootstrap-owner", "", "verified principal allowed to initialize a new workspace")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("serve accepts flags only")
	}
	audiences, err := parseAudiences(*audience)
	if err != nil || !validPrincipal(*owner) {
		return errors.New("serve requires --audience and --bootstrap-owner")
	}
	metadata, err := validateOAuthMetadata(audiences, OAuthMetadata{
		BrowserClientID:     strings.TrimSpace(*browserClientID),
		BrowserClientSecret: os.Getenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET"),
		DeviceClientID:      strings.TrimSpace(*deviceClientID),
		DeviceClientSecret:  os.Getenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET"),
	})
	if err != nil {
		return err
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	verifier, err := NewGoogleVerifierForAudiences(ctx, audiences)
	if err != nil {
		return err
	}
	store, err := bus.OpenPostgres(ctx, dsn, strings.ToLower(*owner))
	if err != nil {
		return errors.New("cannot initialize Postgres; check the configured connection and schema version")
	}
	defer store.Close()
	srv := &http.Server{Addr: *listen, Handler: NewHandlerWithOAuthMetadata(store, verifier, metadata), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 24 << 10}
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
