package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// A claim marker shows a confirmed assignment on its Linear issue: a claim
// note and the agent-working label at confirm, a close note and the label's
// removal at release. The bus row is the desired state; whichever client
// can (the one that made the transition, else the comment watcher) applies
// it through marker_go and marker_settle, and the rows converge.
const (
	markerCapPerHour = 6
	markerStale      = 10 * time.Minute
	markerMaxOutside = 5
	markerDueLimit   = 20
	// markerDueCalls is what applying one marker row costs, charged up
	// front: one connection reused for the row, one binding proof, one issue
	// read, one label page, the writes and their readbacks.
	markerDueCalls = 8
	// markerStateCalls is what a row's state part adds: the issue read
	// again, the update and its readback.
	markerStateCalls = 3
	// MarkerLabel is the label a claim adds; a Linear admin creates it.
	MarkerLabel = "agent-working"
)

// markerReasons are the only words a close note says about why.
var markerReasons = map[string]string{"self": "done", "coordinator": "released by the coordinator", "replaced": "moved to other work", "access_revoked": "access ended"}

func markerReason(reason string) string {
	if r, ok := markerReasons[reason]; ok {
		return r
	}
	return "session ended"
}

// markerBackoff is the wait before attempt n+1 of a marker part.
func markerBackoff(attempts int) time.Duration {
	switch attempts {
	case 0, 1:
		return time.Minute
	case 2:
		return 2 * time.Minute
	case 3:
		return 5 * time.Minute
	case 4:
		return 15 * time.Minute
	}
	return time.Hour
}

func markerKey(assignment, phase string) string {
	return "marker:" + assignment + ":" + phase + ":comment"
}

// Schema 11 (v0.2.8): the engineer behind the claimed session, frozen at
// confirm so every attempt at a note posts the same body. Older rows have
// none and render as before.
const markerEngineerSchemaV11 = `ALTER TABLE assignment_markers ADD COLUMN engineer TEXT NOT NULL DEFAULT '';`

// markerBody is the exact text a marker comment posts: fixed words, the
// confirm-time session name and engineer, and a short ref for humans
// relating the claim and close notes. It names the session itself, so it carries no generic
// agent marker line; the ledger row is what makes it ClawBridge's.
func markerBody(row map[string]any) string {
	ref := sha256.Sum256([]byte(str(row, "assignment_id")))
	shown := str(row, "session_name")
	if len(shown) > 32 {
		shown = shown[:31] + "…"
	}
	if engineer := str(row, "engineer"); engineer != "" {
		shown += " · " + engineer
	}
	text := "🤖 Claimed by ClawBridge agent session " + shown
	if str(row, "phase") == "close" {
		text = "🤖 Released by ClawBridge agent session " + shown + " (" + markerReason(str(row, "reason")) + ")"
	}
	return text + " · ref cb-" + hex.EncodeToString(ref[:4])
}

// markAssignment writes the claim row when an assignment for an issue is
// confirmed, in the confirm transaction.
func markAssignment(ctx context.Context, tx *transaction, pid, assignment, issue, name, engineer string) error {
	if issue == "" {
		return nil
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	state, revision, err := markerStatePart(ctx, tx, pid, "claim")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO assignment_markers(assignment_id,phase,project_id,issue_uuid,session_name,engineer,reason,comment_state,label_state,state_state,state_policy_revision,next_attempt_at,created_at,updated_at) VALUES(?,'claim',?,?,?,?,'','pending','pending',?,?,?,?,?)`, assignment, pid, issue, name, engineer, state, revision, now, now, now)
	return err
}

// markReleaseDone records that a release finished its work: the close row
// moves the issue to the policy's release_done state, frozen at the
// policy's current revision.
func markReleaseDone(ctx context.Context, tx *transaction, pid, assignment string) error {
	state, revision, err := markerStatePart(ctx, tx, pid, "release_done")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE assignment_markers SET outcome='done',state_state=?,state_policy_revision=? WHERE assignment_id=? AND phase='close' AND project_id=?`, state, revision, assignment, pid)
	return err
}

// closeAssignment writes the close row for a released assignment that had
// a claim, on every release path. It waits on the claim (marker_go).
func closeAssignment(ctx context.Context, tx *transaction, pid, assignment, reason string) error {
	claim, err := query(ctx, tx, `SELECT * FROM assignment_markers WHERE assignment_id=? AND phase='claim' AND project_id=?`, assignment, pid)
	if err != nil || len(claim) == 0 {
		return err
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO assignment_markers(assignment_id,phase,project_id,issue_uuid,session_name,engineer,reason,comment_state,label_state,next_attempt_at,created_at,updated_at) VALUES(?,'close',?,?,?,?,?,'waiting_on_claim','pending',?,?,?)`, assignment, pid, claim[0]["issue_uuid"], claim[0]["session_name"], claim[0]["engineer"], reason, now, now, now)
	return err
}

// markerCall is one marker operation. A watcher passes its lease fence;
// any other session needs project write access, which commentOp checked.
type markerCall struct {
	commentCall
	row          map[string]any
	assignment   string
	phase, part  string
	claim, close map[string]any
}

func (c commentCall) marker(ctx context.Context) (markerCall, error) {
	m := markerCall{commentCall: c, assignment: str(c.a, "assignment_id"), phase: str(c.a, "phase"), part: str(c.a, "part")}
	if _, fenced := c.a["lease_token"]; fenced {
		if _, err := c.fence(ctx); err != nil {
			return m, err
		}
	}
	if m.phase != "claim" && m.phase != "close" {
		return m, invalid("phase must be claim or close")
	}
	if m.part != "comment" && m.part != "label" && m.part != "state" {
		return m, invalid("part must be comment, label or state")
	}
	rows, err := query(ctx, c.tx, `SELECT * FROM assignment_markers WHERE assignment_id=? AND project_id=?`, m.assignment, c.pid)
	if err != nil {
		return m, err
	}
	for _, row := range rows {
		if str(row, "phase") == "claim" {
			m.claim = row
		} else {
			m.close = row
		}
	}
	if m.row = m.claim; m.phase == "close" {
		m.row = m.close
	}
	if m.row == nil {
		return m, scoped("no such marker in this project", "not_found")
	}
	return m, nil
}

func (m markerCall) set(ctx context.Context, phase, column, state string) error {
	_, err := m.tx.ExecContext(ctx, `UPDATE assignment_markers SET `+column+`=?,updated_at=? WHERE assignment_id=? AND phase=?`, state, layout(m.now), m.assignment, phase)
	return err
}

func (m markerCall) ledgerRow(ctx context.Context, phase string) (map[string]any, error) {
	rows, err := query(ctx, m.tx, `SELECT * FROM linear_comment_ledger WHERE workspace_id=? AND operation_digest=?`, m.wid, digestOf("marker", markerKey(m.assignment, phase)))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// claimSettled decides one claim part for its close: whether it will not
// change any more, and whether it may have reached Linear. A part never
// attempted is superseded now, so it can never land after its close; a
// part attempted more than markerStale ago counts as "may be done".
func (m markerCall) claimSettled(ctx context.Context, part string) (settled, reached bool, err error) {
	stale := layout(m.now.Add(-markerStale))
	if part == "comment" {
		switch str(m.claim, "comment_state") {
		case "done":
			return true, true, nil
		case "pending":
			row, err := m.ledgerRow(ctx, "claim")
			if err != nil {
				return false, false, err
			}
			if row == nil || str(row, "state") == "failed" {
				return true, false, m.set(ctx, "claim", "comment_state", "superseded")
			}
			if str(row, "state") == "posted" {
				return true, true, m.set(ctx, "claim", "comment_state", "done")
			}
			return str(row, "reserved_at") <= stale, true, nil
		}
		return true, false, nil
	}
	switch str(m.claim, "label_state") {
	case "done":
		return true, true, nil
	case "in_flight":
		return str(m.claim, "label_in_flight_at") <= stale, true, nil
	case "pending", "no_label":
		return true, false, m.set(ctx, "claim", "label_state", "superseded")
	}
	return true, false, nil
}

// markerGo is the go-ahead for one marker part, decided from bus state in
// one transaction: post a comment (the server-rendered body, reserved in
// the ledger outside the conversational cap), add or remove the label,
// wait, or nothing more to do.
func (c commentCall) markerGo(ctx context.Context) (any, error) {
	m, err := c.marker(ctx)
	if err != nil {
		return nil, err
	}
	column := "comment_state"
	switch m.part {
	case "label":
		column = "label_state"
	case "state":
		column = "state_state"
	}
	state := str(m.row, column)
	out := func(action string) map[string]any {
		return map[string]any{"action": action, "assignment_id": m.assignment, "phase": m.phase, "part": m.part, "issue_uuid": m.row["issue_uuid"], "state": state}
	}
	switch state {
	case "pending", "waiting_on_claim", "no_label", "in_flight":
	default:
		return out("none"), nil
	}
	// The row's one next_attempt_at paces both parts: a backoff or wait on
	// one delays the other by at most an hour, which converges the same.
	if str(m.row, "next_attempt_at") > layout(m.now) {
		return out("wait"), nil
	}
	// wait asks again in a minute, without counting an attempt.
	wait := func() (any, error) {
		_, err := m.tx.ExecContext(ctx, `UPDATE assignment_markers SET next_attempt_at=?,updated_at=? WHERE assignment_id=? AND phase=?`, layout(m.now.Add(time.Minute)), layout(m.now), m.assignment, m.phase)
		return out("wait"), err
	}
	if m.part == "state" {
		return m.stateGo(ctx, out, wait)
	}
	if m.phase == "claim" && m.close != nil {
		// A claim part that never started, or whose attempt failed, cannot
		// land after its close.
		started := state == "in_flight"
		if m.part == "comment" {
			row, e := m.ledgerRow(ctx, "claim")
			if e != nil {
				return nil, e
			}
			started = row != nil && str(row, "state") != "failed"
		}
		if !started {
			state = "superseded"
			return out("none"), m.set(ctx, "claim", column, state)
		}
		// A label add in flight is never sent again once the close exists:
		// past markerStale it counts as done, for the close to remove.
		if m.part == "label" {
			if str(m.row, "label_in_flight_at") <= layout(m.now.Add(-markerStale)) {
				state = "done"
				return out("none"), m.set(ctx, "claim", column, state)
			}
			return wait()
		}
	}
	if m.phase == "close" {
		settled, reached, e := m.claimSettled(ctx, m.part)
		if e != nil {
			return nil, e
		}
		if !settled {
			return wait()
		}
		if m.part == "comment" {
			_, labelReached, e := m.claimSettled(ctx, "label")
			if e != nil {
				return nil, e
			}
			reached = reached || labelReached
		}
		if !reached {
			state = "skipped"
			return out("none"), m.set(ctx, "close", column, state)
		}
	}
	if m.part == "label" {
		return m.labelGo(ctx, out, wait)
	}
	return m.commentGo(ctx, out, wait)
}

func (m markerCall) labelGo(ctx context.Context, out func(string) map[string]any, wait func() (any, error)) (any, error) {
	now := layout(m.now)
	action := "add"
	if m.phase == "claim" {
		// R2-H2: a claim's add waits while a close's removal on the same
		// issue may still land.
		closing, err := query(ctx, m.tx, `SELECT 1 FROM assignment_markers WHERE project_id=? AND issue_uuid=? AND phase='close' AND label_state='in_flight' AND label_in_flight_at>?`, m.pid, m.row["issue_uuid"], layout(m.now.Add(-markerStale)))
		if err != nil {
			return nil, err
		}
		if len(closing) > 0 {
			return wait()
		}
	} else {
		action = "remove"
		others, err := query(ctx, m.tx, `SELECT 1 FROM session_assignments WHERE project_id=? AND issue_uuid=? AND state='confirmed'`, m.pid, m.row["issue_uuid"])
		if err != nil {
			return nil, err
		}
		if len(others) > 0 {
			o := out("none")
			o["state"] = "skipped"
			return o, m.set(ctx, "close", "label_state", "skipped")
		}
	}
	if _, err := m.tx.ExecContext(ctx, `UPDATE assignment_markers SET label_state='in_flight',label_in_flight_at=?,updated_at=? WHERE assignment_id=? AND phase=?`, now, now, m.assignment, m.phase); err != nil {
		return nil, err
	}
	o := out(action)
	o["label"] = MarkerLabel
	return o, nil
}

func (m markerCall) commentGo(ctx context.Context, out func(string) map[string]any, wait func() (any, error)) (any, error) {
	body := markerBody(m.row)
	sum := sha256.Sum256([]byte(body))
	digest := digestOf("marker", markerKey(m.assignment, m.phase))
	now := layout(m.now)
	row, err := m.ledgerRow(ctx, m.phase)
	if err != nil {
		return nil, err
	}
	o := out("post")
	o["body"], o["operation_digest"] = body, digest
	switch {
	case row == nil:
		recent, err := query(ctx, m.tx, `SELECT 1 FROM linear_comment_ledger WHERE workspace_id=? AND issue_uuid=? AND marker_key<>'' AND state<>'failed' AND reserved_at>?`, m.wid, m.row["issue_uuid"], layout(m.now.Add(-time.Hour)))
		if err != nil {
			return nil, err
		}
		if len(recent) >= markerCapPerHour {
			if err = m.set(ctx, m.phase, "comment_state", "skipped"); err != nil {
				return nil, err
			}
			o = out("none")
			o["state"], o["reason"] = "skipped", fmt.Sprintf("at most %d marker comments per issue per hour", markerCapPerHour)
			return o, nil
		}
		_, err = m.tx.ExecContext(ctx, `INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,marker_key,issue_uuid,body_sha256,session_id,principal,state,reserved_at) VALUES(?,?,?,?,?,?,?,?,'pending',?)`, m.wid, m.pid, digest, markerKey(m.assignment, m.phase), m.row["issue_uuid"], hex.EncodeToString(sum[:]), m.sid, m.s.principal, now)
		if err != nil {
			return nil, err
		}
	case str(row, "state") == "posted":
		o = out("none")
		o["state"] = "done"
		return o, m.set(ctx, m.phase, "comment_state", "done")
	case str(row, "state") == "pending" && str(row, "reserved_at") > layout(m.now.Add(-markerStale)):
		// An attempt may be posting it now.
		return wait()
	case str(row, "state") == "pending":
		// An attempt that may have landed: look for the exact body in that
		// attempt's window first. marker_settle re-checks the window, and
		// only "absent" hands out a new post.
		reserved := at(str(row, "reserved_at"))
		o = out("check")
		o["body"], o["operation_digest"] = body, digest
		o["check_from"], o["check_to"] = layout(reserved.Add(-2*time.Minute)), layout(reserved.Add(markerStale))
		return o, nil
	default:
		// A failed attempt is posted again as a new attempt.
		if err = m.reserveAgain(ctx, digest); err != nil {
			return nil, err
		}
	}
	if m.phase == "close" && str(m.row, "comment_state") == "waiting_on_claim" {
		if err = m.set(ctx, "close", "comment_state", "pending"); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// reserveAgain makes the caller's session the holder of a new attempt.
func (m markerCall) reserveAgain(ctx context.Context, digest string) error {
	_, err := m.tx.ExecContext(ctx, `UPDATE linear_comment_ledger SET state='pending',session_id=?,principal=?,reserved_at=?,settled_by=NULL,settled_at=NULL WHERE workspace_id=? AND operation_digest=?`, m.sid, m.s.principal, layout(m.now), m.wid, digest)
	return err
}

// markerSettle records how applying a marker part went: done (a comment
// with the comment it posted or found), no_label, failed (nothing was sent),
// uncertain, or out_of_binding. Anything but done is retried with backoff.
func (c commentCall) markerSettle(ctx context.Context) (any, error) {
	m, err := c.marker(ctx)
	if err != nil {
		return nil, err
	}
	outcome := str(c.a, "outcome")
	now := layout(m.now)
	if m.part == "state" {
		return m.stateSettle(ctx, outcome)
	}
	column := "comment_state"
	if m.part == "label" {
		column = "label_state"
	}
	if m.part == "label" && str(m.row, "label_state") != "in_flight" || m.part == "comment" && str(m.row, "comment_state") != "pending" {
		return nil, scoped("this marker part was not handed out; ask marker_go first", "conflict")
	}
	digest := digestOf("marker", markerKey(m.assignment, m.phase))
	var ledger map[string]any
	if m.part == "comment" {
		if ledger, err = m.ledgerRow(ctx, m.phase); err != nil {
			return nil, err
		}
		if ledger == nil || str(ledger, "state") != "pending" {
			return nil, scoped("this marker comment has no attempt in progress; ask marker_go first", "conflict")
		}
	}
	state := ""
	switch outcome {
	case "done":
		state = "done"
		if m.part == "comment" {
			id, e := lowerUUID(c.a, "comment_id", true)
			if e != nil {
				return nil, e
			}
			via := str(c.a, "via")
			switch via {
			case "body_match":
				// MK-M1: a found comment proves itself only by being
				// created inside the attempt's window; the client found
				// the exact body there.
				created, e := time.Parse(time.RFC3339Nano, str(c.a, "created_at"))
				reserved := at(str(ledger, "reserved_at"))
				if e != nil || created.Before(reserved.Add(-2*time.Minute)) || created.After(reserved.Add(markerStale)) {
					return nil, invalid("the comment is outside the marker attempt's window")
				}
			case "", "receipt":
				via = "receipt"
				// MK-M1: a receipt settles only the attempt this session
				// was handed.
				if str(ledger, "session_id") != c.sid {
					return nil, scoped("this marker attempt was handed to another session", "conflict")
				}
			default:
				return nil, invalid("via must be receipt or body_match")
			}
			if e = c.unclaimed(ctx, id); e != nil {
				return nil, e
			}
			if _, e = c.tx.ExecContext(ctx, `UPDATE linear_comment_ledger SET state='posted',comment_id=?,settled_by=?,settled_at=? WHERE workspace_id=? AND operation_digest=? AND state='pending'`, id, via, now, c.wid, digest); e != nil {
				return nil, e
			}
		}
	case "absent":
		// The check found no earlier post: hand this session a new attempt.
		if m.part != "comment" || str(ledger, "reserved_at") > layout(m.now.Add(-markerStale)) {
			return nil, invalid("absent applies to a checked marker comment")
		}
		if err = m.reserveAgain(ctx, digest); err != nil {
			return nil, err
		}
		o := map[string]any{"action": "post", "assignment_id": m.assignment, "phase": m.phase, "part": m.part, "issue_uuid": m.row["issue_uuid"], "body": markerBody(m.row), "operation_digest": digest}
		return o, nil
	case "no_label":
		if m.part != "label" {
			return nil, invalid("no_label applies to the label part")
		}
		state = "no_label"
	case "failed", "uncertain", "out_of_binding":
		state = "pending"
		// PR3-H1: a label write that may have landed keeps counting as
		// in flight from its attempt, so a close still removes the label and
		// the claim-add fence keeps holding.
		if m.part == "label" && outcome == "uncertain" {
			state = "in_flight"
		}
		if m.part == "comment" && outcome == "failed" {
			if _, err = c.tx.ExecContext(ctx, `UPDATE linear_comment_ledger SET state='failed',settled_by='receipt',settled_at=? WHERE workspace_id=? AND operation_digest=? AND state='pending'`, now, c.wid, digest); err != nil {
				return nil, err
			}
		}
	default:
		return nil, invalid("outcome must be done, absent, no_label, failed, uncertain or out_of_binding")
	}
	attempts := asInt(m.row["attempts"])
	next := now
	if state != "done" {
		attempts++
		wait := markerBackoff(attempts)
		if state == "no_label" {
			wait = time.Hour
		}
		next = layout(m.now.Add(wait))
		if outcome == "out_of_binding" && attempts >= markerMaxOutside {
			state = "failed_out_of_binding"
		}
	}
	failure := str(c.a, "error")
	if len(failure) > 200 {
		failure = failure[:200]
	}
	if _, err = c.tx.ExecContext(ctx, `UPDATE assignment_markers SET `+column+`=?,attempts=?,next_attempt_at=?,last_error=?,updated_at=? WHERE assignment_id=? AND phase=?`, state, attempts, next, failure, now, m.assignment, m.phase); err != nil {
		return nil, err
	}
	return map[string]any{"assignment_id": m.assignment, "phase": m.phase, "part": m.part, "state": state, "next_attempt_at": next}, nil
}

// markerDue hands the watcher the marker rows to apply, charging their
// calls to the lease budget first: markers come before comment polls.
func (c commentCall) markerDue(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	used, limit, err := c.charge(ctx, lease, 0)
	if err != nil {
		return nil, err
	}
	rows, err := query(ctx, c.tx, `SELECT assignment_id,phase,issue_uuid,state_state FROM assignment_markers WHERE project_id=? AND next_attempt_at<=? AND (comment_state IN ('pending','waiting_on_claim') OR label_state IN ('pending','in_flight','no_label') OR state_state IN ('pending','in_flight')) ORDER BY next_attempt_at,assignment_id,phase LIMIT ?`, c.pid, layout(c.now), markerDueLimit)
	if err != nil {
		return nil, err
	}
	markers := []any{}
	cost := 0
	for _, row := range rows {
		calls := markerDueCalls
		if s := str(row, "state_state"); s == "pending" || s == "in_flight" {
			calls += markerStateCalls
		}
		if used+cost+calls > limit {
			break
		}
		cost += calls
		markers = append(markers, map[string]any{"assignment_id": row["assignment_id"], "phase": row["phase"], "issue_uuid": row["issue_uuid"]})
	}
	if len(markers) > 0 {
		if _, _, err = c.charge(ctx, map[string]any{"budget_used": used, "budget_hour": layout(c.now)[:13], "budget_limit": limit}, cost); err != nil {
			return nil, err
		}
	}
	return map[string]any{"markers": markers}, nil
}

// watchStatus is what doctor reports about the project's comment watch and
// claim markers. It reads only, and names no Linear text.
func watchStatus(ctx context.Context, tx *transaction, pid string) (any, error) {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	stale := layout(at(nowRaw).Add(-markerStale))
	count := func(q string, args ...any) (int, error) {
		rows, err := query(ctx, tx, `SELECT COUNT(*) AS n FROM `+q, args...)
		if err != nil || len(rows) == 0 {
			return 0, err
		}
		return asInt(rows[0]["n"]), nil
	}
	out := map[string]any{}
	for key, q := range map[string]string{
		"watched_issues":         `watch_cursors WHERE project_id=? AND source='comments' AND active_until IS NULL`,
		"held_issues":            `watch_cursors WHERE project_id=? AND source='comments' AND active_until IS NULL AND last_error<>''`,
		"no_recipient":           `watch_cursors WHERE project_id=? AND source='comments' AND active_until IS NULL AND last_error='no_recipient'`,
		"markers_pending":        `assignment_markers WHERE project_id=? AND created_at<? AND (comment_state IN ('pending','waiting_on_claim') OR label_state IN ('pending','in_flight'))`,
		"markers_no_label":       `assignment_markers WHERE project_id=? AND label_state='no_label'`,
		"markers_out_of_binding": `assignment_markers WHERE project_id=? AND (comment_state='failed_out_of_binding' OR label_state='failed_out_of_binding')`,
		// A close whose removal has not landed while nothing confirmed names
		// the issue: Linear may still show agent-working.
		"close_label_drift": `assignment_markers m WHERE m.project_id=? AND m.phase='close' AND m.label_state IN ('pending','in_flight','no_label','failed_out_of_binding') AND m.created_at<? AND NOT EXISTS(SELECT 1 FROM session_assignments a WHERE a.project_id=m.project_id AND a.issue_uuid=m.issue_uuid AND a.state='confirmed')`,
	} {
		args := []any{pid}
		if key == "markers_pending" || key == "close_label_drift" {
			args = append(args, stale)
		}
		n, err := count(q, args...)
		if err != nil {
			return nil, err
		}
		out[key] = n
	}
	oldest, err := query(ctx, tx, `SELECT MIN(created_at) AS at FROM assignment_markers WHERE project_id=? AND created_at<? AND (comment_state IN ('pending','waiting_on_claim') OR label_state IN ('pending','in_flight'))`, pid, stale)
	if err != nil {
		return nil, err
	}
	out["markers_oldest_pending"] = nullable(str(oldest[0], "at"))
	lease, err := query(ctx, tx, `SELECT expires_at FROM watch_lease WHERE project_id=?`, pid)
	if err != nil {
		return nil, err
	}
	out["watcher_live"] = len(lease) == 1 && str(lease[0], "expires_at") > nowRaw
	return out, nil
}
