package bus

import "context"

// inboxStatus intentionally performs no delivery, read, audit or heartbeat writes.
func (s *Store) inboxStatus(ctx context.Context, tx *transaction, wid, pid, sid string, args map[string]any) (any, error) {
	after, err := cursor(args)
	if err != nil {
		return nil, err
	}
	var unread, latest, fresh int64
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(m.sequence),0),COALESCE(SUM(CASE WHEN m.sequence>? THEN 1 ELSE 0 END),0) FROM messages m JOIN deliveries d ON d.project_id=m.project_id AND d.message_id=m.id WHERE d.project_id=? AND d.recipient_id=? AND d.read_at IS NULL AND m.expires_at>?`, after, pid, sid, stamp()).Scan(&unread, &latest, &fresh)
	if err != nil {
		return nil, err
	}
	return map[string]any{"workspace_id": wid, "project_id": pid, "session_id": sid, "unread_count": unread, "latest_sequence": latest, "new_count": fresh}, nil
}
