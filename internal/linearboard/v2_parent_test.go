package linearboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

const grandparentA = "88888888-8888-4888-8888-888888888888"

func parentIssueV2(id, identifier, team, project, parentIdentifier string) linearprovider.IssueV2 {
	return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: identifier, TeamID: team, ProjectID: project}, ProjectKnown: true, ParentKnown: true, ParentIdentifier: parentIdentifier}
}
func setParentV2(op string, parent *string) WriteInputV2 {
	return WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issueID}, Parent: linearprovider.Field[*string]{Present: true, Value: parent}}
}

func TestLinearV2ParentMustShareTheChildDestination(t *testing.T) {
	// Each parent is itself inside the binding; only its destination differs.
	for name, parent := range map[string]linearprovider.IssueV2{
		"other team and project":   parentIssueV2(parentA, "SYN-2", otherTeamID, projectB, ""),
		"same team, other project": parentIssueV2(parentA, "SYN-2", teamID, projectB, ""),
	} {
		s, a, p, scope := testV2(t)
		p.projects[projectB] = []string{teamID, otherTeamID}
		p.issues[parentA] = parent
		in := setParentV2("cross", ptrV2(parentA))
		if _, e := s.WriteV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "share the child's team and project") || p.writes != 0 || ledgerStateV2(t, s, a, in.OperationID) != "absent" {
			t.Fatalf("%s parent accepted: %v", name, e)
		}
	}
}

func TestLinearV2ParentAncestryResolvesDisplayIdentifiers(t *testing.T) {
	// Native get_issue reports parentId as the parent's identifier.
	s, _, p, scope := testV2(t)
	child := p.issues[issueID]
	child.Identifier = "SYN-1"
	p.issues[issueID] = child
	p.issues[parentA] = parentIssueV2(parentA, "SYN-2", teamID, projectA, "SYN-3")
	p.issues[grandparentA] = parentIssueV2(grandparentA, "SYN-3", teamID, projectA, "")
	if _, e := s.WriteV2(context.Background(), scope, setParentV2("identifier-ancestry", ptrV2(parentA))); e != nil || p.writes != 1 {
		t.Fatal("identifier-form ancestry or receipt refused", e)
	}
	// The fake's receipt reports the parent as "SYN-2", so success above means
	// the identifier-form receipt matched the verified parent.
	// A cycle closed through an identifier is still a cycle.
	s, _, p, scope = testV2(t)
	child = p.issues[issueID]
	child.Identifier = "SYN-1"
	p.issues[issueID] = child
	p.issues[parentA] = parentIssueV2(parentA, "SYN-2", teamID, projectA, "SYN-3")
	p.issues[grandparentA] = parentIssueV2(grandparentA, "SYN-3", teamID, projectA, "SYN-1")
	if _, e := s.WriteV2(context.Background(), scope, setParentV2("identifier-cycle", ptrV2(parentA))); e == nil || !strings.Contains(e.Error(), "cycle") || p.writes != 0 {
		t.Fatal("identifier-form cycle accepted", e)
	}
}

// tamperedParentV2 returns a receipt whose parent differs from the request.
type tamperedParentV2 struct {
	*providerV2
	identifier string
	keep       bool
}

func (p tamperedParentV2) UpdateV2(ctx context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	i, e := p.providerV2.UpdateV2(ctx, in)
	if !p.keep {
		i.ParentKnown, i.ParentID, i.ParentIdentifier = true, "", p.identifier
	}
	return i, e
}

func TestLinearV2ParentReceiptAndClear(t *testing.T) {
	s, _, p, scope := testV2(t)
	p.issues[parentA] = parentIssueV2(parentA, "SYN-2", teamID, projectA, "")
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		return tamperedParentV2{providerV2: p, identifier: "SYN-9"}, nil
	}
	if _, e := s.WriteV2(context.Background(), scope, setParentV2("wrong-receipt", ptrV2(parentA))); e == nil || !strings.Contains(e.Error(), "uncertain") || p.writes != 1 {
		t.Fatal("mismatched receipt parent accepted", e)
	}
	// Clear is enabled and its receipt must report no parent.
	s, _, p, scope = testV2(t)
	child := p.issues[issueID]
	child.ParentIdentifier = "SYN-2"
	p.issues[issueID] = child
	if _, e := s.WriteV2(context.Background(), scope, setParentV2("clear", nil)); e != nil || p.writes != 1 || !p.updated[0].Parent.Present || p.updated[0].Parent.Value != nil {
		t.Fatal("parent clear refused or not explicit", e, p.updated)
	}
	s, _, p, scope = testV2(t)
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		return tamperedParentV2{providerV2: p, identifier: "SYN-2"}, nil
	}
	if _, e := s.WriteV2(context.Background(), scope, setParentV2("clear-ignored", nil)); e == nil || !strings.Contains(e.Error(), "uncertain") {
		t.Fatal("receipt still parented after clear accepted", e)
	}
}

func TestLinearV2ParentRequiresSameDestinationCapability(t *testing.T) {
	s, _, p, scope := testV2(t)
	p.issues[parentA] = parentIssueV2(parentA, "SYN-2", teamID, projectA, "")
	p.caps.ParentSameDestination = false
	for _, parent := range []*string{ptrV2(parentA), nil} {
		if _, e := s.WriteV2(context.Background(), scope, setParentV2("no-capability", parent)); e == nil || p.writes != 0 {
			t.Fatal("parent mutation without capability", e)
		}
	}
}
