package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Issue subscribers (live/ISSUE-SUBSCRIBERS-PLAN.md): on a hosted server an
// agent's Linear writes are the ClawBridge app's, so nobody is subscribed
// to the issues agents create or claim. A project's setting names who is:
// the workspace owner, by email, and up to MaxIssueSubscribers Linear users.
// Each create (as the app) and each confirmed claim queues one row per
// issue; the server watcher applies due rows as the app, after the markers,
// under the project's hourly Linear budget. Rows are kept (B3): an issue is
// subscribed at most once, so someone who unsubscribes isn't added again.

// issueSubscribersSchema is schema 41. A binding linked before it starts
// off (the plan's Q1): the upgrade doesn't begin writing on boards that are
// already linked. A project with no row takes the default.
const issueSubscribersSchema = `
CREATE TABLE issue_subscribers_policy(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),owner INTEGER NOT NULL CHECK(owner IN (0,1)),user_ids TEXT NOT NULL,updated_by TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE issue_subscribers_owner(project_id TEXT PRIMARY KEY REFERENCES projects(id),missing_until TEXT NOT NULL);
CREATE TABLE issue_subscriptions(project_id TEXT NOT NULL REFERENCES projects(id),issue_uuid TEXT NOT NULL,reason TEXT NOT NULL CHECK(reason IN ('create','claim')),state TEXT NOT NULL CHECK(state IN ('queued','done','skipped','failed')),attempts INTEGER NOT NULL DEFAULT 0,due_at TEXT NOT NULL,outcome TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(project_id,issue_uuid));
CREATE INDEX issue_subscriptions_due ON issue_subscriptions(project_id,state,due_at);
INSERT INTO issue_subscribers_policy(project_id,revision,enabled,owner,user_ids,updated_by,updated_at) SELECT project_id,1,0,1,'[]','upgrade',updated_at FROM taskboard_bindings WHERE mapping IS NOT NULL;
`

const (
	// MaxIssueSubscribers caps the Linear users added besides the owner.
	MaxIssueSubscribers = 5
	// subscribeDueLimit is the most rows one pass is handed.
	subscribeDueLimit = 5
	// subscribeAttempts is how often a row is tried before it fails.
	subscribeAttempts = 5
	// ownerMissingFor is how long Linear's refusal of the owner's email is
	// remembered, so each issue doesn't spend a call on it.
	ownerMissingFor = 24 * time.Hour
	// IssueSubscribersUnproven is the refusal before the operator has proven
	// the app may subscribe other people (B2).
	IssueSubscribersUnproven = "issue subscribers are not yet proven on this server (serve --issue-subscribers-proven, after the live check)"
)

// SubscribeRowCalls is one row's Linear calls: the issue read that proves
// its team is bound and its people are members, and one subscribe each.
func SubscribeRowCalls(subscribers int) int { return 1 + subscribers }

// subscribeReasons are the only outcomes a settle records.
var subscribeReasons = map[string]bool{"": true, "not_bound": true, "owner_not_in_linear": true, "partial": true, "no_scope": true, "not_found": true, "error": true, "off": true}

// SetIssueSubscribersProven records serve --issue-subscribers-proven.
func (s *Store) SetIssueSubscribersProven(proven bool) { s.issueSubscribersProven = proven }

func issueSubscribersOperation(op string) bool {
	return op == "issue_subscribers_get" || op == "issue_subscribers_set"
}

// subscribersSystemOperation is what only the server reaches, through
// ExecuteSystem: the queue after an app create, and the watcher's pass.
func subscribersSystemOperation(op string) bool {
	return op == "subscribe_queue" || op == "subscribe_due" || op == "subscribe_settle" || op == "subscribe_claims" || op == "subscribe_charge"
}

// issueSubscribersPolicy is a project's setting, or the default for a
// project linked after schema 41: on, the owner only.
type issueSubscribersPolicy struct {
	Revision       int
	Enabled        bool
	Owner          bool
	UserIDs        []string
	Default        bool
	OwnerMissing   string
	UpdatedBy      string
	UpdatedAt      string
	effectiveOn    bool
	provenOnServer bool
}

func readIssueSubscribers(ctx context.Context, tx *transaction, pid string, proven bool) (issueSubscribersPolicy, error) {
	rows, err := query(ctx, tx, `SELECT * FROM issue_subscribers_policy WHERE project_id=?`, pid)
	if err != nil {
		return issueSubscribersPolicy{}, err
	}
	p := issueSubscribersPolicy{Enabled: true, Owner: true, UserIDs: []string{}, Default: true}
	if len(rows) == 1 {
		r := rows[0]
		p = issueSubscribersPolicy{Revision: asInt(r["revision"]), Enabled: asInt(r["enabled"]) == 1, Owner: asInt(r["owner"]) == 1, UserIDs: []string{},
			UpdatedBy: str(r, "updated_by"), UpdatedAt: str(r, "updated_at")}
		_ = json.Unmarshal([]byte(str(r, "user_ids")), &p.UserIDs)
	}
	// Linear's refusal of the owner's email is remembered apart from the
	// setting, so it never moves its revision.
	missing, err := query(ctx, tx, `SELECT missing_until FROM issue_subscribers_owner WHERE project_id=?`, pid)
	if err != nil {
		return issueSubscribersPolicy{}, err
	}
	if len(missing) == 1 {
		p.OwnerMissing = str(missing[0], "missing_until")
	}
	p.provenOnServer = proven
	p.effectiveOn = proven && p.Enabled && (p.Owner || len(p.UserIDs) > 0)
	return p, nil
}

// view is the setting as members read it. It names no email: the owner is
// a flag (B4).
func (p issueSubscribersPolicy) view(counts map[string]int) map[string]any {
	out := map[string]any{"revision": p.Revision, "enabled": p.Enabled, "owner": p.Owner, "user_ids": p.UserIDs, "default": p.Default,
		"proven": p.provenOnServer, "on": p.effectiveOn, "rows": counts}
	if p.OwnerMissing != "" {
		out["owner_not_in_linear_until"] = p.OwnerMissing
	}
	if !p.provenOnServer {
		out["note"] = IssueSubscribersUnproven
	}
	return out
}

func subscriptionCounts(ctx context.Context, tx *transaction, pid string) (map[string]int, error) {
	rows, err := query(ctx, tx, `SELECT state,COUNT(*) AS n FROM issue_subscriptions WHERE project_id=? GROUP BY state`, pid)
	if err != nil {
		return nil, err
	}
	out := map[string]int{"queued": 0, "done": 0, "skipped": 0, "failed": 0}
	for _, r := range rows {
		out[str(r, "state")] = asInt(r["n"])
	}
	return out, nil
}

// issueSubscribersOp reads the setting (any member or session) or sets it
// (the project owner or a lead, without a session: agents never choose who
// is notified of their work).
func (s *Store) issueSubscribersOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, invalid("instance requires session")
	}
	current, err := readIssueSubscribers(ctx, tx, pid, s.issueSubscribersProven)
	if err != nil {
		return nil, err
	}
	if r.Operation == "issue_subscribers_get" {
		counts, err := subscriptionCounts(ctx, tx, pid)
		if err != nil {
			return nil, err
		}
		return current.view(counts), nil
	}
	if r.Session != "" {
		return nil, scoped("a session can't set who is subscribed to agents' issues; the project owner or a lead runs linear subscribers in their own terminal", "forbidden")
	}
	if projectRole != "owner" && projectRole != "lead" {
		return nil, scoped("project lead or owner permission required", "forbidden")
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("issue_subscribers_set requires expected_revision")
	}
	if current.Revision != expected {
		return nil, conflict(fmt.Sprintf("the issue subscribers setting changed: current revision is %d", current.Revision))
	}
	next := current
	if v, ok := a["enabled"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, invalid("enabled must be true or false")
		}
		next.Enabled = b
	}
	if v, ok := a["owner"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, invalid("owner must be true or false")
		}
		next.Owner = b
	}
	if _, ok := a["user_ids"]; ok {
		ids, err := subscriberIDs(a["user_ids"])
		if err != nil {
			return nil, err
		}
		next.UserIDs = ids
	}
	// B2: nothing is turned on before the operator has proven it; turning
	// off, and editing the list, always work (and nothing runs unproven).
	if a["enabled"] == true && !s.issueSubscribersProven {
		return nil, invalid(IssueSubscribersUnproven)
	}
	encoded, _ := json.Marshal(next.UserIDs)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	revision := expected + 1
	if current.Revision == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO issue_subscribers_policy(project_id,revision,enabled,owner,user_ids,updated_by,updated_at) VALUES(?,?,?,?,?,?,?)`, pid, revision, boolInt(next.Enabled), boolInt(next.Owner), string(encoded), s.principal, now)
	} else {
		var res interface{ RowsAffected() (int64, error) }
		res, err = tx.ExecContext(ctx, `UPDATE issue_subscribers_policy SET revision=?,enabled=?,owner=?,user_ids=?,updated_by=?,updated_at=? WHERE project_id=? AND revision=?`, revision, boolInt(next.Enabled), boolInt(next.Owner), string(encoded), s.principal, now, pid, expected)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return nil, conflict("the issue subscribers setting changed concurrently; read it again")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if !next.Enabled {
		// Off drops what is queued (B3 keeps what was done).
		if _, err = tx.ExecContext(ctx, `UPDATE issue_subscriptions SET state='skipped',outcome='off',updated_at=? WHERE project_id=? AND state='queued'`, now, pid); err != nil {
			return nil, err
		}
	}
	// History names the people by Linear ID and the owner by a flag only.
	if err = s.audit(ctx, tx, wid, pid, "", "issue_subscribers", pid, "set", map[string]any{"revision": revision, "enabled": next.Enabled, "owner": next.Owner, "user_ids": next.UserIDs, "scope": "issue"}); err != nil {
		return nil, err
	}
	saved, err := readIssueSubscribers(ctx, tx, pid, s.issueSubscribersProven)
	if err != nil {
		return nil, err
	}
	counts, err := subscriptionCounts(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	return saved.view(counts), nil
}

// subscriberIDs reads a list of distinct Linear user IDs, at most
// MaxIssueSubscribers.
func subscriberIDs(v any) ([]string, error) {
	raw, ok := v.([]any)
	if !ok {
		if list, isList := v.([]string); isList {
			for _, id := range list {
				raw = append(raw, id)
			}
			ok = true
		}
	}
	if !ok || len(raw) > MaxIssueSubscribers {
		return nil, invalid(fmt.Sprintf("user_ids is a list of at most %d Linear user IDs", MaxIssueSubscribers))
	}
	out, seen := []string{}, map[string]bool{}
	for _, item := range raw {
		id, _ := item.(string)
		id = strings.ToLower(id)
		if !viewIDPattern.MatchString(id) || seen[id] {
			return nil, invalid("user_ids are distinct Linear user IDs")
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// queueSubscription queues one issue's subscribers when the setting is on:
// one row per issue, a second create or claim adding nothing. Only the
// hosted server applies rows, so a local store queues none.
func (s *Store) queueSubscription(ctx context.Context, tx *transaction, pid, issue, reason string) (bool, error) {
	if !s.postgres || !viewIDPattern.MatchString(issue) {
		return false, nil
	}
	p, err := readIssueSubscribers(ctx, tx, pid, s.issueSubscribersProven)
	if err != nil || !p.effectiveOn {
		return false, err
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO issue_subscriptions(project_id,issue_uuid,reason,state,attempts,due_at,outcome,created_at,updated_at) VALUES(?,?,?,'queued',0,?,'',?,?) ON CONFLICT(project_id,issue_uuid) DO NOTHING`, pid, issue, reason, now, now, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// subscribeQueue is the server's queue after an app create (system only).
func (c commentCall) subscribeQueue(ctx context.Context) (any, error) {
	issue, err := lowerUUID(c.a, "issue_uuid", true)
	if err != nil {
		return nil, err
	}
	reason := str(c.a, "reason")
	if reason != "create" && reason != "claim" {
		return nil, invalid("reason must be create or claim")
	}
	queued, err := c.s.queueSubscription(ctx, c.tx, c.pid, issue, reason)
	if err != nil {
		return nil, err
	}
	return map[string]any{"queued": queued}, nil
}

// subscribeClaims lists the issues the project's agents claimed since a
// time, for a backfill (§10): confirmed assignments, released or not.
func (c commentCall) subscribeClaims(ctx context.Context) (any, error) {
	since := str(c.a, "since")
	if _, err := time.Parse(timestampLayout, since); err != nil {
		return nil, invalid("since must be a timestamp")
	}
	rows, err := query(ctx, c.tx, `SELECT issue_uuid,MAX(issue_identifier) AS issue_identifier FROM session_assignments WHERE project_id=? AND confirmed_at IS NOT NULL AND confirmed_at>=? AND issue_uuid<>'' GROUP BY issue_uuid ORDER BY issue_uuid LIMIT 200`, c.pid, since)
	if err != nil {
		return nil, err
	}
	return map[string]any{"claims": rows}, nil
}

// subscribeCharge charges a backfill's own Linear reads to the project's
// hour, as a watch would; with no lease row yet there is no budget to
// charge.
func (c commentCall) subscribeCharge(ctx context.Context) (any, error) {
	calls, err := integer(c.a["calls"])
	if err != nil || calls < 0 || calls > 100 {
		return nil, invalid("calls must be 0–100")
	}
	rows, err := query(ctx, c.tx, `SELECT * FROM watch_lease WHERE project_id=?`, c.pid)
	if err != nil || len(rows) == 0 {
		return map[string]any{"charged": 0}, err
	}
	used, limit, err := c.charge(ctx, rows[0], calls)
	if err != nil {
		return nil, err
	}
	return map[string]any{"charged": calls, "budget_used": used, "budget_limit": limit}, nil
}

// subscribeDue hands the watcher the due rows that fit the hour's budget,
// each with whom to subscribe, and charges them before any call. The
// owner's email is handed only here, to be sent to Linear (B4).
func (c commentCall) subscribeDue(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	p, err := readIssueSubscribers(ctx, c.tx, c.pid, c.s.issueSubscribersProven)
	if err != nil {
		return nil, err
	}
	if !p.effectiveOn {
		return map[string]any{"rows": []any{}, "left": 0, "stopped": "off"}, nil
	}
	used, budget, err := c.charge(ctx, lease, 0)
	if err != nil {
		return nil, err
	}
	rows, err := query(ctx, c.tx, `SELECT issue_uuid,attempts FROM issue_subscriptions WHERE project_id=? AND state='queued' AND due_at<=? ORDER BY due_at,issue_uuid LIMIT ?`, c.pid, layout(c.now), subscribeDueLimit+1)
	if err != nil {
		return nil, err
	}
	owner := ""
	if p.Owner && (p.OwnerMissing == "" || p.OwnerMissing <= layout(c.now)) {
		owner = str(c.w, "owner_principal")
	}
	people := len(p.UserIDs)
	if owner != "" {
		people++
	}
	if people == 0 {
		// Only the owner, whose email Linear refused lately: rows wait.
		return map[string]any{"rows": []any{}, "left": len(rows), "stopped": "owner_not_in_linear"}, nil
	}
	out, cost, stopped := []any{}, 0, ""
	for _, row := range rows {
		if len(out) == subscribeDueLimit {
			stopped = "limit"
			break
		}
		calls := SubscribeRowCalls(people)
		if budget > 0 && used+cost+calls > budget {
			stopped = "budget"
			break
		}
		cost += calls
		item := map[string]any{"issue_uuid": row["issue_uuid"], "user_ids": p.UserIDs, "charged": calls}
		if owner != "" {
			item["owner_email"] = owner
		}
		out = append(out, item)
	}
	if cost > 0 {
		if _, _, err = c.charge(ctx, map[string]any{"budget_used": used, "budget_hour": layout(c.now)[:13], "budget_limit": budget}, cost); err != nil {
			return nil, err
		}
	}
	return map[string]any{"rows": out, "left": len(rows) - len(out), "stopped": stopped}, nil
}

// subscribeSettle records what a row's pass did, and reconciles its charge
// with the calls it made.
func (c commentCall) subscribeSettle(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	issue, err := lowerUUID(c.a, "issue_uuid", true)
	if err != nil {
		return nil, err
	}
	outcome, reason := str(c.a, "outcome"), str(c.a, "reason")
	if outcome != "done" && outcome != "skipped" && outcome != "retry" {
		return nil, invalid("outcome must be done, skipped or retry")
	}
	if !subscribeReasons[reason] {
		return nil, invalid("reason is not one a subscribe pass records")
	}
	calls, err := integer(c.a["calls"])
	if err != nil || calls < 0 || calls > 100 {
		return nil, invalid("calls must be 0–100")
	}
	charged, err := integer(c.a["charged"])
	if err != nil || charged < 0 {
		return nil, invalid("charged must be the row's charge from subscribe_due")
	}
	if most := SubscribeRowCalls(MaxIssueSubscribers + 1); charged > most {
		charged = most
	}
	rows, err := query(ctx, c.tx, `SELECT attempts FROM issue_subscriptions WHERE project_id=? AND issue_uuid=? AND state='queued'`, c.pid, issue)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, scoped("no queued subscription for that issue", "not_found")
	}
	now := layout(c.now)
	attempts := asInt(rows[0]["attempts"]) + 1
	state, due := outcome, now
	if outcome == "retry" {
		state = "queued"
		due = layout(c.now.Add(markerBackoff(attempts)))
		if attempts >= subscribeAttempts {
			state = "failed"
		}
	}
	if _, err = c.tx.ExecContext(ctx, `UPDATE issue_subscriptions SET state=?,attempts=?,due_at=?,outcome=?,updated_at=? WHERE project_id=? AND issue_uuid=?`, state, attempts, due, reason, now, c.pid, issue); err != nil {
		return nil, err
	}
	if c.a["owner_missing"] == true {
		until := layout(c.now.Add(ownerMissingFor))
		if _, err = c.tx.ExecContext(ctx, `INSERT INTO issue_subscribers_owner(project_id,missing_until) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET missing_until=excluded.missing_until`, c.pid, until); err != nil {
			return nil, err
		}
	}
	used, limit, err := c.charge(ctx, lease, calls-charged)
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": state, "budget_used": used, "budget_limit": limit}, nil
}
