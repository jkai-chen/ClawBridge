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

// GoogleVerifier uses Google's certificate-backed validator. Email assertions are
// accepted only after signature, audience, issuer and expiry validation.
type GoogleVerifier struct {
	audience  string
	validator *idtoken.Validator
}

func NewGoogleVerifier(ctx context.Context, audience string) (*GoogleVerifier, error) {
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("an explicit Google ID-token audience is required")
	}
	v, err := idtoken.NewValidator(ctx, idtoken.WithHTTPClient(&http.Client{Timeout: 8 * time.Second}))
	if err != nil {
		return nil, errors.New("cannot initialize Google ID-token verifier")
	}
	return &GoogleVerifier{audience: audience, validator: v}, nil
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
	p, err := v.validator.Validate(ctx, token, v.audience)
	if err != nil {
		return Identity{}, errors.New("invalid identity token")
	}
	return verifiedIdentity(p, v.audience, time.Now())
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
			fail(w, status, code, publicMessage(code))
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
		case "conflict":
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
	audience := f.String("audience", "", "required Google ID-token audience")
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
	if *audience == "" || !validPrincipal(*owner) {
		return errors.New("serve requires --audience and --bootstrap-owner")
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	verifier, err := NewGoogleVerifier(ctx, *audience)
	if err != nil {
		return err
	}
	store, err := bus.OpenPostgres(ctx, dsn, strings.ToLower(*owner))
	if err != nil {
		return errors.New("cannot initialize Postgres; check the configured connection and schema version")
	}
	defer store.Close()
	srv := &http.Server{Addr: *listen, Handler: NewHandler(store, verifier), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 24 << 10}
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
