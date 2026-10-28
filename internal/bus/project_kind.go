package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"clawbridge/internal/principal"
)

// The project kind setting (live/PROJECT-KIND-LABELS-PLAN.md): a project
// names a single-select project-label group on one bound team and its
// values. P1 stores the setting; the label IDs are filled in when the
// group is seeded or adopted (P2), so a value's label_id may be empty.
const projectKindSchema = `
CREATE TABLE project_kind_policy(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),team_id TEXT NOT NULL,scope TEXT NOT NULL CHECK(scope='team'),group_name TEXT NOT NULL,group_label_id TEXT,kind_values TEXT NOT NULL,updated_by TEXT NOT NULL,updated_at TEXT NOT NULL);
`

const (
	maxKindValues     = 20
	defaultKindGroup  = "Type"
	projectKindNoTeam = "the task board isn't linked; a project kind belongs to a bound team"
)

// ProjectKindValue is one kind: its name and, once seeded or adopted, the
// project label's ID.
type ProjectKindValue struct {
	Name    string `json:"name"`
	LabelID string `json:"label_id,omitempty"`
}

func projectKindOperation(op string) bool {
	return op == "project_kind_get" || op == "project_kind_set" || op == "project_kind_record"
}

// projectKindOp reads the setting (any member or session) or sets it (the
// project owner, without a session; on the hosted server only from a
// person's own device).
func (s *Store) projectKindOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, errors.New("instance requires session")
	}
	if r.Operation == "project_kind_get" {
		out, err := readProjectKind(ctx, tx, pid)
		if err != nil {
			return nil, err
		}
		// A later binding change can drop the kind's team (review L1).
		if team, _ := out["team_id"].(string); team != "" {
			b, e := s.kindBinding(ctx, tx, pid)
			out["team_bound"] = e == nil && slices.Contains(b.LinearTeamIDs, team)
		}
		return out, nil
	}
	if r.Session != "" {
		return nil, scoped("a session can't set the project kind; the project owner runs linear kind set in their own terminal", "forbidden")
	}
	if projectRole != "owner" {
		return nil, scoped("project owner permission required", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("the project kind is set only from a person's own signed-in device", "forbidden")
	}
	if r.Operation == "project_kind_record" {
		return s.recordProjectKind(ctx, tx, wid, pid, a)
	}
	return s.setProjectKind(ctx, tx, wid, pid, a)
}

// recordProjectKind stores the label IDs a seed or an adoption proved in
// Linear (P2): the group's and one per value, in the setting's order, at
// the revision the preview read. The team must still be bound.
func (s *Store) recordProjectKind(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("project_kind_record requires expected_revision")
	}
	current, err := readProjectKind(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if asInt(current["revision"]) != expected || expected == 0 {
		return nil, conflict(fmt.Sprintf("the project kind changed: current revision is %d; preview again", asInt(current["revision"])))
	}
	action, proof := str(a, "action"), str(a, "proof")
	if action != "seeded" && action != "adopted" {
		return nil, invalid("action must be seeded or adopted")
	}
	// The one proof the seed makes (review L1: nothing else is accepted).
	if proof != "team_list" {
		return nil, invalid("proof must be team_list")
	}
	team := current["team_id"].(string)
	b, err := s.kindBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(b.LinearTeamIDs, team) {
		return nil, invalid("the project kind's team left the binding; set the kind again on a bound team")
	}
	group, err := lowerUUID(a, "group_label_id", true)
	if err != nil {
		return nil, err
	}
	raw, _ := a["label_ids"].([]any)
	if list, ok := a["label_ids"].([]string); ok {
		for _, id := range list {
			raw = append(raw, id)
		}
	}
	values := current["values"].([]ProjectKindValue)
	if len(raw) != len(values) {
		return nil, invalid("label_ids names one label per value, in the setting's order")
	}
	seen := map[string]bool{group: true}
	for i, v := range raw {
		id, _ := v.(string)
		id = strings.ToLower(id)
		if !viewIDPattern.MatchString(id) || seen[id] {
			return nil, invalid("label_ids are distinct Linear label IDs, none the group's")
		}
		seen[id] = true
		values[i].LabelID = id
	}
	encoded, _ := json.Marshal(values)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	revision := expected + 1
	result, err := tx.ExecContext(ctx, `UPDATE project_kind_policy SET revision=?,group_label_id=?,kind_values=?,updated_by=?,updated_at=? WHERE project_id=? AND revision=?`, revision, group, string(encoded), s.principal, now, pid, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, conflict("the project kind changed concurrently; preview again")
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_kind_policy", pid, action, map[string]any{"revision": revision, "scope": "team:" + team, "group_label_id": group, "values": len(values), "proof": proof}); err != nil {
		return nil, err
	}
	return readProjectKind(ctx, tx, pid)
}

func (s *Store) setProjectKind(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("project_kind_set requires expected_revision")
	}
	current, err := readProjectKind(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if asInt(current["revision"]) != expected {
		return nil, conflict(fmt.Sprintf("the project kind changed: current revision is %d", asInt(current["revision"])))
	}
	if str(a, "scope") != "" && str(a, "scope") != "team" {
		// PROJECT-KIND-LABELS-PLAN Q4: no binding is workspace-wide yet.
		return nil, invalid("a project kind is team-scoped; no binding is workspace-wide, so workspace scope is refused")
	}
	group := strings.TrimSpace(str(a, "group_name"))
	if group == "" {
		group = defaultKindGroup
	}
	if !LinearNameOK(group) {
		return nil, invalid("group_name must be 1 to 80 characters with no control or direction characters")
	}
	names, ok := a["kinds"].([]any)
	if !ok {
		if list, isList := a["kinds"].([]string); isList {
			for _, n := range list {
				names = append(names, n)
			}
			ok = true
		}
	}
	if !ok || len(names) == 0 || len(names) > maxKindValues {
		return nil, invalid(fmt.Sprintf("kinds is a list of 1 to %d kind names", maxKindValues))
	}
	b, err := s.kindBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	team := strings.ToLower(str(a, "team_id"))
	if team == "" {
		team = b.LinearDefaultTeamID
	}
	if !slices.Contains(b.LinearTeamIDs, team) {
		return nil, invalid("team_id must be a team of this project's binding")
	}
	// A value keeps its label ID while the group and team stay the same;
	// otherwise the IDs are for the new group to prove again.
	keep := map[string]string{}
	if current["team_id"] == team && strings.EqualFold(fmt.Sprint(current["group_name"]), group) {
		for _, v := range current["values"].([]ProjectKindValue) {
			keep[strings.ToLower(v.Name)] = v.LabelID
		}
	}
	values := []ProjectKindValue{}
	seen := map[string]bool{}
	for _, raw := range names {
		name, _ := raw.(string)
		name = strings.TrimSpace(name)
		if !LinearNameOK(name) {
			return nil, invalid("each value must be 1 to 80 characters with no control or direction characters")
		}
		key := strings.ToLower(name)
		if seen[key] || strings.EqualFold(name, group) {
			return nil, invalid("value names are unique, and none is the group's name")
		}
		seen[key] = true
		values = append(values, ProjectKindValue{Name: name, LabelID: keep[key]})
	}
	groupID := ""
	if len(keep) > 0 {
		groupID, _ = current["group_label_id"].(string)
	}
	encoded, _ := json.Marshal(values)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	revision := expected + 1
	var groupArg any
	if groupID != "" {
		groupArg = groupID
	}
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO project_kind_policy(project_id,revision,team_id,scope,group_name,group_label_id,kind_values,updated_by,updated_at) VALUES(?,?,?,'team',?,?,?,?,?)`, pid, revision, team, group, groupArg, string(encoded), s.principal, now)
	} else {
		var result interface{ RowsAffected() (int64, error) }
		result, err = tx.ExecContext(ctx, `UPDATE project_kind_policy SET revision=?,team_id=?,group_name=?,group_label_id=?,kind_values=?,updated_by=?,updated_at=? WHERE project_id=? AND revision=?`, revision, team, group, groupArg, string(encoded), s.principal, now, pid, expected)
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				err = conflict("the project kind changed concurrently; read it again")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_kind_policy", pid, "set", map[string]any{"revision": revision, "scope": "team:" + team, "values": len(values)}); err != nil {
		return nil, err
	}
	return readProjectKind(ctx, tx, pid)
}

// kindBinding is the project's binding, which a kind's team must be in.
func (s *Store) kindBinding(ctx context.Context, tx *transaction, pid string) (TaskboardBindingV2, error) {
	b, linked, err := storedBinding(ctx, tx, pid)
	if err == nil && !linked {
		err = invalid(projectKindNoTeam)
	}
	return b, err
}

// storedBinding reads a project's binding from the store; linked is false
// when the task board isn't linked.
func storedBinding(ctx context.Context, tx *transaction, pid string) (b TaskboardBindingV2, linked bool, err error) {
	row, err := one(ctx, tx, `SELECT revision,mapping,mapping_version FROM taskboard_bindings WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return TaskboardBindingV2{}, false, nil
	}
	if err != nil {
		return TaskboardBindingV2{}, false, err
	}
	mapping, ok := row["mapping"].(string)
	if !ok {
		return TaskboardBindingV2{}, false, nil
	}
	b, err = readStoredMapping(mapping, int(asInt(row["mapping_version"])))
	if err != nil {
		return TaskboardBindingV2{}, true, bindingUnreadable(int64(asInt(row["revision"])), errors.Is(err, errMappingFormat))
	}
	return b, true, nil
}

// readProjectKind is the setting, or revision 0 with the default group and
// no values when none is set.
func readProjectKind(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	row, err := one(ctx, tx, `SELECT revision,team_id,scope,group_name,group_label_id,kind_values,updated_by,updated_at FROM project_kind_policy WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return map[string]any{"revision": 0, "team_id": nil, "scope": "team", "group_name": defaultKindGroup, "group_label_id": nil, "values": []ProjectKindValue{}, "seeded": false, "updated_by": nil, "updated_at": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	var values []ProjectKindValue
	if json.Unmarshal([]byte(fmt.Sprint(row["kind_values"])), &values) != nil {
		return nil, errors.New("the stored project kind is unreadable")
	}
	seeded := row["group_label_id"] != nil && len(values) > 0
	for _, v := range values {
		seeded = seeded && v.LabelID != ""
	}
	return map[string]any{"revision": asInt(row["revision"]), "team_id": row["team_id"], "scope": row["scope"], "group_name": row["group_name"], "group_label_id": row["group_label_id"], "values": values, "seeded": seeded, "updated_by": row["updated_by"], "updated_at": row["updated_at"]}, nil
}
