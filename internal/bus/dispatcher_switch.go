package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Turning the dispatcher on and off (live/DISPATCHER-DESIGN.md §12.1, §13
// and the enable amendment). Only the project owner does it, as a person:
// a session never can, so no agent switches its own coordinator. Both are
// compare-and-swap on the policy revision and audited.

// defaultDomainID is the domain enable creates when none is set.
const defaultDomainID = "all-work"

func dispatcherSwitchOperation(op string) bool {
	return op == "dispatcher_enable" || op == "dispatcher_disable"
}

// errSwitchFromSession refuses enable and disable from a session.
var errSwitchFromSession = scoped("the dispatcher is turned on or off by the project owner, not from a session; run it in your own terminal with no session (no --session or CLAWBRIDGE_SESSION)", "forbidden")

// dispatcherSwitch runs enable (a preview, or with confirm the change) and
// disable.
func (s *Store) dispatcherSwitch(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, r Request) (any, error) {
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	wid, pid, a := str(w, "id"), str(p, "id"), r.Args
	domains, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid(r.Operation + " requires expected_revision")
	}
	if meta["revision"].(int64) != int64(expected) {
		return nil, conflict(fmt.Sprintf("the dispatcher policy changed: current revision is %d", meta["revision"]))
	}
	before, err := coordinatorName(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	own, err := ownLatestSession(ctx, tx, pid, s.principal)
	if err != nil {
		return nil, err
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if r.Operation == "dispatcher_disable" {
		if meta["enabled"] != true {
			return nil, invalid("the dispatcher is already off")
		}
		revision := int64(expected) + 1
		res, err := tx.ExecContext(ctx, `UPDATE dispatcher_policies SET enabled=0,revision=?,updated_at=?,updated_by_principal=?,updated_by_session=NULL WHERE project_id=? AND revision=?`, revision, nowRaw, s.principal, pid, expected)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, conflict("the dispatcher policy changed concurrently; read it again")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET status='retired',updated_at=? WHERE project_id=? AND role='system' AND name IN (?,?) AND status='active'`, nowRaw, pid, DispatcherSessionName, DispatcherModelSessionName); err != nil {
			return nil, err
		}
		if err = clearModelOptIns(ctx, tx, pid); err != nil {
			return nil, err
		}
		var next any
		after := ""
		if own != nil {
			next, after = own["id"], str(own, "name")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET coordinator_session=? WHERE id=?`, next, pid); err != nil {
			return nil, err
		}
		details := map[string]any{"revision": revision, "enabled": false, "coordinator_before": before, "coordinator_after": after}
		if err = s.audit(ctx, tx, wid, pid, "", "dispatcher_policy", pid, r.Operation, details); err != nil {
			return nil, err
		}
		return map[string]any{"enabled": false, "revision": revision, "coordinator": after}, nil
	}
	mode := str(a, "mode")
	if mode == "" {
		mode = "rules"
	}
	if mode != "rules" && mode != "model" {
		return nil, invalid("mode is rules or model")
	}
	if meta["enabled"] == true && meta["mode"] == mode {
		return nil, invalid("the dispatcher is already on in mode " + mode)
	}
	// Enable turns advice and the opt-in tools off: each comes back only
	// through its own preview (R132-M1).
	settings, _, err := readModelSettings(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	var model map[string]any
	if mode == "model" {
		keys, err := query(ctx, tx, `SELECT provider_id,model,caps FROM dispatcher_model_keys WHERE owner_principal=? AND state='active'`, s.principal)
		if err != nil {
			return nil, err
		}
		if len(keys) != 1 {
			return nil, invalid("model mode needs your model setup to have passed its test: run clawbridge dispatcher model setup first")
		}
		var caps any
		_ = json.Unmarshal([]byte(str(keys[0], "caps")), &caps)
		// What turning the model on means, in the preview (§12.4).
		// Who can ask it questions (dispatcher chat); the questions go to the
		// provider too (#121 L1).
		chat := "owners"
		if settings.ChatLeads {
			chat = "owners and leads"
		}
		model = map[string]any{"provider": str(keys[0], "provider_id"), "model": str(keys[0], "model"), "caps": caps,
			"issue_text_to_provider": true, "agent_required": true, "chat": chat, "chat_questions_to_provider": true}
		over, err := modelCapPreview(ctx, tx, s.principal, pid)
		if err != nil {
			return nil, err
		}
		if over != nil {
			model["model_projects_over_cap"] = over
		}
	}
	raw, err := s.taskboard(ctx, tx, wid, pid, "", "taskboard_get_v2", map[string]any{})
	if err != nil {
		return nil, err
	}
	b, err := ParseTaskboardBindingV2(raw)
	if err != nil {
		return nil, err
	}
	if !b.Configured {
		return nil, invalid("link the task board before enabling the dispatcher")
	}
	next := domains
	created := false
	if len(domains.Domains) == 0 && len(b.LinearProjectIDs) > 0 {
		if len(b.LinearProjectIDs) > maxDispatcherMatches {
			return nil, invalid(fmt.Sprintf("the binding has more than %d Linear projects: set domains first (dispatcher domains set)", maxDispatcherMatches))
		}
		dom := DispatcherDomain{ID: defaultDomainID, Name: "All work", Match: DispatcherMatch{LinearProjects: append([]string{}, b.LinearProjectIDs...), Labels: []string{}}, OwnerPrincipal: s.principal, TrustedSessions: []DispatcherSessionRef{}, StaleAfterHours: defaultStaleAfterHour}
		if own != nil {
			dom.CoordinatorSession = &DispatcherSessionRef{ID: str(own, "id"), Name: str(own, "name"), Principal: s.principal}
		}
		next, created = DispatcherDomains{Domains: []DispatcherDomain{dom}}, true
	}
	autoConfirm := []string{}
	for _, d := range next.Domains {
		if len(d.Match.LinearProjects) > 0 && len(d.TrustedSessions) > 0 {
			autoConfirm = append(autoConfirm, d.ID)
		}
	}
	ids := []string{}
	for _, d := range next.Domains {
		ids = append(ids, d.ID)
	}
	preview := map[string]any{
		"mode": mode, "coordinator_before": before, "coordinator_after": DispatcherSessionName,
		"sessions": []string{DispatcherSessionName, DispatcherModelSessionName},
		"domains":  ids, "default_domain_created": created, "auto_confirm_domains": autoConfirm,
	}
	if model != nil {
		preview["model"] = model
	}
	if off := modelOptIns(settings); len(off) > 0 {
		preview["turns_off"] = off
	}
	// With no domain every claim would escalate as no_domain, so enable
	// refuses it: the preview says why (R107-L1).
	noDomains := len(next.Domains) == 0
	if noDomains {
		preview["blocked"] = "no_domains"
	}
	overCap := model != nil && model["model_projects_over_cap"] != nil
	if overCap && !noDomains {
		preview["blocked"] = "model_projects_over_cap"
	}
	if a["confirm"] != true {
		preview["preview"] = true
		return preview, nil
	}
	if noDomains {
		return nil, invalid("the dispatcher has no domains and the task board binding names no Linear projects, so every claim would be escalated: set domains first (dispatcher domains set)")
	}
	if overCap {
		return nil, errModelProjectsCap
	}
	if err = clearModelOptIns(ctx, tx, pid); err != nil {
		return nil, err
	}
	current := int64(expected)
	// A new default domain, or a first policy row, is written first.
	if created || current == 0 {
		if current, err = s.writeDispatcher(ctx, tx, pid, current, next, nil); err != nil {
			return nil, err
		}
	}
	revision := current + 1
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_policies SET enabled=1,mode=?,revision=?,updated_at=?,updated_by_principal=?,updated_by_session=NULL WHERE project_id=? AND revision=?`, mode, revision, nowRaw, s.principal, pid, current)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, conflict("the dispatcher policy changed concurrently; read it again")
	}
	if err = s.ensureDispatcherSessions(ctx, tx, pid); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?) WHERE id=?`, pid, DispatcherSessionName, pid); err != nil {
		return nil, err
	}
	details := map[string]any{"revision": revision, "enabled": true, "mode": mode, "coordinator_before": before, "coordinator_after": DispatcherSessionName, "default_domain_created": created}
	if err = s.audit(ctx, tx, wid, pid, "", "dispatcher_policy", pid, r.Operation, details); err != nil {
		return nil, err
	}
	delete(preview, "preview")
	preview["enabled"], preview["revision"] = true, revision
	return preview, nil
}

// coordinatorName is the project's coordinator session's name, or "".
func coordinatorName(ctx context.Context, tx *transaction, p map[string]any) (string, error) {
	if str(p, "coordinator_session") == "" {
		return "", nil
	}
	rows, err := query(ctx, tx, `SELECT name FROM sessions WHERE id=?`, p["coordinator_session"])
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return str(rows[0], "name"), nil
}

// ownLatestSession is principal's most recently seen active session in
// the project, or nil.
func ownLatestSession(ctx context.Context, tx *transaction, pid, principal string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND owner_principal=? AND status='active' AND role<>'system' ORDER BY last_seen DESC,id LIMIT 1`, pid, principal)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}
