package integration

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

type l2ScopedProvider struct {
	l1SyntheticProvider
	projects map[string][]string
	issues   map[string]linearprovider.IssueV2
	queries  []linearprovider.IssueQueryV2
	page     func(linearprovider.IssueQueryV2) linearprovider.Page[linearprovider.IssueV2]
}

func l2ScopedFixture() *l2ScopedProvider {
	return &l2ScopedProvider{projects: map[string][]string{l1Project: {l1TeamA}, l2ProjectB: {l1TeamB}}, issues: map[string]linearprovider.IssueV2{}}
}
func (p *l2ScopedProvider) GetProject(_ context.Context, id string) (linearprovider.Project, error) {
	teams, ok := p.projects[id]
	if !ok {
		return linearprovider.Project{}, errors.New("synthetic project unavailable")
	}
	return linearprovider.Project{ID: id, Name: "Synthetic scoped project", TeamIDs: teams}, nil
}
func (p *l2ScopedProvider) IssueV2(_ context.Context, id string) (linearprovider.IssueV2, error) {
	issue, ok := p.issues[id]
	if !ok {
		return linearprovider.IssueV2{}, errors.New("synthetic issue unavailable")
	}
	return issue, nil
}
func (p *l2ScopedProvider) IssuesV2(_ context.Context, q linearprovider.IssueQueryV2) (linearprovider.Page[linearprovider.IssueV2], error) {
	p.queries = append(p.queries, q)
	if p.page == nil {
		return linearprovider.Page[linearprovider.IssueV2]{Complete: true}, nil
	}
	return p.page(q), nil
}
func (*l2ScopedProvider) Labels(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	return linearprovider.Page[linearprovider.Label]{}, errors.New("unexpected label lookup")
}
func (*l2ScopedProvider) Members(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Member], error) {
	return linearprovider.Page[linearprovider.Member]{}, errors.New("unexpected member lookup")
}
func (*l2ScopedProvider) Capabilities(context.Context) (linearprovider.Capabilities, error) {
	return linearprovider.Capabilities{}, errors.New("unexpected capability lookup")
}
func (*l2ScopedProvider) CreateV2(context.Context, linearprovider.CreateInputV2) (linearprovider.IssueV2, error) {
	return linearprovider.IssueV2{}, errors.New("unexpected V2 mutation")
}
func (*l2ScopedProvider) UpdateV2(context.Context, linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	return linearprovider.IssueV2{}, errors.New("unexpected V2 mutation")
}
func (*l2ScopedProvider) Relation(context.Context, linearprovider.RelationInput) (linearprovider.RelationReceipt, error) {
	return linearprovider.RelationReceipt{}, errors.New("unexpected edge mutation")
}

func l2Issue(id, team, project string) linearprovider.IssueV2 {
	return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, TeamID: team, ProjectID: project, Title: "Synthetic scoped title", Description: "SYNTHETIC_BODY_MUST_NOT_ESCAPE_SCOPE"}, ProjectKnown: true}
}

func TestL2VerifiedPairScopeBeforeBodyExposure(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
	p := l2ScopedFixture()
	s := l1ServiceFor(t, f.store, p)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	for _, scenario := range []string{"valid_pair", "cross_pair", "null_project", "unknown_project", "wrong_issue_id"} {
		t.Run(scenario, func(t *testing.T) {
			issue := l2Issue(l1IssueID, l1TeamA, l1Project)
			switch scenario {
			case "cross_pair":
				issue.ProjectID = l2ProjectB
			case "null_project":
				issue.ProjectID = ""
			case "unknown_project":
				issue.ProjectKnown = false
			case "wrong_issue_id":
				issue.ID = l1StateID
			}
			p.issues[l1IssueID] = issue
			result, err := s.IssueV2(context.Background(), scope, l1IssueID)
			if scenario == "valid_pair" {
				if err != nil || result.ID != l1IssueID {
					t.Fatal("valid pair refused", err)
				}
				return
			}
			if err == nil || !reflect.DeepEqual(result, linearprovider.IssueV2{}) {
				t.Fatalf("scope failure exposed issue body/result: err=%v", err)
			}
		})
	}
	for _, scenario := range []string{"project_without_allowed_team", "allowed_team_unrepresented"} {
		t.Run(scenario, func(t *testing.T) {
			p := l2ScopedFixture()
			if scenario == "project_without_allowed_team" {
				p.projects[l2ProjectB] = []string{l1Workspace}
			} else {
				p.projects[l2ProjectB] = []string{l1TeamA}
			}
			s := l1ServiceFor(t, f.store, p)
			if _, err := s.ContextV2(context.Background(), scope); err == nil {
				t.Fatal("incomplete verified pair coverage accepted")
			}
		})
	}
}

func TestL2PairCursorEmptyPageAndBindingProfileFences(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
	p := l2ScopedFixture()
	cfg := l1ServiceConfig(t, f.store, p)
	s := linearboard.New(cfg)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	p.page = func(q linearprovider.IssueQueryV2) linearprovider.Page[linearprovider.IssueV2] {
		if q.ProjectID == l1Project && q.Cursor == "" {
			return linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{}, NextCursor: "synthetic-next", Complete: false}
		}
		return linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{l2Issue(l1IssueID, q.TeamID, q.ProjectID)}, Complete: true}
	}
	page, err := s.IssuesV2(context.Background(), scope, linearboard.IssuesInputV2{Limit: 2})
	if err != nil || len(page.Items) != 0 || page.Complete || page.NextCursor == "" || len(p.queries) != 1 {
		t.Fatal("empty provider continuation was hidden or scanned eagerly", err)
	}
	firstCursor := page.NextCursor
	page, err = s.IssuesV2(context.Background(), scope, linearboard.IssuesInputV2{Limit: 2, Cursor: firstCursor})
	if err != nil || len(page.Items) != 1 || page.Complete || page.NextCursor == "" || len(p.queries) != 2 {
		t.Fatal("provider continuation/pair continuation lost", err)
	}
	page, err = s.IssuesV2(context.Background(), scope, linearboard.IssuesInputV2{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(page.Items) != 1 || !page.Complete || page.NextCursor != "" || len(p.queries) != 3 {
		t.Fatal("last verified pair did not complete honestly", err)
	}
	for i, q := range p.queries {
		wantProject, wantTeam, wantCursor := l1Project, l1TeamA, ""
		if i == 1 {
			wantCursor = "synthetic-next"
		}
		if i == 2 {
			wantProject, wantTeam = l2ProjectB, l1TeamB
		}
		if q.ProjectID != wantProject || q.TeamID != wantTeam || q.Cursor != wantCursor || q.Limit != 2 {
			t.Fatalf("query%d escaped deterministic pair/page: %+v", i, q)
		}
	}
	before := len(p.queries)
	changed := cfg
	changed.ProfileID = "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := linearboard.New(changed).IssuesV2(context.Background(), scope, linearboard.IssuesInputV2{Limit: 2, Cursor: firstCursor}); err == nil {
		t.Fatal("cursor crossed personal profile")
	}
	f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(1))
	if _, err := s.IssuesV2(context.Background(), scope, linearboard.IssuesInputV2{Limit: 2, Cursor: firstCursor}); err == nil {
		t.Fatal("cursor crossed mapping revision")
	}
	if len(p.queries) != before {
		t.Fatal("invalid cursor fetched provider issue page")
	}
}

func TestL2PairPageRejectsUnknownCompletenessAndConflictingBodies(t *testing.T) {
	for _, scenario := range []string{"unknown_complete", "contradictory_complete", "cross_pair", "conflicting_duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := l2ScopedFixture()
			p.page = func(q linearprovider.IssueQueryV2) linearprovider.Page[linearprovider.IssueV2] {
				issue := l2Issue(l1IssueID, q.TeamID, q.ProjectID)
				page := linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{issue}, Complete: true}
				switch scenario {
				case "unknown_complete":
					page.Complete = false
				case "contradictory_complete":
					page.NextCursor = "more"
				case "cross_pair":
					page.Items[0].ProjectID = l2ProjectB
				case "conflicting_duplicate":
					other := issue
					other.Description = "CONFLICTING_SYNTHETIC_BODY"
					page.Items = append(page.Items, other)
				}
				return page
			}
			page, err := l1ServiceFor(t, f.store, p).IssuesV2(context.Background(), f.request("", "w", "p", "", nil), linearboard.IssuesInputV2{Limit: 2})
			if err == nil || len(page.Items) != 0 || page.NextCursor != "" {
				t.Fatalf("invalid provider page exposed bodies or cursor: %v", err)
			}
			if len(p.queries) != 1 {
				t.Fatal("failure caused unbounded page scanning")
			}
		})
	}
}
