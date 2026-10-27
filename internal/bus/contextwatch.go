package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// contextSchemaV26 adds context watching (CONTEXT-WATCH design r2, §4.3 to
// §4.5): the owner's per-project setting, each session's last report, and
// one row per context notice, which is its dedup key.
const contextSchemaV26 = `
CREATE TABLE context_watch_policies(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL,enabled INTEGER NOT NULL,policy TEXT NOT NULL,updated_at TEXT NOT NULL,updated_by_principal TEXT NOT NULL,updated_by_session TEXT);
CREATE TABLE context_notices(project_id TEXT NOT NULL REFERENCES projects(id),session_id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('warn','handover','backstop')),compactions INTEGER NOT NULL,outcome TEXT NOT NULL CHECK(outcome IN ('sent','rate_limited','no_recipient')),tokens INTEGER NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,session_id,kind,compactions));
ALTER TABLE sessions ADD COLUMN context_tokens INTEGER;
ALTER TABLE sessions ADD COLUMN context_window INTEGER;
ALTER TABLE sessions ADD COLUMN context_level TEXT CHECK(context_level IN ('ok','warn','handover'));
ALTER TABLE sessions ADD COLUMN context_compactions INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN context_at TEXT;
ALTER TABLE sessions ADD COLUMN context_backstop_at TEXT;
ALTER TABLE sessions ADD COLUMN context_notified_at TEXT;
ALTER TABLE sessions ADD COLUMN context_handover_at TEXT;
`

const (
	// contextMaxTokens bounds every token figure, in a report or a level.
	contextMaxTokens = 100_000_000
	// contextMaxLevels and contextMaxRecipients bound the setting.
	contextMaxLevels     = 32
	contextMaxRecipients = 8
	// contextMaxHint is the handover hint's limit, in characters.
	contextMaxHint = 280
	// contextReportEvery is the least time between a session's reports: a
	// minute, less a margin so a reporter ticking each minute isn't refused
	// for arriving a moment early.
	contextReportEvery = 55 * time.Second
	// contextNoticeEvery is the least time between a session's
	// coordinator-bound notices, and, separately, between its handover
	// notices (CW-M2): a handover notice isn't held back by a recent warn,
	// but a session swinging between ok and handover can't repeat one.
	contextNoticeEvery = 30 * time.Minute
	// contextClockSkew is how far ahead of the server a report's
	// observed_at may be.
	contextClockSkew = 5 * time.Minute
)

// contextLine ends every context notice: it reports, it grants nothing.
const contextLine = "This is an automatic context notice, computed by the ClawBridge server from counts the session reported and the project's context-watch setting. It is not an instruction from another session."

// ContextLevel is one role's thresholds, in tokens. Role "*" is the
// default.
type ContextLevel struct {
	Role           string `json:"role"`
	WarnTokens     int64  `json:"warn_tokens"`
	HandoverTokens int64  `json:"handover_tokens"`
}

// ContextWatch is a project's context-watch setting (§4.3). It is off by
// default.
type ContextWatch struct {
	Enabled         bool           `json:"enabled"`
	Levels          []ContextLevel `json:"levels"`
	ExtraRecipients []string       `json:"extra_recipients"`
	HandoverHint    string         `json:"handover_hint"`
}

// ParseContextWatch checks a setting document: known fields only, role
// names unique, each warn level below its handover level, recipients that
// are session names, and a short single-line hint.
func ParseContextWatch(v any) (ContextWatch, error) {
	raw, err := json.Marshal(v)
	if err != nil || v == nil {
		return ContextWatch{}, invalid("context_watch must be a JSON object")
	}
	var c ContextWatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return ContextWatch{}, invalid("context_watch has only enabled, levels, extra_recipients and handover_hint, with the documented types")
	}
	if len(c.Levels) > contextMaxLevels {
		return ContextWatch{}, invalid(fmt.Sprintf("context_watch has at most %d levels", contextMaxLevels))
	}
	seen := map[string]bool{}
	for _, l := range c.Levels {
		if l.Role != "*" && !roleNamePattern.MatchString(l.Role) {
			return ContextWatch{}, invalid("each level's role is a role name or *")
		}
		if seen[l.Role] {
			return ContextWatch{}, invalid("each role has one level")
		}
		seen[l.Role] = true
		if l.WarnTokens < 1 || l.HandoverTokens <= l.WarnTokens || l.HandoverTokens > contextMaxTokens {
			return ContextWatch{}, invalid(fmt.Sprintf("each level has 0 < warn_tokens < handover_tokens <= %d", contextMaxTokens))
		}
	}
	if c.Enabled && len(c.Levels) == 0 {
		return ContextWatch{}, invalid("an enabled context_watch needs at least one level")
	}
	if len(c.ExtraRecipients) > contextMaxRecipients {
		return ContextWatch{}, invalid(fmt.Sprintf("context_watch has at most %d extra_recipients", contextMaxRecipients))
	}
	for _, name := range c.ExtraRecipients {
		if !namePattern.MatchString(name) || strings.EqualFold(name, SystemSessionName) {
			return ContextWatch{}, invalid("each extra recipient is a session name")
		}
	}
	if len([]rune(c.HandoverHint)) > contextMaxHint || strings.IndexFunc(c.HandoverHint, unicode.IsControl) >= 0 {
		return ContextWatch{}, invalid(fmt.Sprintf("handover_hint is one line of at most %d characters", contextMaxHint))
	}
	if c.Levels == nil {
		c.Levels = []ContextLevel{}
	}
	if c.ExtraRecipients == nil {
		c.ExtraRecipients = []string{}
	}
	return c, nil
}

// levelFor is the level that applies to a session of role: its own role's,
// else "*", else none.
func (c ContextWatch) levelFor(role string) *ContextLevel {
	var fallback *ContextLevel
	for i := range c.Levels {
		switch c.Levels[i].Role {
		case role:
			return &c.Levels[i]
		case "*":
			fallback = &c.Levels[i]
		}
	}
	return fallback
}

// contextLevel names where tokens stand against l.
func contextLevel(l *ContextLevel, tokens int64) string {
	switch {
	case l == nil:
		return "ok"
	case tokens >= l.HandoverTokens:
		return "handover"
	case tokens >= l.WarnTokens:
		return "warn"
	}
	return "ok"
}

func contextWatchOperation(op string) bool {
	return op == "context_watch_set"
}

// readContextWatch is the project's setting and its metadata; with no row,
// off at revision 0.
func readContextWatch(ctx context.Context, tx *transaction, pid string) (ContextWatch, map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT revision,enabled,policy,updated_at,updated_by_principal FROM context_watch_policies WHERE project_id=?`, pid)
	if err != nil {
		return ContextWatch{}, nil, err
	}
	if len(rows) == 0 {
		return ContextWatch{Levels: []ContextLevel{}, ExtraRecipients: []string{}}, map[string]any{"revision": int64(0), "updated_at": nil, "updated_by": nil}, nil
	}
	var stored any
	if json.Unmarshal([]byte(str(rows[0], "policy")), &stored) != nil {
		return ContextWatch{}, nil, errors.New("the stored context-watch setting is unreadable")
	}
	c, err := ParseContextWatch(stored)
	if err != nil || c.Enabled != (asInt(rows[0]["enabled"]) == 1) {
		return ContextWatch{}, nil, errors.New("the stored context-watch setting is unreadable")
	}
	return c, map[string]any{"revision": int64(asInt(rows[0]["revision"])), "updated_at": rows[0]["updated_at"], "updated_by": rows[0]["updated_by_principal"]}, nil
}

func contextWatchView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	c, meta, err := readContextWatch(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	meta["context_watch"] = c
	return meta, nil
}

// contextWatchOp sets the project's context-watch setting: the project
// owner, at the expected revision, audited. Reading it is open to every
// member and handled before sessions.
func (s *Store) contextWatchOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid, a := w["id"].(string), p["id"].(string), r.Args
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	old, meta, err := readContextWatch(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid(r.Operation + " requires expected_revision")
	}
	if meta["revision"].(int64) != int64(expected) {
		return nil, conflict(fmt.Sprintf("the context-watch setting changed: current revision is %d", meta["revision"]))
	}
	next, err := ParseContextWatch(a["context_watch"])
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(next)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	var session any
	if actor != nil {
		session = actor["id"]
	}
	enabled, revision := 0, int64(expected)+1
	if next.Enabled {
		enabled = 1
	}
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO context_watch_policies(project_id,revision,enabled,policy,updated_at,updated_by_principal,updated_by_session) VALUES(?,?,?,?,?,?,?)`, pid, revision, enabled, string(encoded), now, s.principal, session)
	} else {
		var result interface{ RowsAffected() (int64, error) }
		result, err = tx.ExecContext(ctx, `UPDATE context_watch_policies SET revision=?,enabled=?,policy=?,updated_at=?,updated_by_principal=?,updated_by_session=? WHERE project_id=? AND revision=?`, revision, enabled, string(encoded), now, s.principal, session, pid, expected)
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				err = conflict("the context-watch setting changed concurrently; read it again")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "context_watch", pid, r.Operation, map[string]any{"old": old, "new": next, "revision": revision}); err != nil {
		return nil, err
	}
	return contextWatchView(ctx, tx, pid)
}

// contextReport is a session's report: integers, an enum and a time.
type contextReport struct {
	tokens, window int64
	compactions    int64
	// trigger is the last compaction's, when the client knows it: manual
	// or auto.
	trigger string
}

func parseContextReport(a map[string]any, now time.Time) (contextReport, error) {
	var c contextReport
	tokens, err := integer(a["tokens"])
	if err != nil || a["tokens"] == nil || tokens < 0 || tokens > contextMaxTokens {
		return c, invalid(fmt.Sprintf("report_context needs tokens, 0 to %d", contextMaxTokens))
	}
	c.tokens = int64(tokens)
	if a["window"] != nil {
		window, err := integer(a["window"])
		if err != nil || window < 1 || window > contextMaxTokens {
			return c, invalid(fmt.Sprintf("window is null or 1 to %d", contextMaxTokens))
		}
		c.window = int64(window)
	}
	compactions, err := integer(a["compactions"])
	if err != nil || a["compactions"] == nil || compactions < 0 {
		return c, invalid("report_context needs compactions, 0 or more")
	}
	c.compactions = int64(compactions)
	switch c.trigger = str(a, "trigger"); c.trigger {
	case "", "manual", "auto":
	default:
		return c, invalid("trigger is manual or auto")
	}
	// The client's level is advisory: the server recomputes it.
	if level := str(a, "level"); level != "ok" && level != "warn" && level != "handover" {
		return c, invalid("level is ok, warn or handover")
	}
	observed, err := time.Parse(time.RFC3339Nano, str(a, "observed_at"))
	if err != nil || observed.After(now.Add(contextClockSkew)) {
		return c, invalid("observed_at is an RFC 3339 time, not in the future")
	}
	return c, nil
}

// reportContextOp stores a session's context report (§4.4) and sends the
// notices its transitions call for (§4.5). It is fenced like a heartbeat:
// the caller's own active session at its current instance. It is not an
// MCP tool.
func (s *Store) reportContextOp(ctx context.Context, tx *transaction, w map[string]any, pid string, r Request) (any, error) {
	instance := str(r.Args, "instance")
	if r.Session == "" || instance == "" {
		return nil, invalid("report_context needs the session and its instance")
	}
	match, args := sessionAddress(r.Session)
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match+` AND instance=? AND owner_principal=?`, append(append([]any{pid}, args...), instance, s.principal)...)
	if err != nil {
		return nil, scopedError(err, "session instance mismatch; pass the instance returned by register", "forbidden")
	}
	if actor["status"] != "active" {
		return nil, invalid("the session is retired or moved; it cannot report")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := at(nowRaw)
	report, err := parseContextReport(r.Args, now)
	if err != nil {
		return nil, err
	}
	policy, _, err := readContextWatch(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !policy.Enabled {
		// Nothing is stored for a project that hasn't turned it on.
		return map[string]any{"enabled": false}, nil
	}
	if last := str(actor, "context_at"); last != "" && now.Sub(at(last)) < contextReportEvery {
		return nil, scoped("report_context rate limit (1/minute) reached; no write queued", "rate_limited")
	}
	// The stored count is monotonic and moves by at most one per report, so
	// a client can't mint notice keys faster than it may report (CW-M2).
	stored := int64(asInt(actor["context_compactions"]))
	if report.compactions < stored {
		return nil, scoped(fmt.Sprintf("invalid_compactions: the session's compaction count is %d, and never goes down", stored), "invalid")
	}
	compacted := report.compactions > stored
	count := stored
	if compacted {
		count++
	}
	levels := effectiveLevels(policy.levelFor(str(actor, "role")), actor)
	previous := str(actor, "context_level")
	level := contextLevel(levels, report.tokens)
	var window any
	if report.window > 0 {
		window = report.window
	}
	// A compaction asked for by hand is no backstop.
	backstop := actor["context_backstop_at"]
	auto := compacted && report.trigger != "manual"
	if auto && previous != "handover" {
		backstop = nowRaw
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET context_tokens=?,context_window=?,context_level=?,context_compactions=?,context_at=?,context_backstop_at=? WHERE id=? AND instance=? AND status='active'`,
		report.tokens, window, level, count, nowRaw, backstop, actor["id"], instance); err != nil {
		return nil, err
	}
	n := &contextNotices{s: s, w: w, pid: pid, actor: actor, now: now, nowRaw: nowRaw, count: count, levels: levels, policy: policy}
	if auto && previous == "warn" && !(report.window > 0 && levels != nil && report.window <= levels.HandoverTokens) {
		// Compacted past warn without reaching its handover level: the
		// auto-compact backstop fired (CW-L1). A smaller window than the
		// handover level would make every compaction one, so it never
		// notifies.
		before := int64(asInt(actor["context_tokens"]))
		if err = n.send(ctx, tx, "backstop", before, fmt.Sprintf("Context: %s compacted at %s tokens before its handover level (auto-compact backstop).", str(actor, "name"), kTokens(before))); err != nil {
			return nil, err
		}
	}
	switch {
	case level == "handover" && previous != "handover":
		if err = n.send(ctx, tx, "handover", report.tokens, fmt.Sprintf("Context: %s reached its handover level (%s); asked for a handover note.", str(actor, "name"), kTokens(levels.HandoverTokens))); err != nil {
			return nil, err
		}
	case level == "warn" && previous != "warn" && previous != "handover":
		if err = n.send(ctx, tx, "warn", report.tokens, fmt.Sprintf("Context: %s is at %s tokens (warn %s, handover %s).", str(actor, "name"), kTokens(report.tokens), kTokens(levels.WarnTokens), kTokens(levels.HandoverTokens))); err != nil {
			return nil, err
		}
	}
	answer := map[string]any{"enabled": true, "level": level, "compactions": count, "handover_hint": policy.HandoverHint, "levels": nil, "notices": n.sent}
	if levels != nil {
		answer["levels"] = map[string]any{"warn_tokens": levels.WarnTokens, "handover_tokens": levels.HandoverTokens}
	}
	return answer, nil
}

// contextNotices sends one report's notices.
type contextNotices struct {
	s           *Store
	w           map[string]any
	pid         string
	actor       map[string]any
	now         time.Time
	nowRaw      string
	count       int64
	levels      *ContextLevel
	policy      ContextWatch
	sent        []string
	systemReady bool
}

// send records the notice of kind once per (session, kind, compaction
// count), and delivers it. A handover notice also goes to the session
// itself, at most once per contextNoticeEvery; the other coordinator-bound
// notices share their own limit of one per contextNoticeEvery.
func (n *contextNotices) send(ctx context.Context, tx *transaction, kind string, tokens int64, body string) error {
	sid := n.actor["id"].(string)
	res, err := tx.ExecContext(ctx, `INSERT INTO context_notices(project_id,session_id,kind,compactions,outcome,tokens,created_at) VALUES(?,?,?,?,'sent',?,?) ON CONFLICT DO NOTHING`, n.pid, sid, kind, n.count, tokens, n.nowRaw)
	if err != nil {
		return err
	}
	if inserted, _ := res.RowsAffected(); inserted != 1 {
		return nil
	}
	if kind == "handover" {
		if last := str(n.actor, "context_handover_at"); last != "" && n.now.Sub(at(last)) < contextNoticeEvery {
			n.sent = append(n.sent, kind+":rate_limited")
			_, err := tx.ExecContext(ctx, `UPDATE context_notices SET outcome='rate_limited' WHERE project_id=? AND session_id=? AND kind=? AND compactions=?`, n.pid, sid, kind, n.count)
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context_handover_at=? WHERE id=?`, n.nowRaw, sid); err != nil {
			return err
		}
		self := fmt.Sprintf("Your context is at %s tokens, past this project's handover level (%s). Write your handover note now, then compact at a clean break.", kTokens(tokens), kTokens(n.levels.HandoverTokens))
		// The hint is the project owner's text, checked when it was set.
		if n.policy.HandoverHint != "" {
			self += " " + n.policy.HandoverHint
		}
		if err := n.deliver(ctx, tx, []map[string]any{n.actor}, self); err != nil {
			return err
		}
	}
	outcome := "sent"
	if last := str(n.actor, "context_notified_at"); kind != "handover" && last != "" && n.now.Sub(at(last)) < contextNoticeEvery {
		outcome = "rate_limited"
	} else {
		recipients, err := n.recipients(ctx, tx)
		if err != nil {
			return err
		}
		if len(recipients) == 0 {
			outcome = "no_recipient"
		} else {
			if err := n.deliver(ctx, tx, recipients, body); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context_notified_at=? WHERE id=?`, n.nowRaw, sid); err != nil {
				return err
			}
			n.actor["context_notified_at"] = n.nowRaw
		}
	}
	if outcome != "sent" {
		if _, err := tx.ExecContext(ctx, `UPDATE context_notices SET outcome=? WHERE project_id=? AND session_id=? AND kind=? AND compactions=?`, outcome, n.pid, sid, kind, n.count); err != nil {
			return err
		}
	}
	n.sent = append(n.sent, kind+":"+outcome)
	return nil
}

// recipients are the project's active coordinator and the setting's extra
// recipients, but never the reporting session itself, which gets its own
// handover notice.
func (n *contextNotices) recipients(ctx context.Context, tx *transaction) ([]map[string]any, error) {
	coordinator, err := n.s.coordinatorTargets(ctx, tx, n.pid, "", nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	seen := map[any]bool{n.actor["id"]: true}
	add := func(rows []map[string]any) {
		for _, row := range rows {
			if !seen[row["id"]] {
				seen[row["id"]] = true
				out = append(out, row)
			}
		}
	}
	add(coordinator)
	for _, name := range n.policy.ExtraRecipients {
		rows, err := query(ctx, tx, `SELECT id,name,role FROM sessions WHERE project_id=? AND name=? AND status='active' AND role<>'system'`, n.pid, name)
		if err != nil {
			return nil, err
		}
		add(rows)
	}
	return out, nil
}

// deliver sends body, with the context line, from the project's system
// session to each recipient.
func (n *contextNotices) deliver(ctx context.Context, tx *transaction, to []map[string]any, body string) error {
	wid := n.w["id"].(string)
	if !n.systemReady {
		if err := n.s.ensureSystemSession(ctx, tx, wid, n.pid); err != nil {
			return err
		}
		n.systemReady = true
	}
	system, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, n.pid, SystemSessionName)
	if err != nil {
		return err
	}
	body += "\n" + contextLine
	retention := asInt(n.w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	for _, c := range to {
		id := newID("msg_")
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'context',?,?,?,?,?,?,NULL)`,
			id, n.pid, n.pid, system["id"], str(c, "name"), id, body, n.nowRaw, layout(n.now.AddDate(0, 0, retention)), systemPrincipal, newID("context_"), digestOf(body)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id) VALUES(?,?,?)`, id, n.pid, c["id"]); err != nil {
			return err
		}
	}
	return nil
}

// kTokens writes a token count in thousands: 612k.
func kTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// purgeContextNotices deletes a project's context notice rows older than
// the workspace's retention. They are dedup keys, and keep no text.
func purgeContextNotices(ctx context.Context, tx *transaction, wid, pid, now string) (int64, error) {
	var retention int64
	if err := tx.QueryRowContext(ctx, `SELECT retention_days FROM workspaces WHERE id=?`, wid).Scan(&retention); err != nil {
		return 0, err
	}
	if retention < 1 {
		retention = 30
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM context_notices WHERE project_id=? AND created_at<=?`, pid, layout(at(now).AddDate(0, 0, -int(retention))))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// contextLevelsSchemaV27 holds a session's own lower levels (§4.3): null
// is the project's.
const contextLevelsSchemaV27 = `
ALTER TABLE sessions ADD COLUMN context_warn_tokens INTEGER;
ALTER TABLE sessions ADD COLUMN context_handover_tokens INTEGER;
`

// effectiveLevels are the project's levels for a session, lowered by the
// session's own where it set lower ones. A session can never raise them.
func effectiveLevels(project *ContextLevel, session map[string]any) *ContextLevel {
	if project == nil {
		return nil
	}
	l := *project
	if v, ok := session["context_warn_tokens"]; ok && v != nil {
		if n := int64(asInt(v)); n >= 1 && n < l.WarnTokens {
			l.WarnTokens = n
		}
	}
	if v, ok := session["context_handover_tokens"]; ok && v != nil {
		if n := int64(asInt(v)); n >= 2 && n < l.HandoverTokens {
			l.HandoverTokens = n
		}
	}
	// A project handover since lowered below the session's own warn keeps
	// warn below handover.
	if l.WarnTokens >= l.HandoverTokens {
		l.WarnTokens = l.HandoverTokens - 1
	}
	return &l
}

// contextLevelsSetOp sets the caller's own session's levels below the
// project's (§4.3), or with clear returns it to the project's. A level not
// given keeps the session's own. Fenced like
// a report: the caller's own active session at its instance. Audited.
func (s *Store) contextLevelsSetOp(ctx context.Context, tx *transaction, w map[string]any, pid string, r Request) (any, error) {
	instance := str(r.Args, "instance")
	if r.Session == "" || instance == "" {
		return nil, invalid("context_levels_set needs the session and its instance")
	}
	match, args := sessionAddress(r.Session)
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match+` AND instance=? AND owner_principal=?`, append(append([]any{pid}, args...), instance, s.principal)...)
	if err != nil {
		return nil, scopedError(err, "session instance mismatch; pass the instance returned by register", "forbidden")
	}
	if actor["status"] != "active" {
		return nil, invalid("the session is retired or moved; it cannot set its levels")
	}
	policy, _, err := readContextWatch(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !policy.Enabled {
		return nil, invalid("context watching is off for this project")
	}
	project := policy.levelFor(str(actor, "role"))
	if project == nil {
		return nil, invalid("this project sets no context level for the session's role")
	}
	a := r.Args
	clear := a["clear"] == true
	var warn, handover any
	read := func(key string) (int64, bool, error) {
		v, ok := a[key]
		if !ok || v == nil {
			return 0, false, nil
		}
		n, err := integer(v)
		if err != nil || n < 1 || n > contextMaxTokens {
			return 0, false, invalid(fmt.Sprintf("%s is 1 to %d", key, contextMaxTokens))
		}
		return int64(n), true, nil
	}
	ownWarn, hasWarn, err := read("warn_tokens")
	if err != nil {
		return nil, err
	}
	ownHandover, hasHandover, err := read("handover_tokens")
	if err != nil {
		return nil, err
	}
	switch {
	case clear && (hasWarn || hasHandover):
		return nil, invalid("clear takes no levels")
	case !clear && !hasWarn && !hasHandover:
		return nil, invalid("set warn_tokens, handover_tokens or both, or clear")
	case !clear:
		// A level not given keeps the session's own, if it set one.
		warn, handover = actor["context_warn_tokens"], actor["context_handover_tokens"]
		effWarn, effHandover := project.WarnTokens, project.HandoverTokens
		if warn != nil && int64(asInt(warn)) < effWarn {
			effWarn = int64(asInt(warn))
		}
		if handover != nil && int64(asInt(handover)) < effHandover {
			effHandover = int64(asInt(handover))
		}
		if hasWarn {
			if ownWarn > project.WarnTokens {
				return nil, invalid(fmt.Sprintf("a session may only lower its levels: the project's warn level is %d", project.WarnTokens))
			}
			effWarn, warn = ownWarn, ownWarn
		}
		if hasHandover {
			if ownHandover > project.HandoverTokens {
				return nil, invalid(fmt.Sprintf("a session may only lower its levels: the project's handover level is %d", project.HandoverTokens))
			}
			effHandover, handover = ownHandover, ownHandover
		}
		if effWarn >= effHandover {
			return nil, invalid(fmt.Sprintf("the warn level (%d) must stay below the handover level (%d)", effWarn, effHandover))
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET context_warn_tokens=?,context_handover_tokens=? WHERE id=? AND instance=? AND status='active'`, warn, handover, actor["id"], instance); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, w["id"].(string), pid, actor["id"].(string), "context_levels", actor["id"].(string), "context_levels_set",
		map[string]any{"warn_tokens": warn, "handover_tokens": handover, "cleared": clear}); err != nil {
		return nil, err
	}
	actor["context_warn_tokens"], actor["context_handover_tokens"] = warn, handover
	l := effectiveLevels(project, actor)
	return map[string]any{"levels": map[string]any{"warn_tokens": l.WarnTokens, "handover_tokens": l.HandoverTokens},
		"own": map[string]any{"warn_tokens": warn, "handover_tokens": handover}}, nil
}
