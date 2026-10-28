package bus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	// LinearOtherTeams lists the teams a binding names besides its write
	// teams, by access: read, comment or linked (CROSS-TEAM-BINDING-DESIGN
	// §3). LinearTeamIDs stays the write teams, so every check that knows
	// only it allows nothing more than before.
	LinearOtherTeams map[string]string
	// LinearParentTeamID is the parent team among the listed teams, for
	// display; sub-teams are never added on their own.
	LinearParentTeamID string
	// Lanes filter which write teams a session writes (§3.1); none means
	// every session writes every write team, as before.
	Lanes []Lane
	// Unpinned is the lane of a session with no pin, or LaneReadOnly; ""
	// when the binding has no lanes.
	Unpinned string
}

// Lane is a named set of write teams a pinned session writes.
type Lane struct {
	Name          string   `json:"name"`
	WriteTeamIDs  []string `json:"write_team_ids"`
	DefaultTeamID string   `json:"default_team_id"`
}

// LaneReadOnly is the unpinned setting under which a session with no pin
// writes no team.
const LaneReadOnly = "read_only"

const maxLanes = 10

var laneName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Lane is the binding's lane of that name.
func (b TaskboardBindingV2) Lane(name string) (Lane, bool) {
	for _, l := range b.Lanes {
		if l.Name == name {
			return l, true
		}
	}
	return Lane{}, false
}

type BindingV2 = TaskboardBindingV2

// Team access levels beside write (the teams in LinearTeamIDs).
const (
	TeamAccessWrite   = "write"
	TeamAccessRead    = "read"
	TeamAccessComment = "comment"
	TeamAccessLinked  = "linked"
)

// TeamAccess is the access the binding gives a team; "" when it doesn't
// list it.
func (b TaskboardBindingV2) TeamAccess(team string) string {
	for _, id := range b.LinearTeamIDs {
		if id == team {
			return TeamAccessWrite
		}
	}
	return b.LinearOtherTeams[team]
}

// TeamReadable: the team's issues are listed and read in full.
func (b TaskboardBindingV2) TeamReadable(team string) bool {
	a := b.TeamAccess(team)
	return a == TeamAccessWrite || a == TeamAccessRead || a == TeamAccessComment
}

// ListedTeamIDs are every team the binding names, sorted.
func (b TaskboardBindingV2) ListedTeamIDs() []string {
	out := append([]string(nil), b.LinearTeamIDs...)
	for id := range b.LinearOtherTeams {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CrossTeam: the binding uses version 3's fields, which a client that
// reads only version 2 must refuse rather than ignore.
func (b TaskboardBindingV2) CrossTeam() bool {
	return len(b.LinearOtherTeams) > 0 || b.LinearParentTeamID != "" || len(b.Lanes) > 0
}

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
	if e := parseCrossTeam(a, &b); e != nil {
		return b, e
	}
	return b, nil
}

// parseCrossTeam reads version 3's optional fields: the other teams by
// access, and the parent team.
func parseCrossTeam(a map[string]any, b *TaskboardBindingV2) error {
	if raw, ok := a["linear_other_teams"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return errors.New("linear_other_teams must be an object of team ID to access")
		}
		for id, v := range m {
			access, _ := v.(string)
			if !taskboardUUID.MatchString(id) || containsString(b.LinearTeamIDs, id) {
				return errors.New("linear_other_teams names canonical team UUIDs that are not write teams")
			}
			if access != TeamAccessRead && access != TeamAccessComment && access != TeamAccessLinked {
				return errors.New("a team's access is read, comment or linked")
			}
			if b.LinearOtherTeams == nil {
				b.LinearOtherTeams = map[string]string{}
			}
			b.LinearOtherTeams[id] = access
		}
		if len(b.LinearTeamIDs)+len(b.LinearOtherTeams) > 20 {
			return errors.New("a binding lists at most 20 teams in all")
		}
	}
	if raw, ok := a["linear_parent_team_id"]; ok && raw != nil {
		id, _ := raw.(string)
		if b.TeamAccess(id) == "" {
			return errors.New("the parent team must be one of the binding's teams")
		}
		b.LinearParentTeamID = id
	}
	if e := parseLanes(a, b); e != nil {
		return e
	}
	if b.CrossTeam() {
		b.MappingVersion = 3
	}
	return nil
}

// ValidateLanes applies the binding's lane rules to a link before it is
// sent: lanes over writeTeams, and the unpinned setting ("" for its
// default, read_only).
func ValidateLanes(writeTeams []string, lanes []Lane, unpinned string) error {
	b := TaskboardBindingV2{LinearTeamIDs: writeTeams}
	a := map[string]any{"lanes": LanesArg(lanes)}
	if lanes == nil {
		a["lanes"] = nil
	}
	if unpinned != "" {
		a["unpinned"] = unpinned
	}
	return parseLanes(a, &b)
}

// LanesArg is lanes as a taskboard_set_v2 argument.
func LanesArg(lanes []Lane) []any {
	out := []any{}
	for _, l := range lanes {
		ids := []any{}
		for _, id := range l.WriteTeamIDs {
			ids = append(ids, id)
		}
		out = append(out, map[string]any{"name": l.Name, "write_team_ids": ids, "default_team_id": l.DefaultTeamID})
	}
	return out
}

// parseLanes reads version 3's lanes and the unpinned setting.
func parseLanes(a map[string]any, b *TaskboardBindingV2) error {
	if raw, ok := a["lanes"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok || len(list) > maxLanes {
			return fmt.Errorf("lanes is a list of at most %d lanes", maxLanes)
		}
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok || len(m) != 3 {
				return errors.New("a lane has exactly name, write_team_ids and default_team_id")
			}
			name, _ := m["name"].(string)
			if !laneName.MatchString(name) || name == LaneReadOnly {
				return errors.New("a lane name is 1 to 32 lowercase letters, digits or dashes, starting with a letter, and not read_only")
			}
			if _, dup := b.Lane(name); dup {
				return errors.New("lane names are unique")
			}
			ids, isArray, ok := stringList(m["write_team_ids"])
			if !isArray || !ok || len(ids) == 0 {
				return errors.New("a lane's write_team_ids is a non-empty list of team IDs")
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] || !containsString(b.LinearTeamIDs, id) {
					return errors.New("a lane's write teams are distinct write teams of the binding")
				}
				seen[id] = true
			}
			sort.Strings(ids)
			def, _ := m["default_team_id"].(string)
			if !containsString(ids, def) {
				return errors.New("a lane's default team is one of its write teams")
			}
			b.Lanes = append(b.Lanes, Lane{Name: name, WriteTeamIDs: ids, DefaultTeamID: def})
		}
		sort.Slice(b.Lanes, func(i, j int) bool { return b.Lanes[i].Name < b.Lanes[j].Name })
	}
	raw, present := a["unpinned"]
	unpinned, isString := raw.(string)
	switch {
	case len(b.Lanes) == 0 && present && raw != nil:
		return errors.New("unpinned needs lanes")
	case len(b.Lanes) == 0:
	case !present || raw == nil:
		b.Unpinned = LaneReadOnly
	case !isString:
		return errors.New("unpinned is read_only or a lane name")
	case unpinned == LaneReadOnly:
		b.Unpinned = LaneReadOnly
	default:
		if _, ok := b.Lane(unpinned); !ok {
			return errors.New("unpinned is read_only or a lane name")
		}
		b.Unpinned = unpinned
	}
	return nil
}

func mappingMapV2(b TaskboardBindingV2) map[string]any {
	b.LinearTeamIDs = append([]string(nil), b.LinearTeamIDs...)
	sort.Strings(b.LinearTeamIDs)
	b.LinearProjectIDs = append([]string{}, b.LinearProjectIDs...)
	sort.Strings(b.LinearProjectIDs)
	m := map[string]any{"provider": "linear", "mode": b.Mode, "linear_team_ids": b.LinearTeamIDs, "linear_default_team_id": b.LinearDefaultTeamID, "linear_project_ids": b.LinearProjectIDs, "linear_workspace_id": nullable(b.LinearWorkspaceID)}
	// A version 2 binding keeps exactly its version 2 form, so its stored
	// mapping, its digest and what an older client reads are unchanged.
	if b.CrossTeam() {
		others := map[string]any{}
		for id, access := range b.LinearOtherTeams {
			others[id] = access
		}
		m["linear_other_teams"], m["linear_parent_team_id"] = others, nullable(b.LinearParentTeamID)
		lanes := []any{}
		for _, l := range b.Lanes {
			lanes = append(lanes, map[string]any{"name": l.Name, "write_team_ids": append([]string(nil), l.WriteTeamIDs...), "default_team_id": l.DefaultTeamID})
		}
		m["lanes"], m["unpinned"] = lanes, nullable(b.Unpinned)
	}
	return m
}
func bindingMapV2(b TaskboardBindingV2) map[string]any {
	var mapping any
	if b.Configured {
		mapping = mappingMapV2(b)
	}
	version := 2
	if b.Configured && b.CrossTeam() {
		version = 3
	}
	return map[string]any{"workspace_id": b.WorkspaceID, "project_id": b.ProjectID, "configured": b.Configured, "revision": b.Revision, "mapping_version": version, "binding": mapping, "updated_at": nullable(b.UpdatedAt)}
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
	// Version 3 adds a binding's other teams and parent. A client that
	// reads only version 2 refuses it: ignoring access levels would let it
	// write a team the binding only reads.
	n, e := integer(m["mapping_version"])
	if e != nil || n != 2 && n != 3 {
		return b, errors.New("mapping_version must be 2 or 3")
	}
	delete(m, "mapping_version")
	var mapping TaskboardBindingV2
	if configured, ok := m["configured"].(bool); ok && configured {
		a, ok := m["binding"].(map[string]any)
		fields := []string{"provider", "mode", "linear_team_ids", "linear_default_team_id", "linear_project_ids", "linear_workspace_id"}
		if n == 3 {
			fields = append(fields, "linear_other_teams", "linear_parent_team_id", "lanes", "unpinned")
		}
		if !ok || len(a) != len(fields) || a["provider"] != "linear" {
			return b, errors.New("invalid V2 mapping")
		}
		for _, k := range fields {
			if _, ok := a[k]; !ok {
				return b, errors.New("missing V2 mapping field")
			}
		}
		mapping, e = parseTaskboardMappingV2(a)
		if e != nil {
			return b, e
		}
		if mapping.CrossTeam() != (n == 3) {
			return b, errors.New("mapping_version does not match the mapping")
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
	if b.CrossTeam() {
		b.MappingVersion = 3
	}
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
	if b.Configured && (len(b.LinearProjectIDs) > 1 || b.CrossTeam()) {
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
