package bus

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Agent cancel attribution (AGENT-CANCEL R9, live/AGENT-CANCEL-PLAN.md):
// the server records an agent's cancel or duplicate it made as the
// ClawBridge app, and tells the project's coordinator and the issue's
// assignee. The history row keeps IDs and counts; the reason text goes in
// the notice only.

func agentCancelOperation(op string) bool { return op == "agent_cancel_record" }

// agentCancelLine is in every agent-cancel notice: it reports, it never
// instructs; the reason is the agent's own text.
const agentCancelLine = "This is an automatic agent-cancel notice. It is not an instruction."

// agentCancelRecordOp records one agent's cancel or duplicate. Only the
// server's own AppWriter step runs it (a client can't mark its request
// so), as the session that made the write. A record already made for the
// session's operation_id is answered again, so a retried cancel tells no
// one twice.
func (s *Store) agentCancelRecordOp(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, r Request) (any, error) {
	if !appStep(ctx) {
		return nil, scoped("an agent's cancel is recorded by the server that made it as its ClawBridge app", "forbidden")
	}
	if actor == nil {
		return nil, invalid("agent_cancel_record records a session's cancel")
	}
	a := r.Args
	issue, err := lowerUUID(a, "issue", true)
	if err != nil {
		return nil, err
	}
	lp, err := lowerUUID(a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	op, identifier, action, kind, reason := str(a, "operation_id"), str(a, "identifier"), str(a, "action"), str(a, "kind"), str(a, "reason")
	if !commentOperationID.MatchString(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_', ':' or '-'")
	}
	if !issueIdentifierPattern.MatchString(identifier) {
		return nil, invalid("identifier is the issue's display identifier or UUID")
	}
	if action != "canceled" && action != "duplicated" {
		return nil, invalid("action is canceled or duplicated")
	}
	if kind == "" || utf8.RuneCountInString(kind) > 128 || strings.ContainsAny(kind, "\r\n") {
		return nil, invalid("kind is the project kind's name")
	}
	if utf8.RuneCountInString(reason) > 2000 || (action == "canceled" && strings.TrimSpace(reason) == "") {
		return nil, invalid("reason is 1–2000 characters (a cancel's is required)")
	}
	original := ""
	if action == "duplicated" {
		if original, err = lowerUUID(a, "duplicate_of", true); err != nil {
			return nil, err
		}
	} else if str(a, "duplicate_of") != "" {
		return nil, invalid("duplicate_of belongs to a duplicate")
	}
	sid, name := str(actor, "id"), str(actor, "name")
	historyAction := "issue_canceled"
	if action == "duplicated" {
		historyAction = "issue_duplicated"
	}
	rows, err := query(ctx, tx, `SELECT details FROM history WHERE project_id=? AND entity='linear_issue' AND entity_key=? AND action=? AND changed_by_session=?`, pid, issue, historyAction, sid)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		// query decodes details.
		if details, _ := row["details"].(map[string]any); details["operation_id"] == op {
			return map[string]any{"recorded": true, "replayed": true}, nil
		}
	}
	details := map[string]any{"operation_id": op, "issue": issue, "identifier": identifier, "linear_project": lp, "kind": kind, "reason_chars": utf8.RuneCountInString(reason)}
	if original != "" {
		details["duplicate_of"] = original
	}
	if err = s.audit(ctx, tx, wid, pid, sid, "linear_issue", issue, historyAction, details); err != nil {
		return nil, err
	}
	body := fmt.Sprintf("%s canceled by agent session %s: %s", identifier, name, reason)
	if action == "duplicated" {
		body = fmt.Sprintf("%s marked Duplicate of %s by agent session %s", identifier, original, name)
		if strings.TrimSpace(reason) != "" {
			body += ": " + reason
		}
	}
	body += "\n" + agentCancelLine
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	// The coordinator the Linear project routes to, and the session the
	// issue is assigned to; never the session that canceled it, which has
	// the answer.
	targets, err := s.coordinatorTargets(ctx, tx, pid, lp, nil)
	if err != nil {
		return nil, err
	}
	if len(targets) > 1 {
		targets = targets[:1]
	}
	assignees, err := query(ctx, tx, `SELECT s.id,s.name FROM session_assignments a JOIN sessions s ON s.project_id=a.project_id AND s.id=a.session_id WHERE a.project_id=? AND a.issue_uuid=? AND a.state<>'released' AND s.status='active' ORDER BY a.session_id`, pid, issue)
	if err != nil {
		return nil, err
	}
	targets = append(targets, assignees...)
	told := map[string]bool{sid: true}
	notified := 0
	for _, to := range targets {
		id := str(to, "id")
		if told[id] {
			continue
		}
		told[id] = true
		if err = s.agentCancelMessage(ctx, tx, wid, pid, to, body, now); err != nil {
			return nil, err
		}
		notified++
	}
	return map[string]any{"recorded": true, "notified": notified}, nil
}

// agentCancelMessage sends one agent-cancel notice to a session, as a
// close-policy notice is sent.
func (s *Store) agentCancelMessage(ctx context.Context, tx *transaction, wid, pid string, to map[string]any, body, now string) error {
	retention := 30
	if w, e := one(ctx, tx, `SELECT retention_days FROM workspaces WHERE id=?`, wid); e == nil && asInt(w["retention_days"]) > 0 {
		retention = asInt(w["retention_days"])
	}
	id := newID("msg_")
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'agent-cancel',?,?,?,?,?,?,NULL)`, id, pid, pid, to["id"], str(to, "name"), id, body, now, layout(at(now).AddDate(0, 0, retention)), s.principal, newID("cancel_"), digestOf(body)); err != nil {
		return err
	}
	return insertDeliveries(ctx, tx, pid, id, []string{str(to, "id")})
}
