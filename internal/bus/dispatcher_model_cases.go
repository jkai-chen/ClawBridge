package bus

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The dispatcher model's cases (DISPATCHER-MODEL-API §2–§4, d5): in mode
// model the watcher's tick opens a decide case for a pending claim instead
// of running the rules; the project's agent takes it with cases/next,
// reads through four tools as the dispatcher-model session, and ends it
// with decide or abstain. A decide is checked against the guardrails' set
// stored at open, then recorded by dispatcherRecord as the dispatcher
// session, which re-checks the bus's facts in the same transaction. Each
// (subject, revision) gets one model case, ever: once it closes, however
// it closed, the tick decides that revision by the rules.
//
// Schema 24:
//   - dispatcher_cases gains what the tick read (the subject's revision,
//     the policy revision, the typed facts, any untrusted text, where the
//     facts came from, the issue's project as the app read it, and the
//     rules' domain), the lease (its key-holder, the agent instance holding
//     it and a generation bumped on each lease), when it was first served,
//     the recorded result and the run's tokens. One case per subject
//     revision and kind.
//   - dispatcher_principal_refs: each principal's opaque per-project ref
//     (p_ and 16 random characters [A6], D5-L3), so no email leaves.
const dispatcherCasesSchemaV24 = `
ALTER TABLE dispatcher_cases ADD COLUMN subject_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_cases ADD COLUMN policy_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatcher_cases ADD COLUMN facts TEXT NOT NULL DEFAULT '{}';
ALTER TABLE dispatcher_cases ADD COLUMN untrusted TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_cases ADD COLUMN facts_source TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_cases ADD COLUMN app_project_id TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_cases ADD COLUMN domain TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_cases ADD COLUMN lease_key TEXT;
ALTER TABLE dispatcher_cases ADD COLUMN lease_instance TEXT;
ALTER TABLE dispatcher_cases ADD COLUMN lease_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatcher_cases ADD COLUMN served_at TEXT;
ALTER TABLE dispatcher_cases ADD COLUMN result TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_cases ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX dispatcher_cases_subject ON dispatcher_cases(project_id,kind,subject_id,subject_revision);
CREATE INDEX dispatcher_cases_lease ON dispatcher_cases(lease_key,served_at);
CREATE TABLE dispatcher_principal_refs(project_id TEXT NOT NULL REFERENCES projects(id),principal TEXT NOT NULL,ref TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,principal),UNIQUE(project_id,ref));
`

const (
	// modelSeenWindow is how recently an agent must have called for the
	// tick to hand it cases, and for agent_cap to count it.
	modelSeenWindow = 5 * time.Minute
	// modelCaseReads bounds a case's read calls; its outcome is always
	// accepted while it is open [A4].
	modelCaseReads = 8
	// modelTakeoverIdle is the least time the agent holding a case must
	// have been silent before another agent of the same key-holder takes
	// it over (D5-L1). A model call makes no request for up to the case's
	// deadline, so the wait is half the case's time when that is longer:
	// a takeover can only cost liveness, never a double decision.
	modelTakeoverIdle = 60 * time.Second
	// The case deadline: the owner's, 30–300 s, else 90 s.
	defaultCaseDeadline = 90
	minCaseDeadline     = 30
	maxCaseDeadline     = 300
	maxUntrustedTitle   = 1024
	maxUntrustedExcerpt = 4096
)

// ErrAgentCap is a hello or cases/next from a key-holder past the server's
// cap on live agents.
var ErrAgentCap = errors.New("agent_cap")

var (
	modelTokenPattern = regexp.MustCompile(`^cbm_[A-Za-z0-9_-]{43}$`)
	reasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// DecideReasonCodes are the reason codes a decide may give: the rules'
// own, so a model decision reads like one.
var DecideReasonCodes = []string{"trusted", "untrusted", "overlap", "no_domain", "label_only", "ambiguous", "session_stale", "session_gone", "issue_closed", "facts_unavailable", "uncertain"}

// AbstainReasonCodes are an abstain's.
var AbstainReasonCodes = []string{"insufficient_facts", "uncertain", "refused"}

// ModelCaller is an authenticated project token: its project, its
// key-holder (the credential's owner, or a hand-minted token's creator)
// and the agent instance it belongs to (its credential, or the token
// itself), which the lease is fenced on.
type ModelCaller struct {
	TokenID, ProjectID, Holder, Instance string
}

// modelTokenAuth checks a project token. With maxAgents above zero it also
// refuses a key-holder not yet counted when the server's live agents are
// at the cap. Every authenticated call stamps the token's last use: the
// agent's heartbeat (D5-M2).
func modelTokenAuth(ctx context.Context, tx *transaction, token, nowRaw string, maxAgents int) (ModelCaller, error) {
	var c ModelCaller
	if !modelTokenPattern.MatchString(token) {
		return c, ErrModelUnauthorized
	}
	rows, err := query(ctx, tx, `SELECT id,project_id,created_by_principal,credential_id FROM dispatcher_model_tokens WHERE token_sha256=? AND revoked_at IS NULL AND expires_at>?`, tokenDigest(token), nowRaw)
	if err != nil {
		return c, err
	}
	if len(rows) != 1 {
		return c, ErrModelUnauthorized
	}
	c = ModelCaller{TokenID: str(rows[0], "id"), ProjectID: str(rows[0], "project_id"), Holder: str(rows[0], "created_by_principal"), Instance: str(rows[0], "credential_id")}
	if c.Instance == "" {
		c.Instance = c.TokenID
	}
	if maxAgents > 0 {
		cutoff := layout(at(nowRaw).Add(-modelSeenWindow))
		live, err := query(ctx, tx, `SELECT DISTINCT created_by_principal FROM dispatcher_model_tokens WHERE last_used_at>=? AND revoked_at IS NULL AND expires_at>?`, cutoff, nowRaw)
		if err != nil {
			return c, err
		}
		counted := false
		for _, r := range live {
			counted = counted || str(r, "created_by_principal") == c.Holder
		}
		if !counted && len(live) >= maxAgents {
			return c, ErrAgentCap
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE dispatcher_model_tokens SET last_used_at=? WHERE id=?`, nowRaw, c.TokenID)
	return c, err
}

// ModelAuth authenticates a project token for a case tool call, and
// stamps it.
func (s *Store) ModelAuth(ctx context.Context, token string) (ModelCaller, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return ModelCaller{}, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return ModelCaller{}, err
	}
	c, err := modelTokenAuth(ctx, tx, token, nowRaw, 0)
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}

// ModelHello is hello's answer (§2).
type ModelHello struct {
	ProjectID       string         `json:"project_id"`
	Enabled         bool           `json:"enabled"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	Caps            map[string]int `json:"caps"`
	OptInTools      []string       `json:"opt_in_tools"`
	DeadlineSeconds int            `json:"deadline_seconds"`
	ServerTime      string         `json:"server_time"`
}

// ModelHello answers hello for a project token: the project's dispatcher
// state and the key-holder's own provider, model and caps.
func (s *Store) ModelHello(ctx context.Context, token string, maxAgents int) (ModelHello, error) {
	var out ModelHello
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return out, err
	}
	c, err := modelTokenAuth(ctx, tx, token, nowRaw, maxAgents)
	if err != nil {
		return out, err
	}
	key, caps, err := holderKey(ctx, tx, c.Holder)
	if err != nil {
		return out, err
	}
	use, settings, err := projectModelUse(ctx, tx, c.ProjectID)
	if err != nil {
		return out, err
	}
	deadline, err := caseDeadline(ctx, tx, c.ProjectID)
	if err != nil {
		return out, err
	}
	out = ModelHello{
		ProjectID: c.ProjectID, Enabled: use != "",
		Provider: str(key, "provider_id"), Model: str(key, "model"),
		Caps:       map[string]int{"runs_per_hour": caps.RunsPerHour, "runs_per_day": caps.RunsPerDay},
		OptInTools: enabledOptInTools(settings), DeadlineSeconds: deadline, ServerTime: at(nowRaw).Format(time.RFC3339),
	}
	return out, tx.Commit()
}

// holderKey is a key-holder's active model key and its caps. A holder
// without one has no agent to serve: unauthorized.
func holderKey(ctx context.Context, tx *transaction, holder string) (map[string]any, ModelCaps, error) {
	rows, err := query(ctx, tx, `SELECT provider_id,model,caps FROM dispatcher_model_keys WHERE owner_principal=? AND state='active'`, holder)
	if err != nil {
		return nil, ModelCaps{}, err
	}
	if len(rows) != 1 {
		return nil, ModelCaps{}, ErrModelUnauthorized
	}
	var raw any
	_ = json.Unmarshal([]byte(str(rows[0], "caps")), &raw)
	caps, err := parseModelCaps(raw)
	if err != nil {
		// Stored caps were checked at setup; fall back to the defaults
		// with no tokens rather than run unbounded.
		caps = ModelCaps{RunsPerHour: 10, RunsPerDay: 50}
	}
	return rows[0], caps, nil
}

// caseDeadline is the project's case deadline in seconds: the owner's
// deadline_seconds in the policy's model settings, else the default.
func caseDeadline(ctx context.Context, tx *transaction, pid string) (int, error) {
	rows, err := query(ctx, tx, `SELECT model FROM dispatcher_policies WHERE project_id=?`, pid)
	if err != nil || len(rows) == 0 {
		return defaultCaseDeadline, err
	}
	var settings struct {
		Deadline int `json:"deadline_seconds"`
	}
	if json.Unmarshal([]byte(str(rows[0], "model")), &settings) != nil || settings.Deadline < minCaseDeadline || settings.Deadline > maxCaseDeadline {
		return defaultCaseDeadline, nil
	}
	return settings.Deadline, nil
}

// overCaps reports whether a key-holder has used up its runs this hour or
// day, or its daily tokens, across all its projects.
func overCaps(ctx context.Context, tx *transaction, holder, nowRaw string) (bool, error) {
	_, caps, err := holderKey(ctx, tx, holder)
	if err != nil {
		return true, err
	}
	now := at(nowRaw)
	hour, day := layout(now.Add(-time.Hour)), layout(now.Add(-24*time.Hour))
	var hourRuns, dayRuns int
	var tokens int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN served_at>=? THEN 1 END),COUNT(*),COALESCE(SUM(tokens),0) FROM dispatcher_cases WHERE lease_key=? AND served_at>=?`, hour, holder, day).Scan(&hourRuns, &dayRuns, &tokens); err != nil {
		return true, err
	}
	return hourRuns >= caps.RunsPerHour || dayRuns >= caps.RunsPerDay || caps.DailyTokenCeiling > 0 && tokens >= caps.DailyTokenCeiling, nil
}

// modelAvailability is whether the tick may hand pid's claims to a model:
// "available" when a key-holder's agent called within modelSeenWindow and
// is under its caps, else "no_agent" or "caps".
func modelAvailability(ctx context.Context, tx *transaction, pid, nowRaw string) (string, error) {
	holders, err := liveHolders(ctx, tx, pid, nowRaw)
	if err != nil || len(holders) == 0 {
		return "no_agent", err
	}
	for _, h := range holders {
		over, err := overCaps(ctx, tx, h, nowRaw)
		if errors.Is(err, ErrModelUnauthorized) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !over {
			return "available", nil
		}
	}
	return "caps", nil
}

// liveHolders are the key-holders whose agents called pid within
// modelSeenWindow.
func liveHolders(ctx context.Context, tx *transaction, pid, nowRaw string) ([]string, error) {
	rows, err := query(ctx, tx, `SELECT DISTINCT created_by_principal FROM dispatcher_model_tokens WHERE project_id=? AND revoked_at IS NULL AND expires_at>? AND last_used_at>=? ORDER BY created_by_principal`, pid, nowRaw, layout(at(nowRaw).Add(-modelSeenWindow)))
	holders := []string{}
	for _, r := range rows {
		holders = append(holders, str(r, "created_by_principal"))
	}
	return holders, err
}

// expireCases closes pid's open cases past their deadline, and reports
// whether it closed a decide case: its subject falls back to the rules. A
// chat has no fallback.
func expireCases(ctx context.Context, tx *transaction, pid, nowRaw string) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET closed_at=?,outcome='expired' WHERE project_id=? AND kind='decide' AND closed_at IS NULL AND deadline<=?`, nowRaw, pid, nowRaw)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	_, err = tx.ExecContext(ctx, `UPDATE dispatcher_cases SET closed_at=?,outcome='expired' WHERE project_id=? AND kind<>'decide' AND closed_at IS NULL AND deadline<=?`, nowRaw, pid, nowRaw)
	return n > 0, err
}

// randomRef is p_ and 16 random lower-case letters and digits.
func randomRef() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "p_" + string(b), nil
}

// principalRef is principal's opaque ref in pid, created on first use. It
// is random, never derived from the principal (D5-L3).
func principalRef(ctx context.Context, tx *transaction, pid, principal, nowRaw string) (string, error) {
	rows, err := query(ctx, tx, `SELECT ref FROM dispatcher_principal_refs WHERE project_id=? AND principal=?`, pid, principal)
	if err != nil {
		return "", err
	}
	if len(rows) == 1 {
		return str(rows[0], "ref"), nil
	}
	ref, err := randomRef()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_principal_refs(project_id,principal,ref,created_at) VALUES(?,?,?,?)`, pid, principal, ref, nowRaw)
	return ref, err
}

// ModelNext is one cases/next check: the case served, if any, and whether
// a case expired, so the server nudges the rules' fallback (D5-M3).
type ModelNext struct {
	Case      map[string]any
	Expired   bool
	ProjectID string
}

// ModelNextCase is one check of cases/next for a project token. It serves
// the key-holder's own open case again (to the instance holding it, or to
// another of its instances once the holder has been silent
// modelTakeoverIdle, bumping the lease generation), else leases the
// project's oldest unleased open case, while the key-holder is under its
// caps. A second live instance of the same key-holder gets
// ErrCaseLeased.
func (s *Store) ModelNextCase(ctx context.Context, token string, maxAgents int) (ModelNext, error) {
	var out ModelNext
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return out, err
	}
	c, err := modelTokenAuth(ctx, tx, token, nowRaw, maxAgents)
	if err != nil {
		return out, err
	}
	out.ProjectID = c.ProjectID
	if out.Expired, err = expireCases(ctx, tx, c.ProjectID, nowRaw); err != nil {
		return out, err
	}
	use, _, err := projectModelUse(ctx, tx, c.ProjectID)
	if err != nil {
		return out, err
	}
	if err = closeCasesOutside(ctx, tx, c.ProjectID, use, nowRaw); err != nil {
		return out, err
	}
	if use == "" {
		return out, tx.Commit()
	}
	own, err := query(ctx, tx, `SELECT * FROM dispatcher_cases WHERE project_id=? AND closed_at IS NULL AND lease_key=? ORDER BY created_at,id LIMIT 1`, c.ProjectID, c.Holder)
	if err != nil {
		return out, err
	}
	if len(own) == 1 {
		row := own[0]
		if str(row, "lease_instance") != c.Instance {
			quiet := max(modelTakeoverIdle, at(str(row, "deadline")).Sub(at(str(row, "created_at")))/2)
			idle, err := instanceIdle(ctx, tx, str(row, "lease_instance"), nowRaw, quiet)
			if err != nil {
				return out, err
			}
			if !idle {
				return out, ErrCaseLeased
			}
			res, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET lease_instance=?,leased_token=?,lease_generation=lease_generation+1 WHERE id=? AND lease_instance=? AND closed_at IS NULL`, c.Instance, c.TokenID, str(row, "id"), str(row, "lease_instance"))
			if err != nil {
				return out, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return out, ErrCaseLeased
			}
		}
		if out.Case, err = caseView(row, nowRaw); err != nil {
			return out, err
		}
		return out, tx.Commit()
	}
	over, err := overCaps(ctx, tx, c.Holder, nowRaw)
	if err != nil || over {
		if err == nil {
			err = tx.Commit()
		}
		return out, err
	}
	// Only a decide or advise case whose claim is still pending at its
	// revision: a decide could only end superseded, and advice would come
	// after a person decided. Decide cases come first, then advise, then
	// chats (d7 Q19), a chat only within the key-holder's chat share
	// (D6-M1). A project has decide and chat cases, or advise ones.
	open, err := query(ctx, tx, `SELECT c.* FROM dispatcher_cases c WHERE c.project_id=? AND c.kind IN ('decide','advise') AND c.closed_at IS NULL AND c.lease_key IS NULL AND c.deadline>? AND EXISTS(SELECT 1 FROM session_assignments a WHERE a.project_id=c.project_id AND a.id=c.subject_id AND a.state='proposed' AND a.revision=c.subject_revision) ORDER BY CASE c.kind WHEN 'decide' THEN 0 ELSE 1 END,c.created_at,c.id LIMIT 1`, c.ProjectID, nowRaw)
	if err == nil && len(open) == 0 {
		var chatOver bool
		if chatOver, err = chatOverCaps(ctx, tx, c.Holder, nowRaw); err == nil && !chatOver {
			open, err = query(ctx, tx, `SELECT * FROM dispatcher_cases WHERE project_id=? AND kind='chat' AND closed_at IS NULL AND lease_key IS NULL AND deadline>? ORDER BY created_at,id LIMIT 1`, c.ProjectID, nowRaw)
		}
	}
	if err != nil || len(open) == 0 {
		if err == nil {
			err = tx.Commit()
		}
		return out, err
	}
	// A concurrent next may lease it first: then this check serves none.
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET lease_key=?,lease_instance=?,leased_token=?,lease_generation=lease_generation+1,served_at=? WHERE id=? AND lease_key IS NULL AND closed_at IS NULL`, c.Holder, c.Instance, c.TokenID, nowRaw, str(open[0], "id"))
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if out.Case, err = caseView(open[0], nowRaw); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// ErrCaseLeased is a call from an agent that doesn't hold the case.
var ErrCaseLeased = errors.New("case_leased")

// errCaseExpired is an outcome whose case expired while it was recorded.
var errCaseExpired = scoped("the case expired", "deadline_passed")

// instanceIdle reports whether an agent instance (its credential's tokens,
// or one hand-minted token) has made no call for quiet.
func instanceIdle(ctx context.Context, tx *transaction, instance, nowRaw string, quiet time.Duration) (bool, error) {
	var last *string
	if err := tx.QueryRowContext(ctx, `SELECT MAX(last_used_at) FROM dispatcher_model_tokens WHERE credential_id=? OR id=?`, instance, instance).Scan(&last); err != nil {
		return false, err
	}
	return last == nil || *last <= layout(at(nowRaw).Add(-quiet)), nil
}

// caseView is a case as the agent sees it (§2).
func caseView(row map[string]any, nowRaw string) (map[string]any, error) {
	var permitted, reasons []string
	var facts map[string]any
	if json.Unmarshal([]byte(str(row, "permitted")), &permitted) != nil || json.Unmarshal([]byte(str(row, "reason_codes")), &reasons) != nil || json.Unmarshal([]byte(str(row, "facts")), &facts) != nil {
		return nil, errors.New("a stored case is unreadable")
	}
	deadline := at(str(row, "deadline"))
	left := deadline.Sub(at(nowRaw)).Milliseconds()
	if left < 0 {
		left = 0
	}
	view := map[string]any{
		"case_id": str(row, "id"), "kind": str(row, "kind"),
		"deadline": deadline.Format(time.RFC3339), "deadline_ms": left,
		"subject":           map[string]any{"type": str(row, "subject_type"), "id": str(row, "subject_id")},
		"permitted_actions": permitted, "reason_codes": reasons, "facts": facts,
	}
	if raw := str(row, "untrusted"); raw != "" {
		var untrusted map[string]any
		if json.Unmarshal([]byte(raw), &untrusted) != nil {
			return nil, errors.New("a stored case is unreadable")
		}
		view["untrusted"] = untrusted
	}
	if raw := str(row, "message"); raw != "" {
		var message map[string]any
		if json.Unmarshal([]byte(raw), &message) != nil {
			return nil, errors.New("a stored case is unreadable")
		}
		view["message"] = message
	}
	return view, nil
}

// dispatcherCaseOpen opens a decide case for a pending claim (or, with
// kind advise, an advise case for its open escalation, d7), as the
// dispatcher session (d5): the facts the tick read, the guardrails'
// permitted set and the policy revision it read them under. The bus adds
// the session's and holders' facts itself, with principals as refs. It is
// idempotent per subject revision and attempt: a revision that ever had a
// case gets no other (D5-M1) until a person asks the model to judge it
// again (R5). A decide case for an issue past its loop budget is
// refused over_budget, before anything is stored but the skip and the
// person's notice (R1).
func (s *Store) dispatcherCaseOpen(ctx context.Context, tx *transaction, pid string, a map[string]any) (any, error) {
	subject, revision := str(a, "subject_id"), str(a, "subject_revision")
	policyRevision, err := integer(a["policy_revision"])
	if err != nil || subject == "" || revision == "" {
		return nil, invalid("dispatcher_case_open needs a subject, its revision and the policy revision read")
	}
	kind := str(a, "kind")
	if kind == "" {
		kind = "decide"
	}
	permitted := []string{}
	list, _ := a["permitted"].([]any)
	for _, v := range list {
		action, _ := v.(string)
		if !dispatcherActions[action] {
			return nil, invalid("a decide case's permitted actions are confirm, decline, wait and escalate")
		}
		permitted = append(permitted, action)
	}
	switch {
	case kind != "decide" && kind != "advise":
		return nil, invalid("dispatcher_case_open opens a decide or an advise case")
	case kind == "decide" && len(permitted) == 0:
		return nil, invalid("a decide case needs its permitted actions")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	live, err := query(ctx, tx, `SELECT a.*,s.name AS session_name,s.owner_principal,s.status AS session_status,s.last_seen FROM session_assignments a JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id WHERE a.project_id=? AND a.id=? AND a.state='proposed' AND a.revision=?`, pid, subject, revision)
	if err != nil {
		return nil, err
	}
	if len(live) != 1 {
		return map[string]any{"opened": false, "reason": "proposal_changed"}, nil
	}
	p := live[0]
	// The attempt this open is for (attempt in the subject index): a
	// decide case's is how many times a person asked the model to judge
	// this revision again (R5), so each re-judge allows one more case.
	attempt := 0
	if kind == "decide" {
		if attempt, err = claimAttempt(ctx, tx, pid, subject, revision); err != nil {
			return nil, err
		}
	}
	// A repeat open answers as before and records nothing, the budget's
	// skip included.
	existing, err := query(ctx, tx, `SELECT 1 FROM dispatcher_cases WHERE project_id=? AND kind=? AND subject_id=? AND subject_revision=? AND attempt=?`, pid, kind, subject, revision, attempt)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return map[string]any{"opened": false, "reason": "case_exists"}, nil
	}
	if kind == "decide" {
		over, err := s.loopBudgetStop(ctx, tx, nil, pid, str(a, "domain"), p)
		if err != nil {
			return nil, err
		}
		if over {
			return map[string]any{"opened": false, "reason": "over_budget"}, nil
		}
	}
	ref, err := principalRef(ctx, tx, pid, str(p, "owner_principal"), nowRaw)
	if err != nil {
		return nil, err
	}
	presence := "active"
	if seen, e := time.Parse(timestampLayout, str(p, "last_seen")); e != nil || at(nowRaw).Sub(seen) > staleAfter {
		presence = "stale"
	}
	holders, err := query(ctx, tx, `SELECT session_id,confirmed_at FROM session_assignments WHERE project_id=? AND state='confirmed' AND issue_uuid=? AND issue_uuid<>'' AND session_id<>? ORDER BY confirmed_at`, pid, str(p, "issue_uuid"), str(p, "session_id"))
	if err != nil {
		return nil, err
	}
	held := []any{}
	for _, h := range holders {
		held = append(held, map[string]any{"session_id": str(h, "session_id"), "since": at(str(h, "confirmed_at")).Format(time.RFC3339)})
	}
	issue, _ := a["issue"].(map[string]any)
	if issue == nil || str(issue, "id") != str(p, "issue_uuid") {
		return nil, invalid("dispatcher_case_open needs the claim's issue facts")
	}
	issue["identifier"] = str(p, "issue_identifier")
	facts := map[string]any{
		"issue":          issue,
		"session":        map[string]any{"id": str(p, "session_id"), "name": str(p, "session_name"), "principal": ref, "presence": presence, "status": str(p, "session_status")},
		"domain_matches": a["domain_matches"],
		"holders":        held,
	}
	if facts["domain_matches"] == nil {
		facts["domain_matches"] = []any{}
	}
	reasons := DecideReasonCodes
	if kind == "advise" {
		// An advise case is about the claim's open escalation (d7 §3).
		var escalation map[string]any
		if permitted, reasons, escalation, err = adviseCaseFacts(ctx, tx, pid, subject, revision); err != nil {
			return nil, err
		}
		if escalation == nil {
			return map[string]any{"opened": false, "reason": "no_escalation"}, nil
		}
		facts["escalation"] = escalation
	}
	untrusted := ""
	if u, ok := a["untrusted"].(map[string]any); ok && len(u) > 0 {
		title, excerpt := trimRunes(str(u, "title"), maxUntrustedTitle), trimRunes(str(u, "description_excerpt"), maxUntrustedExcerpt)
		fenced := map[string]any{"title": title, "description_excerpt": excerpt, "truncated": u["truncated"] == true || len(excerpt) < len(str(u, "description_excerpt"))}
		raw, _ := json.Marshal(fenced)
		untrusted = string(raw)
	}
	deadline, err := caseDeadline(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	factsJSON, _ := json.Marshal(facts)
	permittedJSON, _ := json.Marshal(permitted)
	reasonsJSON, _ := json.Marshal(reasons)
	id := newID("case_")
	res, err := tx.ExecContext(ctx, `INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,attempt,policy_revision,permitted,reason_codes,facts,untrusted,facts_source,app_project_id,domain,deadline,created_at) VALUES(?,?,?,'proposal',?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,kind,subject_id,subject_revision,attempt) DO NOTHING`,
		id, pid, kind, subject, revision, attempt, policyRevision, string(permittedJSON), string(reasonsJSON), string(factsJSON), untrusted, str(a, "facts_source"), str(a, "app_project_id"), str(a, "domain"), layout(at(nowRaw).Add(time.Duration(deadline)*time.Second)), nowRaw)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return map[string]any{"opened": false, "reason": "case_exists"}, nil
	}
	if err = caseNotify(ctx, tx, pid); err != nil {
		return nil, err
	}
	return map[string]any{"opened": true, "case_id": id}, nil
}

// trimRunes cuts s to at most n bytes at a character boundary.
func trimRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// caseFence loads an agent's case and checks it may act on it, in this
// transaction: the case exists in the project, is open, is within its
// deadline (one past it is closed as expired here, so the refusal and the
// rules' fallback can't race), and is leased to the calling instance. It
// returns the case, or the refusal's code.
func caseFence(ctx context.Context, tx *transaction, pid, caseID, instance, nowRaw string) (map[string]any, string, error) {
	rows, err := query(ctx, tx, `SELECT * FROM dispatcher_cases WHERE project_id=? AND id=?`, pid, caseID)
	if err != nil {
		return nil, "", err
	}
	if len(rows) != 1 {
		return nil, "case_not_found", nil
	}
	c := rows[0]
	use, _, err := projectModelUse(ctx, tx, pid)
	if err != nil {
		return nil, "", err
	}
	if c["closed_at"] == nil && !caseKindAllowed(use, str(c, "kind")) {
		if err = closeCasesOutside(ctx, tx, pid, use, nowRaw); err != nil {
			return nil, "", err
		}
		return c, "case_closed", nil
	}
	switch {
	case c["closed_at"] != nil && str(c, "outcome") == "expired":
		return c, "deadline_passed", nil
	case c["closed_at"] != nil:
		return c, "case_closed", nil
	case str(c, "deadline") <= nowRaw:
		if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_cases SET closed_at=?,outcome='expired' WHERE id=? AND closed_at IS NULL`, nowRaw, caseID); err != nil {
			return nil, "", err
		}
		return c, "deadline_passed", nil
	case str(c, "lease_instance") != instance:
		return c, "case_leased", nil
	}
	return c, "", nil
}

// dispatcherCaseRead runs one of a case's four read tools as the
// dispatcher-model session (§3): typed facts only, at most modelCaseReads
// per case, principals as refs.
func (s *Store) dispatcherCaseRead(ctx context.Context, tx *transaction, pid string, a map[string]any) (any, error) {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	c, refusal, err := caseFence(ctx, tx, pid, str(a, "case_id"), str(a, "instance"), nowRaw)
	if err != nil || refusal != "" {
		return caseRefusal(refusal), err
	}
	// list_decisions is chat's own read [DM-M1].
	if str(a, "tool") == "list_decisions" && str(c, "kind") != "chat" {
		return caseRefusal("tool_refused"), nil
	}
	// Counted in one statement, so concurrent reads can't pass the cap.
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET tool_calls=tool_calls+1 WHERE id=? AND tool_calls<?`, str(c, "id"), modelCaseReads)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return caseRefusal("cap_reached"), nil
	}
	var facts map[string]any
	_ = json.Unmarshal([]byte(str(c, "facts")), &facts)
	switch str(a, "tool") {
	case "read_issue_facts":
		issue, _ := facts["issue"].(map[string]any)
		if issue == nil || str(issue, "id") != str(a, "issue_id") {
			return caseRefusal("not_in_case"), nil
		}
		out := map[string]any{"issue": issue}
		if raw := str(c, "untrusted"); raw != "" {
			var u map[string]any
			_ = json.Unmarshal([]byte(raw), &u)
			out["untrusted"] = map[string]any{"title": u["title"]}
		}
		return caseResult(out), nil
	case "list_domain":
		domains, _, err := readDispatcher(ctx, tx, pid)
		if err != nil {
			return nil, err
		}
		for _, d := range domains.Domains {
			if d.ID != str(a, "domain") {
				continue
			}
			owner, err := principalRef(ctx, tx, pid, d.OwnerPrincipal, nowRaw)
			if err != nil {
				return nil, err
			}
			var coordinator any
			if d.CoordinatorSession != nil {
				coordinator = map[string]any{"id": d.CoordinatorSession.ID, "name": d.CoordinatorSession.Name}
			}
			// The trust list's IDs are never listed, so the model can't
			// game trust.
			return caseResult(map[string]any{"id": d.ID, "name": d.Name, "match": d.Match, "owner_principal": owner, "coordinator_session": coordinator, "trusted_count": len(d.TrustedSessions)}), nil
		}
		return caseRefusal("not_in_case"), nil
	case "list_sessions":
		where := ""
		if a["active_only"] == true {
			where = " AND s.status='active'"
		}
		rows, err := query(ctx, tx, `SELECT s.id,s.name,s.owner_principal,s.role,s.status,s.last_seen,(SELECT a.issue_identifier FROM session_assignments a WHERE a.project_id=s.project_id AND a.session_id=s.id AND a.state='confirmed' ORDER BY a.confirmed_at DESC LIMIT 1) AS issue_identifier,(SELECT a.confirmed_at FROM session_assignments a WHERE a.project_id=s.project_id AND a.session_id=s.id AND a.state='confirmed' ORDER BY a.confirmed_at DESC LIMIT 1) AS confirmed_at FROM sessions s WHERE s.project_id=? AND s.role<>'system'`+where+` ORDER BY s.name LIMIT 200`, pid)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, r := range rows {
			ref, err := principalRef(ctx, tx, pid, str(r, "owner_principal"), nowRaw)
			if err != nil {
				return nil, err
			}
			presence := "active"
			if seen, e := time.Parse(timestampLayout, str(r, "last_seen")); e != nil || at(nowRaw).Sub(seen) > staleAfter {
				presence = "stale"
			}
			var assignment any
			if str(r, "issue_identifier") != "" {
				assignment = map[string]any{"issue_identifier": str(r, "issue_identifier"), "since": at(str(r, "confirmed_at")).Format(time.RFC3339)}
			}
			out = append(out, map[string]any{"id": str(r, "id"), "name": str(r, "name"), "principal": ref, "role": str(r, "role"), "presence": presence, "status": str(r, "status"), "assignment": assignment})
		}
		return caseResult(out), nil
	case "session_activity":
		hours := asInt(a["since_hours"])
		rows, err := query(ctx, tx, `SELECT id,name,last_seen FROM sessions WHERE project_id=? AND id=? AND role<>'system'`, pid, str(a, "session_id"))
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || hours < 1 || hours > 168 {
			return caseRefusal("not_in_case"), nil
		}
		sid, since := str(rows[0], "id"), layout(at(nowRaw).Add(-time.Duration(hours)*time.Hour))
		var comments, markers, releases int
		var lastComment, lastClaim *string
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),MAX(settled_at) FROM linear_comment_ledger WHERE project_id=? AND session_id=? AND state='posted' AND settled_at>=?`, pid, sid, since).Scan(&comments, &lastComment); err != nil {
			return nil, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assignment_markers m JOIN session_assignments a ON a.id=m.assignment_id AND a.project_id=m.project_id WHERE m.project_id=? AND a.session_id=? AND m.created_at>=?`, pid, sid, since).Scan(&markers); err != nil {
			return nil, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN state='released' AND released_at>=? THEN 1 END),MAX(proposed_at) FROM session_assignments WHERE project_id=? AND session_id=?`, since, pid, sid).Scan(&releases, &lastClaim); err != nil {
			return nil, err
		}
		var lastActivity any
		for _, t := range []*string{lastComment, lastClaim} {
			if t != nil && (lastActivity == nil || *t > lastActivity.(string)) {
				lastActivity = *t
			}
		}
		if lastActivity != nil {
			lastActivity = at(lastActivity.(string)).Format(time.RFC3339)
		}
		return caseResult(map[string]any{"last_heartbeat": at(str(rows[0], "last_seen")).Format(time.RFC3339), "last_issue_activity": lastActivity, "counts": map[string]any{"comments": comments, "markers": markers, "releases": releases}}), nil
	case "list_decisions":
		hours, limit := asInt(a["since_hours"]), asInt(a["limit"])
		if hours < 1 || hours > 168 || limit < 1 || limit > maxDecisionsListed {
			return caseRefusal("invalid_argument"), nil
		}
		log, err := decisionLog(ctx, tx, pid, nowRaw, hours, limit, "", true)
		if err != nil {
			return nil, err
		}
		return caseResult(log), nil
	}
	return caseRefusal("tool_refused"), nil
}

// caseRefusal and caseResult are a case op's two answers. A refusal is a
// result, not an error, so what it wrote (an expired case closed, a read
// counted) commits.
func caseRefusal(code string) map[string]any { return map[string]any{"error": code} }
func caseResult(v any) map[string]any        { return map[string]any{"result": v} }

// dispatcherCaseOutcome ends a case with decide or abstain, as the
// dispatcher-model session (§3, d5). A decide's action must be in the
// case's stored permitted set; it is then recorded by dispatcherRecord as
// the dispatcher session, which re-checks the proposal, the policy, the
// session, the holders and the domain's trust in this transaction, and
// records anything changed as superseded. An abstain closes the case for
// the rules. A retry of the same outcome on the closed case returns the
// recorded result; any other body gets case_closed [A5].
func (s *Store) dispatcherCaseOutcome(ctx context.Context, tx *transaction, w, p map[string]any, a map[string]any) (any, error) {
	pid := str(p, "id")
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	tool := str(a, "tool")
	body := map[string]any{"tool": tool, "action": str(a, "action"), "reason_code": str(a, "reason_code"), "explanation": str(a, "explanation")}
	if tool == "reply" {
		body = map[string]any{"tool": tool, "text": str(a, "text")}
	}
	bodyJSON, _ := json.Marshal(body)
	c, refusal, err := caseFence(ctx, tx, pid, str(a, "case_id"), str(a, "instance"), nowRaw)
	if err != nil {
		return nil, err
	}
	if refusal == "case_closed" && str(c, "lease_instance") == str(a, "instance") && str(c, "outcome") == tool && str(c, "outcome_body") == string(bodyJSON) {
		var recorded any
		_ = json.Unmarshal([]byte(str(c, "result")), &recorded)
		return caseResult(recorded), nil
	}
	if refusal != "" {
		return caseRefusal(refusal), nil
	}
	usage, _ := a["usage"].(map[string]any)
	usageJSON, _ := json.Marshal(usage)
	tokens := asInt(usage["input_tokens"]) + asInt(usage["output_tokens"])
	// decide is a decide case's outcome, reply a chat's, recommend an
	// advise case's; abstain ends any [DM-M1, d7].
	kind := str(c, "kind")
	if owner := map[string]string{"decide": "decide", "reply": "chat", "recommend": "advise"}[tool]; owner != "" && owner != kind {
		return caseRefusal("tool_refused"), nil
	}
	var result map[string]any
	switch tool {
	case "recommend":
		var code string
		if result, code, err = s.recommendOutcome(ctx, tx, w, c, a, nowRaw); err != nil || code != "" {
			return caseRefusal(code), err
		}
	case "reply":
		if text := str(a, "text"); strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > maxChatReply {
			return caseRefusal("invalid_argument"), nil
		}
		result = map[string]any{}
	case "decide":
		var permitted, reasons []string
		_ = json.Unmarshal([]byte(str(c, "permitted")), &permitted)
		_ = json.Unmarshal([]byte(str(c, "reason_codes")), &reasons)
		if !containsString(permitted, str(a, "action")) {
			return map[string]any{"error": "tool_refused"}, nil
		}
		if !containsString(reasons, str(a, "reason_code")) {
			return caseRefusal("invalid_argument"), nil
		}
		dispatcher, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, DispatcherSessionName)
		if err != nil {
			return nil, err
		}
		// The action is carried out as the dispatcher session, never as
		// the model's [DM-L1].
		as := *s
		as.principal = dispatcherPrincipal
		modelActor := tx.actorID
		tx.actorID = str(dispatcher, "id")
		recorded, err := as.dispatcherRecord(ctx, tx, w, p, dispatcher, map[string]any{
			"subject_id": str(c, "subject_id"), "subject_revision": str(c, "subject_revision"), "policy_revision": asInt(c["policy_revision"]),
			"engine": "model", "proposal": str(a, "action"), "reason_code": str(a, "reason_code"), "guardrail": "permitted",
			"action": str(a, "action"), "domain": str(c, "domain"), "facts_source": str(c, "facts_source"), "app_project_id": str(c, "app_project_id"),
			"explanation": str(a, "explanation"),
		})
		tx.actorID = modelActor
		if err != nil {
			return nil, err
		}
		taken, reason := str(recorded.(map[string]any), "action"), str(recorded.(map[string]any), "reason_code")
		result = map[string]any{"guardrail": "permitted", "action_taken": taken, "reason_code": reason}
		if taken == "superseded" {
			// Nothing was done, and the claim is decided again next tick
			// (by the rules: this revision had its model case).
			result = map[string]any{"guardrail": "overridden", "action_taken": "wait", "reason_code": reason}
		}
	case "abstain":
		if !containsString(AbstainReasonCodes, str(a, "reason_code")) {
			return caseRefusal("invalid_argument"), nil
		}
		result = map[string]any{}
		// Only a decide case falls back to the rules: a chat has none, and
		// an advise case's escalation already went out.
		if kind == "decide" {
			result["fallback"] = "rules"
		}
	default:
		return caseRefusal("tool_refused"), nil
	}
	resultJSON, _ := json.Marshal(result)
	// The lease read by caseFence must still hold: a takeover committed by
	// a cases/next meanwhile fences this outcome out.
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET closed_at=?,outcome=?,outcome_body=?,result=?,usage=?,tokens=? WHERE id=? AND closed_at IS NULL AND lease_instance=? AND lease_generation=?`, nowRaw, tool, string(bodyJSON), string(resultJSON), string(usageJSON), tokens, str(c, "id"), str(c, "lease_instance"), asInt(c["lease_generation"]))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// A cases/next expired or took it over meanwhile: roll the
		// decision back.
		return nil, errCaseExpired
	}
	return caseResult(result), nil
}

// ModelToolResult is a case tool call's answer: a result, or a refusal's
// code. Fallback asks the server to nudge the rules (D5-M3): the case
// ended without a decision.
type ModelToolResult struct {
	Result    any
	Error     string
	Fallback  bool
	ProjectID string
}

// ModelTool runs one case tool for a project token: it authenticates (and
// stamps) the token, then runs the read or the outcome as the project's
// dispatcher-model session, under its allow-list.
func (s *Store) ModelTool(ctx context.Context, token, caseID, tool string, args map[string]any) (ModelToolResult, error) {
	var out ModelToolResult
	c, err := s.ModelAuth(ctx, token)
	if err != nil {
		return out, err
	}
	out.ProjectID = c.ProjectID
	op := "dispatcher_case_read"
	switch tool {
	case "decide", "abstain", "reply", "recommend":
		op = "dispatcher_case_outcome"
	case "route_notice":
		op = "dispatcher_route"
	case "ask_session":
		op = "dispatcher_ask_session"
	}
	a := map[string]any{"case_id": caseID, "instance": c.Instance, "tool": tool}
	for k, v := range args {
		a[k] = v
	}
	r, err := s.ExecuteDispatcher(ctx, c.ProjectID, DispatcherModelSessionName, Request{Operation: op, Args: a})
	var coded interface{ Code() string }
	if errors.As(err, &coded) && coded.Code() == "deadline_passed" {
		// Only a decide case has a rules fallback to nudge (#121 L3).
		out.Error, out.Fallback = "deadline_passed", s.decideCase(ctx, c.ProjectID, caseID)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	m, _ := r.Data.(map[string]any)
	out.Result, out.Error = m["result"], str(m, "error")
	result, _ := out.Result.(map[string]any)
	out.Fallback = out.Error == "deadline_passed" && s.decideCase(ctx, c.ProjectID, caseID) || out.Error == "" && tool == "abstain" && result["fallback"] == "rules"
	return out, nil
}

// decideCase reports whether caseID is one of pid's decide cases.
func (s *Store) decideCase(ctx context.Context, pid, caseID string) bool {
	var kind string
	_ = s.db.QueryRowContext(ctx, `SELECT kind FROM dispatcher_cases WHERE id=? AND project_id=?`, caseID, pid).Scan(&kind)
	return kind == "decide"
}
