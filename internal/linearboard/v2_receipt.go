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
	switch r.Kind {
	case "issue":
		return r.Outcome == "accepted" && linearprovider.ValidID(r.ID) && linearprovider.ValidID(r.TeamID) && (r.ProjectID == nil || linearprovider.ValidID(*r.ProjectID)) && r.IssueID == "" && r.Action == "" && r.Type == "" && r.FromID == "" && r.ToID == ""
	case "comment":
		return r.Outcome == "accepted" && linearprovider.ValidID(r.ID) && linearprovider.ValidID(r.IssueID) && r.TeamID == "" && r.ProjectID == nil && r.Action == "" && r.Type == "" && r.FromID == "" && r.ToID == ""
	case "edge":
		return (r.ID == "" || linearprovider.ValidID(r.ID)) && (r.Action == "add" || r.Action == "remove") && (r.Type == "blocks" || r.Type == "related") && linearprovider.ValidID(r.FromID) && linearprovider.ValidID(r.ToID) && r.FromID != r.ToID && (r.Type != "related" || r.FromID < r.ToID) && r.TeamID == "" && r.ProjectID == nil && r.IssueID == ""
	}
	return false
}
