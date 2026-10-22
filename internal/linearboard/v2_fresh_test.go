package linearboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"clawbridge/internal/linearprovider"
)

func TestLinearV2ProofsRefreshInsideOperationLockBeforeMutation(t *testing.T) {
	for _, mode := range []string{"member revoked", "source moved", "new human label", "parent unavailable"} {
		t.Run(mode, func(t *testing.T) {
			s, a, p, scope := testV2(t)
			in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "fresh-proof", IssueID: issueID}}
			p.members = []linearprovider.Member{{ID: memberA, TeamID: teamID, Active: true, EligibilityKnown: true}}
			switch mode {
			case "member revoked":
				in.Assignee = linearprovider.Field[*string]{Present: true, Value: ptrV2(memberA)}
			case "source moved":
				in.Title = ptrV2("explicit title")
				in.Assignee = linearprovider.Field[*string]{Present: true, Value: nil}
			case "new human label":
				in.Labels = linearprovider.Field[[]string]{Present: true, Value: []string{}}
			case "parent unavailable":
				in.Parent = linearprovider.Field[*string]{Present: true, Value: ptrV2(parentA)}
				p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, ParentKnown: true}
			}
			a.afterAccess = func(n int) {
				if n != 2 {
					return
				}
				switch mode {
				case "member revoked":
					p.members[0].Active = false
				case "source moved":
					i := p.issues[issueID]
					i.TeamID = otherTeamID
					i.ProjectID = projectB
					p.issues[issueID] = i
				case "new human label":
					i := p.issues[issueID]
					i.LabelIDs = []string{labelA}
					p.issues[issueID] = i
				case "parent unavailable":
					i := p.issues[parentA]
					i.ParentKnown = false
					p.issues[parentA] = i
				}
			}
			if _, e := s.WriteV2(context.Background(), scope, in); e == nil {
				t.Fatal("stale proof dispatched")
			}
			if p.writes != 0 {
				t.Fatal("provider mutation occurred after revocation/scope change")
			}
			if entries, _ := os.ReadDir(filepath.Join(s.cfg.LedgerDirectory, digest(digest([]string{s.cfg.Authority, a.b.WorkspaceID, a.b.ProjectID, a.actor})))); len(entries) != 0 {
				t.Fatal("never-dispatched refusal left a pending record", entries)
			}
			// The same operation ID is re-evaluated against the now-current
			// state: still-invalid intent stays refused, valid intent dispatches once.
			_, e := s.WriteV2(context.Background(), scope, in)
			switch mode {
			case "member revoked", "parent unavailable":
				if e == nil || p.writes != 0 {
					t.Fatal("refreshed refusal was bypassed on retry", e)
				}
			default:
				if e != nil || p.writes != 1 {
					t.Fatal("never-dispatched operation ID was burned", e, p.writes)
				}
			}
		})
	}
}
func TestLinearV2ObservedRelationEvidenceIsRefreshedInsideLock(t *testing.T) {
	for _, action := range []string{"add", "remove"} {
		t.Run(action, func(t *testing.T) {
			s, a, p, scope := testV2(t)
			p.caps.RelationAdd = false
			p.caps.RelationRemove = false
			from := p.issues[issueID]
			from.RelationsKnown = true
			edge := linearprovider.Relation{Type: "blocks", FromID: issueID, ToID: parentA}
			if action == "add" {
				from.Relations = []linearprovider.Relation{edge}
			}
			p.issues[issueID] = from
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
			a.afterAccess = func(n int) {
				if n != 2 {
					return
				}
				i := p.issues[issueID]
				if action == "add" {
					i.Relations = nil
				} else {
					i.Relations = []linearprovider.Relation{edge}
				}
				p.issues[issueID] = i
			}
			if _, e := s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "fresh-edge", Action: action, Type: "blocks", FromID: issueID, ToID: parentA}); e == nil {
				t.Fatal("stale observed relation reported unchanged")
			}
			if len(p.edges) != 0 {
				t.Fatal("missing safe edge capability dispatched")
			}
		})
	}
}
func TestLinearV2KnownEdgeReceiptSurvivesSaveFaultAndMappingChange(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true}
	operationID := "edge-save-fault"
	namespace := digest([]string{s.cfg.Authority, a.b.WorkspaceID, a.b.ProjectID, a.actor})
	path := filepath.Join(s.cfg.LedgerDirectory, digest(namespace), digest(operationID)+".json")
	var fault error
	p.after = func() {
		a.b.Revision++
		if fault = os.Remove(path); fault == nil {
			fault = os.Mkdir(path, 0700)
		}
	}
	r, e := s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: operationID, Action: "add", Type: "blocks", FromID: issueID, ToID: parentA})
	if fault != nil {
		t.Fatal(fault)
	}
	var receiptError *ReceiptError
	if r == nil || !errors.As(e, &receiptError) || receiptError.Outcome != "binding_changed_ledger_unrecorded" {
		t.Fatal("known V2 receipt/context lost on save failure", r, e)
	}
	if receipt := r.(ReceiptV2); receipt.ID != "" || receipt.FromID != issueID || receipt.ToID != parentA {
		t.Fatal("edge UUID fabricated or normalized receipt lost", receipt)
	}
	if _, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: operationID, Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}); e == nil || p.writes != 1 {
		t.Fatal("save-uncertain operation replayed")
	}
}
