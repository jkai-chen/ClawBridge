package bus

import (
	"context"
	"encoding/json"
	"time"
)

// The owner's own model setup, as the CLI and TUI read it: the config key
// rotation sends again (dispatcher_model_config, §11.6), and the model's
// lines under dispatcher show (§11.5). Neither ever returns the key or
// anything derived from it.

// modelConfig is the caller's own active and pending setups, each nil when
// there is none.
func (s *Store) modelConfig(ctx context.Context, tx *transaction) (any, error) {
	rows, err := query(ctx, tx, `SELECT state,provider_id,api,base_url,model,caps,egress_text_version,config_revision,created_at FROM dispatcher_model_keys WHERE owner_principal=?`, s.principal)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"active": nil, "pending": nil}
	for _, r := range rows {
		var caps any
		_ = json.Unmarshal([]byte(str(r, "caps")), &caps)
		config := map[string]any{"provider_id": str(r, "provider_id"), "api": str(r, "api"), "base_url": str(r, "base_url"), "model": str(r, "model"),
			"caps": caps, "egress_text_version": str(r, "egress_text_version"), "config_revision": asInt(r["config_revision"]),
			"created_at": at(str(r, "created_at")).Format(time.RFC3339)}
		switch str(r, "state") {
		case "active":
			out["active"] = config
		case "pending_test":
			out["pending"] = config
		}
	}
	return out, nil
}

// ownerModelView is what dispatcher_get adds for a project owner: the
// project's model use (off, engine or advisor), and the owner's own
// provider and model (the active key's, else the pending one's), key state
// and agent, as doctor tells them.
func (s *Store) ownerModelView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	use, _, err := projectModelUse(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if use == "" {
		use = "off"
	}
	out := map[string]any{"use": use, "provider_id": "", "model": "", "key": "none", "agent": "never_seen"}
	// active sorts before pending_test.
	keys, err := query(ctx, tx, `SELECT state,provider_id,model FROM dispatcher_model_keys WHERE owner_principal=? ORDER BY state`, s.principal)
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		out["provider_id"], out["model"], out["key"] = str(keys[0], "provider_id"), str(keys[0], "model"), str(keys[0], "state")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	var last *string
	if err = tx.QueryRowContext(ctx, `SELECT MAX(last_used_at) FROM dispatcher_model_tokens WHERE created_by_principal=? AND project_id=?`, s.principal, pid).Scan(&last); err != nil {
		return nil, err
	}
	if last != nil {
		out["agent"] = "offline"
		if *last >= layout(at(nowRaw).Add(-modelSeenWindow)) {
			out["agent"] = "running"
		}
	}
	return out, nil
}
