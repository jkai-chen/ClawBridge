package bus

import (
	"bytes"
	"encoding/json"
	"errors"
)

// TaskboardAccess reflects current ClawBridge permission, never Linear permission.
type TaskboardAccess struct {
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	SessionID   string `json:"session_id"`
	// SessionName is the actor session's name, so a client that addressed
	// the session by ID can still show its name (claim markers).
	SessionName string `json:"session_name"`
	CanWrite    bool   `json:"can_write"`
	ProjectRole string `json:"project_role"`
	// Engineer names the person behind the session for agent markers, when
	// the client asked for it (engineer: 1).
	Engineer string `json:"engineer,omitempty"`
	// Lane is the session's lane, or LaneReadOnly, when the client asked
	// (lane_scope: 1) and the binding has lanes; LaneWriteTeamIDs are the teams
	// it writes, empty when read-only, and nil when the server named no
	// lane.
	Lane             string   `json:"lane,omitempty"`
	LaneWriteTeamIDs []string `json:"lane_write_team_ids,omitempty"`
	// Coordinator is true when the client asked (coordinator_scope: 1) and
	// the session is the project's active designated coordinator.
	Coordinator bool `json:"coordinator,omitempty"`
}

func accessMap(a TaskboardAccess) map[string]any {
	m := map[string]any{"workspace_id": a.WorkspaceID, "project_id": a.ProjectID, "session_id": nullable(a.SessionID), "session_name": nullable(a.SessionName), "can_write": a.CanWrite, "project_role": a.ProjectRole}
	if a.Engineer != "" {
		m["engineer"] = a.Engineer
	}
	if a.LaneWriteTeamIDs != nil {
		m["lane"], m["lane_write_team_ids"] = a.Lane, append([]string{}, a.LaneWriteTeamIDs...)
	}
	if a.Coordinator {
		m["coordinator"] = true
	}
	return m
}
func (a TaskboardAccess) MarshalJSON() ([]byte, error) { return json.Marshal(accessMap(a)) }
func (a *TaskboardAccess) UnmarshalJSON(raw []byte) error {
	v, e := ParseTaskboardAccess(json.RawMessage(raw))
	if e == nil {
		*a = v
	}
	return e
}
func ParseTaskboardAccess(v any) (TaskboardAccess, error) {
	var a TaskboardAccess
	raw, e := json.Marshal(v)
	if e != nil || len(raw) > 4096 {
		return a, errors.New("invalid taskboard access")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&m) != nil {
		return a, errors.New("invalid taskboard access fields")
	}
	extra := 0
	if m["engineer"] != nil {
		extra++
	}
	_, lane := m["lane"]
	_, teams := m["lane_write_team_ids"]
	if lane != teams {
		return a, errors.New("an access lane comes with its write teams")
	}
	if lane {
		extra += 2
		name, ok := m["lane"].(string)
		ids, isArray, listOK := stringList(m["lane_write_team_ids"])
		if !ok || !(laneName.MatchString(name) || name == LaneReadOnly) || !isArray || !listOK || len(ids) > 20 {
			return a, errors.New("invalid access lane")
		}
		for _, id := range ids {
			if !taskboardUUID.MatchString(id) {
				return a, errors.New("invalid access lane team")
			}
		}
		if ids == nil {
			ids = []string{}
		}
		a.Lane, a.LaneWriteTeamIDs = name, ids
	}
	if v, present := m["coordinator"]; present {
		if v != true || m["session_id"] == nil {
			return a, errors.New("invalid access coordinator")
		}
		extra++
		a.Coordinator = true
	}
	if len(m) != 6+extra {
		return a, errors.New("invalid taskboard access fields")
	}
	if v, present := m["engineer"]; present {
		e, ok := v.(string)
		if !ok || !ValidEngineer(e) {
			return a, errors.New("invalid access engineer")
		}
		a.Engineer = e
	}
	for _, k := range []string{"workspace_id", "project_id", "session_id", "session_name", "can_write", "project_role"} {
		if _, ok := m[k]; !ok {
			return a, errors.New("missing taskboard access field")
		}
	}
	var ok bool
	a.WorkspaceID, ok = m["workspace_id"].(string)
	if !ok || !namePattern.MatchString(a.WorkspaceID) {
		return a, errors.New("invalid access workspace")
	}
	a.ProjectID, ok = m["project_id"].(string)
	if !ok || !namePattern.MatchString(a.ProjectID) {
		return a, errors.New("invalid access project")
	}
	if m["session_id"] != nil {
		a.SessionID, ok = m["session_id"].(string)
		if !ok || !namePattern.MatchString(a.SessionID) {
			return a, errors.New("invalid access session")
		}
	}
	if m["session_name"] != nil {
		a.SessionName, ok = m["session_name"].(string)
		if !ok || !namePattern.MatchString(a.SessionName) || a.SessionID == "" {
			return a, errors.New("invalid access session name")
		}
	} else if a.SessionID != "" {
		return a, errors.New("access session has no name")
	}
	a.CanWrite, ok = m["can_write"].(bool)
	if !ok {
		return a, errors.New("invalid can_write")
	}
	a.ProjectRole, ok = m["project_role"].(string)
	if !ok {
		return a, errors.New("invalid access role")
	}
	switch a.ProjectRole {
	case "owner", "lead", "member":
	case "reader":
		if a.CanWrite {
			return a, errors.New("reader cannot write")
		}
	default:
		return a, errors.New("invalid access role")
	}
	return a, nil
}
