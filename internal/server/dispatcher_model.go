package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
}

// modelPath reports whether path is one of the launcher or agent endpoints.
func modelPath(path string) bool {
	switch path {
	case LauncherWantedPath, LauncherCredentialPath, LauncherReportPath, AgentConfigPath, AgentRenewPath, AgentTestResultPath, AgentStatusPath:
		return true
	}
	return false
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

func modelOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newModelHandler serves the launcher and agent endpoints for store.
func newModelHandler(store ModelStore, maxAgents int, now func() time.Time) http.Handler {
	failures := &modelLockouts{now: now, keys: map[string]*lockoutState{}}
	// Each authenticated token's calls: polls and renewals are well under
	// these. Only calls that authenticated are counted, so random tokens
	// can't fill the table.
	calls := &tokenRate{now: now, perMinute: 60, seen: map[string]window{}}
	reports := &tokenRate{now: now, perMinute: 6, seen: map[string]window{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := http.MethodPost
		if r.URL.Path == LauncherWantedPath {
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
		address := "ip:" + clientAddress(r)
		token, ok := modelBearer(r)
		prefix := "token:" + token[:min(len(token), 12)]
		// A failure from a locked-out address or prefix gets 429; it is
		// counted only while not locked out, so the lockout runs its
		// fixed 10 minutes.
		unauthorized := func() {
			if failures.locked(address) || failures.locked(prefix) {
				modelFail(w, 429, "rate_limited")
				return
			}
			failures.fail(address)
			failures.fail(prefix)
			modelFail(w, 401, "unauthorized")
		}
		launcher := strings.HasPrefix(r.URL.Path, "/v1/dispatcher/launcher/")
		if !ok || launcher != strings.HasPrefix(token, "cbl_") || !launcher && !strings.HasPrefix(token, "cba_") {
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
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		out, err := serveModel(ctx, store, r, token, body, maxAgents)
		if err == nil || !errors.Is(err, bus.ErrModelUnauthorized) && !errors.Is(err, bus.ErrModelUnavailable) {
			limit.record(tokenKey(token))
		}
		switch {
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
			case "not_found":
				modelFail(w, 404, "not_found")
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
