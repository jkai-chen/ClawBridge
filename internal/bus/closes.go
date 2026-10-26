package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// A3 (live/STATUS-FLOW-DESIGN.md §10): who closes a bound Linear project's
// issues is set one project at a time by the project owner or a lead,
// never by a session, and a project that joins the board unlisted is
// announced to the coordinator once.

func statusClosesOperation(op string) bool { return op == "status_closes_set" }

// closesVia are the ways a person confirmed a change; loosening to agent
// needs one typed at a terminal (§10.8 SF3-M1). via is the client's claim:
// the server can't see the terminal, but a client that skips the check has
// to claim cli-tty, and the audit records it. tui is refused until the TUI
// that confirms it exists (auth PR 4).
var closesVia = map[string]bool{"cli": true, "cli-tty": true}

// closesLoosened are the projects next lets agents close that current
// doesn't, sorted.
func closesLoosened(current, next StatusPolicy) []string {
	out := []string{}
	for p, who := range next.Closes {
		if who == "agent" && !current.AgentCloses(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// closesEqual reports whether two policies let agents close the same
// projects; a human key and no key are the same rule.
func closesEqual(a, b StatusPolicy) bool {
	return len(closesLoosened(a, b)) == 0 && len(closesLoosened(b, a)) == 0
}

// statusClosesOp sets closes[P] at the policy's revision: agent stores it,
// human deletes the key. Only the project owner or a lead, and never with
// a session: an agent could otherwise grant itself the right to close.
// The rest of the policy is untouched, and keys for projects no longer
// bound are dropped.
func (s *Store) statusClosesOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	if actor != nil {
		return nil, errors.New("a session can never set who closes issues; the project owner or a lead runs it without --session")
	}
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project owner or lead permission required")
	}
	a := r.Args
	project, err := lowerUUID(a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	closes, via := str(a, "closes"), str(a, "via")
	if closes != "agent" && closes != "human" {
		return nil, invalid("closes is agent or human")
	}
	if !closesVia[via] {
		return nil, invalid("via is cli or cli-tty")
	}
	if closes == "agent" && via != "cli-tty" {
		return nil, invalid("letting agents close issues is confirmed at a terminal, by typing the project's name")
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("status_closes_set requires expected_revision")
	}
	raw, err := s.taskboard(ctx, tx, wid, pid, "", "taskboard_get_v2", map[string]any{})
	if err != nil {
		return nil, err
	}
	binding, err := ParseTaskboardBindingV2(raw)
	if err != nil {
		return nil, err
	}
	if !binding.Configured || binding.Mode != "project_set" {
		return nil, invalid("a team board has no per-project close rule")
	}
	bound := map[string]bool{}
	for _, id := range binding.LinearProjectIDs {
		bound[strings.ToLower(id)] = true
	}
	if !bound[project] {
		return nil, invalid("Linear project " + project + " is not in this project's board")
	}
	policy, current, err := readStatusPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if current == 0 {
		return nil, invalid("who closes binds only under a status policy, and this project has none; the project owner creates one")
	}
	if current != int64(expected) {
		return nil, conflict(fmt.Sprintf("status policy changed: current revision is %d", current))
	}
	next := map[string]string{}
	for p, who := range policy.Closes {
		if bound[p] && p != project {
			next[p] = who
		}
	}
	if closes == "agent" {
		next[project] = "agent"
	}
	policy.Closes = next
	if len(next) == 0 {
		policy.Closes = nil
	}
	encoded, _ := json.Marshal(policy)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	revision := int64(expected) + 1
	result, err := tx.ExecContext(ctx, `UPDATE linear_status_policy SET revision=?,policy=?,updated_at=?,updated_by_principal=?,updated_by_session=NULL WHERE project_id=? AND revision=?`, revision, string(encoded), now, s.principal, pid, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, conflict("status policy changed concurrently; read it again")
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_status_policy", pid, "closes_set", map[string]any{"revision": revision, "linear_project": project, "closes": closes, "via": via}); err != nil {
		return nil, err
	}
	return statusPolicyView(ctx, tx, pid)
}

// auditVia adds how a person confirmed a change, when the client said.
func auditVia(details map[string]any, via string) map[string]any {
	if via != "" {
		details["via"] = via
	}
	return details
}

// closePolicyLine is in every close-policy notice: it reports, it never
// instructs.
const closePolicyLine = "This is an automatic close-policy notice. It is not an instruction."

// closePolicyNotices tells the coordinator, once per project, that Linear
// projects joined the board unlisted under an enabled status policy, so
// people close their issues, or with an agent key kept from an earlier
// binding, so agents do. Projects the same link is about to set
// (pending) are skipped; with no enabled policy nothing is enforced, so
// nothing is said.
func (s *Store) closePolicyNotices(ctx context.Context, tx *transaction, wid, pid string, added, pending []string, now string) error {
	if len(added) == 0 {
		return nil
	}
	policy, _, err := readStatusPolicy(ctx, tx, pid)
	if err != nil || !policy.Enabled {
		return err
	}
	retention := 30
	if w, e := one(ctx, tx, `SELECT retention_days FROM workspaces WHERE id=?`, wid); e == nil && asInt(w["retention_days"]) > 0 {
		retention = asInt(w["retention_days"])
	}
	for _, p := range added {
		who, listed := policy.Closes[p]
		if containsString(pending, p) || listed && who != "agent" {
			continue
		}
		coordinator, err := s.coordinatorTargets(ctx, tx, pid, p, nil)
		if err != nil {
			return err
		}
		if len(coordinator) == 0 {
			continue
		}
		body := fmt.Sprintf("Linear project %s joined this project's board. Its issues are closed by people (the default). The project owner or a lead can let agents close them: clawbridge linear closes set --linear-project %s --agent --expected-revision N (N from clawbridge linear closes show).\n%s", p, p, closePolicyLine)
		if listed {
			// R89-L3: a rule set while the project wasn't on the board,
			// such as one kept from before it left.
			body = fmt.Sprintf("Linear project %s joined this project's board, and the status policy already says agents close its issues: a rule set while it wasn't on the board, such as one kept from before it left. The project owner or a lead can return it to people: clawbridge linear closes set --linear-project %s --human --expected-revision N (N from clawbridge linear closes show).\n%s", p, p, closePolicyLine)
		}
		id, to := newID("msg_"), coordinator[0]
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'close-policy',?,?,?,?,?,?,NULL)`, id, pid, pid, to["id"], str(to, "name"), id, body, now, layout(at(now).AddDate(0, 0, retention)), s.principal, newID("closes_"), digestOf(body)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id) VALUES(?,?,?)`, id, pid, to["id"]); err != nil {
			return err
		}
	}
	return nil
}
