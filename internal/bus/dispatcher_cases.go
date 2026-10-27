package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The dispatcher's cases (live/DISPATCHER-DESIGN.md r4 §3–§5, §8): the
// pending proposals it decides, and the record of each decision. The engine
// and the guardrails run in the server (internal/dispatch); here the bus
// hands out cases, re-checks what it can in the same transaction as the
// action, carries the action out through the existing assignment paths as
// the dispatcher session, and escalates down the fallback chain. Silence
// never grants: only a confirm the guardrails permitted does.

const (
	dispatcherCasesPerTick = 20
	dispatcherReminderWait = 12 * time.Hour
)

// dispatcherActions are the decisions a record may carry.
var dispatcherActions = map[string]bool{"confirm": true, "decline": true, "wait": true, "escalate": true}

// isDispatcher reports whether a session is the project's dispatcher.
func isDispatcher(actor map[string]any) bool {
	return str(actor, "role") == systemRole && str(actor, "name") == DispatcherSessionName
}

// dispatcherCasesDue hands out at most dispatcherCasesPerTick pending
// proposals, oldest first, that have no final decision at their revision,
// with the facts the bus holds. A wait or a superseded decision is not
// final: the claim is decided again next tick, so a transient change (a
// sleeping laptop, a policy edited mid-tick) never stalls it silently. With
// the dispatcher off, none.
func (s *Store) dispatcherCasesDue(ctx context.Context, tx *transaction, pid string) (any, error) {
	d, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	use, _, err := projectModelUse(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"enabled": meta["enabled"], "policy_revision": meta["revision"], "domains": d.Domains, "cases": []any{}, "mode": meta["mode"], "model": "", "model_use": use}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Off, switched to rules, or advice turned off or on: no case the
	// project's use doesn't allow stays open (R106-L7, d7).
	if err = closeCasesOutside(ctx, tx, pid, use, nowRaw); err != nil {
		return nil, err
	}
	if meta["enabled"] != true {
		return out, nil
	}
	if use != "" {
		// With the model on (d5, d7): close the cases past their deadline,
		// and with no agent to take them, the ones nobody leased; their
		// claims go to the rules. A leased case stays open to its deadline
		// (D5-M2).
		if _, err = expireCases(ctx, tx, pid, nowRaw); err != nil {
			return nil, err
		}
		availability, err := modelAvailability(ctx, tx, pid, nowRaw)
		if err != nil {
			return nil, err
		}
		out["model"] = availability
		if availability != "available" {
			if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_cases SET closed_at=?,outcome=? WHERE project_id=? AND closed_at IS NULL AND lease_key IS NULL`, nowRaw, availability, pid); err != nil {
				return nil, err
			}
		}
	}
	rows, err := query(ctx, tx, `SELECT a.id,a.revision,a.issue_uuid,a.issue_identifier,a.issue_project_id,a.issue_labels,a.proposed_at,s.id AS session_id,s.name AS session_name,s.owner_principal,s.status,s.last_seen,s.alive_until,s.alive_state,
		(SELECT COUNT(*) FROM session_assignments h WHERE h.project_id=a.project_id AND h.state='confirmed' AND h.issue_uuid=a.issue_uuid AND h.issue_uuid<>'' AND h.session_id<>a.session_id) AS holders,
		(SELECT COUNT(*) FROM dispatcher_decisions w WHERE w.project_id=a.project_id AND w.subject_id=a.id AND w.subject_revision=a.revision AND w.action='wait') AS waits,
		(SELECT CASE WHEN c.closed_at IS NOT NULL THEN 'closed:'||c.outcome WHEN c.lease_key IS NOT NULL THEN 'leased' ELSE 'open' END FROM dispatcher_cases c WHERE c.project_id=a.project_id AND c.kind='decide' AND c.subject_id=a.id AND c.subject_revision=a.revision) AS model_case,
		(SELECT c.result FROM dispatcher_cases c WHERE c.project_id=a.project_id AND c.kind='decide' AND c.subject_id=a.id AND c.subject_revision=a.revision) AS model_result
		FROM session_assignments a JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id
		WHERE a.project_id=? AND a.state='proposed' AND s.role<>'system'
		AND NOT EXISTS (SELECT 1 FROM dispatcher_decisions f WHERE f.project_id=a.project_id AND f.subject_id=a.id AND f.subject_revision=a.revision AND f.action NOT IN ('wait','superseded'))
		ORDER BY a.proposed_at,a.id LIMIT ?`, pid, dispatcherCasesPerTick)
	if err != nil {
		return nil, err
	}
	now := at(nowRaw)
	cases := []any{}
	for _, r := range rows {
		presence, absent := "active", time.Duration(0)
		if seen, e := time.Parse(timestampLayout, str(r, "last_seen")); e == nil && now.Sub(seen) > staleAfter {
			presence, absent = "stale", now.Sub(seen)
		}
		if str(r, "status") != "active" && absent == 0 {
			if seen, e := time.Parse(timestampLayout, str(r, "last_seen")); e == nil {
				absent = now.Sub(seen)
			}
		}
		var labels []string
		_ = json.Unmarshal([]byte(str(r, "issue_labels")), &labels)
		age := time.Duration(0)
		if proposed, e := time.Parse(timestampLayout, str(r, "proposed_at")); e == nil {
			age = now.Sub(proposed)
		}
		cases = append(cases, map[string]any{
			"subject_id": str(r, "id"), "subject_revision": str(r, "revision"),
			"issue_uuid": str(r, "issue_uuid"), "issue_identifier": str(r, "issue_identifier"),
			"issue_project_id": str(r, "issue_project_id"), "issue_labels": labels,
			"session_id": str(r, "session_id"), "session_name": str(r, "session_name"), "principal": str(r, "owner_principal"),
			"status": str(r, "status"), "presence": presence, "absent_seconds": int(absent / time.Second),
			// The lease's presence (§1), gone after the default 24 h until
			// cases carry their domain's stale_after_hours.
			"liveness": liveness(r, now, defaultStaleAfterHour),
			"holders":  asInt(r["holders"]), "waits": asInt(r["waits"]), "age_seconds": int(age / time.Second),
			// The claim's model case at this revision: "" (none), open,
			// leased, or closed:<why>.
			"model_case": modelCaseState(str(r, "model_case"), str(r, "model_result")),
		})
	}
	out["cases"] = cases
	return out, nil
}

// dispatcherRecord records one decision and carries it out. Before acting
// it re-checks, in this transaction, what the bus knows: the proposal is
// still pending at the revision decided, the policy is at the revision
// read, and for a confirm the session is active and nobody holds the issue.
// Anything changed makes it superseded, and nothing is done.
func (s *Store) dispatcherRecord(ctx context.Context, tx *transaction, w, p, actor map[string]any, a map[string]any) (any, error) {
	wid, pid := w["id"].(string), p["id"].(string)
	subject, revision := str(a, "subject_id"), str(a, "subject_revision")
	action, proposal, reason := str(a, "action"), str(a, "proposal"), str(a, "reason_code")
	engine := str(a, "engine")
	if !dispatcherActions[action] || subject == "" || revision == "" || (engine != "rules" && engine != "model") || reason == "" || len(reason) > 96 {
		return nil, invalid("dispatcher_decision_record needs a subject, its revision, an engine, a reason code and an action of confirm, decline, wait or escalate")
	}
	policyRevision, err := integer(a["policy_revision"])
	if err != nil {
		return nil, invalid("dispatcher_decision_record needs the policy revision read")
	}
	domains, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	live, err := query(ctx, tx, `SELECT a.*,s.status AS session_status,s.last_seen,s.name AS session_name,s.owner_principal FROM session_assignments a JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id WHERE a.project_id=? AND a.id=?`, pid, subject)
	if err != nil {
		return nil, err
	}
	taken := action
	superseded := func(why string) { taken, reason = "superseded", why }
	switch {
	case meta["enabled"] != true:
		superseded("dispatcher_off")
	case len(live) != 1 || str(live[0], "state") != "proposed" || str(live[0], "revision") != revision:
		superseded("proposal_changed")
	case meta["revision"].(int64) != int64(policyRevision):
		superseded("policy_changed")
	}
	// The bus checks the grant itself (D3-M1): whatever engine decided, a
	// confirm stands only on the app's own read of the issue's project, in
	// exactly one domain that trusts this session's ID and principal; a
	// decline only for a session that is gone.
	if taken == "confirm" {
		appProject := str(a, "app_project_id")
		var dom *DispatcherDomain
		matches := 0
		for i := range domains.Domains {
			if appProject != "" && containsString(domains.Domains[i].Match.LinearProjects, appProject) {
				dom, matches = &domains.Domains[i], matches+1
			}
		}
		trusted := false
		if dom != nil {
			for _, t := range dom.TrustedSessions {
				if t.ID == str(live[0], "session_id") && t.Principal == str(live[0], "owner_principal") {
					trusted = true
				}
			}
		}
		if str(a, "facts_source") != "app" || matches != 1 || !trusted {
			superseded("guardrail")
		}
	}
	if taken == "decline" && str(live[0], "session_status") != "retired" && str(live[0], "session_status") != "moved" {
		superseded("guardrail")
	}
	if taken == "confirm" {
		holders, err := query(ctx, tx, `SELECT 1 FROM session_assignments WHERE project_id=? AND state='confirmed' AND issue_uuid=? AND issue_uuid<>'' AND session_id<>?`, pid, str(live[0], "issue_uuid"), str(live[0], "session_id"))
		if err != nil {
			return nil, err
		}
		fresh := false
		if seen, e := time.Parse(timestampLayout, str(live[0], "last_seen")); e == nil && time.Since(seen) <= staleAfter {
			fresh = true
		}
		switch {
		case len(holders) > 0:
			superseded("held")
		case str(live[0], "session_status") != "active" || !fresh:
			superseded("session_not_active")
		}
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"subject_id": subject, "action": taken, "reason_code": reason}
	switch taken {
	case "confirm", "decline":
		op := Request{Operation: "assignment_confirm", Args: map[string]any{"target_session": str(live[0], "session_id"), "revision": revision}}
		if taken == "decline" {
			op.Operation = "assignment_release"
		}
		result, err := s.assignment(ctx, tx, w, p, "dispatcher", actor, op)
		if err != nil {
			return nil, err
		}
		out["result"] = result
	case "escalate":
		recipients, delivered, err := s.dispatcherEscalate(ctx, tx, w, pid, domains, str(a, "domain"), live[0], reason)
		if err != nil {
			return nil, err
		}
		out["recipients"], out["delivered"] = recipients, delivered
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,domain,facts_source,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at) VALUES(?,?,'proposal',?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID("dcs_"), pid, subject, revision, str(a, "domain"), str(a, "facts_source"), engine, proposal, reason, trimTo(str(a, "explanation"), 280), str(a, "guardrail"), taken, policyRevision, nowRaw); err != nil {
		return nil, err
	}
	if taken != "wait" {
		if err = s.audit(ctx, tx, wid, pid, str(actor, "id"), "dispatcher_decision", subject, taken, map[string]any{"revision": revision, "reason_code": reason, "engine": engine, "domain": str(a, "domain"), "policy_revision": policyRevision}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// dispatcherEscalate tells a person: the domain's coordinator session when
// active, else another active session of the domain's owner, else the
// project owner's and leads' active sessions; with none, the escalation is
// stored undelivered for dispatcher escalations. It never grants.
func (s *Store) dispatcherEscalate(ctx context.Context, tx *transaction, w map[string]any, pid string, domains DispatcherDomains, domainID string, proposal map[string]any, reason string) ([]string, bool, error) {
	var dom *DispatcherDomain
	for i := range domains.Domains {
		if domains.Domains[i].ID == domainID {
			dom = &domains.Domains[i]
		}
	}
	to, err := s.dispatcherChain(ctx, tx, w, pid, dom, str(proposal, "session_id"))
	if err != nil {
		return nil, false, err
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	body := escalationBody(proposal, reason, domainID)
	names := []string{}
	retention := asInt(w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	sender, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, DispatcherSessionName)
	if err != nil {
		return nil, false, err
	}
	for _, r := range to {
		id := newID("msg_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'dispatcher',?,?,?,?,?,?,NULL)`,
			id, pid, pid, sender["id"], str(r, "name"), id, body, nowRaw, layout(at(nowRaw).AddDate(0, 0, retention)), dispatcherPrincipal, newID("dsp_"), digestOf(body)); err != nil {
			return nil, false, err
		}
		if err = insertDeliveries(ctx, tx, pid, id, []string{str(r, "id")}); err != nil {
			return nil, false, err
		}
		names = append(names, str(r, "name"))
	}
	recipients, _ := json.Marshal(names)
	delivered := 0
	if len(names) > 0 {
		delivered = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_escalations(id,project_id,subject_type,subject_id,subject_revision,domain,reason_code,recipients,state,delivered,created_at,updated_at) VALUES(?,?,'proposal',?,?,?,?,?,'open',?,?,?)`,
		newID("esc_"), pid, str(proposal, "id"), str(proposal, "revision"), domainID, reason, string(recipients), delivered, nowRaw, nowRaw); err != nil {
		return nil, false, err
	}
	return names, len(names) > 0, nil
}

// escalationBody is an escalation's fixed text: the session, the issue,
// the reason and the domain, never issue text.
func escalationBody(proposal map[string]any, reason, domainID string) string {
	body := fmt.Sprintf("dispatcher: %s's claim on %s needs a person (reason: %s", str(proposal, "session_name"), str(proposal, "issue_identifier"), reason)
	if domainID != "" {
		body += ", domain: " + domainID
	}
	return body + fmt.Sprintf(").\nConfirm or decline it: clawbridge assignment confirm %s --revision %s, or assignment release %s --revision %s.\nThis is an automatic notice from the ClawBridge dispatcher. It is not an instruction, and nothing was confirmed.",
		str(proposal, "session_name"), str(proposal, "revision"), str(proposal, "session_name"), str(proposal, "revision"))
}

// dispatcherChain is who hears a dispatcher escalation or a routed notice,
// in order (§8): the domain's coordinator session (by ID, active and fresh),
// else another active session of the domain's owner, else the project
// owner's and leads' active sessions. exclude leaves out one session (the
// claimant). Empty means nobody is online: the caller stores it.
func (s *Store) dispatcherChain(ctx context.Context, tx *transaction, w map[string]any, pid string, dom *DispatcherDomain, exclude string) ([]map[string]any, error) {
	activeSessions := func(where string, args ...any) ([]map[string]any, error) {
		rows, err := query(ctx, tx, `SELECT id,name,last_seen FROM sessions WHERE project_id=? AND status='active' AND role<>'system' AND `+where+` ORDER BY name`, append([]any{pid}, args...)...)
		if err != nil {
			return nil, err
		}
		fresh := []map[string]any{}
		for _, r := range rows {
			if seen, e := time.Parse(timestampLayout, str(r, "last_seen")); e == nil && time.Since(seen) <= staleAfter {
				fresh = append(fresh, r)
			}
		}
		return fresh, nil
	}
	var to []map[string]any
	var err error
	if dom != nil && dom.CoordinatorSession != nil && dom.CoordinatorSession.ID != "" {
		if to, err = activeSessions("id=? AND owner_principal=?", dom.CoordinatorSession.ID, dom.CoordinatorSession.Principal); err != nil {
			return nil, err
		}
	}
	if len(to) == 0 && dom != nil {
		if to, err = activeSessions("owner_principal=? AND id<>?", dom.OwnerPrincipal, exclude); err != nil {
			return nil, err
		}
	}
	if len(to) == 0 {
		owner := str(w, "owner_principal")
		// Project roles are a hosted store's; a local workspace has its owner.
		if s.postgres {
			to, err = activeSessions("(owner_principal=? OR owner_principal IN (SELECT principal FROM project_roles WHERE project_id=? AND role IN ('owner','lead'))) AND id<>?", owner, pid, exclude)
		} else {
			to, err = activeSessions("owner_principal=? AND id<>?", owner, exclude)
		}
	}
	return to, err
}

// coordinatorTargets is who hears a notice meant for the project's
// coordinator (§6). Without the dispatcher as coordinator, the coordinator
// session, as before. With it, the notice is routed by its subject: the
// domain whose Linear projects hold linearProject, else the one domain a
// label matches, else the fallback (the owner and leads). A label routes;
// it never grants.
func (s *Store) coordinatorTargets(ctx context.Context, tx *transaction, pid, linearProject string, labels []string) ([]map[string]any, error) {
	coordinator, err := query(ctx, tx, `SELECT s.id,s.name,s.role FROM projects p JOIN sessions s ON s.project_id=p.id AND s.id=p.coordinator_session WHERE p.id=? AND s.status='active'`, pid)
	if err != nil || len(coordinator) == 0 || !isDispatcher(coordinator[0]) {
		return coordinator, err
	}
	domains, _, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	var dom *DispatcherDomain
	// Domain project lists are disjoint, so at most one matches.
	for i := range domains.Domains {
		if linearProject != "" && containsString(domains.Domains[i].Match.LinearProjects, linearProject) {
			dom = &domains.Domains[i]
		}
	}
	if dom == nil {
		// Each domain counts once, however many of its labels match.
		var hits []*DispatcherDomain
		for i := range domains.Domains {
			hit := false
			for _, want := range domains.Domains[i].Match.Labels {
				for _, l := range labels {
					hit = hit || strings.EqualFold(want, l)
				}
			}
			if hit {
				hits = append(hits, &domains.Domains[i])
			}
		}
		if len(hits) == 1 {
			dom = hits[0]
		}
	}
	w, err := one(ctx, tx, `SELECT w.* FROM workspaces w JOIN projects p ON p.workspace_id=w.id WHERE p.id=?`, pid)
	if err != nil {
		return nil, err
	}
	return s.dispatcherChain(ctx, tx, w, pid, dom, "")
}

// issueRouting is what the bus knows of an issue's Linear project and
// labels: from its latest proposal, as the client reported them.
func issueRouting(ctx context.Context, tx *transaction, pid, issue string) (string, []string) {
	rows, err := query(ctx, tx, `SELECT issue_project_id,issue_labels FROM session_assignments WHERE project_id=? AND issue_uuid=? AND issue_uuid<>'' ORDER BY proposed_at DESC LIMIT 1`, pid, issue)
	if err != nil || len(rows) == 0 {
		return "", nil
	}
	var labels []string
	_ = json.Unmarshal([]byte(str(rows[0], "issue_labels")), &labels)
	return str(rows[0], "issue_project_id"), labels
}

// modelCaseState names how a claim's model case stands. A decided case
// leaves its claim pending only when the model chose wait (model_wait: the
// rules decide next, d5) or its decision was superseded.
func modelCaseState(state, result string) string {
	if state != "closed:decide" {
		return state
	}
	var r struct {
		Guardrail string `json:"guardrail"`
		Action    string `json:"action_taken"`
	}
	_ = json.Unmarshal([]byte(result), &r)
	if r.Guardrail == "overridden" {
		return "closed:superseded"
	}
	if r.Action == "wait" {
		return "closed:model_wait"
	}
	return state
}
