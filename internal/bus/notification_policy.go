package bus

import (
	"context"
	"errors"
)

func (s *Store) setNotificationPolicy(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	policy := str(a, "policy")
	if policy != "accept" && policy != "hold" && policy != "refuse" {
		return nil, errors.New("policy must be accept, hold or refuse")
	}
	sid := actor["id"].(string)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET notification_policy=?,updated_at=? WHERE project_id=? AND id=?`, policy, stamp(), pid, sid); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, tx, wid, pid, sid, "session", sid, "notification_policy", map[string]any{"old": actor["notification_policy"], "new": policy}); err != nil {
		return nil, err
	}
	return map[string]any{"workspace_id": wid, "project_id": pid, "session_id": sid, "policy": policy}, nil
}
