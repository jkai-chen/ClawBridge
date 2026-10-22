package integration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

const l2LabelA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const l2LabelB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

type l2IntentProvider struct {
	*l2ScopedProvider
	labels           []linearprovider.Label
	members          []linearprovider.Member
	caps             linearprovider.Capabilities
	updates          []linearprovider.UpdateInputV2
	edges            []linearprovider.RelationInput
	lostEdgeResponse bool
}

func l2IntentFixture() *l2IntentProvider {
	p := &l2IntentProvider{l2ScopedProvider: l2ScopedFixture()}
	p.caps = linearprovider.Capabilities{Labels: true, Members: true, CreateProject: true, UpdateProject: true, ReplaceLabels: true, Parent: true, Assignee: true, ParentPreservesDestination: true, RelationAdd: true, RelationRemove: true}
	p.labels = []linearprovider.Label{{ID: l2LabelA, TeamID: l1TeamA, EligibilityKnown: true}, {ID: l2LabelB, TeamID: l1TeamA, EligibilityKnown: true}}
	issue := l2Issue(l1IssueID, l1TeamA, l1Project)
	issue.LabelIDs = []string{l2LabelA, l2LabelB}
	issue.LabelsKnown = true
	issue.ParentKnown = true
	issue.AssigneeKnown = true
	issue.RelationsKnown = true
	p.issues[l1IssueID] = issue
	other := l2Issue(l1StateID, l1TeamB, l2ProjectB)
	other.ParentKnown = true
	other.RelationsKnown = true
	p.issues[l1StateID] = other
	return p
}
func (p *l2IntentProvider) Capabilities(context.Context) (linearprovider.Capabilities, error) {
	return p.caps, nil
}
func (p *l2IntentProvider) Labels(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	return linearprovider.Page[linearprovider.Label]{Items: p.labels, Complete: true}, nil
}
func (p *l2IntentProvider) Members(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Member], error) {
	return linearprovider.Page[linearprovider.Member]{Items: p.members, Complete: true}, nil
}
func (p *l2IntentProvider) UpdateV2(_ context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	p.updates = append(p.updates, in)
	issue := p.issues[in.ID]
	if in.Project.Present {
		issue.ProjectID = ""
		if in.Project.Value != nil {
			issue.ProjectID = *in.Project.Value
		}
	}
	if in.Labels.Present {
		issue.LabelIDs = append([]string{}, in.Labels.Value...)
	}
	if in.Parent.Present {
		issue.ParentID = ""
		if in.Parent.Value != nil {
			issue.ParentID = *in.Parent.Value
		}
	}
	if in.Assignee.Present {
		issue.AssigneeID = ""
		if in.Assignee.Value != nil {
			issue.AssigneeID = *in.Assignee.Value
		}
	}
	return issue, nil
}
func (p *l2IntentProvider) Relation(_ context.Context, in linearprovider.RelationInput) (linearprovider.RelationReceipt, error) {
	p.edges = append(p.edges, in)
	if p.lostEdgeResponse {
		return linearprovider.RelationReceipt{}, errors.New("synthetic response lost AFTER one accepted edge dispatch")
	}
	return linearprovider.RelationReceipt{Action: in.Action, Type: in.Type, FromID: in.FromID, ToID: in.ToID}, nil
}

type l2ReadConnections struct{ l1SyntheticConnections }

func (l2ReadConnections) Status(_ context.Context, id string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: id, Access: linearconn.Read}, nil
}

func TestL2ExplicitLabelsReplaceClearAndOmissionPreserve(t *testing.T) {
	for _, intent := range []string{"replace", "clear", "omitted", "stale_expectation", "unknown_existing", "unsafe_label", "unknown_eligibility"} {
		t.Run(intent, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := l2IntentFixture()
			// Replacement must state the complete set the caller last read.
			in := linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: "update", OperationID: "explicit-label-" + intent, IssueID: l1IssueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelB}}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelA, l2LabelB}}}
			switch intent {
			case "clear":
				in.Labels.Value = []string{}
			case "omitted":
				in.Labels = linearprovider.Field[[]string]{}
				in.ExpectedLabels = linearprovider.Field[[]string]{}
				title := "explicit title only"
				in.Title = &title
			case "stale_expectation":
				in.ExpectedLabels.Value = []string{l2LabelA}
			case "unknown_existing":
				issue := p.issues[l1IssueID]
				issue.LabelsKnown = false
				p.issues[l1IssueID] = issue
			case "unsafe_label":
				// A real workspace label (explicit null team) without eligibility proof.
				p.labels[1].Global = true
				p.labels[1].TeamID = ""
				p.labels[1].EligibilityKnown = false
			case "unknown_eligibility":
				p.labels[1].EligibilityKnown = false
			}
			_, err := l1ServiceFor(t, f.store, p).WriteV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), in)
			denied := intent == "stale_expectation" || intent == "unknown_existing" || intent == "unsafe_label" || intent == "unknown_eligibility"
			if denied {
				if err == nil || len(p.updates) != 0 || p.writes.Load() != 0 {
					t.Fatal("unsafe/unknown label replacement dispatched", err)
				}
				return
			}
			if err != nil || len(p.updates) != 1 {
				t.Fatalf("explicit label intent not dispatched exactly once: %d %v", len(p.updates), err)
			}
			got := p.updates[0]
			if got.Labels.Present != in.Labels.Present || !reflect.DeepEqual(got.Labels.Value, in.Labels.Value) {
				t.Fatal("labels silently merged, removed, or cleared from omission")
			}
			if got.Project.Present || got.Parent.Present || got.Assignee.Present {
				t.Fatal("label/title intent invented unrelated clears")
			}
		})
	}
}

func TestL2MutationVerificationRejectsContradictoryDuplicateTarget(t *testing.T) {
	for _, mode := range []string{"label_eligibility_conflict", "label_foreign_team", "member_inactive", "member_foreign_team", "member_unknown_eligibility"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := l2IntentFixture()
			in := linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: "update", OperationID: "duplicate-target-" + mode, IssueID: l1IssueID}}
			if strings.HasPrefix(mode, "label_") {
				good := linearprovider.Label{ID: l2LabelB, TeamID: l1TeamA, EligibilityKnown: true}
				conflicting := good
				if mode == "label_foreign_team" {
					conflicting.TeamID = l1TeamB
				} else {
					conflicting.Archived = true
				}
				p.labels = []linearprovider.Label{good, conflicting}
				in.Labels = linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelB}}
				in.ExpectedLabels = linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelA, l2LabelB}}
			} else {
				good := linearprovider.Member{ID: l2LabelA, TeamID: l1TeamA, Active: true, EligibilityKnown: true}
				conflicting := good
				switch mode {
				case "member_inactive":
					conflicting.Active = false
				case "member_foreign_team":
					conflicting.TeamID = l1TeamB
				case "member_unknown_eligibility":
					conflicting.EligibilityKnown = false
				}
				p.members = []linearprovider.Member{good, conflicting}
				id := l2LabelA
				in.Assignee = linearprovider.Field[*string]{Present: true, Value: &id}
			}
			result, err := l1ServiceFor(t, f.store, p).WriteV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), in)
			if err == nil || result != nil || len(p.updates) != 0 || p.writes.Load() != 0 {
				t.Fatalf("eligible first row hid contradictory same-ID metadata and reached mutation: V2updates=%d legacywrites=%d err=%v", len(p.updates), p.writes.Load(), err)
			}
		})
	}
}

func TestL2ObservedUnchangedRequiresFreshWriteAuthorizationAndRetryAvoidsIO(t *testing.T) {
	for _, mode := range []string{"write_control", "personal_read", "retired_actor", "fenced_actor"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := l2IntentFixture()
			cfg := l1ServiceConfig(t, f.store, p)
			if mode == "personal_read" {
				cfg.Connections = l2ReadConnections{}
			}
			if mode == "retired_actor" {
				f.call("retire_session", "w", "p", "worker", actor("worker", nil))
			}
			if mode == "fenced_actor" {
				f.call("register_session", "w", "p", "worker", map[string]any{"instance": "worker-instance", "resume": true})
			}
			in := linearboard.RelationInputV2{OperationID: "fresh-noop", Action: "remove", Type: "blocks", FromID: l1IssueID, ToID: l1StateID}
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			result, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in)
			if mode != "write_control" {
				if err == nil || result != nil {
					t.Fatal("fresh no-op bypassed mutation authorization", err)
				}
				return
			}
			if err != nil {
				t.Fatal("authorized no-op refused", err)
			}
			if object(t, jsonData(t, result))["outcome"] != "observed_unchanged" {
				t.Fatal("no-op not truthfully recorded")
			}
			before := l1LedgerBytes(t, cfg.LedgerDirectory)
			forbidden := new(l2ForbiddenConnections)
			cfg.Connections = forbidden
			retry, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in)
			if err != nil || !reflect.DeepEqual(jsonData(t, result), jsonData(t, retry)) || forbidden.calls.Load() != 0 || !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
				t.Fatal("accepted identical no-op retry lost receipt or opened provider/credentials", err)
			}
			if len(p.edges) != 0 {
				t.Fatal("observed absence dispatched provider removal")
			}
			f.call("retire_session", "w", "p", "worker", actor("worker", nil))
			if _, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in); err == nil {
				t.Fatal("accepted no-op retry bypassed current authority after actor retirement")
			}
			if forbidden.calls.Load() != 0 || !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
				t.Fatal("denied accepted retry opened credentials or altered its receipt")
			}
		})
	}
}

func TestL2ObservedUnchangedPostgresReaderCannotWrite(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	f.call(pgOwner, "taskboard_set_v2", "p", "", l2MappingArgs(0))
	p := l2IntentFixture()
	executor := notificationExecutorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
		return f.store.ExecuteAs(ctx, pgPrincipal("reader"), r)
	})
	s := l1ServiceFor(t, executor, p)
	for _, actorName := range []string{"", "reader"} {
		args := map[string]any{}
		if actorName != "" {
			args = actor(actorName, nil)
		}
		result, err := s.RelationV2(context.Background(), f.request("", "p", actorName, args), linearboard.RelationInputV2{OperationID: "reader-noop", Action: "remove", Type: "blocks", FromID: l1IssueID, ToID: l1StateID})
		if err == nil || result != nil || len(p.edges) != 0 {
			t.Fatal("project reader received a fresh mutation-intent no-op receipt", err)
		}
	}
}

func TestL2NormalizedOneEdgeAndLostResponseNeverReplay(t *testing.T) {
	for _, mode := range []string{"inverse_blocks", "related_pair", "lost_response"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := l2IntentFixture()
			cfg := l1ServiceConfig(t, f.store, p)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			in := linearboard.RelationInputV2{OperationID: "one-edge", Action: "add", Type: "blocked-by", FromID: l1IssueID, ToID: l1StateID}
			expected := linearprovider.RelationInput{Action: "add", Type: "blocks", FromID: l1StateID, ToID: l1IssueID}
			if mode == "related_pair" {
				in.Type = "related"
				in.FromID, in.ToID = l1StateID, l1IssueID
				expected = linearprovider.RelationInput{Action: "add", Type: "related", FromID: l1IssueID, ToID: l1StateID}
			}
			if mode == "lost_response" {
				p.lostEdgeResponse = true
			}
			result, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in)
			if len(p.edges) != 1 || p.edges[0] != expected {
				t.Fatalf("wrong edge direction/pair or dispatch count: %v", p.edges)
			}
			if mode == "lost_response" {
				if err == nil || result != nil || !strings.Contains(err.Error(), "uncertain") {
					t.Fatal("lost response not recorded uncertain", err)
				}
				before := l1LedgerBytes(t, cfg.LedgerDirectory)
				forbidden := new(l2ForbiddenConnections)
				cfg.Connections = forbidden
				if _, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in); err == nil || !strings.Contains(err.Error(), "uncertain") {
					t.Fatal("pending edge was replayable", err)
				}
				if forbidden.calls.Load() != 0 || len(p.edges) != 1 || !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
					t.Fatal("pending retry reopened provider or changed ledger")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// The semantically identical normalized edge must share the old ID,
			// not conflict or dispatch a second mutation under alternate spelling.
			in.Type, in.FromID, in.ToID = expected.Type, expected.FromID, expected.ToID
			if _, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in); err != nil || len(p.edges) != 1 {
				t.Fatal("equivalent normalized edge replayed/conflicted", err)
			}
		})
	}
}
