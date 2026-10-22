package integration

import (
	"context"
	"fmt"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

type l2ObservedIntentProvider struct {
	*l2IntentProvider
	reads int
}

func (p *l2ObservedIntentProvider) IssueV2(ctx context.Context, id string) (linearprovider.IssueV2, error) {
	p.reads++
	return p.l2IntentProvider.IssueV2(ctx, id)
}

// l2SameDestinationParent shares the child's team and project, the only
// parent the board accepts.
const l2SameDestinationParent = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

func TestL2ParentExplicitIntentAndBoundedScopedAncestry(t *testing.T) {
	for _, mode := range []string{"same_destination", "clear", "cross_destination", "self", "cycle", "unknown_ancestry", "outside_scope", "no_capability", "clear_no_capability", "depth64", "depth65"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := &l2ObservedIntentProvider{l2IntentProvider: l2IntentFixture()}
			sibling := l2Issue(l2SameDestinationParent, l1TeamA, l1Project)
			sibling.ParentKnown = true
			p.issues[l2SameDestinationParent] = sibling
			parentID := l2SameDestinationParent
			in := linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: "update", OperationID: "parent-" + mode, IssueID: l1IssueID}, Parent: linearprovider.Field[*string]{Present: true, Value: &parentID}}
			switch mode {
			case "clear":
				in.Parent.Value = nil
			case "cross_destination":
				// Another allowed team and project: refused, because Linear
				// would pass that destination on to the child.
				parentID = l1StateID
			case "self":
				parentID = l1IssueID
			case "cycle":
				issue := p.issues[parentID]
				issue.ParentID = l1IssueID
				p.issues[parentID] = issue
			case "unknown_ancestry":
				issue := p.issues[parentID]
				issue.ParentKnown = false
				p.issues[parentID] = issue
			case "outside_scope":
				issue := p.issues[parentID]
				issue.TeamID = l1Workspace
				p.issues[parentID] = issue
			case "no_capability":
				p.caps.ParentSameDestination = false
			case "clear_no_capability":
				in.Parent.Value = nil
				p.caps.ParentSameDestination = false
			case "depth64", "depth65":
				count := 64
				if mode == "depth65" {
					count = 65
				}
				ids := []string{}
				for i := 0; i < count; i++ {
					ids = append(ids, fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 100+i))
				}
				parentID = ids[0]
				for i, id := range ids {
					issue := l2Issue(id, l1TeamA, l1Project)
					issue.ParentKnown = true
					if i+1 < len(ids) {
						issue.ParentID = ids[i+1]
					}
					p.issues[id] = issue
				}
			}
			_, err := l1ServiceFor(t, f.store, p).WriteV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), in)
			allowed := mode == "same_destination" || mode == "clear" || mode == "depth64"
			if !allowed {
				if err == nil || len(p.updates) != 0 {
					t.Fatal("unsafe/incomplete parent dispatched", err)
				}
				if p.reads > 66 {
					t.Fatal("refused ancestry exceeded bounded reads", p.reads)
				}
				return
			}
			if err != nil || len(p.updates) != 1 {
				t.Fatal("supported explicit parent intent refused", err)
			}
			got := p.updates[0]
			if !got.Parent.Present || (in.Parent.Value == nil) != (got.Parent.Value == nil) {
				t.Fatal("explicit parent set/clear lost")
			}
			if got.Parent.Value != nil && *got.Parent.Value != parentID {
				t.Fatal("parent identity changed")
			}
			if got.Project.Present || got.Assignee.Present || got.Labels.Present {
				t.Fatal("parent mutation invented inheritance, destination/assignee or label changes")
			}
			if p.reads > 132 {
				t.Fatal("ancestry refresh exceeded bounded64-per-pass reads", p.reads)
			}
		})
	}
}

func TestL2AssigneeExplicitEligibilitySetClearAndOmission(t *testing.T) {
	for _, mode := range []string{"active_member", "clear", "omitted", "inactive", "eligibility_unknown", "foreign_team", "not_member", "no_capability"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := l2IntentFixture()
			assigneeID := l2LabelA
			p.members = []linearprovider.Member{{ID: assigneeID, TeamID: l1TeamA, Active: true, EligibilityKnown: true}}
			in := linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: "update", OperationID: "assignee-" + mode, IssueID: l1IssueID}, Assignee: linearprovider.Field[*string]{Present: true, Value: &assigneeID}}
			switch mode {
			case "clear":
				in.Assignee.Value = nil
			case "omitted":
				in.Assignee = linearprovider.Field[*string]{}
				in.Parent = linearprovider.Field[*string]{Present: true}
			case "inactive":
				p.members[0].Active = false
			case "eligibility_unknown":
				p.members[0].EligibilityKnown = false
			case "foreign_team":
				p.members[0].TeamID = l1TeamB
			case "not_member":
				p.members = []linearprovider.Member{}
			case "no_capability":
				p.caps.Assignee = false
			}
			_, err := l1ServiceFor(t, f.store, p).WriteV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), in)
			allowed := mode == "active_member" || mode == "clear" || mode == "omitted"
			if !allowed {
				if err == nil || len(p.updates) != 0 {
					t.Fatal("unverified assignee dispatched", err)
				}
				return
			}
			if err != nil || len(p.updates) != 1 {
				t.Fatal("explicit set/clear or omitted assignee intent lost", err)
			}
			got := p.updates[0]
			if got.Assignee.Present != in.Assignee.Present || (got.Assignee.Value == nil) != (in.Assignee.Value == nil) {
				t.Fatal("omitted assignee became clear or explicit clear lost")
			}
			if got.Assignee.Value != nil && *got.Assignee.Value != assigneeID {
				t.Fatal("assignee canonical identity changed")
			}
			if got.Project.Present || got.Labels.Present {
				t.Fatal("assignee intent invented destination or label changes")
			}
		})
	}
}
