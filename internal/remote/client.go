// Package remote is the authenticated cloud client, with explicit mirror sync.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/auth"
	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const (
	CredentialSourceGcloud = "gcloud"
	CredentialSourceADC    = "adc"
	CredentialSourceOAuth  = "oauth"
)

type Config struct {
	URL                       string
	Audience                  string
	Account                   string
	ImpersonateServiceAccount string
	Principal                 string
	CachePath                 string
	CredentialSource          string
	CredentialsFile           string
	OAuthClientID             string
	OAuthDeviceClientID       string
	OAuthClientSecret         string
	OAuthDeviceClientSecret   string
	OAuthKeyringService       string
}

// Route is the safe, non-secret routing metadata needed to restart a cloud
// client. It intentionally cannot carry OAuth secrets, refresh tokens,
// credential-file contents or minted tokens.
type Route struct {
	URL                       string
	Audience                  string
	Account                   string
	ImpersonateServiceAccount string
	Principal                 string
	CachePath                 string
	CredentialSource          string
	CredentialsFile           string
	OAuthClientID             string
	OAuthDeviceClientID       string
	OAuthKeyringService       string
}

func (c Config) PublicRoute() Route {
	return Route{
		URL:                       c.URL,
		Audience:                  c.Audience,
		Account:                   c.Account,
		ImpersonateServiceAccount: c.ImpersonateServiceAccount,
		Principal:                 c.Principal,
		CachePath:                 c.CachePath,
		CredentialSource:          c.CredentialSource,
		CredentialsFile:           c.CredentialsFile,
		OAuthClientID:             c.OAuthClientID,
		OAuthDeviceClientID:       c.OAuthDeviceClientID,
		OAuthKeyringService:       c.OAuthKeyringService,
	}
}

type TokenSource interface {
	Token(context.Context) (string, error)
}
type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// credentialError is a definitive local credential failure, not a server outage.
// It prevents reads from silently falling back to an unrelated cached view.
type credentialError struct{ error }

func (e *credentialError) Unwrap() error { return e.error }
func (e *credentialError) Code() string  { return "unauthenticated" }

type metadataError struct{ error }

func (e *metadataError) Code() string { return "invalid" }

func asCredentialError(err error, message string) error {
	if err == nil {
		return nil
	}
	var credential *credentialError
	if errors.As(err, &credential) {
		return err
	}
	return &credentialError{errors.New(message)}
}

type wrappedOAuthTokenSource struct {
	source TokenSource
}

func (s *wrappedOAuthTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil || s.source == nil {
		return "", &credentialError{errors.New("OAuth token source is not configured")}
	}
	token, err := s.source.Token(ctx)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", asCredentialError(err, "cannot refresh or validate the ClawBridge OAuth credential; run login again")
	}
	return token, nil
}

// GcloudTokenSource never persists tokens. Its explicit selection avoids silently
// borrowing the default project/account or misattributing a shared service account.
type GcloudTokenSource struct {
	Account, Audience, ImpersonateServiceAccount string
	mu                                           sync.Mutex
	token                                        string
	until                                        time.Time
}

func (s *GcloudTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.until) {
		return s.token, nil
	}
	if s.Account == "" || s.Audience == "" {
		return "", &credentialError{errors.New("select an explicit Google account and audience before cloud requests")}
	}
	if s.ImpersonateServiceAccount == "" {
		return "", &credentialError{errors.New("audience-bound gcloud credentials require an explicitly selected impersonation service account; generic human development tokens are unsupported")}
	}
	if s.Account == s.ImpersonateServiceAccount {
		return "", &credentialError{errors.New("self-impersonation is not supported; select the initiating human or workload account")}
	}
	args := []string{"auth", "print-identity-token", "--quiet", "--account", s.Account, "--audiences", s.Audience, "--impersonate-service-account", s.ImpersonateServiceAccount, "--include-email"}
	bounded, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, "gcloud", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return "", &credentialError{errors.New("cannot obtain an audience-bound Google token; check account login, explicit impersonation permission and connectivity (no credentials logged)")}
	}
	token := strings.TrimSpace(string(raw))
	if len(token) == 0 || len(token) > 16384 || strings.ContainsAny(token, " \r\n\t") {
		return "", &credentialError{errors.New("gcloud returned an invalid token")}
	}
	s.token = token
	s.until = time.Now().Add(4 * time.Minute)
	return token, nil
}

// ADCTokenSource obtains an audience-bound ID token by impersonating the
// explicitly selected service account with authorized-user ADC as base
// credentials. Errors are deliberately sanitized and definitive.
type ADCTokenSource struct {
	source oauth2.TokenSource
}

func (s *ADCTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil || s.source == nil {
		return "", &credentialError{errors.New("authorized-user ADC token source is not configured")}
	}
	if ctx != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	token, err := s.source.Token()
	if err != nil {
		return "", &credentialError{errors.New("cannot obtain an audience-bound Google token from authorized-user ADC; reauthenticate the credential and verify explicit impersonation permission (no credentials logged)")}
	}
	if token == nil || token.AccessToken == "" || len(token.AccessToken) > 16384 || strings.ContainsAny(token.AccessToken, " \r\n\t") {
		return "", &credentialError{errors.New("authorized-user ADC returned an invalid identity token")}
	}
	return token.AccessToken, nil
}

type Client struct {
	config     Config
	tokens     TokenSource
	http       *http.Client
	mu         sync.Mutex
	selections map[string][]mirror.Selection
}

func New(c Config) (*Client, error) {
	tokens, err := credentialTokenSource(context.Background(), c)
	if err != nil {
		return nil, err
	}
	return NewWithTokenSource(c, tokens)
}

// NewTokenSource constructs the explicit credential backend for diagnostics.
// It does not contact the ClawBridge server and never returns or renders token
// material.
func NewTokenSource(ctx context.Context, c Config) (TokenSource, error) {
	return credentialTokenSource(ctx, c)
}

func credentialTokenSource(ctx context.Context, c Config) (TokenSource, error) {
	source, err := ValidateRoute(c)
	if err != nil {
		return nil, &credentialError{err}
	}
	switch source.CredentialSource {
	case CredentialSourceGcloud:
		return &GcloudTokenSource{Account: source.Account, Audience: source.Audience, ImpersonateServiceAccount: source.ImpersonateServiceAccount}, nil
	case CredentialSourceADC:
		return newADCTokenSource(ctx, source)
	case CredentialSourceOAuth:
		if c.Audience != source.OAuthClientID {
			return nil, &credentialError{errors.New("OAuth audience must equal the active OAuth client ID; run login again")}
		}
		tokens, err := auth.NewTokenSource(ctx, auth.Config{
			ClientID:          source.OAuthClientID,
			ClientSecret:      c.OAuthClientSecret,
			ExpectedPrincipal: source.Principal,
			Store:             auth.KeyringStore{Service: source.OAuthKeyringService},
		})
		if err != nil {
			return nil, asCredentialError(err, "cannot load or validate the ClawBridge OAuth credential; run login again")
		}
		return &wrappedOAuthTokenSource{source: tokens}, nil
	}
	return nil, errors.New("credential source must be oauth, gcloud or adc")
}

func newADCTokenSource(ctx context.Context, c Config) (TokenSource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Audience == "" {
		return nil, &credentialError{errors.New("authorized-user ADC requires an explicit audience")}
	}
	if c.Account == "" {
		return nil, &credentialError{errors.New("authorized-user ADC requires an explicit initiating account")}
	}
	if c.ImpersonateServiceAccount == "" {
		return nil, &credentialError{errors.New("authorized-user ADC requires an explicit impersonation service account")}
	}
	if c.Account == c.ImpersonateServiceAccount {
		return nil, &credentialError{errors.New("self-impersonation is not supported; select the initiating user and a distinct controlled service account")}
	}
	if c.CredentialsFile == "" || !filepath.IsAbs(c.CredentialsFile) {
		return nil, &credentialError{errors.New("authorized-user ADC requires an absolute credentials file path")}
	}
	info, err := os.Stat(c.CredentialsFile)
	if err != nil || !info.Mode().IsRegular() {
		return nil, &credentialError{errors.New("authorized-user ADC credentials file must be an existing regular file; file contents were not logged")}
	}
	if info.Size() > 4<<20 {
		return nil, &credentialError{errors.New("authorized-user ADC credentials file exceeds the safety limit")}
	}
	raw, err := os.ReadFile(c.CredentialsFile)
	if err != nil {
		return nil, &credentialError{errors.New("cannot read authorized-user ADC credentials file; file contents were not logged")}
	}
	var credentialType struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &credentialType) != nil || credentialType.Type != string(option.AuthorizedUser) {
		return nil, &credentialError{errors.New("ADC credentials file must contain authorized-user credentials; file contents were not logged")}
	}
	source, err := impersonate.IDTokenSource(ctx, impersonate.IDTokenConfig{
		Audience:        c.Audience,
		TargetPrincipal: c.ImpersonateServiceAccount,
		IncludeEmail:    true,
	}, option.WithAuthCredentialsFile(option.AuthorizedUser, c.CredentialsFile))
	if err != nil {
		return nil, &credentialError{errors.New("cannot load authorized-user ADC credentials or configure impersonation; verify the credential type and file permissions (no file contents logged)")}
	}
	return &ADCTokenSource{source: source}, nil
}

// ValidateRoute applies the single explicit credential-source policy used by
// the runtime and diagnostic/installer surfaces. There is no default source and
// no cross-source fallback.
func ValidateRoute(c Config) (Config, error) {
	source := c.CredentialSource
	if source == "" {
		// Phase 2 pilot compatibility: only a missing value selects gcloud.
		source = CredentialSourceGcloud
	}
	switch source {
	case CredentialSourceGcloud:
		if c.Account == "" || c.Audience == "" || c.ImpersonateServiceAccount == "" {
			return Config{}, errors.New("gcloud credential source requires explicit account, audience and impersonation service account")
		}
		if c.Account == c.ImpersonateServiceAccount {
			return Config{}, errors.New("self-impersonation is not supported")
		}
		if c.CredentialsFile != "" || c.OAuthClientID != "" || c.OAuthDeviceClientID != "" ||
			c.OAuthClientSecret != "" || c.OAuthDeviceClientSecret != "" || c.OAuthKeyringService != "" {
			return Config{}, errors.New("gcloud credential source cannot be combined with ADC or OAuth settings")
		}
	case CredentialSourceADC:
		if c.Account == "" || c.Audience == "" || c.ImpersonateServiceAccount == "" || c.CredentialsFile == "" {
			return Config{}, errors.New("ADC credential source requires an explicit account, audience, impersonation service account and credentials file")
		}
		if c.Account == c.ImpersonateServiceAccount {
			return Config{}, errors.New("self-impersonation is not supported")
		}
		if c.OAuthClientID != "" || c.OAuthDeviceClientID != "" || c.OAuthClientSecret != "" ||
			c.OAuthDeviceClientSecret != "" || c.OAuthKeyringService != "" {
			return Config{}, errors.New("ADC credential source cannot be combined with OAuth client settings")
		}
	case CredentialSourceOAuth:
		if c.OAuthClientID == "" {
			return Config{}, errors.New("OAuth credential source requires an explicit client ID")
		}
		if c.Audience != c.OAuthClientID {
			return Config{}, errors.New("OAuth audience must equal the active OAuth client ID")
		}
		if c.Account != "" || c.ImpersonateServiceAccount != "" || c.CredentialsFile != "" {
			return Config{}, errors.New("OAuth credential source cannot be combined with gcloud account, impersonation or credentials-file settings")
		}
	default:
		return Config{}, errors.New("explicit credential source must be oauth, gcloud or adc; no fallback is available")
	}
	if c.URL == "" || c.Audience == "" || c.Principal == "" {
		return Config{}, errors.New("cloud credential routing requires server URL, audience and expected principal")
	}
	if c.CachePath == "" {
		return Config{}, errors.New("cloud credential routing requires an explicit cache path")
	}
	c.CredentialSource = source
	return c, nil
}

// NewWithTokenSource accepts an alternative credential adapter without changing
// server authentication. Tests can supply synthetic tokens to a test verifier.
func NewWithTokenSource(c Config, tokens TokenSource) (*Client, error) {
	return NewWithHTTPClient(c, tokens, &http.Client{Timeout: 20 * time.Second})
}
func NewWithHTTPClient(c Config, tokens TokenSource, httpClient *http.Client) (*Client, error) {
	if c.CredentialSource != "" {
		var err error
		c, err = ValidateRoute(c)
		if err != nil {
			return nil, err
		}
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid cloud server URL")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("cloud server requires HTTPS; HTTP is permitted only on loopback")
	}
	if c.Audience == "" || tokens == nil || httpClient == nil {
		return nil, errors.New("cloud client requires an audience, token source and HTTP client")
	}
	c.URL = strings.TrimRight(u.String(), "/")
	c.Principal = strings.ToLower(c.Principal)
	copyClient := *httpClient
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Timeout == 0 {
		copyClient.Timeout = 20 * time.Second
	}
	return &Client{config: c, tokens: tokens, http: &copyClient, selections: map[string][]mirror.Selection{}}, nil
}

type HTTPError struct {
	Status int
	Code   string
	// Message is the server's caller-facing reason for a 400 refusal, a
	// conflict or unreadable task board binding (409) or a rate limit (429),
	// kept only when it is one line of printable text.
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("cloud request rejected (%d, %s): %s; no write queued", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("cloud request rejected (%d, %s); no write queued", e.Status, e.Code)
}
func (c *Client) call(ctx context.Context, r bus.Request) (bus.Result, bool, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		// Token acquisition happens entirely before transport. Cancellation remains
		// distinct, while every callback/source error is definitive and therefore
		// cannot authorize a mirror read.
		if ctx != nil && ctx.Err() != nil {
			return bus.Result{}, false, ctx.Err()
		}
		var credential *credentialError
		if errors.As(err, &credential) {
			return bus.Result{}, false, err
		}
		return bus.Result{}, false, asCredentialError(err, "cannot acquire the configured ClawBridge credential; run login again")
	}
	if len(token) == 0 || len(token) > 16384 || strings.ContainsAny(token, " \r\n\t") {
		return bus.Result{}, false, errors.New("invalid identity token from credential source")
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 128<<10 {
		return bus.Result{}, false, errors.New("invalid or oversized cloud request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL+"/v1/execute", bytes.NewReader(raw))
	if err != nil {
		return bus.Result{}, false, errors.New("invalid cloud request URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return bus.Result{}, ctx.Err() == nil, errors.New("cloud transport unavailable; operation outcome may be uncertain, no write queued")
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil || len(encoded) > 16<<20 {
		return bus.Result{}, false, errors.New("invalid or oversized cloud response")
	}
	if response.StatusCode != http.StatusOK {
		code, message := "rejected", ""
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(encoded, &body) == nil {
			switch body.Error.Code {
			case "unauthenticated", "forbidden", "invalid", "conflict", "not_found", "rate_limited", "unavailable", "binding_unreadable":
				code = body.Error.Code
			}
			if (response.StatusCode == http.StatusBadRequest && code == "invalid") || (response.StatusCode == http.StatusConflict && (code == "binding_unreadable" || code == "conflict")) || (response.StatusCode == http.StatusTooManyRequests && code == "rate_limited") {
				message, _ = bus.SafePublicText(body.Error.Message)
			}
		}
		return bus.Result{}, response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504, &HTTPError{Status: response.StatusCode, Code: code, Message: message}
	}
	var result bus.Result
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result.Source != "server" {
		return bus.Result{}, false, errors.New("invalid authoritative cloud response")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return bus.Result{}, false, errors.New("invalid trailing cloud response")
	}
	return result, false, nil
}

func (c *Client) binding(r bus.Request) mirror.Binding {
	i, _ := r.Args["instance"].(string)
	return mirror.Binding{Server: c.config.URL, Audience: c.config.Audience, Principal: c.config.Principal, Workspace: r.Workspace, Project: r.Project, Session: r.Session, Instance: i}
}
func (c *Client) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	result, unavailable, err := c.call(ctx, r)
	if err == nil {
		return result, nil
	}
	if r.Operation == "inbox_status" || r.Operation == "notification_status" || r.Operation == "taskboard_get" || r.Operation == "taskboard_get_v2" || r.Operation == "taskboard_access" {
		// Status and binding metadata require the authority, never a mirror.
		var credential *credentialError
		var httpErr *HTTPError
		if !unavailable && ctx.Err() == nil && !errors.As(err, &credential) && !errors.As(err, &httpErr) {
			return bus.Result{}, &metadataError{err}
		}
		return bus.Result{}, err
	}
	if unavailable && mirror.Readable(r.Operation) && c.config.CachePath != "" && c.config.Principal != "" {
		cached, e := mirror.Offline(ctx, c.config.CachePath, c.binding(r), r)
		if e == nil {
			return cached, nil
		}
		return bus.Result{}, fmt.Errorf("cloud unavailable and scoped mirror unreadable: %w", e)
	}
	if unavailable && !mirror.Readable(r.Operation) {
		return bus.Result{}, errors.New("cloud write failed or its outcome is uncertain; no write queued; preserve the operation ID for an identical retry and tell the human")
	}
	return bus.Result{}, err
}

func (c *Client) ResolveProject(ctx context.Context, w, p, gitRemote string) (string, string, error) {
	if gitRemote != "" {
		if normalized, e := bus.NormalizeRemote(gitRemote); e == nil {
			gitRemote = normalized
		}
	}
	selection := mirror.Selection{Workspace: w, Project: p, GitRemote: gitRemote}
	args := map[string]any{}
	if gitRemote != "" {
		args["git_remote"] = gitRemote
	}
	result, unavailable, err := c.call(ctx, bus.Request{Operation: "resolve_project", Workspace: w, Project: p, Args: args})
	if err != nil {
		if unavailable && c.config.CachePath != "" && c.config.Principal != "" {
			return mirror.Resolve(ctx, c.config.CachePath, mirror.Binding{Server: c.config.URL, Audience: c.config.Audience, Principal: c.config.Principal}, selection)
		}
		return "", "", err
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		return "", "", errors.New("invalid project resolution response")
	}
	wid, _ := data["workspace_id"].(string)
	pid, _ := data["project_id"].(string)
	if wid == "" || pid == "" {
		return "", "", errors.New("missing immutable project resolution")
	}
	c.mu.Lock()
	scopeKey := wid + "\x00" + pid
	seen := false
	for _, existing := range c.selections[scopeKey] {
		if existing == selection {
			seen = true
			break
		}
	}
	if !seen {
		c.selections[scopeKey] = append(c.selections[scopeKey], selection)
	}
	c.mu.Unlock()
	return wid, pid, nil
}

// Sync is the only entry point that publishes server data to the local mirror.
// It is online-only, validates its full binding, and preserves the previous view
// on any transport, authorization, dataset or SQLite error.
func (c *Client) Sync(ctx context.Context, w, p, session, instance string) (bus.Result, error) {
	if c.config.CachePath == "" || c.config.Principal == "" || session == "" || instance == "" {
		return bus.Result{}, errors.New("sync requires explicit cache path, principal, session and instance")
	}
	wid, pid, err := c.ResolveProject(ctx, w, p, "")
	if err != nil {
		return bus.Result{}, err
	}
	request := bus.Request{Operation: "snapshot", Workspace: wid, Project: pid, Session: session, Args: map[string]any{"instance": instance}}
	result, _, err := c.call(ctx, request)
	if err != nil {
		return bus.Result{}, err
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		return bus.Result{}, errors.New("invalid snapshot response")
	}
	workspace, _ := data["workspace"].(map[string]any)
	project, _ := data["project"].(map[string]any)
	actor, _ := data["session"].(map[string]any)
	updated, _ := data["snapshot_at"].(string)
	if workspace["id"] != wid || project["id"] != pid || actor["instance"] != instance || (actor["id"] != session && actor["name"] != session) || data["principal"] != c.config.Principal || data["verified"] != true {
		return bus.Result{}, errors.New("snapshot identity or scope mismatch; previous mirror preserved")
	}
	for _, key := range []string{"sessions", "messages", "sent_messages", "events"} {
		rows, ok := data[key].([]any)
		if !ok || len(rows) > 10000 {
			return bus.Result{}, errors.New("invalid or excessive snapshot dataset; previous mirror preserved")
		}
		for _, row := range rows {
			if _, ok := row.(map[string]any); !ok {
				return bus.Result{}, errors.New("invalid snapshot row; previous mirror preserved")
			}
		}
	}
	c.mu.Lock()
	selections := append([]mirror.Selection{}, c.selections[wid+"\x00"+pid]...)
	c.mu.Unlock()
	selections = append(selections, mirror.Selection{Workspace: w, Project: p})
	snapshot := mirror.Snapshot{Binding: c.binding(request), Selections: selections, UpdatedAt: updated, Data: data}
	if err = mirror.Save(ctx, c.config.CachePath, snapshot); err != nil {
		return bus.Result{}, fmt.Errorf("snapshot was not published: %w", err)
	}
	return bus.Result{Source: "server", Data: map[string]any{"workspace_id": wid, "project_id": pid, "session": session, "principal": c.config.Principal, "snapshot_at": updated, "cached_messages": len(data["messages"].([]any)), "cache_updated": true}}, nil
}
