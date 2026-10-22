package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

const taskboardSchema = `CREATE TABLE taskboard_bindings(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision BIGINT NOT NULL CHECK(revision>0),mapping TEXT,updated_at TEXT NOT NULL);`
const maxTaskboardRevision = 9007199254740991

var taskboardUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TaskboardBinding is shared authority metadata, never a personal connection.
type TaskboardBinding struct {
	WorkspaceID               string   `json:"workspace_id"`
	ProjectID                 string   `json:"project_id"`
	Configured                bool     `json:"configured"`
	Revision                  int64    `json:"revision"`
	UpdatedAt                 string   `json:"updated_at"`
	Provider                  string   `json:"provider,omitempty"`
	LinearTeamIDs             []string `json:"linear_team_ids,omitempty"`
	LinearDefaultTeamID       string   `json:"linear_default_team_id,omitempty"`
	LinearProjectID           string   `json:"linear_project_id,omitempty"`
	LinearWorkspaceID         string   `json:"linear_workspace_id,omitempty"`
	LinearWorkspaceProvenance string   `json:"linear_workspace_provenance,omitempty"`
}

func taskboardExpected(a map[string]any) (int64, error) {
	v, ok := a["expected_revision"]
	if !ok {
		return 0, errors.New("expected_revision is required")
	}
	n, e := integer(v)
	if e != nil || n < 0 || int64(n) >= maxTaskboardRevision {
		return 0, errors.New("expected_revision must be 0–9007199254740990")
	}
	return int64(n), nil
}
func parseTaskboardMapping(a map[string]any) (TaskboardBinding, error) {
	b := TaskboardBinding{Provider: "linear", LinearDefaultTeamID: str(a, "linear_default_team_id")}
	switch ids := a["linear_team_ids"].(type) {
	case []string:
		b.LinearTeamIDs = append([]string(nil), ids...)
	case []any:
		for _, v := range ids {
			s, ok := v.(string)
			if !ok {
				return b, errors.New("linear_team_ids must contain strings")
			}
			b.LinearTeamIDs = append(b.LinearTeamIDs, s)
		}
	}
	if len(b.LinearTeamIDs) < 1 || len(b.LinearTeamIDs) > 20 {
		return b, errors.New("1–20 teams are required")
	}
	seen := map[string]bool{}
	for _, id := range b.LinearTeamIDs {
		if !taskboardUUID.MatchString(id) || seen[id] {
			return b, errors.New("team IDs must be unique canonical UUIDs")
		}
		seen[id] = true
	}
	if !seen[b.LinearDefaultTeamID] {
		return b, errors.New("default team must belong to selected teams")
	}
	if _, ok := a["linear_project_id"]; !ok {
		return b, errors.New("linear_project_id is required (UUID or null)")
	}
	for _, key := range []string{"linear_project_id", "linear_workspace_id"} {
		if v, ok := a[key]; ok && v != nil {
			s, yes := v.(string)
			if !yes || !taskboardUUID.MatchString(s) {
				return b, errors.New(key + " must be UUID or null")
			}
			if key == "linear_project_id" {
				b.LinearProjectID = s
			} else {
				b.LinearWorkspaceID = s
			}
		}
	}
	return b, nil
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func mappingMap(b TaskboardBinding) map[string]any {
	return map[string]any{"provider": "linear", "linear_team_ids": b.LinearTeamIDs, "linear_default_team_id": b.LinearDefaultTeamID, "linear_project_id": nullable(b.LinearProjectID), "linear_workspace_id": nullable(b.LinearWorkspaceID)}
}
func bindingMap(b TaskboardBinding) map[string]any {
	var mapping any
	if b.Configured {
		mapping = mappingMap(b)
	}
	return map[string]any{"workspace_id": b.WorkspaceID, "project_id": b.ProjectID, "configured": b.Configured, "revision": b.Revision, "binding": mapping, "updated_at": nullable(b.UpdatedAt)}
}
func (b TaskboardBinding) MarshalJSON() ([]byte, error) { return json.Marshal(bindingMap(b)) }
func (b *TaskboardBinding) UnmarshalJSON(raw []byte) error {
	v, e := ParseTaskboardBinding(json.RawMessage(raw))
	if e == nil {
		*b = v
	}
	return e
}

// ParseTaskboardBinding validates the exact six-field authority wire format.
func ParseTaskboardBinding(v any) (TaskboardBinding, error) {
	var b TaskboardBinding
	raw, e := json.Marshal(v)
	if e != nil || len(raw) > 10000 {
		return b, errors.New("invalid taskboard binding")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&m) != nil || len(m) != 6 {
		return b, errors.New("invalid taskboard binding fields")
	}
	for _, k := range []string{"workspace_id", "project_id", "configured", "revision", "binding", "updated_at"} {
		if _, ok := m[k]; !ok {
			return b, errors.New("missing taskboard binding field")
		}
	}
	var ok bool
	b.WorkspaceID, ok = m["workspace_id"].(string)
	if !ok || !namePattern.MatchString(b.WorkspaceID) {
		return b, errors.New("invalid workspace ID")
	}
	b.ProjectID, ok = m["project_id"].(string)
	if !ok || !namePattern.MatchString(b.ProjectID) {
		return b, errors.New("invalid project ID")
	}
	b.Configured, ok = m["configured"].(bool)
	if !ok {
		return b, errors.New("invalid configured flag")
	}
	n, e := integer(m["revision"])
	if e != nil || n < 0 {
		return b, errors.New("invalid revision")
	}
	b.Revision = int64(n)
	if m["updated_at"] != nil {
		b.UpdatedAt, ok = m["updated_at"].(string)
		if !ok || b.UpdatedAt == "" {
			return b, errors.New("invalid updated_at")
		}
	}
	if (b.Revision == 0) != (m["updated_at"] == nil) {
		return b, errors.New("invalid initial timestamp")
	}
	if !b.Configured {
		if m["binding"] != nil {
			return b, errors.New("unconfigured binding must be null")
		}
		return b, nil
	}
	a, ok := m["binding"].(map[string]any)
	if !ok || len(a) != 5 || a["provider"] != "linear" || b.Revision == 0 {
		return b, errors.New("invalid configured binding")
	}
	for _, k := range []string{"provider", "linear_team_ids", "linear_default_team_id", "linear_project_id", "linear_workspace_id"} {
		if _, ok := a[k]; !ok {
			return b, errors.New("missing mapping field")
		}
	}
	mapping, e := parseTaskboardMapping(a)
	if e != nil {
		return b, e
	}
	b.Provider = mapping.Provider
	b.LinearTeamIDs = mapping.LinearTeamIDs
	b.LinearDefaultTeamID = mapping.LinearDefaultTeamID
	b.LinearProjectID = mapping.LinearProjectID
	b.LinearWorkspaceID = mapping.LinearWorkspaceID
	return b, nil
}
