package bus

import (
	"context"
	"errors"
	"strings"
)

// `operator health-run` (v0.3.5): the host operator forces one health run
// of a bound Linear project, so a posting smoke can see a post now, and
// deletes the post such a run made, by its own ID only. These are the
// operator's own reads and records; the run itself is the watcher's, under
// the project's watch lease.

// HealthPostDeleteCalls is what an operator's delete of a forced post
// spends: the delete and the read-back.
const HealthPostDeleteCalls = 2

// operatorTx opens a short operator transaction, bounded on Postgres.
func (s *Store) operatorTx(ctx context.Context) (*transaction, error) {
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("authority unavailable")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	if s.postgres {
		for _, set := range []string{`SET LOCAL statement_timeout='5s'`, `SET LOCAL lock_timeout='5s'`} {
			if _, err = tx.ExecContext(ctx, set); err != nil {
				tx.Rollback()
				return nil, errors.New("authority unavailable")
			}
		}
	}
	return tx, nil
}

// OperatorHealthProject is the ClawBridge project whose health is enabled
// for linearProject. One Linear project enabled in two ClawBridge projects
// is refused: the operator could not tell which one runs.
func (s *Store) OperatorHealthProject(ctx context.Context, linearProject string) (string, error) {
	lp, err := lowerUUID(map[string]any{"linear_project": linearProject}, "linear_project", true)
	if err != nil {
		return "", err
	}
	tx, err := s.operatorTx(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT project_id FROM project_health WHERE linear_project_id=? AND enabled=1 ORDER BY project_id`, lp)
	if err != nil {
		return "", errors.New("authority unavailable")
	}
	switch len(rows) {
	case 0:
		return "", errors.New("project health is not enabled for that Linear project")
	case 1:
		return str(rows[0], "project_id"), nil
	}
	return "", errors.New("that Linear project's health is enabled in more than one ClawBridge project; disable all but one")
}

// HealthPostRef is a post a forced health run made, as the operator's
// delete finds it.
type HealthPostRef struct {
	ProjectID     string
	LinearProject string
	Day           string
	UpdateID      string
}

// OperatorForcedHealthPost finds the post with updateID that a forced run
// made on linearProject and Linear accepted, and that no delete recorded
// yet. Any other update is refused: the operator deletes only its own.
func (s *Store) OperatorForcedHealthPost(ctx context.Context, linearProject, updateID string) (HealthPostRef, error) {
	ids, err := lowerUUIDs(linearProject, updateID)
	if err != nil {
		return HealthPostRef{}, err
	}
	tx, err := s.operatorTx(ctx)
	if err != nil {
		return HealthPostRef{}, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT project_id,day,error FROM health_posts WHERE linear_project_id=? AND update_id=? AND act='forced' AND state='accepted'`, ids[0], ids[1])
	if err != nil {
		return HealthPostRef{}, errors.New("authority unavailable")
	}
	if len(rows) != 1 {
		return HealthPostRef{}, errors.New("that update is not a post a forced health run made on that Linear project; nothing was deleted")
	}
	if str(rows[0], "error") == "deleted" {
		return HealthPostRef{}, errors.New("that post's delete is already recorded; nothing was deleted")
	}
	return HealthPostRef{ProjectID: str(rows[0], "project_id"), LinearProject: ids[0], Day: str(rows[0], "day"), UpdateID: ids[1]}, nil
}

// OperatorForcedHealthPostDeleted records that the operator deleted post
// from Linear: its row keeps state accepted with error "deleted", the
// calls are charged to the project's watch budget, and it is audited.
func (s *Store) OperatorForcedHealthPostDeleted(ctx context.Context, post HealthPostRef) error {
	tx, err := s.operatorTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := one(ctx, tx, `SELECT workspace_id FROM projects WHERE id=?`, post.ProjectID)
	if err != nil {
		return errors.New("authority unavailable")
	}
	// The same lock as every system call, so the watcher's charge can't
	// overwrite this one.
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+str(p, "workspace_id")); err != nil {
			return errors.New("authority unavailable")
		}
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE health_posts SET error='deleted',settled_at=? WHERE project_id=? AND linear_project_id=? AND day=? AND update_id=? AND act='forced' AND state='accepted' AND error<>'deleted'`, nowRaw, post.ProjectID, post.LinearProject, post.Day, post.UpdateID)
	if err != nil {
		return errors.New("authority unavailable")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("that post's row changed meanwhile; its delete is not recorded")
	}
	hour := nowRaw[:13]
	charged, err := tx.ExecContext(ctx, `UPDATE watch_lease SET budget_used=CASE WHEN budget_hour=? THEN budget_used+? ELSE ? END,budget_people=CASE WHEN budget_hour=? THEN budget_people ELSE 0 END,budget_agents=CASE WHEN budget_hour=? THEN budget_agents ELSE 0 END,budget_hour=? WHERE project_id=?`, hour, HealthPostDeleteCalls, HealthPostDeleteCalls, hour, hour, hour, post.ProjectID)
	if err != nil {
		return errors.New("authority unavailable")
	}
	// Its kind's row, only when the hour was charged: a project no watcher
	// ever ran for has no lease row to charge.
	if n, _ := charged.RowsAffected(); n == 1 {
		if err = recordCallKind(ctx, tx, post.ProjectID, hour, "operator.health", HealthPostDeleteCalls); err != nil {
			return errors.New("authority unavailable")
		}
	}
	scoped := *s
	scoped.principal = OperatorPrincipal
	if err = scoped.audit(ctx, tx, str(p, "workspace_id"), post.ProjectID, "", "project_health", post.LinearProject, "post_deleted", map[string]any{"day": post.Day, "update_id": post.UpdateID}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return errors.New("authority commit failed")
	}
	return nil
}

// lowerUUIDs checks each of ids is a UUID and lowers it.
func lowerUUIDs(ids ...string) ([]string, error) {
	out := []string{}
	for _, id := range ids {
		v, err := lowerUUID(map[string]any{"id": id}, "id", true)
		if err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(v))
	}
	return out, nil
}
