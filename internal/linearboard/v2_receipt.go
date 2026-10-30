package linearboard

import (
	"bytes"
	"encoding/json"
	"io"

	"clawbridge/internal/linearprovider"
)

func validReceiptV2(raw []byte) bool {
	var r ReceiptV2
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return false
	}
	if r.Outcome != "accepted" && r.Outcome != "observed_unchanged" {
		return false
	}
	// A duplicate edge (agent_cancel.go) is only ever added, and no read
	// shows it, so its receipt may name it unverified.
	duplicate := r.Kind == "edge" && r.Type == "duplicate" && r.Action == "add"
	if duplicate && r.Unverified != nil {
		return len(r.Unverified) == 1 && r.Unverified[0] == "relation" && r.Outcome == "accepted" && validEdgeV2(r)
	}
	fields := map[string]bool{}
	for _, f := range r.Unverified {
		if fields[f] || (f != "labels" && f != "assignee" && f != "parent" && f != "state" && f != "priority") {
			return false
		}
		fields[f] = true
	}
	if r.Unverified != nil && (len(r.Unverified) == 0 || r.Kind != "issue" || r.Outcome != "accepted") {
		return false
	}
	deltas := func(ids []string) bool {
		if ids == nil {
			return true
		}
		_, e := sortedUniqueIDs(ids, 20)
		return e == nil && len(ids) > 0
	}
	if (r.LabelsAdded != nil || r.LabelsRemoved != nil) && (r.Kind != "issue" || r.Outcome != "accepted" || !deltas(r.LabelsAdded) || !deltas(r.LabelsRemoved)) {
		return false
	}
	if r.InResponseTo != "" && (r.Kind != "comment" || !linearprovider.ValidID(r.InResponseTo)) {
		return false
	}
	switch r.Kind {
	case "issue":
		return linearprovider.ValidID(r.ID) && linearprovider.ValidID(r.TeamID) && (r.ProjectID == nil || linearprovider.ValidID(*r.ProjectID)) && r.IssueID == "" && r.Action == "" && r.Type == "" && r.FromID == "" && r.ToID == ""
	case "comment":
		return r.Outcome == "accepted" && linearprovider.ValidID(r.ID) && linearprovider.ValidID(r.IssueID) && r.TeamID == "" && r.ProjectID == nil && r.Action == "" && r.Type == "" && r.FromID == "" && r.ToID == ""
	case "edge":
		return (r.Type != "duplicate" || duplicate) && validEdgeV2(r)
	}
	return false
}

func validEdgeV2(r ReceiptV2) bool {
	return (r.ID == "" || linearprovider.ValidID(r.ID)) && (r.Action == "add" || r.Action == "remove") && (r.Type == "blocks" || r.Type == "related" || r.Type == "duplicate") && linearprovider.ValidID(r.FromID) && linearprovider.ValidID(r.ToID) && r.FromID != r.ToID && (r.Type != "related" || r.FromID < r.ToID) && r.TeamID == "" && r.ProjectID == nil && r.IssueID == ""
}
