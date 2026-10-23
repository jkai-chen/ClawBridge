package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Schema 8: the project owner's approved list of Linear labels that the
// coordinator-only admin commands may create or rename to. It is a
// human-set audit and review gate, not a boundary against the owner's own
// sessions. The same DDL serves SQLite and PostgreSQL.
const labelPolicySchemaV8 = `
CREATE TABLE linear_label_policy(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL,policy TEXT NOT NULL,updated_at TEXT NOT NULL,updated_by_principal TEXT NOT NULL,updated_by_session TEXT);
`

const (
	maxPolicyLabels  = 200
	maxPolicyRenames = 50
	maxPolicyName    = 80
)

// LabelPolicy is the approved list: names that may be created, and the
// renames allowed from an existing name to a new one.
type LabelPolicy struct {
	Labels  []string          `json:"labels"`
	Renames map[string]string `json:"renames"`
}

// policyMention is an @ that a letter or digit does not precede: a mention,
// not an email address. Policy names reach Linear, so they never hold one.
var policyMention = regexp.MustCompile(`(?:\A|[^A-Za-z0-9])@`)

// ValidLabelName checks a name the admin commands may send to Linear as a
// label name, the same rule as a policy entry.
func ValidLabelName(v string) error { return policyName(v) }

func policyName(v string) error {
	if v != strings.TrimSpace(v) || v == "" || utf8.RuneCountInString(v) > maxPolicyName || !utf8.ValidString(v) {
		return fmt.Errorf("label name %q must be 1–%d characters without surrounding spaces", v, maxPolicyName)
	}
	if strings.ContainsFunc(v, unicode.IsControl) || policyMention.MatchString(v) || strings.Contains(strings.ToLower(v), "mention://") {
		return fmt.Errorf("label name %q holds a control character or a mention", v)
	}
	return nil
}

// ParseLabelPolicy decodes a policy strictly: exactly the keys labels and
// renames, string values only, bounded, unique ignoring case, and no rename
// whose target collides with another rename or source.
func ParseLabelPolicy(v any) (LabelPolicy, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 64<<10 {
		return LabelPolicy{}, invalid("label policy must be a JSON object under 64 KiB")
	}
	var p LabelPolicy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return LabelPolicy{}, invalid("label policy must be {\"labels\": [names], \"renames\": {old: new}} and nothing else")
	}
	if p.Labels == nil {
		p.Labels = []string{}
	}
	if p.Renames == nil {
		p.Renames = map[string]string{}
	}
	if len(p.Labels) > maxPolicyLabels || len(p.Renames) > maxPolicyRenames {
		return LabelPolicy{}, invalid(fmt.Sprintf("label policy holds at most %d labels and %d renames", maxPolicyLabels, maxPolicyRenames))
	}
	seen := map[string]bool{}
	for _, name := range p.Labels {
		if err := policyName(name); err != nil {
			return LabelPolicy{}, invalid(err.Error())
		}
		key := strings.ToLower(name)
		if seen[key] {
			return LabelPolicy{}, invalid(fmt.Sprintf("label %q is listed twice", name))
		}
		seen[key] = true
	}
	sources, targets := map[string]bool{}, map[string]bool{}
	for from, to := range p.Renames {
		for _, name := range []string{from, to} {
			if err := policyName(name); err != nil {
				return LabelPolicy{}, invalid(err.Error())
			}
		}
		f, t := strings.ToLower(from), strings.ToLower(to)
		if f == t || sources[f] || targets[t] {
			return LabelPolicy{}, invalid(fmt.Sprintf("rename %q → %q repeats or renames to itself", from, to))
		}
		sources[f], targets[t] = true, true
	}
	for t := range targets {
		if sources[t] {
			return LabelPolicy{}, invalid("a rename target is also renamed; renames do not chain")
		}
	}
	sort.Strings(p.Labels)
	return p, nil
}

func labelPolicyOperation(op string) bool {
	switch op {
	case "label_policy_set", "linear_admin_audit":
		return true
	}
	return false
}

// labelPolicyOp sets the approved list and records admin writes: the
// project owner only, from an active session. Reading the list
// (label_policy_get) is open to every member and handled before sessions.
func (s *Store) labelPolicyOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid := w["id"].(string), p["id"].(string)
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	if actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	current, err := readLabelPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	switch r.Operation {
	case "label_policy_set":
		expected, err := integer(r.Args["expected_revision"])
		if err != nil || r.Args["expected_revision"] == nil {
			return nil, invalid("label_policy_set requires expected_revision")
		}
		if asInt(current["revision"]) != expected {
			return nil, conflict(fmt.Sprintf("label policy changed: current revision is %d", asInt(current["revision"])))
		}
		policy, err := ParseLabelPolicy(r.Args["label_policy"])
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(policy)
		now, err := dbStamp(ctx, tx)
		if err != nil {
			return nil, err
		}
		revision := expected + 1
		if expected == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO linear_label_policy(project_id,revision,policy,updated_at,updated_by_principal,updated_by_session) VALUES(?,?,?,?,?,?)`, pid, revision, string(encoded), now, s.principal, actor["id"])
		} else {
			var result interface{ RowsAffected() (int64, error) }
			result, err = tx.ExecContext(ctx, `UPDATE linear_label_policy SET revision=?,policy=?,updated_at=?,updated_by_principal=?,updated_by_session=? WHERE project_id=? AND revision=?`, revision, string(encoded), now, s.principal, actor["id"], pid, expected)
			if err == nil {
				if n, _ := result.RowsAffected(); n != 1 {
					err = conflict("label policy changed concurrently; read it again")
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, wid, pid, "", "linear_label_policy", pid, "set", map[string]any{"revision": revision, "labels": len(policy.Labels), "renames": len(policy.Renames)}); err != nil {
			return nil, err
		}
		return readLabelPolicy(ctx, tx, pid)
	case "linear_admin_audit":
		return s.adminAudit(ctx, tx, wid, pid, current, r.Args)
	}
	return nil, fmt.Errorf("unsupported operation %q", r.Operation)
}

func readLabelPolicy(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	row, err := one(ctx, tx, `SELECT revision,policy,updated_at,updated_by_principal FROM linear_label_policy WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return map[string]any{"revision": 0, "labels": []string{}, "renames": map[string]string{}, "updated_at": nil, "updated_by": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	var stored any
	if e := json.Unmarshal([]byte(fmt.Sprint(row["policy"])), &stored); e != nil {
		return nil, errors.New("stored label policy is unreadable")
	}
	policy, err := ParseLabelPolicy(stored)
	if err != nil {
		return nil, errors.New("stored label policy is unreadable")
	}
	return map[string]any{"revision": asInt(row["revision"]), "labels": policy.Labels, "renames": policy.Renames, "updated_at": row["updated_at"], "updated_by": row["updated_by_principal"]}, nil
}

// conflict is a revision refusal whose message is written for the caller.
func conflict(message string) error {
	return &serviceError{code: "conflict", cause: errors.New(message), public: true}
}

var adminAuditKinds = map[string]bool{"label_create": true, "label_rename": true, "project_create": true}
var adminAuditOutcomes = map[string]bool{"accepted": true, "observed_unchanged": true, "accepted_duplicate_observed": true, "accepted_duplicate_unknown": true, "uncertain": true}

// adminAudit records one coordinator-only Linear write in history: IDs, the
// names involved and the policy revision it was checked against, never other
// Linear text. A label write must name the current policy revision.
func (s *Store) adminAudit(ctx context.Context, tx *transaction, wid, pid string, policy map[string]any, a map[string]any) (any, error) {
	kind, outcome := str(a, "kind"), str(a, "outcome")
	if !adminAuditKinds[kind] || !adminAuditOutcomes[outcome] {
		return nil, invalid("linear_admin_audit requires a known kind and outcome")
	}
	team, err := lowerUUID(a, "linear_team_id", true)
	if err != nil {
		return nil, err
	}
	id, err := lowerUUID(a, "linear_id", false)
	if err != nil {
		return nil, err
	}
	if !commentOperationID.MatchString(str(a, "operation_id")) {
		return nil, invalid("linear_admin_audit requires operation_id")
	}
	details := map[string]any{"linear_team_id": team, "linear_id": id, "operation_id": str(a, "operation_id"), "outcome": outcome}
	for _, k := range []string{"name", "previous_name"} {
		if v := str(a, k); v != "" {
			if err := policyName(v); err != nil {
				return nil, invalid(err.Error())
			}
			details[k] = v
		}
	}
	entity := "linear_project"
	if kind != "project_create" {
		entity = "linear_label"
		revision, err := integer(a["policy_revision"])
		if err != nil || revision < 1 {
			return nil, invalid("a label write names the policy revision it was checked against")
		}
		// The write has already happened in Linear, so it is recorded even
		// when the list changed since; the current revision is kept beside it.
		details["policy_revision"] = revision
		if now := asInt(policy["revision"]); now != revision {
			details["policy_revision_now"] = now
		}
	}
	key := id
	if key == "" {
		key = team
	}
	// Recording is idempotent by operation ID, so a retried write's audit is
	// never doubled.
	rows, err := query(ctx, tx, `SELECT details FROM history WHERE workspace_id=? AND project_id=? AND entity=? AND entity_key=? AND action=?`, wid, pid, entity, key, kind)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		// query decodes the details column.
		if seen, ok := row["details"].(map[string]any); ok && seen["operation_id"] == details["operation_id"] {
			return map[string]any{"recorded": true, "already": true}, nil
		}
	}
	if err := s.audit(ctx, tx, wid, pid, "", entity, key, kind, details); err != nil {
		return nil, err
	}
	return map[string]any{"recorded": true}, nil
}
