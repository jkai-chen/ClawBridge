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
	"os/exec"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
)

type Config struct{ URL, Audience, Account, ImpersonateServiceAccount, Principal, CachePath string }
type TokenSource interface {
	Token(context.Context) (string, error)
}
type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// credentialError is a definitive local credential failure, not a server outage.
// It prevents reads from silently falling back to an unrelated cached view.
type credentialError struct{ error }

func (e *credentialError) Unwrap() error { return e.error }

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

type Client struct {
	config     Config
	tokens     TokenSource
	http       *http.Client
	mu         sync.Mutex
	selections map[string][]mirror.Selection
}

func New(c Config) (*Client, error) {
	return NewWithTokenSource(c, &GcloudTokenSource{Account: c.Account, Audience: c.Audience, ImpersonateServiceAccount: c.ImpersonateServiceAccount})
}

// NewWithTokenSource accepts an alternative credential adapter without changing
// server authentication. Tests can supply synthetic tokens to a test verifier.
func NewWithTokenSource(c Config, tokens TokenSource) (*Client, error) {
	return NewWithHTTPClient(c, tokens, &http.Client{Timeout: 20 * time.Second})
}
func NewWithHTTPClient(c Config, tokens TokenSource, httpClient *http.Client) (*Client, error) {
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
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("cloud request rejected (%d, %s); no write queued", e.Status, e.Code)
}
func (c *Client) call(ctx context.Context, r bus.Request) (bus.Result, bool, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		var credential *credentialError
		return bus.Result{}, ctx.Err() == nil && !errors.As(err, &credential), err
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
		code := "rejected"
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(encoded, &body) == nil {
			switch body.Error.Code {
			case "unauthenticated", "forbidden", "invalid", "conflict", "not_found", "rate_limited", "unavailable":
				code = body.Error.Code
			}
		}
		return bus.Result{}, response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504, &HTTPError{Status: response.StatusCode, Code: code}
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
