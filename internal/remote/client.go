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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/mirror"
	"clawbridge/internal/outbox"
)

// CredentialSourceDevice signs each request with this device's key
// (live/AUTH-INVITE-DESIGN.md §5): there is no bearer token. It is the only
// credential source since v1.0 removed Google sign-in (oauth, gcloud, adc).
const CredentialSourceDevice = "device"

// GoogleRouteRemoved answers a profile that still signs in with Google.
const GoogleRouteRemoved = "this profile signs in with Google, which was removed in v1.0: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"

type Config struct {
	URL string
	// Audience labels a client built over an injected token source (tests
	// and embedded hosts); a device route never uses it.
	Audience         string
	Principal        string
	CachePath        string
	CredentialSource string
	// DeviceID and KeyStore select a device route's key; KeyHome is the
	// state directory a file key lives under.
	DeviceID string
	KeyStore string
	KeyHome  string
	// ClockOffset is a device route's stored clock correction, measured at
	// ClockOffsetAt; SaveClockOffset, when set, keeps a new one.
	ClockOffset     time.Duration
	ClockOffsetAt   time.Time
	SaveClockOffset func(time.Duration, time.Time)
}

// Route is the safe, non-secret routing metadata needed to restart a cloud
// client. It intentionally cannot carry OAuth secrets, refresh tokens,
// credential-file contents or minted tokens.
type Route struct {
	URL              string
	Principal        string
	CachePath        string
	CredentialSource string
	DeviceID         string
	KeyStore         string
}

func (c Config) PublicRoute() Route {
	return Route{
		URL:              c.URL,
		Principal:        c.Principal,
		CachePath:        c.CachePath,
		CredentialSource: c.CredentialSource,
		DeviceID:         c.DeviceID,
		KeyStore:         c.KeyStore,
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

// CredentialFailure marks err as the route's own credential failing
// before anything was sent, as a token source's error is.
func CredentialFailure(err error) error { return &credentialError{err} }

// IsCredentialFailure reports whether err is this route's own credential
// failing before anything was sent.
func IsCredentialFailure(err error) bool {
	var c *credentialError
	return errors.As(err, &c)
}

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

type Client struct {
	config     Config
	tokens     TokenSource
	signer     *devicesign.Signer
	http       *http.Client
	mu         sync.Mutex
	selections map[string][]mirror.Selection
	// The profile's outbox, opened on first use (outbox.go).
	outboxOnce  sync.Once
	outboxStore atomic.Pointer[outbox.Store]
	// outboxKick is signalled when a call succeeds while writes wait.
	outboxKick chan struct{}
	// maintenanceUntil is when the server's maintenance ends, in Unix
	// nanoseconds, as its last answer said; 0 when it isn't in one.
	maintenanceUntil atomic.Int64
}

func New(c Config) (*Client, error) {
	signer, err := NewDeviceSigner(c)
	if err != nil {
		return nil, err
	}
	return NewWithSigner(c, signer, &http.Client{Timeout: 20 * time.Second})
}

// NewDeviceSigner loads a device route's key from its store and signs for
// the server's origin.
func NewDeviceSigner(c Config) (*devicesign.Signer, error) {
	route, err := ValidateRoute(c)
	if err != nil {
		return nil, &credentialError{err}
	}
	if route.CredentialSource != CredentialSourceDevice {
		return nil, &credentialError{errors.New("not a device route")}
	}
	u, _ := url.Parse(route.URL)
	if route.KeyStore == devicesign.StoreFile && !filepath.IsAbs(route.KeyHome) {
		return nil, &credentialError{errors.New("a file device key needs the absolute ClawBridge state directory")}
	}
	key, err := devicesign.Load(devicesign.Location{Store: route.KeyStore, Host: u.Host, Home: route.KeyHome, DeviceID: route.DeviceID})
	if err != nil {
		return nil, &credentialError{err}
	}
	return devicesign.NewSigner(key, Origin(u), c.ClockOffset, c.ClockOffsetAt), nil
}

// Origin is a server URL's origin, as device signatures cover it.
func Origin(u *url.URL) string { return strings.ToLower(u.Scheme + "://" + u.Host) }

// ValidateRoute checks a device route, the only credential source. A
// profile that still names a Google source (oauth, gcloud, adc, or none,
// which was gcloud) is refused with GoogleRouteRemoved.
func ValidateRoute(c Config) (Config, error) {
	if c.CredentialSource != CredentialSourceDevice {
		return Config{}, errors.New(GoogleRouteRemoved)
	}
	if !devicekey.ValidID(c.DeviceID) {
		return Config{}, errors.New("device credential source requires this device's ID")
	}
	if !devicesign.ValidStore(c.KeyStore) {
		return Config{}, errors.New("device credential source requires key_store keyring, file or env")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return Config{}, errors.New("a device route's server URL must be the server's origin, with no path")
	}
	if c.URL == "" || c.Principal == "" || c.CachePath == "" {
		return Config{}, errors.New("cloud credential routing requires server URL, expected principal and an explicit cache path")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c, nil
}

// NewWithTokenSource sends an injected bearer token: tests and embedded hosts
// pair it with a server's injected verifier. A served ClawBridge refuses
// Bearer since v1.0, so no route of a profile uses it.
func NewWithTokenSource(c Config, tokens TokenSource) (*Client, error) {
	return NewWithHTTPClient(c, tokens, &http.Client{Timeout: 20 * time.Second})
}
func NewWithHTTPClient(c Config, tokens TokenSource, httpClient *http.Client) (*Client, error) {
	if tokens == nil {
		return nil, errors.New("cloud client requires an audience, token source and HTTP client")
	}
	return newClient(c, tokens, nil, httpClient)
}

// NewWithSigner is a client that signs every request with a device key.
func NewWithSigner(c Config, signer *devicesign.Signer, httpClient *http.Client) (*Client, error) {
	if signer == nil {
		return nil, errors.New("a device client requires a signer")
	}
	if c.DeviceID != "" && c.DeviceID != signer.DeviceID() {
		return nil, errors.New("the device signer's key is not this route's device")
	}
	if c.Principal == "" {
		return nil, errors.New("a device client requires its expected principal")
	}
	c.CredentialSource, c.DeviceID = CredentialSourceDevice, signer.DeviceID()
	return newClient(c, nil, signer, httpClient)
}

func newClient(c Config, tokens TokenSource, signer *devicesign.Signer, httpClient *http.Client) (*Client, error) {
	// A signer was built from a validated route, or given by a caller that
	// holds the key; the origin check below binds it to this server. An
	// injected token source is a test's or embedded host's own.
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
	if (signer == nil && c.Audience == "") || httpClient == nil {
		return nil, errors.New("cloud client requires an audience, token source and HTTP client")
	}
	if signer != nil && (signer.Origin() != Origin(u) || (u.Path != "" && u.Path != "/")) {
		return nil, errors.New("the device signer's origin is not this server's, or the server URL has a path")
	}
	c.URL = strings.TrimRight(u.String(), "/")
	c.Principal = strings.ToLower(c.Principal)
	copyClient := *httpClient
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Timeout == 0 {
		copyClient.Timeout = 20 * time.Second
	}
	return &Client{config: c, tokens: tokens, signer: signer, http: &copyClient, selections: map[string][]mirror.Selection{}, outboxKick: make(chan struct{}, 1)}, nil
}

// LinearWriteTimeout bounds a linear_write request: past the server's
// 60-second write deadline, so the client hears its answer.
const LinearWriteTimeout = 75 * time.Second

type HTTPError struct {
	Status int
	Code   string
	// RetryAfter is the server's Retry-After, when it sent one.
	RetryAfter time.Duration
	// Message is the server's caller-facing reason for a 400 refusal, a
	// conflict or unreadable task board binding (409) or a rate limit (429),
	// kept only when it is one line of printable text.
	Message string
}

// RetryAfterHint is the server's Retry-After, for callers that wait on a
// refusal without importing this package.
func (e *HTTPError) RetryAfterHint() time.Duration { return e.RetryAfter }

// Rejection reports the status, code and kept message, for callers that
// tell refusals apart without importing this package.
func (e *HTTPError) Rejection() (status int, code, message string) {
	return e.Status, e.Code, e.Message
}

func (e *HTTPError) Error() string {
	if e.Code == "maintenance" {
		return fmt.Sprintf("the server is in maintenance; nothing was sent; retry after %d s with the same operation ID; no write queued", max(1, int(e.RetryAfter/time.Second)))
	}
	if e.Message != "" {
		return fmt.Sprintf("cloud request rejected (%d, %s): %s; no write queued", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("cloud request rejected (%d, %s); no write queued", e.Status, e.Code)
}
func (c *Client) call(ctx context.Context, r bus.Request) (bus.Result, bool, error) {
	token := ""
	if c.signer == nil {
		var err error
		token, err = c.tokens.Token(ctx)
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
			return bus.Result{}, false, asCredentialError(err, "cannot acquire the configured ClawBridge credential")
		}
		if len(token) == 0 || len(token) > 16384 || strings.ContainsAny(token, " \r\n\t") {
			return bus.Result{}, false, errors.New("invalid identity token from credential source")
		}
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 128<<10 {
		return bus.Result{}, false, errors.New("invalid or oversized cloud request")
	}
	client := c.http
	if r.Operation == "linear_write" {
		// The server may take up to a minute on a write it makes as the
		// ClawBridge app, and finishes it even if this client goes away;
		// the caller's context still bounds the wait.
		longer := *c.http
		longer.Timeout = LinearWriteTimeout
		client = &longer
	}
	status, encoded, header, err := c.send(ctx, client, http.MethodPost, "/v1/execute", raw, token)
	if err != nil {
		return bus.Result{}, status == 0 && ctx.Err() == nil, err
	}
	if status != http.StatusOK {
		code, message := "rejected", ""
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(encoded, &body) == nil {
			switch body.Error.Code {
			case "unauthenticated", "forbidden", "invalid", "conflict", "not_found", "rate_limited", "unavailable", "binding_unreadable", "in_progress", "clock_skew", "device_auth_failed":
				code = body.Error.Code
			case "maintenance":
				if status == http.StatusServiceUnavailable {
					code = body.Error.Code
				}
			}
			if (status == http.StatusBadRequest && code == "invalid") || (status == http.StatusConflict && (code == "binding_unreadable" || code == "conflict" || code == "in_progress")) || (status == http.StatusTooManyRequests && code == "rate_limited") || (status == http.StatusUnauthorized && c.signer != nil) {
				message, _ = bus.SafePublicText(body.Error.Message)
			}
		}
		failure := &HTTPError{Status: status, Code: code, Message: message, RetryAfter: retryAfter(header)}
		if code == "maintenance" {
			c.maintenanceUntil.Store(time.Now().Add(max(failure.RetryAfter, time.Second)).UnixNano())
		}
		// A server in maintenance refused before doing anything: that is
		// definitive (nothing was sent), not an uncertain outage (§2.4).
		return bus.Result{}, code != "maintenance" && (status == 502 || status == 503 || status == 504), failure
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
	if result.PendingDeviceEvents != nil && *result.PendingDeviceEvents > 0 && c.signer != nil {
		c.noticeDeviceEvents(ctx, r)
	}
	c.noteMaintenance(header)
	c.kickOutbox()
	return result, false, nil
}

// deviceEventsNotice receives this device's unacknowledged device events
// (live/AUTH-INVITE-DESIGN.md §17.3) when a signed answer reports some.
// The CLI sets it to print one line each on stderr; nothing else does, so
// a TUI or MCP server never writes over its own output.
var (
	deviceEventsMu     sync.Mutex
	deviceEventsNotice func([]map[string]any)
	deviceEventsAt     time.Time
)

// deviceEventsEvery bounds how often one process fetches the events.
const deviceEventsEvery = time.Minute

// SetDeviceEventsNotice sets (or, with nil, clears) the receiver of this
// process's device events.
func SetDeviceEventsNotice(f func([]map[string]any)) {
	deviceEventsMu.Lock()
	defer deviceEventsMu.Unlock()
	deviceEventsNotice, deviceEventsAt = f, time.Time{}
}

// noticeDeviceEvents fetches the events after r's answer reported some, at
// most once a minute, and hands them to the receiver. A failure is
// silent: the next answer reports them again.
func (c *Client) noticeDeviceEvents(ctx context.Context, r bus.Request) {
	if r.Operation == "device_events" || r.Operation == "device_ack" || r.Workspace == "" {
		return
	}
	deviceEventsMu.Lock()
	notice := deviceEventsNotice
	due := notice != nil && time.Since(deviceEventsAt) >= deviceEventsEvery
	if due {
		deviceEventsAt = time.Now()
	}
	deviceEventsMu.Unlock()
	if !due {
		return
	}
	result, _, err := c.call(ctx, bus.Request{Operation: "device_events", Workspace: r.Workspace, Args: map[string]any{}})
	if err != nil {
		return
	}
	data, _ := result.Data.(map[string]any)
	raw, _ := data["events"].([]any)
	events := []map[string]any{}
	for _, v := range raw {
		if e, ok := v.(map[string]any); ok {
			events = append(events, e)
		}
	}
	if len(events) > 0 {
		notice(events)
	}
}

// send makes one authenticated request for target (path and query, in the
// one form the server accepts) and reads the answer. A device route signs
// target and sends exactly it; a clock_skew refusal is corrected by the
// server's time and retried once, with a new nonce and signature. status
// is 0 when nothing came back.
func (c *Client) send(ctx context.Context, client *http.Client, method, target string, body []byte, token string) (int, []byte, http.Header, error) {
	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, method, target, body, token)
		if err != nil {
			var credential *credentialError
			if errors.As(err, &credential) {
				return -1, nil, nil, err
			}
			return 0, nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return 0, nil, nil, errors.New("cloud transport unavailable; operation outcome may be uncertain, no write queued")
		}
		encoded, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
		response.Body.Close()
		if err != nil || len(encoded) > 16<<20 {
			return -1, nil, nil, errors.New("invalid or oversized cloud response")
		}
		if c.signer == nil || attempt > 0 || response.StatusCode != http.StatusUnauthorized {
			return response.StatusCode, encoded, response.Header, nil
		}
		serverTime, skewed := clockSkewTime(encoded)
		if !skewed {
			return response.StatusCode, encoded, response.Header, nil
		}
		offset, err := c.signer.Correct(serverTime)
		if err != nil {
			return -1, nil, nil, &credentialError{err}
		}
		if c.config.SaveClockOffset != nil {
			_, at := c.signer.Offset()
			c.config.SaveClockOffset(offset, at)
		}
	}
}

// newRequest is one authenticated request for target (path and query, in
// the one form the server accepts).
func (c *Client) newRequest(ctx context.Context, method, target string, body []byte, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.config.URL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid cloud request URL")
	}
	// Opaque is sent verbatim as the request target, so nothing
	// re-encodes what was signed. A bearer route keeps its server URL's
	// path in front, as before (R85-L1); a device route has none, since
	// the signature covers the target alone.
	path, query, _ := strings.Cut(target, "?")
	if c.signer == nil {
		path = strings.TrimSuffix(req.URL.EscapedPath(), "/") + path
	}
	req.URL.Opaque, req.URL.RawQuery = path, query
	if c.signer != nil {
		header, e := c.signer.Authorization(method, target, body)
		if e != nil {
			return nil, &credentialError{e}
		}
		req.Header.Set("Authorization", header)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// clockSkewTime reads a clock_skew refusal's server_time.
func clockSkewTime(encoded []byte) (int64, bool) {
	var body struct {
		Error struct {
			Code       string `json:"code"`
			ServerTime int64  `json:"server_time"`
		} `json:"error"`
	}
	if json.Unmarshal(encoded, &body) != nil || body.Error.Code != "clock_skew" || body.Error.ServerTime <= 0 {
		return 0, false
	}
	return body.Error.ServerTime, true
}

// Signer is the device signer of a device route, or nil.
func (c *Client) Signer() *devicesign.Signer { return c.signer }

func (c *Client) binding(r bus.Request) mirror.Binding {
	i, _ := r.Args["instance"].(string)
	return mirror.Binding{Server: c.config.URL, Audience: MirrorAudience(c.config.Audience, c.config.CredentialSource, c.config.DeviceID), Principal: c.config.Principal, Workspace: r.Workspace, Project: r.Project, Session: r.Session, Instance: i}
}

// ErrOverviewOffline is project_overview's answer when the server can't be
// reached: the overview is read from the authority only.
var ErrOverviewOffline = errors.New("project overview unavailable offline: the server could not be reached")

// ErrMarkersOffline is marker_status's answer when the server can't be
// reached: marker states are read from the authority only.
var ErrMarkersOffline = errors.New("the claim markers are unavailable offline: the server could not be reached")

// ErrThreadsOffline answers a thread read when the server can't be
// reached: the project's threads are read from the authority only.
var ErrThreadsOffline = errors.New("the project's threads are unavailable offline: the server could not be reached")

func (c *Client) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if outbox.Queueable(r) {
		if ob := c.outbox(); ob != nil {
			return c.executeQueueable(ctx, ob, r)
		}
	}
	result, unavailable, err := c.call(ctx, r)
	return c.answer(ctx, r, result, unavailable, err)
}

// answer is Execute's answer to one call: the result, a mirror read when
// the authority can't be reached, or the error.
func (c *Client) answer(ctx context.Context, r bus.Request, result bus.Result, unavailable bool, err error) (bus.Result, error) {
	if err == nil {
		return result, nil
	}
	if unavailable && (r.Operation == "project_threads" || r.Operation == "project_thread") {
		return bus.Result{}, ErrThreadsOffline
	}
	if r.Operation == "inbox_status" || r.Operation == "notification_status" || r.Operation == "taskboard_get" || r.Operation == "taskboard_get_v2" || r.Operation == "taskboard_access" || r.Operation == "label_policy_get" || r.Operation == "doc_policy_get" {
		// Status and binding metadata require the authority, never a mirror.
		var credential *credentialError
		var httpErr *HTTPError
		if !unavailable && ctx.Err() == nil && !errors.As(err, &credential) && !errors.As(err, &httpErr) {
			return bus.Result{}, &metadataError{err}
		}
		return bus.Result{}, err
	}
	if unavailable && r.Operation == "linear_issue_history" {
		// A read the server makes as the ClawBridge app: never mirrored,
		// and it writes nothing to Linear.
		return bus.Result{}, fmt.Errorf("the issue's history could not be read: the ClawBridge server could not be reached or could not read Linear; nothing was written; retry: %w", err)
	}
	if unavailable && r.Operation == "project_overview" {
		// A summary read from the authority, never mirrored: say so plainly.
		return bus.Result{}, ErrOverviewOffline
	}
	if unavailable && r.Operation == "marker_status" {
		return bus.Result{}, ErrMarkersOffline
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
			return mirror.Resolve(ctx, c.config.CachePath, mirror.Binding{Server: c.config.URL, Audience: MirrorAudience(c.config.Audience, c.config.CredentialSource, c.config.DeviceID), Principal: c.config.Principal}, selection)
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

// MirrorAudience is the audience part of a route's mirror binding. A Google
// route's is its token audience; a device route has none, so its key is
// the device (R85-M2): a snapshot synced under one device key is never
// read under another.
func MirrorAudience(audience, credentialSource, deviceID string) string {
	if credentialSource == CredentialSourceDevice && deviceID != "" {
		return "device:" + deviceID
	}
	return audience
}
