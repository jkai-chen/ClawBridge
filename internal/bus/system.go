package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Each project has one system session, named clawbridge, that the server's
// own watcher acts as (live/INTAKE-DESIGN.md §2). It holds the watch lease
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
func ensureSystemSession(ctx context.Context, tx *transaction, pid string) error {
	rows, err := query(ctx, tx, `SELECT 1 FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
	if err != nil || len(rows) > 0 {
		return err
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

// ensureSystemSessions gives every project its system session at the
// schema-11 migration. A session a person already named clawbridge keeps
// working under a new name.
func ensureSystemSessions(ctx context.Context, tx *transaction) error {
	taken, err := query(ctx, tx, `SELECT id,project_id FROM sessions WHERE lower(name)=? AND role<>'system'`, SystemSessionName)
	if err != nil {
		return err
	}
	for i, row := range taken {
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET name=?,updated_at=? WHERE id=?`, fmt.Sprintf("clawbridge-user-%d", i+1), stamp(), row["id"]); err != nil {
			return err
		}
	}
	projects, err := query(ctx, tx, `SELECT id FROM projects ORDER BY id`)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err = ensureSystemSession(ctx, tx, str(p, "id")); err != nil {
			return err
		}
	}
	return nil
}

// systemOperation is what the system session may run: the reads and
// writes of the comment watch, claim markers and the task board binding.
func systemOperation(op string) bool {
	return commentOperation(op) || op == "taskboard_get_v2" || op == "taskboard_access"
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
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND role='system' AND name=?`, projectID, SystemSessionName)
	if err != nil {
		return Result{}, classify(scopedError(err, "the project has no system session", "not_found"))
	}
	tx.actorID = str(actor, "id")
	scoped := *s
	scoped.principal = systemPrincipal
	var out any
	switch r.Operation {
	case "taskboard_get_v2":
		out, err = scoped.taskboard(ctx, tx, wid, projectID, "owner", r.Operation, r.Args)
	case "taskboard_access":
		out = accessMap(TaskboardAccess{WorkspaceID: wid, ProjectID: projectID, SessionID: str(actor, "id"), SessionName: SystemSessionName, CanWrite: true, ProjectRole: "owner"})
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
