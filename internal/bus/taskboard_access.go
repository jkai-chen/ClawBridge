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
	CanWrite    bool   `json:"can_write"`
	ProjectRole string `json:"project_role"`
}

func accessMap(a TaskboardAccess) map[string]any {
	return map[string]any{"workspace_id": a.WorkspaceID, "project_id": a.ProjectID, "session_id": nullable(a.SessionID), "can_write": a.CanWrite, "project_role": a.ProjectRole}
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
	if e != nil || len(raw) > 2048 {
		return a, errors.New("invalid taskboard access")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&m) != nil || len(m) != 5 {
		return a, errors.New("invalid taskboard access fields")
	}
	for _, k := range []string{"workspace_id", "project_id", "session_id", "can_write", "project_role"} {
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
