package linearboard

import (
	"context"
	"testing"

	"clawbridge/internal/linearprovider"
)

func TestLinearV2IdentifierFormEdgeIsObservedOnAdd(t *testing.T) {
	s, _, p, scope := testV2(t)
	from := p.issues[issueID]
	from.Identifier = "SYN-1"
	from.Relations = []linearprovider.Relation{{Type: "blocks", FromID: issueID, FromIdentifier: "SYN-1", ToIdentifier: "SYN-2"}}
	p.issues[issueID] = from
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, Identifier: "SYN-2", TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true}
	p.caps.RelationAdd = false
	r, e := s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "identifier-edge", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA})
	if e != nil || r.(ReceiptV2).Outcome != "observed_unchanged" || len(p.edges) != 0 {
		t.Fatal("identifier-form existing edge not observed", r, e)
	}
	// Without a completeness proof, the same visible list cannot prove a
	// different edge absent: with no remove capability it must refuse.
	p.caps.RelationRemove = false
	if _, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "identifier-remove", Action: "remove", Type: "related", FromID: issueID, ToID: parentA}); e == nil || len(p.edges) != 0 {
		t.Fatal("incomplete native edge list proved absence", e)
	}
	// An edge to an issue outside this request is ignored, not an error.
	from.Relations = append(from.Relations, linearprovider.Relation{Type: "related", FromID: issueID, FromIdentifier: "SYN-1", ToIdentifier: "SYN-99"})
	p.issues[issueID] = from
	if _, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "unrelated-edge", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}); e != nil {
		t.Fatal("edge to another issue broke relation evidence", e)
	}
}
