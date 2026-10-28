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
	return op == "project_kind_get" || op == "project_kind_set"
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
		return readProjectKind(ctx, tx, pid)
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
	return s.setProjectKind(ctx, tx, wid, pid, a)
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
	row, err := one(ctx, tx, `SELECT revision,mapping,mapping_version FROM taskboard_bindings WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return TaskboardBindingV2{}, invalid(projectKindNoTeam)
	}
	if err != nil {
		return TaskboardBindingV2{}, err
	}
	mapping, ok := row["mapping"].(string)
	if !ok {
		return TaskboardBindingV2{}, invalid(projectKindNoTeam)
	}
	b, err := readStoredMapping(mapping, int(asInt(row["mapping_version"])))
	if err != nil {
		return TaskboardBindingV2{}, bindingUnreadable(int64(asInt(row["revision"])), errors.Is(err, errMappingFormat))
	}
	return b, nil
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
