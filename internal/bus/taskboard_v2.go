package bus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	ids, isArray, ok := stringList(a["linear_project_ids"])
	switch {
	case !isArray:
		return b, errors.New("project IDs must be an array")
	case !ok:
		return b, errors.New("project IDs must be strings")
	case ids == nil:
		return b, errors.New("project IDs must not be null")
	}
	b.LinearProjectIDs = ids
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

// bindingUnreadable refuses every use of a stored binding this binary cannot
// parse. Nothing is guessed from it; only a project owner's explicit repair
// clear (at the stored revision) replaces it. A well-formed mapping this
// binary cannot read may come from a newer ClawBridge (after a downgrade), so
// that refusal says to upgrade rather than repair, which would delete it.
func bindingUnreadable(revision int64, unknownFormat bool) error {
	if unknownFormat {
		return &serviceError{code: "binding_unreadable", cause: fmt.Errorf("this project's task board binding (revision %d) is in a format this ClawBridge cannot read and is not used; after a downgrade, upgrade instead of repairing; else an owner can run: clawbridge linear unlink --expected-revision %d --confirm --repair", revision, revision), public: true}
	}
	return &serviceError{code: "binding_unreadable", cause: fmt.Errorf("this project's task board binding (revision %d) is unreadable and is not used; a project owner can clear it with: clawbridge linear unlink --expected-revision %d --confirm --repair", revision, revision), public: true}
}

// errMappingFormat marks a stored mapping that is a JSON object this binary
// cannot read, as opposed to one that is not a JSON object at all.
var errMappingFormat = errors.New("unknown taskboard mapping format")

// readStoredMapping parses a stored binding mapping of either version.
func readStoredMapping(mapping string, version int) (TaskboardBindingV2, error) {
	var saved map[string]any
	if json.Unmarshal([]byte(mapping), &saved) != nil || saved == nil {
		return TaskboardBindingV2{}, errors.New("corrupt taskboard mapping")
	}
	b, err := parseStoredMapping(saved, version)
	if err != nil {
		return b, errMappingFormat
	}
	return b, nil
}

func parseStoredMapping(saved map[string]any, version int) (TaskboardBindingV2, error) {
	switch version {
	case 1:
		old, err := parseTaskboardMapping(saved)
		return convertLegacyBinding(old), err
	case 2:
		return parseTaskboardMappingV2(saved)
	}
	return TaskboardBindingV2{}, errors.New("unsupported stored mapping version")
}

func (s *Store) taskboard(ctx context.Context, tx *transaction, wid, pid, role, op string, a map[string]any) (any, error) {
	rows, e := query(ctx, tx, `SELECT revision,mapping,updated_at,mapping_version,names FROM taskboard_bindings WHERE project_id=?`, pid)
	if e != nil {
		return nil, e
	}
	b := TaskboardBindingV2{WorkspaceID: wid, ProjectID: pid, MappingVersion: 2}
	// unreadable is the digest of a stored mapping that does not parse;
	// unknownFormat says it is a JSON object, possibly from a newer binary.
	unreadable := ""
	unknownFormat := false
	prevNames := storedNames(nil)
	if len(rows) > 0 {
		prevNames = storedNames(rows[0]["names"])
		b.Revision = rows[0]["revision"].(int64)
		b.UpdatedAt = rows[0]["updated_at"].(string)
		if mapping, ok := rows[0]["mapping"].(string); ok {
			parsed, err := readStoredMapping(mapping, int(rows[0]["mapping_version"].(int64)))
			if err != nil {
				sum := sha256.Sum256([]byte(mapping))
				unreadable = hex.EncodeToString(sum[:])
				unknownFormat = errors.Is(err, errMappingFormat)
			} else {
				parsed.WorkspaceID = wid
				parsed.ProjectID = pid
				parsed.Revision = b.Revision
				parsed.UpdatedAt = b.UpdatedAt
				parsed.Configured = true
				b = parsed
			}
		}
	}
	repair, _ := a["repair"].(bool)
	if unreadable != "" && (!repair || role != "owner" || (op != "taskboard_clear" && op != "taskboard_clear_v2")) {
		return nil, bindingUnreadable(b.Revision, unknownFormat)
	}
	if repair && unreadable == "" {
		return nil, invalid("repair clears only an unreadable task board binding; this one is readable, so unlink it normally")
	}
	if op == "taskboard_names_set" {
		expected, e := taskboardExpected(a)
		if e != nil {
			return nil, e
		}
		if expected != b.Revision {
			return nil, &serviceError{code: "conflict", cause: errors.New("taskboard revision conflict; refresh binding")}
		}
		return s.setNames(ctx, tx, wid, pid, b, prevNames, a)
	}
	v2 := strings.HasSuffix(op, "_v2")
	if !v2 && unreadable == "" {
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
	// A binding pinned to a Linear workspace keeps it: a set may not change
	// or clear the pin; only unlinking (clear) resets it.
	if (op == "taskboard_set_v2" || op == "taskboard_set") && b.Configured && b.LinearWorkspaceID != "" && next.LinearWorkspaceID != b.LinearWorkspaceID {
		return nil, invalid("this binding is pinned to its Linear workspace; a link must name the same workspace, or unlink it first")
	}
	next.WorkspaceID = wid
	next.ProjectID = pid
	next.Revision = expected + 1
	next.UpdatedAt = stamp()
	// A link keeps or replaces the names; unlinking leaves no ID bound, so
	// it clears them.
	n, e := nextNames(a, prevNames, next, next.UpdatedAt)
	if e != nil {
		return nil, e
	}
	names := namesColumn(n)
	var affected int64
	if len(rows) == 0 {
		r, err := tx.ExecContext(ctx, `INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at,mapping_version,names) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id) DO NOTHING`, pid, next.Revision, mapping, next.UpdatedAt, version, names)
		if err != nil {
			return nil, err
		}
		affected, _ = r.RowsAffected()
	} else {
		r, err := tx.ExecContext(ctx, `UPDATE taskboard_bindings SET revision=?,mapping=?,updated_at=?,mapping_version=?,names=? WHERE project_id=? AND revision=?`, next.Revision, mapping, next.UpdatedAt, version, names, pid, expected)
		if err != nil {
			return nil, err
		}
		affected, _ = r.RowsAffected()
	}
	if affected != 1 {
		return nil, &serviceError{code: "conflict", cause: errors.New("taskboard revision conflict; refresh binding")}
	}
	details := map[string]any{"revision": next.Revision, "configured": next.Configured}
	if unreadable != "" {
		details["repaired_unreadable_sha256"] = unreadable
	}
	// A3: projects the same link is about to set who closes (closes_pending)
	// get no people-default notice; the audit records them (§10.8 L2).
	var pending []string
	if op == "taskboard_set_v2" {
		list, _, _ := stringList(a["closes_pending"])
		for _, id := range list {
			if !lowerUUIDPattern.MatchString(id) || !containsString(next.LinearProjectIDs, id) {
				return nil, invalid("closes_pending names Linear projects of this link")
			}
		}
		pending = list
		if len(pending) > 0 {
			details["closes_pending"] = pending
		}
	}
	if e = s.audit(ctx, tx, wid, pid, "", "taskboard", pid, op, details); e != nil {
		return nil, e
	}
	if op == "taskboard_set_v2" && next.Mode == "project_set" {
		var added []string
		for _, id := range next.LinearProjectIDs {
			if !(b.Configured && b.Mode == "project_set" && containsString(b.LinearProjectIDs, id)) {
				added = append(added, id)
			}
		}
		if e = s.closePolicyNotices(ctx, tx, wid, pid, added, pending, next.UpdatedAt); e != nil {
			return nil, e
		}
	}
	return result(next)
}
