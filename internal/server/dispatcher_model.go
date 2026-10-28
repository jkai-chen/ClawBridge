package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
)

// The dispatcher model's launcher and agent endpoints (DISPATCHER-MODEL-API
// d4 §1b and d4.1): agentd's launcher token (cbl_) opens wanted, credential
// and report; the one-time agent credential (cba_) opens config once, then
// renew, test-result and status with its binding secret. They sit outside
// the session API, with their own authentication, limits and error shape
// ({"error": code}); no body of theirs is ever logged.
const (
	LauncherWantedPath     = "/v1/dispatcher/launcher/wanted"
	LauncherCredentialPath = "/v1/dispatcher/launcher/credential"
	LauncherReportPath     = "/v1/dispatcher/launcher/report"
	AgentConfigPath        = "/v1/dispatcher-model/agent/config"
	AgentRenewPath         = "/v1/dispatcher-model/agent/renew"
	AgentTestResultPath    = "/v1/dispatcher-model/agent/test-result"
	AgentStatusPath        = "/v1/dispatcher-model/agent/status"
)

const (
	modelAPIVersion = "1"
	modelBodyBytes  = 256 << 10
	// modelAuthFailures failed authentications in modelAuthWindow from an
	// address, or for a token prefix, refuse its failures with 429 for
	// modelLockout (§1). A valid token always gets through.
	modelAuthFailures = 10
	modelAuthWindow   = time.Minute
	modelLockout      = 10 * time.Minute
	// modelLockoutKeys bounds the lockout table; past it, failures from
	// keys it doesn't hold count together under one overflow key.
	modelLockoutKeys = 10000
	// defaultMaxAgents is dispatcher_model_max_agents' default (§5.1).
	defaultMaxAgents = 2
	maxAgentsCeiling = 20
)

// ModelStore is the hosted store's launcher and agent API.
type ModelStore interface {
	LauncherWanted(ctx context.Context, token string) ([]bus.LauncherAgent, error)
	LauncherCredential(ctx context.Context, token, ref string) (string, time.Time, error)
	LauncherReport(ctx context.Context, token, ref, code string) error
	RedeemAgentCredential(ctx context.Context, credential, instanceID, source string) (bus.AgentConfig, error)
	RenewAgentTokens(ctx context.Context, credential, secret string) ([]bus.ModelProjectToken, error)
	AgentTestResult(ctx context.Context, credential, secret string, revision int, result, reason string) error
	AgentStatus(ctx context.Context, credential, secret, status string) error
	ModelHello(ctx context.Context, token string, maxAgents int) (bus.ModelHello, error)
	ModelNextCase(ctx context.Context, token string, maxAgents int) (bus.ModelNext, error)
	ModelTool(ctx context.Context, token, caseID, tool string, args map[string]any) (bus.ModelToolResult, error)
}

// modelPath reports whether path is one of the launcher, agent or case
// endpoints.
func modelPath(path string) bool {
	switch path {
	case LauncherWantedPath, LauncherCredentialPath, LauncherReportPath, AgentConfigPath, AgentRenewPath, AgentTestResultPath, AgentStatusPath, ModelHelloPath, ModelCasesNextPath:
		return true
	}
	_, _, ok := casePath(path)
	return ok
}

// modelFail answers in the model API's own error shape.
func modelFail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"error": code}
	if code == "unsupported_version" {
		body["supported"] = []int{1}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// modelRefuse answers a case refusal; tool_refused names the tool.
func modelRefuse(w http.ResponseWriter, e caseError) {
	status, known := caseRefusals[e.code]
	if !known {
		status, e.code = 503, "server_busy"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"error": e.code}
	if e.code == "tool_refused" && codePattern.MatchString(e.tool) {
		body["tool"] = e.tool
	}
	_ = json.NewEncoder(w).Encode(body)
}

func modelOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newModelHandler serves the launcher, agent and case endpoints for store.
// nudge, when set, asks the watcher for an early pass of a project; hub,
// when set, wakes cases/next on each new case.
func newModelHandler(store ModelStore, hub *StreamHub, maxAgents int, now func() time.Time, nudge func(string)) http.Handler {
	failures := &modelLockouts{now: now, keys: map[string]*lockoutState{}}
	// Each authenticated token's calls: polls and renewals are well under
	// these. Only calls that authenticated are counted, so random tokens
	// can't fill the table.
	calls := &tokenRate{now: now, perMinute: 60, seen: map[string]window{}}
	reports := &tokenRate{now: now, perMinute: 6, seen: map[string]window{}}
	polls := &casePolls{held: map[string]bool{}, limit: modelLockoutKeys}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := http.MethodPost
		if r.URL.Path == LauncherWantedPath || r.URL.Path == ModelHelloPath {
			want = http.MethodGet
		}
		if r.Method != want {
			modelFail(w, 405, "invalid_argument")
			return
		}
		if r.Header.Get("ClawBridge-Model-API") != modelAPIVersion {
			modelFail(w, 400, "unsupported_version")
			return
		}
		// A token travels only over HTTPS, or to a client on this machine
		// (§1): the agent's, and the launcher's, which mints credentials
		// that open every owner's provider key (G9-3).
		if plainRemote(r) {
			modelFail(w, 403, "unauthorized")
			return
		}
		address := "ip:" + clientAddress(r)
		token, ok := modelBearer(r)
		tokenPrefix := "token:" + token[:min(len(token), 12)]
		// A failure from a locked-out address or prefix gets 429; it is
		// counted only while not locked out, so the lockout runs its
		// fixed 10 minutes.
		unauthorized := func() {
			if failures.locked(address) || failures.locked(tokenPrefix) {
				modelFail(w, 429, "rate_limited")
				return
			}
			failures.fail(address)
			failures.fail(tokenPrefix)
			modelFail(w, 401, "unauthorized")
		}
		// Each endpoint takes one kind of token: the launcher's (cbl_), the
		// agent credential (cba_), or a project token (cbm_).
		prefix := "cba_"
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/dispatcher/launcher/"):
			prefix = "cbl_"
		case !strings.HasPrefix(r.URL.Path, "/v1/dispatcher-model/agent/"):
			prefix = "cbm_"
		}
		if !ok || !strings.HasPrefix(token, prefix) {
			unauthorized()
			return
		}
		limit := calls
		if r.URL.Path == LauncherReportPath || r.URL.Path == AgentStatusPath {
			limit = reports
		}
		if limit.over(tokenKey(token)) {
			modelFail(w, 429, "rate_limited")
			return
		}
		var body map[string]json.RawMessage
		if want == http.MethodPost {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, modelBodyBytes))
			if err != nil || json.Unmarshal(raw, &body) != nil || body == nil {
				modelFail(w, 400, "invalid_argument")
				return
			}
		}
		// A long-poll may wait casePollLimit before its last check.
		limitTime := 15 * time.Second
		if r.URL.Path == ModelCasesNextPath {
			limitTime += casePollLimit
		}
		ctx, cancel := context.WithTimeout(r.Context(), limitTime)
		defer cancel()
		var out any
		var err error
		switch caseID, tool, isTool := casePath(r.URL.Path); {
		case r.URL.Path == ModelHelloPath:
			out, err = store.ModelHello(ctx, token, maxAgents)
		case r.URL.Path == ModelCasesNextPath:
			out, err = serveCasesNext(ctx, store, hub, token, body, maxAgents, polls, nudge)
		case isTool:
			out, err = serveCaseTool(ctx, store, token, caseID, tool, body, nudge)
		default:
			out, err = serveModel(ctx, store, r, token, body, maxAgents)
		}
		if err == nil || !errors.Is(err, bus.ErrModelUnauthorized) && !errors.Is(err, bus.ErrModelUnavailable) {
			limit.record(tokenKey(token))
		}
		var refusal caseError
		switch {
		case errors.As(err, &refusal):
			modelRefuse(w, refusal)
		case errors.Is(err, bus.ErrAgentCap):
			modelFail(w, 503, "agent_cap")
		case errors.Is(err, errPollBusy):
			// The agent reads retry_after from the body; the header is for
			// anything else.
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "server_busy", "retry_after": 1})
		case errors.Is(err, bus.ErrModelUnavailable):
			// No seal key on this server: not the caller's failure.
			modelFail(w, 401, "unauthorized")
		case errors.Is(err, bus.ErrModelUnauthorized):
			unauthorized()
		case errors.Is(err, errModelArgument):
			modelFail(w, 400, "invalid_argument")
		case errors.Is(err, bus.ErrModelConflict):
			modelFail(w, 409, "conflict")
		case err != nil:
			code, _ := serviceError(err)
			switch code {
			case "invalid":
				modelFail(w, 400, "invalid_argument")
			case "rate_limited":
				modelFail(w, 429, "rate_limited")
			case "not_found":
				// The case API's code for it; agentd's launcher keeps
				// not_found.
				if strings.HasPrefix(r.URL.Path, "/v1/dispatcher/launcher/") || strings.HasPrefix(r.URL.Path, "/v1/dispatcher-model/agent/") {
					modelFail(w, 404, "not_found")
				} else {
					modelFail(w, 404, "case_not_found")
				}
			default:
				modelFail(w, 503, "server_busy")
			}
		default:
			modelOK(w, out)
		}
	})
}

var errModelArgument = errors.New("invalid_argument")

// fields reads body as exactly the named fields, all present: requests are
// closed.
func fields(body map[string]json.RawMessage, names ...string) (map[string]json.RawMessage, error) {
	if len(body) != len(names) {
		return nil, errModelArgument
	}
	for _, n := range names {
		if _, ok := body[n]; !ok {
			return nil, errModelArgument
		}
	}
	return body, nil
}

func stringField(raw json.RawMessage) (string, error) {
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return "", errModelArgument
	}
	return v, nil
}

// serveModel runs one authenticated-by-shape request against the store,
// which checks the token itself.
func serveModel(ctx context.Context, store ModelStore, r *http.Request, token string, body map[string]json.RawMessage, maxAgents int) (any, error) {
	switch r.URL.Path {
	case LauncherWantedPath:
		agents, err := store.LauncherWanted(ctx, token)
		if err != nil {
			return nil, err
		}
		return map[string]any{"agents": agents, "max_agents": maxAgents}, nil
	case LauncherCredentialPath:
		f, err := fields(body, "owner_ref")
		if err != nil {
			return nil, err
		}
		ref, err := stringField(f["owner_ref"])
		if err != nil {
			return nil, err
		}
		credential, expires, err := store.LauncherCredential(ctx, token, ref)
		if err != nil {
			return nil, err
		}
		return map[string]any{"credential": credential, "expires_at": expires.UTC().Format(time.RFC3339)}, nil
	case LauncherReportPath:
		f, err := fields(body, "owner_ref", "code")
		if err != nil {
			return nil, err
		}
		ref, err1 := stringField(f["owner_ref"])
		code, err2 := stringField(f["code"])
		if err1 != nil || err2 != nil {
			return nil, errModelArgument
		}
		return map[string]any{"ok": true}, store.LauncherReport(ctx, token, ref, code)
	case AgentConfigPath:
		f, err := fields(body, "instance_id")
		if err != nil {
			return nil, err
		}
		instance, err := stringField(f["instance_id"])
		if err != nil {
			return nil, err
		}
		cfg, err := store.RedeemAgentCredential(ctx, token, instance, clientAddress(r))
		if err != nil {
			return nil, err
		}
		return cfg, nil
	case AgentRenewPath:
		f, err := fields(body, "binding_secret")
		if err != nil {
			return nil, err
		}
		secret, err := stringField(f["binding_secret"])
		if err != nil {
			return nil, err
		}
		projects, err := store.RenewAgentTokens(ctx, token, secret)
		if err != nil {
			return nil, err
		}
		return map[string]any{"projects": projects}, nil
	case AgentTestResultPath:
		names := []string{"binding_secret", "config_revision", "result"}
		if _, ok := body["reason"]; ok {
			names = append(names, "reason")
		}
		f, err := fields(body, names...)
		if err != nil {
			return nil, err
		}
		secret, err1 := stringField(f["binding_secret"])
		result, err2 := stringField(f["result"])
		var revision int
		err3 := json.Unmarshal(f["config_revision"], &revision)
		reason := ""
		var err4 error
		if raw, ok := f["reason"]; ok {
			reason, err4 = stringField(raw)
		}
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || revision < 1 {
			return nil, errModelArgument
		}
		return map[string]any{"ok": true}, store.AgentTestResult(ctx, token, secret, revision, result, reason)
	case AgentStatusPath:
		f, err := fields(body, "binding_secret", "status")
		if err != nil {
			return nil, err
		}
		secret, err1 := stringField(f["binding_secret"])
		status, err2 := stringField(f["status"])
		if err1 != nil || err2 != nil {
			return nil, errModelArgument
		}
		return map[string]any{"ok": true}, store.AgentStatus(ctx, token, secret, status)
	}
	return nil, errModelArgument
}

// plainRemote reports a request that reached the server over plain HTTP
// from another machine. Behind the bundled proxy (a loopback or private
// peer) the scheme is the proxy's X-Forwarded-Proto; otherwise it is the
// connection's own.
func plainRemote(r *http.Request) bool {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if peer := net.ParseIP(host); peer != nil && (peer.IsLoopback() || peer.IsPrivate()) {
		if values := r.Header.Values("X-Forwarded-Proto"); len(values) > 0 {
			entries := strings.Split(values[len(values)-1], ",")
			scheme = strings.ToLower(strings.TrimSpace(entries[len(entries)-1]))
		}
	}
	client := net.ParseIP(clientAddress(r))
	return scheme != "https" && (client == nil || !client.IsLoopback())
}

// modelBearer is the request's one Bearer credential, at most 256 bytes.
func modelBearer(r *http.Request) (string, bool) {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 {
		return "", false
	}
	fields := strings.Fields(headers[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 256 {
		return "", false
	}
	return fields[1], true
}

// tokenKey is a token's rate-limit key: its digest, never the token.
// modelLockouts is the model API's failed-authentication lockout (§1).
type modelLockouts struct {
	mu   sync.Mutex
	now  func() time.Time
	keys map[string]*lockoutState
}

type lockoutState struct {
	start time.Time
	count int
	until time.Time
}

const lockoutOverflow = "overflow"

// held is key's state, or the overflow's when the table doesn't hold key
// and is full.
func (l *modelLockouts) held(key string, now time.Time) *lockoutState {
	if v, ok := l.keys[key]; ok {
		return v
	}
	if len(l.keys) >= modelLockoutKeys {
		for k, v := range l.keys {
			if now.Sub(v.start) >= modelAuthWindow && !now.Before(v.until) {
				delete(l.keys, k)
			}
		}
	}
	if len(l.keys) >= modelLockoutKeys {
		key = lockoutOverflow
		if v, ok := l.keys[key]; ok {
			return v
		}
	}
	v := &lockoutState{start: now}
	l.keys[key] = v
	return v
}

func (l *modelLockouts) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if v, ok := l.keys[key]; ok {
		return now.Before(v.until)
	}
	v, ok := l.keys[lockoutOverflow]
	return len(l.keys) >= modelLockoutKeys && ok && now.Before(v.until)
}

// fail counts one failure for key; the tenth within a minute locks it out
// for modelLockout.
func (l *modelLockouts) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	v := l.held(key, now)
	if now.Sub(v.start) >= modelAuthWindow {
		v.start, v.count = now, 0
	}
	if v.count++; v.count >= modelAuthFailures {
		v.until, v.start, v.count = now.Add(modelLockout), now, 0
	}
}

// tokenRate limits each authenticated token's calls per minute. over only
// reads; record counts a call that authenticated.
type tokenRate struct {
	mu        sync.Mutex
	now       func() time.Time
	perMinute int
	seen      map[string]window
}

func (t *tokenRate) over(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.seen[key]
	return ok && t.now().Sub(v.start) < time.Minute && v.count >= t.perMinute
}

func (t *tokenRate) record(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	v := t.seen[key]
	if now.Sub(v.start) >= time.Minute {
		if len(t.seen) >= modelLockoutKeys {
			for k, w := range t.seen {
				if now.Sub(w.start) >= time.Minute {
					delete(t.seen, k)
				}
			}
		}
		v = window{start: now}
	}
	v.count++
	t.seen[key] = v
}

func tokenKey(token string) string {
	sum := bus.TokenDigest(token)
	return "model:" + sum[:16]
}

// readModelSettings reads the model seal keys and the agent cap from the
// environment. Without CLAWBRIDGE_MODEL_KEY_SEAL_KEY model setup is off;
// the previous key is set only while a rotation is under way.
func readModelSettings(getenv func(string) string) (*bus.ModelSeal, int, error) {
	maxAgents := defaultMaxAgents
	if raw := getenv("CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAgentsCeiling {
			return nil, 0, fmt.Errorf("CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS must be 1–%d", maxAgentsCeiling)
		}
		maxAgents = n
	}
	current := getenv("CLAWBRIDGE_MODEL_KEY_SEAL_KEY")
	previous := getenv("CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS")
	if current == "" {
		if previous != "" {
			return nil, 0, errors.New("CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS needs CLAWBRIDGE_MODEL_KEY_SEAL_KEY")
		}
		return nil, maxAgents, nil
	}
	keys := [][]byte{}
	for _, raw := range []string{current, previous} {
		if raw == "" {
			keys = append(keys, nil)
			continue
		}
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) != 32 {
			// The value is never echoed.
			return nil, 0, errors.New("the model key seal keys must be 32 bytes, base64-encoded")
		}
		keys = append(keys, key)
	}
	if keys[1] != nil && bytes.Equal(keys[0], keys[1]) {
		return nil, 0, errors.New("CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS must differ from the current key")
	}
	seal, err := bus.NewModelSeal(keys[0], keys[1])
	return seal, maxAgents, err
}

// memInfoPath is where the host's available memory is read, for doctor's
// headroom warning (§5.1).
var memInfoPath = "/proc/meminfo"

// memAvailable reads MemAvailable from memInfoPath, in bytes; ok is false
// where there is none (not Linux).
func memAvailable() (uint64, bool) {
	raw, err := os.ReadFile(memInfoPath)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			n, err := strconv.ParseUint(fields[1], 10, 64)
			return n << 10, err == nil
		}
	}
	return 0, false
}
