package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const projectA = "33333333-3333-4333-8333-333333333333"
const projectB = "44444444-4444-4444-8444-444444444444"
const labelA = "55555555-5555-4555-8555-555555555555"
const memberA = "66666666-6666-4666-8666-666666666666"
const parentA = "77777777-7777-4777-8777-777777777777"

type authorityV2 struct {
	b           bus.TaskboardBindingV2
	old         bus.TaskboardBinding
	actor       string
	deny        bool
	afterGet    func()
	gets        int
	accesses    int
	afterAccess func(int)
}

func (a *authorityV2) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "taskboard_access" {
		a.accesses++
		if a.afterAccess != nil {
			a.afterAccess(a.accesses)
		}
		return bus.Result{Source: "local-authority", Data: bus.TaskboardAccess{WorkspaceID: a.b.WorkspaceID, ProjectID: a.b.ProjectID, SessionID: a.actor, CanWrite: !a.deny, ProjectRole: "member"}}, nil
	}
	if r.Operation == "taskboard_get" {
		return bus.Result{Source: "local-authority", Data: a.old}, nil
	}
	if r.Operation == "taskboard_get_v2" {
		a.gets++
		if a.afterGet != nil {
			a.afterGet()
		}
		return bus.Result{Source: "local-authority", Data: a.b}, nil
	}
	return bus.Result{}, errors.New("unexpected authority operation")
}

type providerV2 struct {
	fakeProvider
	projects     map[string][]string
	issues       map[string]linearprovider.IssueV2
	caps         linearprovider.Capabilities
	labels       []linearprovider.Label
	members      []linearprovider.Member
	pages        []linearprovider.Page[linearprovider.IssueV2]
	queries      []linearprovider.IssueQueryV2
	created      []linearprovider.CreateInputV2
	updated      []linearprovider.UpdateInputV2
	edges        []linearprovider.RelationInput
	after        func()
	preflightErr error
	writeErr     error
}

func (p *providerV2) GetProject(_ context.Context, id string) (linearprovider.Project, error) {
	return linearprovider.Project{ID: id, TeamIDs: p.projects[id]}, nil
}
func (p *providerV2) IssueV2(_ context.Context, id string) (linearprovider.IssueV2, error) {
	p.reads++
	i, ok := p.issues[id]
	if !ok {
		return i, errors.New("inaccessible issue")
	}
	return i, nil
}
func (p *providerV2) IssuesV2(_ context.Context, q linearprovider.IssueQueryV2) (linearprovider.Page[linearprovider.IssueV2], error) {
	p.queries = append(p.queries, q)
	if len(p.pages) == 0 {
		return linearprovider.Page[linearprovider.IssueV2]{Complete: true}, nil
	}
	out := p.pages[0]
	p.pages = p.pages[1:]
	return out, nil
}
func (p *providerV2) Labels(_ context.Context, team string, _ linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	var items []linearprovider.Label
	for _, l := range p.labels {
		if l.TeamID == team {
			items = append(items, l)
		}
	}
	return linearprovider.Page[linearprovider.Label]{Items: items, Complete: true}, nil
}
func (p *providerV2) Members(_ context.Context, team string, _ linearprovider.PageOptions) (linearprovider.Page[linearprovider.Member], error) {
	var items []linearprovider.Member
	for _, m := range p.members {
		if m.TeamID == team {
			items = append(items, m)
		}
	}
	return linearprovider.Page[linearprovider.Member]{Items: items, Complete: true}, nil
}
func (p *providerV2) Capabilities(context.Context) (linearprovider.Capabilities, error) {
	return p.caps, nil
}
func (p *providerV2) ValidateCreateV2(linearprovider.CreateInputV2) error { return p.preflightErr }
func (p *providerV2) ValidateUpdateV2(linearprovider.UpdateInputV2) error { return p.preflightErr }
func (p *providerV2) CreateV2(_ context.Context, in linearprovider.CreateInputV2) (linearprovider.IssueV2, error) {
	p.created = append(p.created, in)
	p.writes++
	i := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueID, TeamID: in.TeamID, ProjectID: in.ProjectID}, ProjectKnown: true}
	if p.after != nil {
		p.after()
	}
	return i, p.writeErr
}
func (p *providerV2) UpdateV2(_ context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	p.updated = append(p.updated, in)
	p.writes++
	i := p.issues[in.ID]
	if in.Project.Present {
		i.ProjectID = ""
		if in.Project.Value != nil {
			i.ProjectID = *in.Project.Value
		}
	}
	if p.after != nil {
		p.after()
	}
	return i, p.writeErr
}
func (p *providerV2) Relation(_ context.Context, in linearprovider.RelationInput) (linearprovider.RelationReceipt, error) {
	p.edges = append(p.edges, in)
	p.writes++
	if p.after != nil {
		p.after()
	}
	return linearprovider.RelationReceipt{Action: in.Action, Type: in.Type, FromID: in.FromID, ToID: in.ToID}, p.writeErr
}
func testV2(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	a := &authorityV2{actor: "ses", b: bus.TaskboardBindingV2{WorkspaceID: "ws", ProjectID: "prj", Configured: true, Revision: 1, MappingVersion: 2, UpdatedAt: "2026-10-22T12:00:00Z", Provider: "linear", Mode: "project_set", LinearTeamIDs: []string{teamID, otherTeamID}, LinearDefaultTeamID: teamID, LinearProjectIDs: []string{projectA, projectB}}}
	p := &providerV2{projects: map[string][]string{projectA: {teamID}, projectB: {otherTeamID}}, issues: map[string]linearprovider.IssueV2{issueID: {Issue: linearprovider.Issue{ID: issueID, TeamID: teamID, ProjectID: projectA}, ProjectKnown: true, LabelsKnown: true, ParentKnown: true}}, caps: linearprovider.Capabilities{Labels: true, Members: true, CreateProject: true, UpdateProject: true, ReplaceLabels: true, Parent: true, Assignee: true, ParentPreservesDestination: true, RelationAdd: true, RelationRemove: true}}
	a.old = bus.TaskboardBinding{WorkspaceID: a.b.WorkspaceID, ProjectID: a.b.ProjectID, Configured: true, Revision: 1, UpdatedAt: a.b.UpdatedAt, Provider: "linear", LinearTeamIDs: []string{teamID}, LinearDefaultTeamID: teamID, LinearProjectID: projectA}
	s := New(Config{Authority: "local-v2", Executor: a, Connections: fakeConnections{}, ProfileID: "profile-1", LedgerDirectory: filepath.Join(t.TempDir(), "ledger"), Open: func(context.Context, *http.Client, bool) (Provider, error) { return p, nil }})
	return s, a, p, bus.Request{Workspace: "ws", Project: "prj", Session: "ses", Args: map[string]any{"instance": "secret-instance"}}
}
func ptrV2(s string) *string { return &s }

func TestLinearV2PairPaginationUsesOnePageIncludingEmptyContinuation(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{NextCursor: "next-page", Complete: false}, {Complete: true}, {Items: []linearprovider.IssueV2{{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true}}, Complete: true}}
	cursor := ""
	for n := 0; n < 3; n++ {
		page, e := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10, Cursor: cursor})
		if e != nil {
			t.Fatal(e)
		}
		if len(p.queries) != n+1 {
			t.Fatal("hidden issue scan")
		}
		if n < 2 && page.Complete {
			t.Fatal("empty page incorrectly exhausted board")
		}
		cursor = page.NextCursor
	}
	if cursor != "" || p.queries[0].ProjectID != projectA || p.queries[1].Cursor != "next-page" || p.queries[2].TeamID != otherTeamID {
		t.Fatal(p.queries, cursor)
	}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{{Issue: linearprovider.Issue{ID: issueID, TeamID: otherTeamID, ProjectID: projectA}, ProjectKnown: true}}, Complete: true}}
	if _, e := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10}); e == nil {
		t.Fatal("cross-product pair accepted")
	}
	a.b.Revision++
	if _, e := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10, Cursor: "invalid"}); e == nil {
		t.Fatal("invalid cursor accepted")
	}
}
func TestLinearV2DestinationAndTriStateMutationLedger(t *testing.T) {
	s, _, p, scope := testV2(t)
	title := "new issue"
	in := WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "v2-create", Title: &title}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("multi-project create chose an implicit destination")
	}
	in.Project = linearprovider.Field[*string]{Present: true, Value: ptrV2(projectA)}
	first, e := s.WriteV2(context.Background(), scope, in)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.created) != 1 || !p.created[0].Labels.Present || p.created[0].Labels.Value == nil {
		t.Fatal("explicit empty label intent lost")
	}
	s.cfg.Connections = nil
	again, e := s.WriteV2(context.Background(), scope, in)
	if e != nil {
		t.Fatal(e)
	}
	f, _ := json.Marshal(first)
	var normalized any
	if json.Unmarshal(f, &normalized) != nil {
		t.Fatal("invalid first receipt")
	}
	if digest(normalized) != digest(again) || p.writes != 1 {
		t.Fatal("accepted V2 retry changed receipt or dispatched")
	}
	in.Labels.Present = false
	in.Labels.Value = nil
	if _, e = s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("changed presence reused operation")
	}
	in.Operation = "update"
	in.IssueID = issueID
	in.OperationID = "clear-project"
	in.Title = nil
	in.Project.Value = nil
	if _, e = s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("project-set project clear accepted")
	}
}
func TestLinearV2EligibilityAncestryAndNegotiationFailBeforePending(t *testing.T) {
	s, _, p, scope := testV2(t)
	p.members = []linearprovider.Member{{ID: memberA, TeamID: teamID, Active: true, EligibilityKnown: false}}
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "eligibility", IssueID: issueID}, Assignee: linearprovider.Field[*string]{Present: true, Value: ptrV2(memberA)}}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("unknown eligibility accepted")
	}
	p.members[0].EligibilityKnown = true
	p.members[0].Active = false
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("inactive user accepted")
	}
	in.Assignee.Present = false
	in.Assignee.Value = nil
	in.Parent = linearprovider.Field[*string]{Present: true, Value: ptrV2(parentA)}
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, ParentKnown: true, ParentID: issueID}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("parent cycle accepted")
	}
	i := p.issues[parentA]
	i.ParentID = ""
	p.issues[parentA] = i
	p.preflightErr = errors.New("unsupported explicit null or typed schema")
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil {
		t.Fatal("preflight refusal ignored")
	}
	if _, e := os.Stat(s.cfg.LedgerDirectory); !os.IsNotExist(e) || p.writes != 0 {
		t.Fatal("preflight refusal created pending state or dispatched", e)
	}
	p.preflightErr = nil
	if _, e := s.WriteV2(context.Background(), scope, in); e != nil {
		t.Fatal("valid cross-project parent failed", e)
	}
}
func TestLinearV2EdgeNormalizationObservedReceiptAndKnownContextChange(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
	in := RelationInputV2{OperationID: "one-edge", Action: "add", Type: "blocked-by", FromID: issueID, ToID: parentA}
	p.after = func() { a.b.Revision++ }
	r, e := s.RelationV2(context.Background(), scope, in)
	var changed *ReceiptError
	if r == nil || !errors.As(e, &changed) || changed.Outcome != "binding_changed" {
		t.Fatal("known receipt lost on mapping change", r, e)
	}
	if len(p.edges) != 1 || p.edges[0].FromID != parentA || p.edges[0].ToID != issueID || p.edges[0].Type != "blocks" {
		t.Fatal("blocked-by direction not normalized", p.edges)
	}
	if _, e = s.RelationV2(context.Background(), scope, in); e == nil || len(p.edges) != 1 {
		t.Fatal("revision change bypassed operation ledger")
	}
	s, a, p, scope = testV2(t)
	i := p.issues[issueID]
	i.RelationsKnown = true
	i.Relations = []linearprovider.Relation{{Type: "related", FromID: issueID, ToID: parentA}}
	p.issues[issueID] = i
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
	p.caps.RelationAdd = false
	r, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "observed-edge", Action: "add", Type: "related", FromID: parentA, ToID: issueID})
	if e != nil {
		t.Fatal(e)
	}
	if r.(ReceiptV2).Outcome != "observed_unchanged" || p.writes != 0 {
		t.Fatal("observed edge caused provider write")
	}
	if _, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "observed-edge", Action: "add", Type: "related", FromID: issueID, ToID: parentA}); e != nil {
		t.Fatal("normalized retry failed", e)
	}
}

func TestLinearV2ParentAncestryBoundary64(t *testing.T) {
	for _, length := range []int{64, 65} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			s, _, p, scope := testV2(t)
			ids := []string{}
			for n := 0; n < length; n++ {
				ids = append(ids, fmt.Sprintf("00000000-0000-4000-8000-%012d", n+1))
			}
			for n, id := range ids {
				parent := ""
				if n+1 < len(ids) {
					parent = ids[n+1]
				}
				p.issues[id] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, TeamID: teamID, ProjectID: projectA}, ProjectKnown: true, ParentKnown: true, ParentID: parent}
			}
			_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "parent-bound", IssueID: issueID}, Parent: linearprovider.Field[*string]{Present: true, Value: &ids[0]}})
			if length == 64 {
				if e != nil || p.writes != 1 {
					t.Fatal("exactly64 complete ancestors refused", e)
				}
			} else if e == nil || p.writes != 0 {
				t.Fatal("65 ancestors dispatched", e)
			}
		})
	}
}

func TestLinearV2LinkPreviewKeepsOriginalRevisionAndVerifiesPairs(t *testing.T) {
	s, a, p, scope := testV2(t)
	in := LinkInputV2{TeamIDs: []string{otherTeamID, teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectB, projectA}, ExpectedRevision: 1}
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil {
		t.Fatal(e)
	}
	preview := r.(LinkPreviewV2)
	if preview.Binding.Revision != 1 || len(preview.Projects) != 2 || preview.DefaultTeamID != teamID || preview.Confirmed || p.writes != 0 {
		t.Fatal("preview committed or lost original context", preview)
	}
	a.b.Revision = 2
	in.Confirm = true
	if _, e = s.LinkV2(context.Background(), scope, in); e == nil {
		t.Fatal("preview original expected revision replaced")
	}
	in.ExpectedRevision = 2
	p.projects[projectB] = []string{teamID}
	if _, e = s.LinkV2(context.Background(), scope, in); e == nil {
		t.Fatal("unrepresented allowed team accepted")
	}
}

func TestLinearV2ProviderErrorStaysUncertainWhenBindingChanges(t *testing.T) {
	for _, op := range []string{"update", "relation"} {
		t.Run(op, func(t *testing.T) {
			s, a, p, scope := testV2(t)
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
			i := p.issues[issueID]
			i.RelationsKnown = true
			p.issues[issueID] = i
			p.writeErr = errors.New("lost provider response")
			p.after = func() { a.b.Revision++ }
			title := "task"
			write := func() (any, error) {
				if op == "update" {
					return s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "lost-" + op, IssueID: issueID, Title: &title}})
				}
				return s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "lost-" + op, Action: "add", Type: "blocks", FromID: issueID, ToID: parentA})
			}
			r, e := write()
			var coded *ReceiptError
			if r != nil || e == nil || errors.As(e, &coded) || !strings.Contains(e.Error(), "outcome uncertain") || !strings.Contains(e.Error(), "binding also changed") {
				t.Fatal("binding change hid an uncertain provider outcome", r, e)
			}
			// The original binding retry must find the operation still pending.
			p.after = nil
			a.b.Revision--
			before := p.writes
			if _, e = write(); e == nil || !strings.Contains(e.Error(), "uncertain") || p.writes != before {
				t.Fatal("uncertain operation was not left pending", e, p.writes)
			}
		})
	}
}

func TestLinearV2PreDispatchFailureClassification(t *testing.T) {
	t.Run("transient write cancel releases the ID", func(t *testing.T) {
		s, a, p, scope := testV2(t)
		ctx, cancel := context.WithCancel(context.Background())
		a.afterAccess = func(n int) {
			if n == 3 {
				cancel()
			}
		}
		title := "task"
		in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "cancel-before-dispatch", IssueID: issueID, Title: &title}}
		if _, e := s.WriteV2(ctx, scope, in); !errors.Is(e, context.Canceled) || p.writes != 0 || ledgerStateV2(t, s, a, in.OperationID) != "absent" {
			t.Fatal("cancellation before dispatch was not released", e, p.writes)
		}
		a.afterAccess = nil
		if _, e := s.WriteV2(context.Background(), scope, in); e != nil || p.writes != 1 {
			t.Fatal("never-dispatched operation ID was burned", e, p.writes)
		}
	})
	t.Run("transient provider read failure releases the ID", func(t *testing.T) {
		s, a, p, scope := testV2(t)
		p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
		i := p.issues[issueID]
		i.RelationsKnown = true
		p.issues[issueID] = i
		failing := &failingReadsV2{providerV2: p}
		s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return failing, nil }
		a.afterAccess = func(n int) { failing.fail = n == 2 }
		in := RelationInputV2{OperationID: "read-failure-before-dispatch", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}
		if _, e := s.RelationV2(context.Background(), scope, in); e == nil || strings.Contains(e.Error(), "uncertain") || len(p.edges) != 0 || ledgerStateV2(t, s, a, in.OperationID) != "absent" {
			t.Fatal("provider read failure before dispatch was not released", e, p.edges)
		}
		a.afterAccess, failing.fail = nil, false
		if _, e := s.RelationV2(context.Background(), scope, in); e != nil || len(p.edges) != 1 {
			t.Fatal("transiently refused operation ID was burned", e, p.edges)
		}
	})
	t.Run("relation actor revoked inside lock is terminal", func(t *testing.T) {
		s, a, p, scope := testV2(t)
		p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
		i := p.issues[issueID]
		i.RelationsKnown = true
		p.issues[issueID] = i
		a.afterAccess = func(n int) { a.deny = n == 2 }
		in := RelationInputV2{OperationID: "revoked-before-dispatch", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}
		if _, e := s.RelationV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "refused before dispatch; nothing was sent") || len(p.edges) != 0 || ledgerStateV2(t, s, a, in.OperationID) != "refused" {
			t.Fatal("access revocation was not a terminal refusal", e, p.edges)
		}
		a.afterAccess, a.deny = nil, false
		s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
			t.Fatal("terminal refusal retry opened Linear")
			return nil, nil
		}
		if _, e := s.RelationV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "refused before dispatch") || len(p.edges) != 0 {
			t.Fatal("same-ID retry after drift was not the recorded refusal", e)
		}
		s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return p, nil }
		in.OperationID = "revoked-then-new-id"
		if _, e := s.RelationV2(context.Background(), scope, in); e != nil || len(p.edges) != 1 {
			t.Fatal("new operation ID was not evaluated normally", e, p.edges)
		}
	})
}

type failingReadsV2 struct {
	*providerV2
	fail bool
}

func (p *failingReadsV2) IssueV2(ctx context.Context, id string) (linearprovider.IssueV2, error) {
	if p.fail {
		return linearprovider.IssueV2{}, &linearprovider.Error{Kind: "unavailable"}
	}
	return p.providerV2.IssueV2(ctx, id)
}
