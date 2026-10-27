package bus

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The dispatcher model's launcher side (DISPATCHER-AGENT-DESIGN r2 §11,
// DISPATCHER-MODEL-API d4 §1b and d4.1): the owner's sealed provider key,
// the launcher's two endpoints and its report, and the agent credential's
// one-time redemption. Schema 17 holds the keys, credentials, launcher
// tokens and project tokens; schema 23 adds what the flow records.
//
// Schema 23:
//   - dispatcher_owners: each key-holder's opaque ref (p_ and 16 random
//     characters, stable across rotations; the provider and agentd see only
//     it [N3]) and the last config revision handed out.
//   - the API kind of a key, and its row's creation time.
//   - a credential's redemption: the source address, and a flag for doctor
//     (a second redemption, or a wrong binding secret [N2, AG-M2a]).
//   - the project tokens a credential issued, so they die with it.
//   - per key-holder, the agent's status, the launcher's report and the last
//     setup test's failure.
//
// The same DDL serves SQLite and PostgreSQL.
const dispatcherModelSchemaV23 = `
CREATE TABLE dispatcher_owners(owner_principal TEXT PRIMARY KEY,ref TEXT NOT NULL UNIQUE,last_revision INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL);
ALTER TABLE dispatcher_model_keys ADD COLUMN api TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_agent_credentials ADD COLUMN redeemed_from TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_agent_credentials ADD COLUMN flagged TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_model_tokens ADD COLUMN credential_id TEXT;
CREATE TABLE dispatcher_agent_status(owner_principal TEXT PRIMARY KEY,status TEXT NOT NULL DEFAULT '',status_at TEXT,launcher_report TEXT NOT NULL DEFAULT '',report_at TEXT,test_revision INTEGER NOT NULL DEFAULT 0,test_failure TEXT NOT NULL DEFAULT '',test_at TEXT);
`

const (
	// agentCredentialTTL is how long a minted credential waits for its
	// first redemption.
	agentCredentialTTL = 5 * time.Minute
	// modelTokenTTL is a project token's life; the agent renews it [N5].
	modelTokenTTL = time.Hour
	// setupTestWait is how long a pending key waits for its agent before
	// setup reports agent_unavailable.
	setupTestWait = 10 * time.Minute
	// setupTestLimit ends a pending key even when an agent took it up and
	// never answered.
	setupTestLimit = 30 * time.Minute
	// maxModelKey bounds a provider credential: a key, or a Bedrock or
	// Vertex credential document.
	maxModelKey = 16 << 10
)

var (
	// ErrModelUnauthorized is every launcher or agent authentication
	// failure: one answer, whatever failed.
	ErrModelUnauthorized = errors.New("unauthorized")
	// ErrModelUnavailable is a redemption on a server with no seal key: the
	// same 401, but not the caller's failure.
	ErrModelUnavailable = errors.New("unauthorized")
	// ErrModelConflict is a test result for a revision that is not pending.
	ErrModelConflict = errors.New("conflict")

	ownerRefPattern       = regexp.MustCompile(`^p_[a-z0-9]{16}$`)
	launcherTokenPattern  = regexp.MustCompile(`^cbl_[A-Za-z0-9_-]{43}$`)
	agentCredentialPattrn = regexp.MustCompile(`^cba_[A-Za-z0-9_-]{43}$`)
	bindingSecretPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	instanceIDPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	providerIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	modelIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
	egressVersionPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	// providerBaseURL is the agent's own rule [A8]: HTTPS, or plain HTTP to
	// a loopback host only.
	providerBaseURL = regexp.MustCompile(`^(https://[^/?#@\s]+|http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]{1,5})?)(/[^?#\s]*)?$`)

	// SetupTestFailures are the fixed reasons a setup test fails (§11.3).
	SetupTestFailures = []string{"invalid_key", "no_model_access", "quota", "rate_limited", "no_tool_use", "egress_blocked", "provider_error", "timeout"}
	// LauncherReportCodes are what agentd may report for doctor: a fixed
	// set, open to additions.
	LauncherReportCodes = []string{"agent_crashloop"}
)

// ModelSeal holds the seal keys for owners' provider keys: the current
// one, and the previous one while a rotation is under way [AG-L1].
type ModelSeal struct{ current, previous cipher.AEAD }

// NewModelSeal builds a ModelSeal from 32-byte keys; previous may be nil.
func NewModelSeal(current, previous []byte) (*ModelSeal, error) {
	c, err := modelAEAD(current)
	if err != nil {
		return nil, err
	}
	m := &ModelSeal{current: c}
	if previous != nil {
		if m.previous, err = modelAEAD(previous); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func modelAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("a model key seal key is 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetModelSeal enables model setup; without it setup is unavailable.
func (s *Store) SetModelSeal(m *ModelSeal) { s.modelSeal = m }

// modelAAD binds a sealed key to its owner, revision and everything that
// says where it is sent (provider, API, base URL, model), so a ciphertext
// moved to another row, or a row whose base_url was pointed elsewhere by
// someone without the seal key, fails to open.
func modelAAD(owner, provider, api, baseURL, model string, revision int) []byte {
	return []byte(strings.Join([]string{owner, provider, api, baseURL, model, strconv.Itoa(revision)}, "\x00"))
}

func (m *ModelSeal) seal(key, aad []byte) (ciphertext, nonce string, err error) {
	n := make([]byte, m.current.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(m.current.Seal(nil, n, key, aad)), base64.StdEncoding.EncodeToString(n), nil
}

func (m *ModelSeal) open(ciphertext, nonce string, aad []byte) ([]byte, error) {
	c, err1 := base64.StdEncoding.DecodeString(ciphertext)
	n, err2 := base64.StdEncoding.DecodeString(nonce)
	if err1 != nil || err2 != nil {
		return nil, errors.New("sealed model key is unreadable")
	}
	for _, a := range []cipher.AEAD{m.current, m.previous} {
		if a == nil || len(n) != a.NonceSize() {
			continue
		}
		if key, err := a.Open(nil, n, c, aad); err == nil {
			return key, nil
		}
	}
	return nil, errors.New("sealed model key does not open")
}

// randomToken is prefix and 32 random bytes, base64url without padding.
func randomToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenDigest is a token's SHA-256 in hex: what is stored and looked up.
func TokenDigest(token string) string { return tokenDigest(token) }

func tokenDigest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ownerRef returns principal's ref, creating it on first use.
func ownerRef(ctx context.Context, tx *transaction, principal, nowRaw string) (string, error) {
	rows, err := query(ctx, tx, `SELECT ref FROM dispatcher_owners WHERE owner_principal=?`, principal)
	if err != nil || len(rows) == 1 {
		if err != nil {
			return "", err
		}
		return str(rows[0], "ref"), nil
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	ref := "p_" + string(b)
	_, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_owners(owner_principal,ref,created_at) VALUES(?,?,?)`, principal, ref, nowRaw)
	return ref, err
}

// ModelCaps are the owner's limits, set at setup (§11.1 step 4).
type ModelCaps struct {
	RunsPerHour       int   `json:"runs_per_hour"`
	RunsPerDay        int   `json:"runs_per_day"`
	MaxInputTokens    int   `json:"max_input_tokens_per_run"`
	MaxOutputTokens   int   `json:"max_output_tokens_per_run"`
	DailyTokenCeiling int64 `json:"daily_tokens"`
}

// parseModelCaps decodes caps strictly over the defaults; the daily token
// ceiling has none, since it is the owner's bill.
func parseModelCaps(v any) (ModelCaps, error) {
	c := ModelCaps{RunsPerHour: 10, RunsPerDay: 50, MaxInputTokens: 20000, MaxOutputTokens: 2000}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 1<<10 {
		return c, invalid("caps must be a small JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil {
		return c, invalid("caps fields are runs_per_hour, runs_per_day, max_input_tokens_per_run, max_output_tokens_per_run and daily_tokens")
	}
	switch {
	case c.RunsPerHour < 1 || c.RunsPerHour > 100:
		return c, invalid("runs_per_hour must be 1–100")
	case c.RunsPerDay < c.RunsPerHour || c.RunsPerDay > 1000:
		return c, invalid("runs_per_day must be from runs_per_hour to 1000")
	case c.MaxInputTokens < 1000 || c.MaxInputTokens > 200000:
		return c, invalid("max_input_tokens_per_run must be 1,000–200,000")
	case c.MaxOutputTokens < 100 || c.MaxOutputTokens > 32000:
		return c, invalid("max_output_tokens_per_run must be 100–32,000")
	case c.DailyTokenCeiling < 1000 || c.DailyTokenCeiling > 100_000_000:
		return c, invalid("daily_tokens is required: the owner's daily token ceiling, 1,000–100,000,000")
	}
	return c, nil
}

// providerTarget is a base URL's host:port as the egress proxy matches it:
// a lower-case host, an IPv6 literal in brackets, and an explicit port.
func providerTarget(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid base URL")
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port), nil
}

func dispatcherModelOperation(op string) bool {
	return op == "dispatcher_model_setup" || op == "dispatcher_model_status" || op == "dispatcher_model_key_delete" || op == "dispatcher_model_doctor"
}

// dispatcherModelOp runs the owner's model setup commands (§11.1, §11.7).
// The key is the caller's own, not the project's: the project names whose
// owner may set it. Nothing here ever returns, records or reports the key.
func (s *Store) dispatcherModelOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, r Request) (any, error) {
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	switch r.Operation {
	case "dispatcher_model_setup":
		return s.modelSetup(ctx, tx, w, p, nowRaw, r.Args)
	case "dispatcher_model_status":
		revision, e := integer(r.Args["config_revision"])
		if e != nil || revision < 1 {
			return nil, invalid("dispatcher_model_status requires config_revision")
		}
		return s.modelSetupStatus(ctx, tx, nowRaw, revision)
	case "dispatcher_model_doctor":
		return s.modelDoctor(ctx, tx, w, p, nowRaw)
	}
	return s.modelKeyDelete(ctx, tx, w, p, nowRaw)
}

func (s *Store) modelSetup(ctx context.Context, tx *transaction, w, p map[string]any, nowRaw string, a map[string]any) (any, error) {
	if s.modelSeal == nil {
		return nil, scoped("model setup is unavailable: the server has no model key seal key", "unavailable")
	}
	provider, api, model, baseURL := str(a, "provider_id"), str(a, "api"), str(a, "model"), str(a, "base_url")
	key, _ := a["key"].(string)
	switch {
	case !providerIDPattern.MatchString(provider):
		return nil, invalid("provider_id must be a provider ID from the providers list")
	case !providerIDPattern.MatchString(api):
		return nil, invalid("api must be the provider's API kind from the providers list")
	case !modelIDPattern.MatchString(model):
		return nil, invalid("model must be a model ID")
	case !providerBaseURL.MatchString(baseURL):
		return nil, invalid("base_url must be https, or http to a loopback host")
	case strings.TrimSpace(key) == "" || len(key) > maxModelKey || !utf8.ValidString(key):
		// Never echoed: the message names the field only.
		return nil, invalid("key must be the provider credential, at most 16 KiB")
	case a["egress_confirmed"] != true:
		return nil, invalid("the egress disclosure must be confirmed (egress_confirmed: true)")
	case !egressVersionPattern.MatchString(str(a, "egress_text_version")):
		return nil, invalid("egress_text_version must name the disclosure text shown")
	}
	if _, err := providerTarget(baseURL); err != nil {
		return nil, invalid("base_url must be https, or http to a loopback host")
	}
	caps, err := parseModelCaps(a["caps"])
	if err != nil {
		return nil, err
	}
	owner := s.principal
	if _, err = ownerRef(ctx, tx, owner, nowRaw); err != nil {
		return nil, err
	}
	var revision int
	if err = tx.QueryRowContext(ctx, `SELECT last_revision+1 FROM dispatcher_owners WHERE owner_principal=?`, owner).Scan(&revision); err != nil {
		return nil, err
	}
	ciphertext, nonce, err := s.modelSeal.seal([]byte(key), modelAAD(owner, provider, api, baseURL, model, revision))
	if err != nil {
		return nil, errors.New("the key could not be sealed")
	}
	fingerprint := key
	if len(fingerprint) > 4 {
		fingerprint = fingerprint[len(fingerprint)-4:]
	}
	capsJSON, _ := json.Marshal(caps)
	// A new setup replaces a pending one that never finished its test.
	if _, err = tx.ExecContext(ctx, `DELETE FROM dispatcher_model_keys WHERE owner_principal=? AND state='pending_test'`, owner); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,api,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,caps,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES(?,'pending_test',?,?,?,?,?,?,?,?,?,?,?,?,?)`, owner, provider, api, baseURL, model, ciphertext, nonce, fingerprint, revision, string(capsJSON), str(a, "egress_text_version"), nowRaw, owner, nowRaw); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_owners SET last_revision=? WHERE owner_principal=?`, revision, owner); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, str(w, "id"), str(p, "id"), "", "dispatcher_model_key", strconv.Itoa(revision), "setup", map[string]any{"provider_id": provider, "model": model, "egress_text_version": str(a, "egress_text_version")}); err != nil {
		return nil, err
	}
	return map[string]any{"config_revision": revision, "test": "pending"}, nil
}

// modelSetupStatus is a setup's test result: pending, active, or the fixed
// failure code. A pending key no agent took up within setupTestWait fails
// as agent_unavailable, and is deleted like any failure.
func (s *Store) modelSetupStatus(ctx context.Context, tx *transaction, nowRaw string, revision int) (any, error) {
	owner := s.principal
	rows, err := query(ctx, tx, `SELECT state,created_at FROM dispatcher_model_keys WHERE owner_principal=? AND config_revision=?`, owner, revision)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"config_revision": revision}
	if len(rows) == 1 {
		if str(rows[0], "state") == "active" {
			out["test"] = "active"
			return out, nil
		}
		expired, err := pendingExpired(ctx, tx, owner, str(rows[0], "created_at"), nowRaw)
		if err != nil {
			return nil, err
		}
		if !expired {
			out["test"] = "pending"
			return out, nil
		}
		if err = recordTestFailure(ctx, tx, owner, revision, "agent_unavailable", nowRaw); err != nil {
			return nil, err
		}
	}
	st, err := query(ctx, tx, `SELECT test_failure FROM dispatcher_agent_status WHERE owner_principal=? AND test_revision=?`, owner, revision)
	if err != nil {
		return nil, err
	}
	if len(st) == 1 && str(st[0], "test_failure") != "" {
		out["test"] = str(st[0], "test_failure")
		return out, nil
	}
	return nil, scoped("no model setup with that revision", "not_found")
}

// pendingExpired reports whether a pending key created at createdAt has
// waited too long for its test: setupTestWait while no agent has redeemed
// a credential for it, or setupTestLimit even once one has.
func pendingExpired(ctx context.Context, tx *transaction, owner, createdAt, nowRaw string) (bool, error) {
	now := at(nowRaw)
	if createdAt <= layout(now.Add(-setupTestLimit)) {
		return true, nil
	}
	if createdAt > layout(now.Add(-setupTestWait)) {
		return false, nil
	}
	claimed, err := query(ctx, tx, `SELECT 1 FROM dispatcher_agent_credentials WHERE owner_principal=? AND revoked_at IS NULL AND used_at IS NOT NULL AND used_at>=?`, owner, createdAt)
	return len(claimed) == 0, err
}

// recordTestFailure deletes revision's pending key and records why. An
// owner left with no key keeps no live credential: its agent stops getting
// project tokens, and agentd stops it.
func recordTestFailure(ctx context.Context, tx *transaction, owner string, revision int, reason, nowRaw string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM dispatcher_model_keys WHERE owner_principal=? AND state='pending_test' AND config_revision=?`, owner, revision); err != nil {
		return err
	}
	left, err := query(ctx, tx, `SELECT 1 FROM dispatcher_model_keys WHERE owner_principal=?`, owner)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		if err = revokeOwnerCredentials(ctx, tx, owner, nowRaw); err != nil {
			return err
		}
	}
	return upsertAgentStatus(ctx, tx, owner, `test_revision=?,test_failure=?,test_at=?`, revision, reason, nowRaw)
}

func upsertAgentStatus(ctx context.Context, tx *transaction, owner, set string, args ...any) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO dispatcher_agent_status(owner_principal) VALUES(?) ON CONFLICT DO NOTHING`, owner); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE dispatcher_agent_status SET `+set+` WHERE owner_principal=?`, append(args, owner)...)
	return err
}

// modelKeyDelete deletes the caller's sealed keys and revokes its agent
// credentials and project tokens; agentd then stops its container.
func (s *Store) modelKeyDelete(ctx context.Context, tx *transaction, w, p map[string]any, nowRaw string) (any, error) {
	owner := s.principal
	res, err := tx.ExecContext(ctx, `DELETE FROM dispatcher_model_keys WHERE owner_principal=?`, owner)
	if err != nil {
		return nil, err
	}
	deleted, _ := res.RowsAffected()
	if err = revokeOwnerCredentials(ctx, tx, owner, nowRaw); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, str(w, "id"), str(p, "id"), "", "dispatcher_model_key", owner, "deleted", map[string]any{"rows": deleted}); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": deleted > 0}, nil
}

// revokeOwnerCredentials revokes every live agent credential of owner and
// the project tokens they issued.
func revokeOwnerCredentials(ctx context.Context, tx *transaction, owner, nowRaw string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE dispatcher_model_tokens SET revoked_at=? WHERE revoked_at IS NULL AND credential_id IN (SELECT id FROM dispatcher_agent_credentials WHERE owner_principal=?)`, nowRaw, owner); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE dispatcher_agent_credentials SET revoked_at=? WHERE owner_principal=? AND revoked_at IS NULL`, nowRaw, owner)
	return err
}

// maxModelProjects is how many projects one owner's agent serves: the
// agent's schema caps its project list at 100. Enable and advise on refuse
// one more; an owner can still pass it by gaining projects (a role or a
// workspace), and then the first 100 by project ID are served and doctor
// says so.
const maxModelProjects = 100

// modelProjects are the projects owner's agent gets tokens for [Q5, d7
// Q7], and so the launcher's reason to run it: the first maxModelProjects
// of allModelProjects.
func modelProjects(ctx context.Context, tx *transaction, owner string) ([]string, error) {
	all, err := allModelProjects(ctx, tx, owner)
	if len(all) > maxModelProjects {
		all = all[:maxModelProjects]
	}
	return all, err
}

// allModelProjects are owner's projects with the model on, in mode model
// or as the advisor in rules mode, by project ID.
func allModelProjects(ctx context.Context, tx *transaction, owner string) ([]string, error) {
	// A local store has no project roles: its workspace owner owns all.
	owns, args := `w.owner_principal=?`, []any{owner}
	if tx.postgres {
		owns, args = `w.owner_principal=? OR EXISTS(SELECT 1 FROM project_roles r WHERE r.project_id=p.id AND r.principal=? AND r.role='owner')`, []any{owner, owner}
	}
	rows, err := query(ctx, tx, `SELECT p.id,d.mode,d.model FROM projects p JOIN workspaces w ON w.id=p.workspace_id JOIN dispatcher_policies d ON d.project_id=p.id WHERE d.enabled=1 AND (`+owns+`) ORDER BY p.id`, args...)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rows {
		if modelUseOf(true, str(r, "mode"), str(r, "model")) != "" {
			out = append(out, str(r, "id"))
		}
	}
	return out, nil
}

// LauncherAgent is one key-holder that needs an agent.
type LauncherAgent struct {
	OwnerRef       string `json:"owner_ref"`
	Provider       string `json:"provider"`
	ConfigRevision int    `json:"config_revision"`
}

// LauncherTokenTTL is a launcher token's life from its creation [F5]: the
// operator creates the next one and revokes the old one before then.
const LauncherTokenTTL = 90 * 24 * time.Hour

// launcherAuth checks a launcher token and records its use: the launcher
// polls, so its last use is its heartbeat for doctor. An expired token gets
// the same refusal as an unknown one.
func launcherAuth(ctx context.Context, tx *transaction, token, nowRaw string) error {
	if !launcherTokenPattern.MatchString(token) {
		return ErrModelUnauthorized
	}
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_launcher_tokens SET last_used_at=? WHERE token_sha256=? AND revoked_at IS NULL AND created_at>?`, nowRaw, tokenDigest(token), layout(at(nowRaw).Add(-LauncherTokenTTL)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrModelUnauthorized
	}
	return nil
}

// wantedAgents is every key-holder that needs an agent: one with a pending
// key (its setup test runs in the agent), or with an active key and a
// model-enabled project. The provider is the pending key's when there is
// one, since the test reaches it.
func wantedAgents(ctx context.Context, tx *transaction, nowRaw string) ([]LauncherAgent, error) {
	rows, err := query(ctx, tx, `SELECT owner_principal,state,base_url,config_revision,created_at FROM dispatcher_model_keys ORDER BY owner_principal,state DESC`)
	if err != nil {
		return nil, err
	}
	// Owners with an active key come first, so an abandoned setup can't take
	// their place under max_agents.
	var active, pending []LauncherAgent
	seen := map[string]bool{}
	for _, r := range rows {
		owner := str(r, "owner_principal")
		if seen[owner] {
			continue
		}
		// pending_test sorts before active. A pending key no agent took up in
		// time fails as agent_unavailable here, so it holds no agent slot.
		if str(r, "state") == "pending_test" {
			expired, err := pendingExpired(ctx, tx, owner, str(r, "created_at"), nowRaw)
			if err != nil {
				return nil, err
			}
			if expired {
				if err = recordTestFailure(ctx, tx, owner, asInt(r["config_revision"]), "agent_unavailable", nowRaw); err != nil {
					return nil, err
				}
				continue
			}
		}
		if str(r, "state") == "active" {
			projects, err := modelProjects(ctx, tx, owner)
			if err != nil {
				return nil, err
			}
			if len(projects) == 0 {
				continue
			}
		}
		target, err := providerTarget(str(r, "base_url"))
		if err != nil {
			continue
		}
		ref, err := ownerRef(ctx, tx, owner, nowRaw)
		if err != nil {
			return nil, err
		}
		seen[owner] = true
		agent := LauncherAgent{OwnerRef: ref, Provider: target, ConfigRevision: asInt(r["config_revision"])}
		if str(r, "state") == "active" {
			active = append(active, agent)
		} else {
			pending = append(pending, agent)
		}
	}
	return append(append([]LauncherAgent{}, active...), pending...), nil
}

// LauncherWanted answers GET launcher/wanted for a launcher token.
func (s *Store) LauncherWanted(ctx context.Context, token string) ([]LauncherAgent, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = launcherAuth(ctx, tx, token, nowRaw); err != nil {
		return nil, err
	}
	out, err := wantedAgents(ctx, tx, nowRaw)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// ownerByRef is the key-holder behind a wanted ref.
func ownerByRef(ctx context.Context, tx *transaction, ref, nowRaw string) (string, error) {
	if !ownerRefPattern.MatchString(ref) {
		return "", scoped("unknown owner ref", "not_found")
	}
	wanted, err := wantedAgents(ctx, tx, nowRaw)
	if err != nil {
		return "", err
	}
	for _, w := range wanted {
		if w.OwnerRef == ref {
			var owner string
			err = tx.QueryRowContext(ctx, `SELECT owner_principal FROM dispatcher_owners WHERE ref=?`, ref).Scan(&owner)
			return owner, err
		}
	}
	return "", scoped("that owner needs no agent", "not_found")
}

// LauncherCredential mints a one-time agent credential for a wanted ref,
// valid agentCredentialTTL until its first redemption. It revokes the
// owner's previous credentials and their project tokens: one instance at a
// time.
func (s *Store) LauncherCredential(ctx context.Context, token, ref string) (string, time.Time, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return "", time.Time{}, err
	}
	if err = launcherAuth(ctx, tx, token, nowRaw); err != nil {
		return "", time.Time{}, err
	}
	owner, err := ownerByRef(ctx, tx, ref, nowRaw)
	if err != nil {
		return "", time.Time{}, err
	}
	// The owner's row is the lock: a concurrent mint waits here, then
	// revokes this one's credential instead of missing it.
	if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_owners SET last_revision=last_revision WHERE owner_principal=?`, owner); err != nil {
		return "", time.Time{}, err
	}
	if err = revokeOwnerCredentials(ctx, tx, owner, nowRaw); err != nil {
		return "", time.Time{}, err
	}
	credential, err := randomToken("cba_")
	if err != nil {
		return "", time.Time{}, err
	}
	expires := at(nowRaw).Add(agentCredentialTTL)
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_agent_credentials(id,owner_principal,credential_sha256,created_at,expires_at) VALUES(?,?,?,?,?)`, newID("cred_"), owner, tokenDigest(credential), nowRaw, layout(expires)); err != nil {
		return "", time.Time{}, err
	}
	return credential, expires, tx.Commit()
}

// LauncherReport records a fixed code agentd reports for ref, for doctor.
func (s *Store) LauncherReport(ctx context.Context, token, ref, code string) error {
	if !containsString(LauncherReportCodes, code) {
		return invalid("code must be one of " + strings.Join(LauncherReportCodes, ", "))
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	if err = launcherAuth(ctx, tx, token, nowRaw); err != nil {
		return err
	}
	if !ownerRefPattern.MatchString(ref) {
		return scoped("unknown owner ref", "not_found")
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT owner_principal FROM dispatcher_owners WHERE ref=?`, ref).Scan(&owner); err != nil {
		return scopedError(err, "unknown owner ref", "not_found")
	}
	if err = upsertAgentStatus(ctx, tx, owner, `launcher_report=?,report_at=?`, code, nowRaw); err != nil {
		return err
	}
	return tx.Commit()
}

// ModelProjectToken is a project token an agent credential issued.
type ModelProjectToken struct {
	ProjectID string `json:"project_id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// AgentConfig is agent/config's answer: on the first redemption only, and
// the only answer that ever carries the provider key [AG-M2b].
type AgentConfig struct {
	ConfigRevision int `json:"config_revision"`
	Provider       struct {
		ID      string `json:"id"`
		API     string `json:"api"`
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
		Key     string `json:"key"`
	} `json:"provider"`
	Projects []ModelProjectToken `json:"projects"`
	Limits   struct {
		MaxInputTokens  int `json:"max_input_tokens_per_run"`
		MaxOutputTokens int `json:"max_output_tokens_per_run"`
	} `json:"limits"`
	BindingSecret string `json:"binding_secret"`
	Status        string `json:"status"`
}

// agentCredential finds a live credential by its value.
func agentCredential(ctx context.Context, tx *transaction, credential string) (map[string]any, error) {
	if !agentCredentialPattrn.MatchString(credential) {
		return nil, ErrModelUnauthorized
	}
	rows, err := query(ctx, tx, `SELECT * FROM dispatcher_agent_credentials WHERE credential_sha256=? AND revoked_at IS NULL`, tokenDigest(credential))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, ErrModelUnauthorized
	}
	return rows[0], nil
}

// flagCredential marks a credential for doctor, in a transaction of its
// own so the refusal that follows doesn't roll it back.
// It outlives the request, and keeps the first flag.
func (s *Store) flagCredential(ctx context.Context, id, flag string) {
	ctx = context.WithoutCancel(ctx)
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_agent_credentials SET flagged=? WHERE id=? AND flagged=''`, flag, id); err == nil {
		_ = tx.Commit()
	}
}

// issueProjectTokens issues credential's owner a fresh token per model
// project, valid modelTokenTTL.
func issueProjectTokens(ctx context.Context, tx *transaction, owner, credentialID, nowRaw string) ([]ModelProjectToken, error) {
	projects, err := modelProjects(ctx, tx, owner)
	if err != nil {
		return nil, err
	}
	expires := layout(at(nowRaw).Add(modelTokenTTL))
	out := []ModelProjectToken{}
	for _, pid := range projects {
		token, err := randomToken("cbm_")
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_model_tokens(id,project_id,token_sha256,created_by_principal,created_at,expires_at,credential_id) VALUES(?,?,?,?,?,?,?)`, newID("mtok_"), pid, tokenDigest(token), owner, nowRaw, expires, credentialID); err != nil {
			return nil, err
		}
		out = append(out, ModelProjectToken{ProjectID: pid, Token: token, ExpiresAt: at(expires).Format(time.RFC3339)})
	}
	return out, nil
}

// RedeemAgentCredential answers agent/config: the first redemption of a
// live, unexpired credential returns its owner's provider config with the
// key, a binding secret (its hash kept) and project tokens. The instance
// ID and source address are recorded only [AG-M2a, N2]. Any later
// redemption is refused and flagged.
func (s *Store) RedeemAgentCredential(ctx context.Context, credential, instanceID, source string) (AgentConfig, error) {
	var out AgentConfig
	if !instanceIDPattern.MatchString(instanceID) {
		return out, invalid("instance_id must be 1–128 letters, digits or ._:-")
	}
	if s.modelSeal == nil {
		return out, ErrModelUnavailable
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return out, err
	}
	cred, err := agentCredential(ctx, tx, credential)
	if err != nil {
		return out, err
	}
	if cred["used_at"] != nil {
		tx.Rollback()
		s.flagCredential(ctx, str(cred, "id"), "second_redemption")
		return out, ErrModelUnauthorized
	}
	if str(cred, "expires_at") <= nowRaw {
		return out, ErrModelUnauthorized
	}
	owner := str(cred, "owner_principal")
	keys, err := query(ctx, tx, `SELECT * FROM dispatcher_model_keys WHERE owner_principal=? ORDER BY state DESC`, owner)
	if err != nil {
		return out, err
	}
	if len(keys) == 0 {
		return out, ErrModelUnauthorized
	}
	// pending_test sorts first: a new instance runs the setup test.
	k := keys[0]
	key, err := s.modelSeal.open(str(k, "ciphertext"), str(k, "nonce"), modelAAD(owner, str(k, "provider_id"), str(k, "api"), str(k, "base_url"), str(k, "model"), asInt(k["config_revision"])))
	if err != nil {
		return out, errors.New("the sealed model key does not open; the owner runs setup again")
	}
	var caps ModelCaps
	_ = json.Unmarshal([]byte(str(k, "caps")), &caps)
	secret, err := randomToken("")
	if err != nil {
		return out, err
	}
	// The claim: a concurrent redemption that got here first leaves no row
	// to update, and this one is the second.
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_agent_credentials SET used_at=?,bound_instance=?,binding_secret_sha256=?,redeemed_from=? WHERE id=? AND used_at IS NULL AND revoked_at IS NULL`, nowRaw, instanceID, tokenDigest(secret), source, str(cred, "id"))
	if err != nil {
		return out, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		tx.Rollback()
		s.flagCredential(ctx, str(cred, "id"), "second_redemption")
		return AgentConfig{}, ErrModelUnauthorized
	}
	if out.Projects, err = issueProjectTokens(ctx, tx, owner, str(cred, "id"), nowRaw); err != nil {
		return out, err
	}
	out.ConfigRevision = asInt(k["config_revision"])
	out.Provider.ID, out.Provider.API, out.Provider.BaseURL, out.Provider.Model, out.Provider.Key = str(k, "provider_id"), str(k, "api"), str(k, "base_url"), str(k, "model"), string(key)
	out.Limits.MaxInputTokens, out.Limits.MaxOutputTokens = caps.MaxInputTokens, caps.MaxOutputTokens
	out.BindingSecret, out.Status = secret, str(k, "state")
	if err = tx.Commit(); err != nil {
		return AgentConfig{}, err
	}
	return out, nil
}

// wrongBinding is ErrModelUnauthorized for a redeemed credential presented
// with a wrong binding secret: the caller flags it once its transaction
// has ended.
type wrongBinding struct{ id string }

func (wrongBinding) Error() string        { return ErrModelUnauthorized.Error() }
func (wrongBinding) Is(target error) bool { return target == ErrModelUnauthorized }

// boundCredential checks a redeemed credential and its binding secret.
func boundCredential(ctx context.Context, tx *transaction, credential, secret string) (map[string]any, error) {
	cred, err := agentCredential(ctx, tx, credential)
	if err != nil {
		return nil, err
	}
	stored := str(cred, "binding_secret_sha256")
	if cred["used_at"] == nil || stored == "" || !bindingSecretPattern.MatchString(secret) || subtle.ConstantTimeCompare([]byte(stored), []byte(tokenDigest(secret))) != 1 {
		if cred["used_at"] != nil {
			return nil, wrongBinding{str(cred, "id")}
		}
		return nil, ErrModelUnauthorized
	}
	return cred, nil
}

// refused ends tx and flags a wrong binding secret for doctor, in a
// transaction of its own; it returns err.
func (s *Store) refused(ctx context.Context, tx *transaction, err error) error {
	var wrong wrongBinding
	if errors.As(err, &wrong) {
		tx.Rollback()
		s.flagCredential(ctx, wrong.id, "wrong_binding")
	}
	return err
}

// RenewAgentTokens answers agent/renew: fresh project tokens only, never
// the key [AG-M2b].
func (s *Store) RenewAgentTokens(ctx context.Context, credential, secret string) ([]ModelProjectToken, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	cred, err := boundCredential(ctx, tx, credential, secret)
	if err != nil {
		return nil, s.refused(ctx, tx, err)
	}
	keys, err := query(ctx, tx, `SELECT 1 FROM dispatcher_model_keys WHERE owner_principal=?`, str(cred, "owner_principal"))
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, ErrModelUnauthorized
	}
	out, err := issueProjectTokens(ctx, tx, str(cred, "owner_principal"), str(cred, "id"), nowRaw)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// AgentTestResult records the setup test of revision (d4.1): active makes
// the pending key the owner's active one (the old one deleted, not kept),
// failed deletes it and records the fixed reason. It is idempotent per
// revision; a result for any other revision is ErrModelConflict.
func (s *Store) AgentTestResult(ctx context.Context, credential, secret string, revision int, result, reason string) error {
	if result == "active" && reason != "" || result == "failed" && !containsString(SetupTestFailures, reason) || result != "active" && result != "failed" {
		return invalid("result is active, or failed with a fixed reason")
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	cred, err := boundCredential(ctx, tx, credential, secret)
	if err != nil {
		return s.refused(ctx, tx, err)
	}
	owner := str(cred, "owner_principal")
	keys, err := query(ctx, tx, `SELECT state FROM dispatcher_model_keys WHERE owner_principal=? AND config_revision=?`, owner, revision)
	if err != nil {
		return err
	}
	switch {
	case len(keys) == 1 && str(keys[0], "state") == "pending_test" && result == "active":
		if _, err = tx.ExecContext(ctx, `DELETE FROM dispatcher_model_keys WHERE owner_principal=? AND state='active'`, owner); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_model_keys SET state='active',tested_at=? WHERE owner_principal=? AND config_revision=?`, nowRaw, owner, revision); err != nil {
			return err
		}
		err = upsertAgentStatus(ctx, tx, owner, `test_revision=?,test_failure='',test_at=?`, revision, nowRaw)
	case len(keys) == 1 && str(keys[0], "state") == "pending_test":
		err = recordTestFailure(ctx, tx, owner, revision, reason, nowRaw)
	case len(keys) == 1 && result == "active":
		// Already active: the same result again.
		return nil
	default:
		st, e := query(ctx, tx, `SELECT 1 FROM dispatcher_agent_status WHERE owner_principal=? AND test_revision=? AND test_failure=?`, owner, revision, reason)
		if e != nil {
			return e
		}
		if result == "failed" && len(st) == 1 {
			return nil
		}
		return ErrModelConflict
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// AgentStatus records the agent's provider-auth signal for doctor (d4.1).
func (s *Store) AgentStatus(ctx context.Context, credential, secret, status string) error {
	if status != "ok" && status != "provider_auth" {
		return invalid("status is ok or provider_auth")
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	cred, err := boundCredential(ctx, tx, credential, secret)
	if err != nil {
		return s.refused(ctx, tx, err)
	}
	if err = upsertAgentStatus(ctx, tx, str(cred, "owner_principal"), `status=?,status_at=?`, status, nowRaw); err != nil {
		return err
	}
	return tx.Commit()
}

// OperatorLauncherTokenCreate mints a launcher token (cbl_…): only its
// hash is stored, and the value is returned once for the caller to write
// into agentd's secret file.
func (s *Store) OperatorLauncherTokenCreate(ctx context.Context) (id, token string, err error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return "", "", err
	}
	if token, err = randomToken("cbl_"); err != nil {
		return "", "", err
	}
	id = newID("lt_")
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_launcher_tokens(id,token_sha256,created_by,created_at) VALUES(?,?,?,?)`, id, tokenDigest(token), OperatorPrincipal, nowRaw); err != nil {
		return "", "", errors.New("authority unavailable")
	}
	if err = tx.Commit(); err != nil {
		return "", "", errors.New("authority commit failed")
	}
	return id, token, nil
}

// OperatorLauncherTokenRevoke revokes a launcher token by its ID.
func (s *Store) OperatorLauncherTokenRevoke(ctx context.Context, id string) error {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_launcher_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, nowRaw, id)
	if err != nil {
		return errors.New("authority unavailable")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("no live launcher token %q", id)
	}
	if err = tx.Commit(); err != nil {
		return errors.New("authority commit failed")
	}
	return nil
}
