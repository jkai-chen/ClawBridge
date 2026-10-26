package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Each project has one system session, named clawbridge, that the server's
// own watcher acts as (live/INTAKE-DESIGN.md §2). It is created when the
// watcher first acts for the project, so no migration touches sessions. It holds the watch lease
// and sends the watcher's notices. No request reaches it: its owner
// principal is empty, which no caller's principal can equal, every session
// address excludes it, and its name is reserved.
const (
	SystemSessionName = "clawbridge"
	systemRole        = "system"
	// systemPrincipal is what the system session's audit rows name.
	systemPrincipal = "system:clawbridge"
)

// ensureSystemSession creates a project's system session if it has none.
// A session a person named clawbridge before the name was reserved keeps
// working under a new name.
func (s *Store) ensureSystemSession(ctx context.Context, tx *transaction, wid, pid string) error {
	rows, err := query(ctx, tx, `SELECT 1 FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
	if err != nil || len(rows) > 0 {
		return err
	}
	taken, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND lower(name)=? AND role<>'system'`, pid, SystemSessionName)
	if err != nil {
		return err
	}
	// W-L3: a person's session holding the reserved name moves to the first
	// free clawbridge-user-N, and the rename is in history.
	n := 0
	for _, row := range taken {
		name := ""
		for name == "" {
			n++
			candidate := fmt.Sprintf("clawbridge-user-%d", n)
			used, err := query(ctx, tx, `SELECT 1 FROM sessions WHERE project_id=? AND lower(name)=?`, pid, candidate)
			if err != nil {
				return err
			}
			if len(used) == 0 {
				name = candidate
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET name=?,updated_at=? WHERE id=?`, name, stamp(), row["id"]); err != nil {
			return err
		}
		if err = s.audit(ctx, tx, wid, pid, "", "session", str(row, "id"), "renamed", map[string]any{"from": str(row, "name"), "to": name, "reason": "reserved_name"}); err != nil {
			return err
		}
	}
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return err
	}
	now := stamp()
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,project_id,name,instance,owner_principal,role,agent_kind,model,host,account,transport,notes,status,last_seen,created_at,updated_at) VALUES(?,?,?,?,'',?,'system','','','','{}','ClawBridge server watcher','active',?,?,?)`,
		"sys_"+strings.TrimPrefix(pid, "prj_"), pid, SystemSessionName, hex.EncodeToString(b[:]), systemRole, now, now, now)
	return err
}

// systemOperation is what the system session may run: the reads and
// writes of the comment watch, claim markers, project health and the task
// board binding.
func systemOperation(op string) bool {
	return commentOperation(op) || op == "taskboard_get_v2" || op == "taskboard_get" || op == "taskboard_access" || op == "status_policy_get" || healthSystemOperation(op) || op == "marker_spent" || docWatchOperation(op)
}

// ExecuteSystem runs one bus operation as a project's system session. It
// is a Go call for the server's own watcher only; no request path reaches
// it.
func (s *Store) ExecuteSystem(ctx context.Context, projectID string, r Request) (Result, error) {
	if !systemOperation(r.Operation) {
		return Result{}, fmt.Errorf("unsupported system operation %q", r.Operation)
	}
	if r.Args == nil {
		r.Args = map[string]any{}
	}
	if err := validateArgs(r.Operation, r.Args); err != nil {
		return Result{}, classify(invalid(err.Error()))
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	p, err := one(ctx, tx, `SELECT * FROM projects WHERE id=?`, projectID)
	if err != nil {
		return Result{}, classify(scopedError(err, "project not found", "not_found"))
	}
	wid := str(p, "workspace_id")
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+wid); err != nil {
			return Result{}, errors.New("authority unavailable")
		}
	}
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, wid)
	if err != nil {
		return Result{}, err
	}
	scoped := *s
	scoped.principal = systemPrincipal
	if err = scoped.ensureSystemSession(ctx, tx, wid, projectID); err != nil {
		return Result{}, err
	}
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND role='system' AND name=?`, projectID, SystemSessionName)
	if err != nil {
		return Result{}, classify(scopedError(err, "the project has no system session", "not_found"))
	}
	tx.actorID = str(actor, "id")
	var out any
	switch r.Operation {
	case "taskboard_get_v2", "taskboard_get":
		out, err = scoped.taskboard(ctx, tx, wid, projectID, "owner", r.Operation, r.Args)
	case "status_policy_get":
		out, err = statusPolicyView(ctx, tx, projectID)
	case "taskboard_access":
		out = accessMap(TaskboardAccess{WorkspaceID: wid, ProjectID: projectID, SessionID: str(actor, "id"), SessionName: SystemSessionName, CanWrite: true, ProjectRole: "owner"})
	case "health_due", "health_force", "health_report", "health_post_settle", "project_flow_settle":
		out, err = scoped.healthSystemOp(ctx, tx, w, p, actor, r)
	case "doc_watch_due", "doc_watch_report", "doc_verify_report":
		out, err = scoped.docWatchOp(ctx, tx, w, p, actor, r)
	default:
		out, err = scoped.commentOp(ctx, tx, w, p, "owner", actor, r)
	}
	if err != nil {
		return Result{}, classify(err)
	}
	if err = tx.Commit(); err != nil {
		return Result{}, errors.New("authority commit failed")
	}
	return Result{Source: "server", Data: out}, nil
}

// systemTx opens a write transaction on a project's workspace lock.
func (s *Store) systemTx(ctx context.Context, pid string) (*transaction, map[string]any, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, nil, err
	}
	p, err := one(ctx, tx, `SELECT * FROM projects WHERE id=?`, pid)
	if err != nil {
		tx.Rollback()
		return nil, nil, classify(scopedError(err, "project not found", "not_found"))
	}
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+str(p, "workspace_id")); err != nil {
			tx.Rollback()
			return nil, nil, errors.New("authority unavailable")
		}
	}
	return tx, p, nil
}

// WatchProjects lists up to limit projects with a task board binding, the
// longest since their last tick first, so ticks rotate over projects
// (IN-M1).
func (s *Store) WatchProjects(ctx context.Context, limit int) ([]string, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT p.id FROM projects p JOIN taskboard_bindings b ON b.project_id=p.id LEFT JOIN watch_ticks t ON t.project_id=p.id WHERE b.mapping IS NOT NULL ORDER BY COALESCE(t.last_tick_at,''),p.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, str(r, "id"))
	}
	return out, nil
}

// WatchProjectID is the ID of the project a request named, by workspace
// ID or name and project ID or slug, when it is bound to a Linear board:
// the project a nudge serves. "" when there is none.
func (s *Store) WatchProjectID(ctx context.Context, workspace, project string) (string, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id JOIN taskboard_bindings b ON b.project_id=p.id WHERE (w.id=? OR w.name=?) AND (p.id=? OR p.slug=?) AND b.mapping IS NOT NULL`, workspace, workspace, project, project)
	if err != nil || len(rows) != 1 {
		return "", err
	}
	return str(rows[0], "id"), nil
}

// WatchTicked records a project's tick, and when it succeeded.
func (s *Store) WatchTicked(ctx context.Context, pid string, ok bool, failure string) error {
	tx, _, err := s.systemTx(ctx, pid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	if len(failure) > 200 {
		failure = failure[:200]
	}
	okAt := any(nil)
	if ok {
		okAt = now
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO watch_ticks(project_id,last_tick_at,last_ok_at,last_error) VALUES(?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET last_tick_at=excluded.last_tick_at,last_ok_at=COALESCE(excluded.last_ok_at,watch_ticks.last_ok_at),last_error=excluded.last_error`, pid, now, okAt, failure); err != nil {
		return err
	}
	return tx.Commit()
}

// IntakeState is what the server's intake watcher needs of a project: its
// policy, its trusted Linear users, and its intake source cursors.
type IntakeState struct {
	Mode    string
	Signals []string
	Trusted map[string]bool
	// Cursors maps "source|subject" to the cursor time and whether the
	// source is due now.
	Cursors map[string]IntakeCursor
	Now     string
}

// IntakeCursor is one intake source's place: the time its last complete
// read covered, and whether it is due.
type IntakeCursor struct {
	Time string
	Due  bool
}

// IntakeSystemState reads a project's intake state for the server watcher.
func (s *Store) IntakeSystemState(ctx context.Context, pid string) (IntakeState, error) {
	tx, _, err := s.systemTx(ctx, pid)
	if err != nil {
		return IntakeState{}, err
	}
	defer tx.Rollback()
	policy, err := intakePolicy(ctx, tx, pid)
	if err != nil {
		return IntakeState{}, err
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return IntakeState{}, err
	}
	st := IntakeState{Mode: str(policy, "mode"), Signals: policy["signals"].([]string), Trusted: map[string]bool{}, Cursors: map[string]IntakeCursor{}, Now: now}
	trusted, err := query(ctx, tx, `SELECT linear_user_id FROM intake_trusted WHERE project_id=?`, pid)
	if err != nil {
		return IntakeState{}, err
	}
	for _, r := range trusted {
		st.Trusted[str(r, "linear_user_id")] = true
	}
	cursors, err := query(ctx, tx, `SELECT source,subject,cursor_time,next_poll_at FROM watch_cursors WHERE project_id=? AND source LIKE 'intake-%'`, pid)
	if err != nil {
		return IntakeState{}, err
	}
	for _, r := range cursors {
		st.Cursors[str(r, "source")+"|"+str(r, "subject")] = IntakeCursor{Time: str(r, "cursor_time"), Due: str(r, "next_poll_at") <= now}
	}
	return st, nil
}

// IntakeCursorSet records an intake source's complete read up to polled
// (the read's start). It polls again in 5 minutes while it saw a signal in
// the last hour, otherwise in 15 (IN-M4). A new source starts at polled:
// history never triggers.
func (s *Store) IntakeCursorSet(ctx context.Context, pid, source, subject, polled string, signal bool) error {
	if !strings.HasPrefix(source, "intake-") {
		return invalid("not an intake source")
	}
	tx, _, err := s.systemTx(ctx, pid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := query(ctx, tx, `SELECT last_activity_at FROM watch_cursors WHERE project_id=? AND source=? AND subject=?`, pid, source, subject)
	if err != nil {
		return err
	}
	activity := ""
	if len(rows) == 1 {
		activity = str(rows[0], "last_activity_at")
	}
	if signal {
		activity = now
	}
	wait := 15 * time.Minute
	if activity != "" && activity > layout(at(now).Add(-time.Hour)) {
		wait = 5 * time.Minute
	}
	next := layout(at(now).Add(wait))
	if len(rows) == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,last_activity_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, pid, source, subject, polled, polled, next, nullable(activity), now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE watch_cursors SET cursor_time=?,next_poll_at=?,last_activity_at=?,failures=0,last_error='',updated_at=? WHERE project_id=? AND source=? AND subject=?`, polled, next, nullable(activity), now, pid, source, subject)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// IntakeEnqueueSystem queues one event from the server's intake watcher.
// A transition at or before its issue's closed row's close counts only
// after a rearm (§3.5, IN2-L2).
func (s *Store) IntakeEnqueueSystem(ctx context.Context, pid string, ev IntakeEvent) (string, error) {
	tx, p, err := s.systemTx(ctx, pid)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	policy, err := intakePolicy(ctx, tx, pid)
	if err != nil {
		return "", err
	}
	if str(policy, "mode") == "off" || !containsString(policy["signals"].([]string), ev.Signal) {
		return "intake_off", nil
	}
	rows, err := query(ctx, tx, `SELECT state,closed_at,rearmed FROM intake_queue WHERE project_id=? AND issue_uuid=?`, pid, ev.Issue.UUID)
	if err != nil {
		return "", err
	}
	if len(rows) == 1 && rows[0]["closed_at"] != nil && asInt(rows[0]["rearmed"]) == 0 {
		// Linear's and the bus's time formats differ: compare instants.
		happened, e := time.Parse(time.RFC3339Nano, ev.At)
		if e != nil || !happened.After(at(str(rows[0], "closed_at"))) {
			return "stale", nil
		}
	}
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, str(p, "workspace_id"))
	if err != nil {
		return "", err
	}
	result, err := s.intakeEnqueue(ctx, tx, w, pid, policy, ev)
	if err != nil {
		return "", err
	}
	return result, tx.Commit()
}
