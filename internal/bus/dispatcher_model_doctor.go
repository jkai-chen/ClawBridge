package bus

import (
	"context"
	"time"
)

const (
	// ModelAgentMemory is one agent container's memory limit (§5.1): the
	// headroom doctor wants per agent slot not yet running.
	ModelAgentMemory = 160 << 20
	// launcherReportFresh is how long agentd's report stays news for doctor.
	launcherReportFresh = time.Hour
	// credentialFlagFresh is how long a flagged credential stays news.
	credentialFlagFresh = 7 * 24 * time.Hour
	// launcherExpiryWarn is how early doctor warns of the launcher token's
	// expiry.
	launcherExpiryWarn = 14 * 24 * time.Hour
)

// SetModelHost tells doctor what only the serving process knows: the cap on
// live agents, and the host's available memory (ok false when unknown).
func (s *Store) SetModelHost(maxAgents int, memAvailable func() (uint64, bool)) {
	s.modelMaxAgents, s.memAvailable = maxAgents, memAvailable
}

// modelDoctor is what doctor reports about the project's model mode
// (§11.5), for a project owner. It reads only, and never the key: the seal
// keys as set or not, each key-holder who owns the project, agentd's
// heartbeat and its token's expiry, and the live agents against the cap.
// Times are ages in seconds on the server's clock.
func (s *Store) modelDoctor(ctx context.Context, tx *transaction, w, p map[string]any, nowRaw string) (any, error) {
	now, pid := at(nowRaw), str(p, "id")
	ago := func(stamp string) any {
		if stamp == "" {
			return nil
		}
		return int64(now.Sub(at(stamp)).Seconds())
	}
	out := map[string]any{
		"seal_configured": s.modelSeal != nil,
		"seal_previous":   s.modelSeal != nil && s.modelSeal.previous != nil,
	}
	policy, err := query(ctx, tx, `SELECT enabled,mode FROM dispatcher_policies WHERE project_id=?`, pid)
	if err != nil {
		return nil, err
	}
	out["model_on"] = len(policy) == 1 && asInt(policy[0]["enabled"]) == 1 && str(policy[0], "mode") == "model"
	// A local store has no project roles: its workspace owner owns all.
	owns, args := `k.owner_principal=?`, []any{str(w, "owner_principal")}
	if tx.postgres {
		owns, args = `k.owner_principal=? OR EXISTS(SELECT 1 FROM project_roles r WHERE r.project_id=? AND r.principal=k.owner_principal AND r.role='owner')`, append(args, pid)
	}
	keyed, err := query(ctx, tx, `SELECT DISTINCT k.owner_principal FROM dispatcher_model_keys k WHERE `+owns+` ORDER BY k.owner_principal`, args...)
	if err != nil {
		return nil, err
	}
	seenCutoff := layout(now.Add(-modelSeenWindow))
	holders := []any{}
	for _, row := range keyed {
		owner := str(row, "owner_principal")
		h := map[string]any{"principal": owner, "you": owner == s.principal, "key": "pending_test"}
		keys, err := query(ctx, tx, `SELECT state,config_revision FROM dispatcher_model_keys WHERE owner_principal=? ORDER BY state`, owner)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			if str(k, "state") == "active" {
				h["key"], h["config_revision"] = "active", asInt(k["config_revision"])
			} else {
				h["pending_revision"] = asInt(k["config_revision"])
			}
		}
		st, err := query(ctx, tx, `SELECT status,launcher_report,report_at,test_revision,test_failure FROM dispatcher_agent_status WHERE owner_principal=?`, owner)
		if err != nil {
			return nil, err
		}
		if len(st) == 1 {
			h["agent_status"] = str(st[0], "status")
			if str(st[0], "launcher_report") != "" && str(st[0], "report_at") > layout(now.Add(-launcherReportFresh)) {
				h["launcher_report"], h["launcher_report_age"] = str(st[0], "launcher_report"), ago(str(st[0], "report_at"))
			}
			// A failure counts only for a revision newer than the active key.
			if f := str(st[0], "test_failure"); f != "" && asInt(st[0]["test_revision"]) > asInt(h["config_revision"]) {
				h["test_failure"], h["test_revision"] = f, asInt(st[0]["test_revision"])
			}
		}
		var last *string
		if err = tx.QueryRowContext(ctx, `SELECT MAX(last_used_at) FROM dispatcher_model_tokens WHERE created_by_principal=? AND project_id=?`, owner, pid).Scan(&last); err != nil {
			return nil, err
		}
		h["agent"], h["last_seen_age"] = "offline", nil
		if last != nil {
			h["last_seen_age"] = ago(*last)
			if *last >= seenCutoff {
				h["agent"] = "running"
			}
		}
		flags, err := query(ctx, tx, `SELECT flagged FROM dispatcher_agent_credentials WHERE owner_principal=? AND flagged<>'' AND created_at>? ORDER BY created_at DESC`, owner, layout(now.Add(-credentialFlagFresh)))
		if err != nil {
			return nil, err
		}
		if len(flags) > 0 {
			h["credential_flag"] = str(flags[0], "flagged")
		}
		holders = append(holders, h)
	}
	out["holders"] = holders
	// agentd: the live launcher token it used last, its heartbeat and expiry.
	tokens, err := query(ctx, tx, `SELECT created_at,last_used_at FROM dispatcher_launcher_tokens WHERE revoked_at IS NULL AND created_at>? ORDER BY created_at`, layout(now.Add(-LauncherTokenTTL)))
	if err != nil {
		return nil, err
	}
	launcher := map[string]any{"tokens": len(tokens), "reporting": false, "last_used_age": nil, "expires_in": nil, "expiring": false}
	var used map[string]any
	for _, t := range tokens {
		if str(t, "last_used_at") != "" && (used == nil || str(t, "last_used_at") > str(used, "last_used_at")) {
			used = t
		}
	}
	if used != nil {
		expires := at(str(used, "created_at")).Add(LauncherTokenTTL)
		launcher["reporting"] = str(used, "last_used_at") >= seenCutoff
		launcher["last_used_age"] = ago(str(used, "last_used_at"))
		launcher["expires_in"] = int64(expires.Sub(now).Seconds())
		launcher["expiring"] = expires.Sub(now) < launcherExpiryWarn
	}
	out["launcher"] = launcher
	live, err := query(ctx, tx, `SELECT COUNT(DISTINCT created_by_principal) AS n FROM dispatcher_model_tokens WHERE last_used_at>=? AND revoked_at IS NULL AND expires_at>?`, seenCutoff, nowRaw)
	if err != nil {
		return nil, err
	}
	capacity := map[string]any{"live_agents": asInt(live[0]["n"]), "max_agents": s.modelMaxAgents, "agent_memory": ModelAgentMemory, "mem_available": nil}
	if s.memAvailable != nil {
		if m, ok := s.memAvailable(); ok {
			capacity["mem_available"] = m
		}
	}
	out["capacity"] = capacity
	return out, nil
}
