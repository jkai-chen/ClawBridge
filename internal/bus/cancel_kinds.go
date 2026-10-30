package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AGENT-CANCEL R1-R2 (live/AGENT-CANCEL-PLAN.md): the project kinds in
// whose Linear projects agents may cancel or mark Duplicate an issue. It is
// set as who closes is: by the project owner or a lead, never by a
// session, and letting agents cancel is typed at a terminal or in the TUI.
// Until the enforcement ships the setting only records the choice.

func statusCancelKindsOperation(op string) bool { return op == "status_cancel_kinds_set" }

// CancelKindsLine says who may cancel in one line, as doctor, the CLI
// and the TUI say it.
func CancelKindsLine(kinds []string) string {
	if len(kinds) == 0 {
		return "Agents never cancel issues."
	}
	return "Agents may cancel or mark Duplicate in projects of kind " + strings.Join(kinds, ", ") + "."
}

// cancelKindsAdded are the names next lists that current doesn't, ignoring
// case, in next's order.
func cancelKindsAdded(current, next []string) []string {
	have := map[string]bool{}
	for _, name := range current {
		have[strings.ToLower(name)] = true
	}
	out := []string{}
	for _, name := range next {
		if !have[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	return out
}

// cancelKindsEqual reports whether two lists name the same kinds, ignoring
// case and order.
func cancelKindsEqual(a, b []string) bool {
	return len(cancelKindsAdded(a, b)) == 0 && len(cancelKindsAdded(b, a)) == 0
}

// canonicalCancelKinds spells each name of next as the project's kind
// setting does. A name already in kept may stay though it is no longer a
// kind (doctor warns of it); any other must be a kind.
func canonicalCancelKinds(ctx context.Context, tx *transaction, pid string, kept, next []string) ([]string, error) {
	if len(next) == 0 {
		return nil, nil
	}
	setting, err := readProjectKind(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	values, _ := setting["values"].([]ProjectKindValue)
	spelled := map[string]string{}
	for _, v := range values {
		spelled[strings.ToLower(v.Name)] = v.Name
	}
	allowed := map[string]bool{}
	for _, name := range kept {
		allowed[strings.ToLower(name)] = true
	}
	out := make([]string, 0, len(next))
	for _, name := range next {
		key := strings.ToLower(name)
		switch {
		case spelled[key] != "":
			out = append(out, spelled[key])
		case allowed[key]:
			out = append(out, name)
		case len(values) == 0:
			return nil, invalid("agents may cancel only in projects of a kind, and this project has no kinds: set the project kinds first: clawbridge linear kind set")
		default:
			names := make([]string, 0, len(values))
			for _, v := range values {
				names = append(names, v.Name)
			}
			return nil, invalid(fmt.Sprintf("%q is not a kind of this project (its kinds: %s)", name, strings.Join(names, ", ")))
		}
	}
	return out, nil
}

// cancelKindsArg reads the op's kinds: a list of names, each once.
func cancelKindsArg(v any) ([]string, error) {
	var names []string
	switch list := v.(type) {
	case []string:
		names = list
	case []any:
		for _, raw := range list {
			name, ok := raw.(string)
			if !ok {
				return nil, invalid("kinds is a list of kind names")
			}
			names = append(names, name)
		}
	default:
		return nil, invalid("kinds is a list of kind names; an empty list means agents never cancel")
	}
	return cancelKindNames(names)
}

// statusCancelKindsOp sets agent_may_cancel_kinds at the policy's
// revision. Only the project owner or a lead, and never with a session:
// an agent could otherwise grant itself the right to cancel. A non-empty
// list is typed by the person (cli-tty or tui). The rest of the policy is
// untouched.
func (s *Store) statusCancelKindsOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	if actor != nil {
		return nil, errors.New("a session can never set who may cancel issues; the project owner or a lead runs it without --session")
	}
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project owner or lead permission required")
	}
	a := r.Args
	via := str(a, "via")
	if !closesVia[via] {
		return nil, invalid("via is cli, cli-tty or tui")
	}
	kinds, err := cancelKindsArg(a["kinds"])
	if err != nil {
		return nil, err
	}
	if len(kinds) > 0 && !closesTyped(via) {
		return nil, invalid("letting agents cancel issues is confirmed at a terminal, by typing the kind names")
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("status_cancel_kinds_set requires expected_revision")
	}
	policy, current, err := readStatusPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if current == 0 {
		return nil, invalid("who may cancel binds only under a status policy, and this project has none; the project owner creates one")
	}
	if current != int64(expected) {
		return nil, conflict(fmt.Sprintf("status policy changed: current revision is %d", current))
	}
	if kinds, err = canonicalCancelKinds(ctx, tx, pid, nil, kinds); err != nil {
		return nil, err
	}
	old := policy.AgentMayCancelKinds
	policy.AgentMayCancelKinds = kinds
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
	// The history keeps counts, not the names people typed.
	if err = s.audit(ctx, tx, wid, pid, "", "linear_status_policy", pid, "cancel_kinds_set", map[string]any{"revision": revision, "kinds_before": len(old), "kinds_after": len(kinds), "via": via}); err != nil {
		return nil, err
	}
	if !cancelKindsEqual(old, kinds) {
		// The role, never the principal: agents read this message, and
		// a person's sign-in identity is not theirs to pass on.
		by := "the project owner"
		if projectRole == "lead" {
			by = "a lead"
		}
		body := "Agents no longer cancel issues in this project (set by " + by + ")."
		if len(kinds) > 0 {
			body = "Agents may now cancel or mark Duplicate issues in projects of kind " + strings.Join(kinds, ", ") + " (set by " + by + ")."
		}
		if err = s.closePolicyMessage(ctx, tx, wid, pid, "", body+"\n"+closePolicyLine, now); err != nil {
			return nil, err
		}
	}
	return statusPolicyView(ctx, tx, pid, s.projectFlowProven)
}
