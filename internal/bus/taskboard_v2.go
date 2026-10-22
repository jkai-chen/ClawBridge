package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

const taskboardSchemaV4 = `ALTER TABLE taskboard_bindings ADD COLUMN mapping_version INTEGER NOT NULL DEFAULT 1 CHECK(mapping_version IN (1,2));`

// TaskboardBindingV2 is separate from all legacy receipt and binding types.
type TaskboardBindingV2 struct {
	WorkspaceID         string
	ProjectID           string
	Configured          bool
	Revision            int64
	MappingVersion      int
	UpdatedAt           string
	Provider            string
	Mode                string
	LinearTeamIDs       []string
	LinearDefaultTeamID string
	LinearProjectIDs    []string
	LinearWorkspaceID   string
}
type BindingV2 = TaskboardBindingV2

func parseTaskboardMappingV2(a map[string]any) (TaskboardBindingV2, error) {
	b := TaskboardBindingV2{MappingVersion: 2, Provider: "linear", Mode: str(a, "mode")}
	legacy := map[string]any{"linear_team_ids": a["linear_team_ids"], "linear_default_team_id": a["linear_default_team_id"], "linear_project_id": nil}
	if v, ok := a["linear_workspace_id"]; ok {
		legacy["linear_workspace_id"] = v
	}
	old, e := parseTaskboardMapping(legacy)
	if e != nil {
		return b, e
	}
	b.LinearTeamIDs = old.LinearTeamIDs
	b.LinearDefaultTeamID = old.LinearDefaultTeamID
	b.LinearWorkspaceID = old.LinearWorkspaceID
	sort.Strings(b.LinearTeamIDs)
	switch ids := a["linear_project_ids"].(type) {
	case []string:
		if ids == nil {
			return b, errors.New("project IDs must not be null")
		}
		b.LinearProjectIDs = append([]string{}, ids...)
	case []any:
		if ids == nil {
			return b, errors.New("project IDs must not be null")
		}
		b.LinearProjectIDs = []string{}
		for _, v := range ids {
			id, ok := v.(string)
			if !ok {
				return b, errors.New("project IDs must be strings")
			}
			b.LinearProjectIDs = append(b.LinearProjectIDs, id)
		}
	default:
		return b, errors.New("project IDs must be an array")
	}
	if b.Mode != "team_board" && b.Mode != "project_set" || b.Mode == "team_board" && len(b.LinearProjectIDs) != 0 || b.Mode == "project_set" && (len(b.LinearProjectIDs) < 1 || len(b.LinearProjectIDs) > 50) {
		return b, errors.New("invalid mapping mode/project count")
	}
	seen := map[string]bool{}
	for _, id := range b.LinearProjectIDs {
		if !taskboardUUID.MatchString(id) || seen[id] {
			return b, errors.New("project IDs must be unique canonical UUIDs")
		}
		seen[id] = true
	}
	sort.Strings(b.LinearProjectIDs)
	return b, nil
}
func mappingMapV2(b TaskboardBindingV2) map[string]any {
	b.LinearTeamIDs = append([]string(nil), b.LinearTeamIDs...)
	sort.Strings(b.LinearTeamIDs)
	b.LinearProjectIDs = append([]string{}, b.LinearProjectIDs...)
	sort.Strings(b.LinearProjectIDs)
	return map[string]any{"provider": "linear", "mode": b.Mode, "linear_team_ids": b.LinearTeamIDs, "linear_default_team_id": b.LinearDefaultTeamID, "linear_project_ids": b.LinearProjectIDs, "linear_workspace_id": nullable(b.LinearWorkspaceID)}
}
func bindingMapV2(b TaskboardBindingV2) map[string]any {
	var mapping any
	if b.Configured {
		mapping = mappingMapV2(b)
	}
	return map[string]any{"workspace_id": b.WorkspaceID, "project_id": b.ProjectID, "configured": b.Configured, "revision": b.Revision, "mapping_version": 2, "binding": mapping, "updated_at": nullable(b.UpdatedAt)}
}
func (b TaskboardBindingV2) MarshalJSON() ([]byte, error) { return json.Marshal(bindingMapV2(b)) }
func (b *TaskboardBindingV2) UnmarshalJSON(raw []byte) error {
	v, e := ParseTaskboardBindingV2(json.RawMessage(raw))
	if e == nil {
		*b = v
	}
	return e
}
func ParseBindingV2(v any) (TaskboardBindingV2, error) { return ParseTaskboardBindingV2(v) }
func ParseTaskboardBindingV2(v any) (TaskboardBindingV2, error) {
	var b TaskboardBindingV2
	raw, e := json.Marshal(v)
	if e != nil || len(raw) > 20000 {
		return b, errors.New("invalid V2 binding")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&m) != nil || len(m) != 7 {
		return b, errors.New("invalid V2 binding fields")
	}
	n, e := integer(m["mapping_version"])
	if e != nil || n != 2 {
		return b, errors.New("mapping_version must be 2")
	}
	delete(m, "mapping_version")
	var mapping TaskboardBindingV2
	if configured, ok := m["configured"].(bool); ok && configured {
		a, ok := m["binding"].(map[string]any)
		if !ok || len(a) != 6 || a["provider"] != "linear" {
			return b, errors.New("invalid V2 mapping")
		}
		for _, k := range []string{"provider", "mode", "linear_team_ids", "linear_default_team_id", "linear_project_ids", "linear_workspace_id"} {
			if _, ok := a[k]; !ok {
				return b, errors.New("missing V2 mapping field")
			}
		}
		mapping, e = parseTaskboardMappingV2(a)
		if e != nil {
			return b, e
		}
		old := TaskboardBinding{LinearTeamIDs: mapping.LinearTeamIDs, LinearDefaultTeamID: mapping.LinearDefaultTeamID, LinearWorkspaceID: mapping.LinearWorkspaceID}
		m["binding"] = mappingMap(old)
	}
	old, e := ParseTaskboardBinding(m)
	if e != nil {
		return b, e
	}
	b = mapping
	b.WorkspaceID = old.WorkspaceID
	b.ProjectID = old.ProjectID
	b.Revision = old.Revision
	b.UpdatedAt = old.UpdatedAt
	b.Configured = old.Configured
	b.MappingVersion = 2
	return b, nil
}
func convertLegacyBinding(b TaskboardBinding) TaskboardBindingV2 {
	next := TaskboardBindingV2{WorkspaceID: b.WorkspaceID, ProjectID: b.ProjectID, Configured: b.Configured, Revision: b.Revision, UpdatedAt: b.UpdatedAt, MappingVersion: 2, Provider: b.Provider, Mode: "team_board", LinearTeamIDs: append([]string(nil), b.LinearTeamIDs...), LinearDefaultTeamID: b.LinearDefaultTeamID, LinearWorkspaceID: b.LinearWorkspaceID, LinearProjectIDs: []string{}}
	if b.LinearProjectID != "" {
		next.Mode = "project_set"
		next.LinearProjectIDs = []string{b.LinearProjectID}
	}
	return next
}
func legacyBinding(b TaskboardBindingV2) (TaskboardBinding, error) {
	if b.Configured && len(b.LinearProjectIDs) > 1 {
		return TaskboardBinding{}, &serviceError{code: "unsupported_mapping", cause: errors.New("mapping requires a V2-capable client")}
	}
	old := TaskboardBinding{WorkspaceID: b.WorkspaceID, ProjectID: b.ProjectID, Configured: b.Configured, Revision: b.Revision, UpdatedAt: b.UpdatedAt, Provider: b.Provider, LinearTeamIDs: b.LinearTeamIDs, LinearDefaultTeamID: b.LinearDefaultTeamID, LinearWorkspaceID: b.LinearWorkspaceID}
	if len(b.LinearProjectIDs) == 1 {
		old.LinearProjectID = b.LinearProjectIDs[0]
	}
	return old, nil
}
func (s *Store) taskboard(ctx context.Context, tx *transaction, wid, pid, op string, a map[string]any) (any, error) {
	rows, e := query(ctx, tx, `SELECT revision,mapping,updated_at,mapping_version FROM taskboard_bindings WHERE project_id=?`, pid)
	if e != nil {
		return nil, e
	}
	b := TaskboardBindingV2{WorkspaceID: wid, ProjectID: pid, MappingVersion: 2}
	storedVersion := 1
	if len(rows) > 0 {
		b.Revision = rows[0]["revision"].(int64)
		b.UpdatedAt = rows[0]["updated_at"].(string)
		storedVersion = int(rows[0]["mapping_version"].(int64))
		if mapping, ok := rows[0]["mapping"].(string); ok {
			var saved map[string]any
			if json.Unmarshal([]byte(mapping), &saved) != nil {
				return nil, errors.New("corrupt taskboard mapping")
			}
			var parsed TaskboardBindingV2
			if storedVersion == 1 {
				old, err := parseTaskboardMapping(saved)
				e = err
				parsed = convertLegacyBinding(old)
			} else if storedVersion == 2 {
				parsed, e = parseTaskboardMappingV2(saved)
			} else {
				return nil, errors.New("unsupported stored mapping version")
			}
			if e != nil {
				return nil, errors.New("corrupt taskboard mapping")
			}
			parsed.WorkspaceID = wid
			parsed.ProjectID = pid
			parsed.Revision = b.Revision
			parsed.UpdatedAt = b.UpdatedAt
			parsed.Configured = true
			b = parsed
		}
	}
	v2 := strings.HasSuffix(op, "_v2")
	if !v2 {
		if _, e = legacyBinding(b); e != nil {
			return nil, e
		}
	}
	result := func(next TaskboardBindingV2) (any, error) {
		if v2 {
			return bindingMapV2(next), nil
		}
		old, e := legacyBinding(next)
		if e != nil {
			return nil, e
		}
		return bindingMap(old), nil
	}
	if op == "taskboard_get" || op == "taskboard_get_v2" {
		return result(b)
	}
	expected, e := taskboardExpected(a)
	if e != nil {
		return nil, e
	}
	if expected != b.Revision {
		return nil, &serviceError{code: "conflict", cause: errors.New("taskboard revision conflict; refresh binding")}
	}
	next := TaskboardBindingV2{WorkspaceID: wid, ProjectID: pid, Revision: expected + 1, UpdatedAt: stamp(), MappingVersion: 2}
	var mapping any
	version := 1
	if v2 {
		version = 2
	}
	if op == "taskboard_set_v2" {
		next, e = parseTaskboardMappingV2(a)
		if e != nil {
			return nil, e
		}
		mapping = asJSON(mappingMapV2(next))
		next.Configured = true
	} else if op == "taskboard_set" {
		old, err := parseTaskboardMapping(a)
		if err != nil {
			return nil, err
		}
		mapping = asJSON(mappingMap(old))
		next = convertLegacyBinding(old)
		next.Configured = true
	}
	next.WorkspaceID = wid
	next.ProjectID = pid
	next.Revision = expected + 1
	next.UpdatedAt = stamp()
	var affected int64
	if len(rows) == 0 {
		r, err := tx.ExecContext(ctx, `INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at,mapping_version) VALUES(?,?,?,?,?) ON CONFLICT(project_id) DO NOTHING`, pid, next.Revision, mapping, next.UpdatedAt, version)
		if err != nil {
			return nil, err
		}
		affected, _ = r.RowsAffected()
	} else {
		r, err := tx.ExecContext(ctx, `UPDATE taskboard_bindings SET revision=?,mapping=?,updated_at=?,mapping_version=? WHERE project_id=? AND revision=?`, next.Revision, mapping, next.UpdatedAt, version, pid, expected)
		if err != nil {
			return nil, err
		}
		affected, _ = r.RowsAffected()
	}
	if affected != 1 {
		return nil, &serviceError{code: "conflict", cause: errors.New("taskboard revision conflict; refresh binding")}
	}
	if e = s.audit(ctx, tx, wid, pid, "", "taskboard", pid, op, map[string]any{"revision": next.Revision, "configured": next.Configured}); e != nil {
		return nil, e
	}
	return result(next)
}
