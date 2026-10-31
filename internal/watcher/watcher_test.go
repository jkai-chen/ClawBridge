package watcher

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

const (
	team        = "11111111-1111-4111-8111-111111111111"
	otherTeam   = "22222222-2222-4222-8222-222222222222"
	issueA      = "33333333-3333-4333-8333-333333333333"
	issueB      = "44444444-4444-4444-8444-444444444444"
	issueOut    = "55555555-5555-4555-8555-555555555555"
	labelNeeds  = "66666666-6666-4666-8666-666666666666"
	appUser     = "77777777-7777-4777-8777-777777777777"
	trustedUser = "88888888-8888-4888-8888-888888888888"
	strangerID  = "99999999-9999-4999-8999-999999999999"
	humanNote   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// fakeLinear is Linear as the app sees it: two issues in the bound team,
// one in another team, comments, labels, and GraphQL answers.
type fakeLinear struct {
	// subscribeTeam and subscribeMembers answer the subscribe pass's issue
	// read (no team: no issue); subscribeClass answers SubscribeIssue by
	// email or user ID (default subscribed), and subscribed records each.
	subscribeTeam    string
	subscribeProject string
	subscribeMembers []string
	subscribeErr     error
	subscribeClass   map[string]string
	subscribed       []string
	// users answers userQuery by Linear user ID; a missing one fails.
	users     map[string]map[string]any
	comments  []linearprovider.Comment
	labelled  []string
	delegated []string
	// delegatedMore makes every delegated page say there is another.
	delegatedMore bool
	history       map[string][]map[string]any
	// historyErr fails every history read.
	historyErr error
	// historyOnly limits historyErr to one history query, when set.
	historyOnly string
	// creator, creatorApp, createdAt and historyMore shape the S1 history
	// answer: the issue's creator (an integration if creatorApp), its
	// creation time, and a first page that is cut off.
	creator, createdAt      string
	creatorApp, historyMore bool
	// issueLabels answers the app's read of an issue's label IDs; an issue
	// not in it is unreadable. labelsMore says each list has another page.
	issueLabels map[string][]string
	labelsMore  bool
	mentions    []map[string]any
	queries     []string
	// onLabelled runs on each labelled-list read; block makes every app
	// GraphQL read wait for its context to end.
	onLabelled func()
	block      bool
	onComments func(context.Context)
	// rowLabelsMore marks rows whose labels run past the read's page.
	rowLabelsMore map[string]bool
	notes         int
	// healthIssues and activity answer project health's reads.
	healthIssues []map[string]any
	activity     map[string]map[string]any
	healthMore   bool
	// updates are the project's newest updates; noWrite withholds the
	// write scope; statusPosts records each status update sent, and
	// statusErr fails them.
	updates     []map[string]any
	noWrite     bool
	statusPosts []string
	statusErr   error
	// docRefs, docMore and docContent answer the document watch; events
	// records the order of comment and document reads.
	docRefs []linearprovider.DocRef
	// refuseDocs makes the app refuse documentsQuery as an invalid query;
	// providerDocLists counts the provider's document lists.
	refuseDocs       bool
	providerDocLists int
	// docSince makes the app's list keep only documents changed after the
	// query's since, as Linear does; docAscending orders it oldest first;
	// docEnds records the end each list read asked for.
	docSince     bool
	docAscending bool
	docEnds      []string
	// docLastAsFirst makes the app answer last as if it were first.
	docLastAsFirst bool
	docMore        bool
	docContent     map[string]string
	events         []string
	// dispatchIssues answers the dispatcher's issue facts read.
	dispatchIssues map[string]any
	// Project flow: projectStatus is the project's status ID, statuses the
	// status list; saves records each save_project {project, status}, and
	// saveLands applies it so a read-back shows it.
	projectStatus string
	statuses      []map[string]any
	saves         []string
	saveLands     bool
	// project, when set, is the Linear project issueA and issueB are in.
	project string
	// projects are the Linear projects GetProject finds, all in team;
	// extra are more issues, and unlisted the ones whose list row hides
	// their project. labelledPage pages the labelled list; issueReads
	// counts IssueV2 calls.
	projects     map[string]bool
	extra        map[string]linearprovider.IssueV2
	unlisted     map[string]bool
	labelledPage int
	issueReads   int
	// spill makes the labelled list also return other teams' issues, as a
	// parent team's filter may return its sub-teams'.
	spill bool
	// parents are teams' parent teams (get_team's parentId); teamLabels,
	// when set, are each team's label list; carries, when set, are the
	// label IDs each labelled issue carries, which the labelled list
	// filters by.
	parents    map[string]string
	teamLabels map[string][]linearprovider.Label
	carries    map[string][]string
	// refuseIn makes the app refuse the labelled read's set filter as an
	// invalid query, as Linear would a filter its schema doesn't take.
	// sinceIgnored makes it return rows updated before since anyway, and
	// rowLabels, when set, are the label IDs a row shows, whatever it
	// carries. updated sets a labelled row's updatedAt (default now).
	refuseIn     bool
	labelledErr  error
	sinceIgnored bool
	rowLabels    map[string][]string
	updated      map[string]string
	// projectTeams, when set, are each project's teams (default team).
	projectTeams map[string][]string
	// refuseLabelSet makes the app refuse needsAgentQuery as an invalid
	// query, and labelSetErr fail it; hiddenTeams are teams the app can't
	// see, and answerAs answers a team's query about another.
	refuseLabelSet bool
	labelSetErr    error
	hiddenTeams    map[string]bool
	answerAs       map[string]string
	// labelSets are the sets each team's labelled read last asked for;
	// labelListings counts the provider's label listing pages.
	labelSets     map[string][]string
	labelListings int
}

// labelNodes are the team's labels as the app's GraphQL shows them, from
// the same lists the provider's label listing returns: a team's own, or
// the workspace's (Global) when team is "".
func (f *fakeLinear) labelNodes(listing, team string) map[string]any {
	items := []linearprovider.Label{{ID: labelNeeds, Name: needsAgentLabel, TeamID: listing, EligibilityKnown: true}}
	if f.teamLabels != nil {
		items = f.teamLabels[listing]
	}
	nodes := []any{}
	for _, l := range items {
		if l.Global != (team == "") || (!l.Global && l.TeamID != team) {
			continue
		}
		var owner any
		if !l.Global {
			owner = map[string]any{"id": l.TeamID}
		}
		var archived any
		if l.Archived {
			archived = "2026-01-01T00:00:00.000Z"
		}
		nodes = append(nodes, map[string]any{"id": l.ID, "name": l.Name, "isGroup": l.Group, "archivedAt": archived, "team": owner})
	}
	return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}
}

func (f *fakeLinear) issue(id string) (linearprovider.IssueV2, error) {
	if i, ok := f.extra[id]; ok {
		return i, nil
	}
	switch id {
	case issueA, issueB:
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "FIX-" + id[:1], Title: "title " + id[:1], TeamID: team, ProjectID: f.project}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
	case issueOut:
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "OUT-1", TeamID: otherTeam}, ProjectKnown: true}, nil
	}
	return linearprovider.IssueV2{}, errors.New("inaccessible issue")
}

// status is the listed project status with that ID.
func (f *fakeLinear) status(id string) map[string]any {
	for _, st := range f.statuses {
		if st["id"] == id {
			return map[string]any{"id": id, "name": st["name"], "type": st["type"]}
		}
	}
	return nil
}

type fakeProvider struct{ f *fakeLinear }

func (p fakeProvider) Close() error { return nil }
func (p fakeProvider) GetTeam(_ context.Context, id string) (linearprovider.Team, error) {
	return linearprovider.Team{ID: id, Name: "T", ParentID: p.f.parents[id]}, nil
}
func (p fakeProvider) GetProject(_ context.Context, id string) (linearprovider.Project, error) {
	if p.f.projects[id] {
		if teams, ok := p.f.projectTeams[id]; ok {
			return linearprovider.Project{ID: id, Name: "P", TeamIDs: teams}, nil
		}
		return linearprovider.Project{ID: id, Name: "P", TeamIDs: []string{team}}, nil
	}
	return linearprovider.Project{}, errors.New("no projects")
}
func (p fakeProvider) Issues(context.Context, linearprovider.IssueQuery) (linearprovider.Page[linearprovider.Issue], error) {
	return linearprovider.Page[linearprovider.Issue]{Complete: true}, nil
}
func (p fakeProvider) GetIssue(_ context.Context, id string) (linearprovider.Issue, error) {
	i, err := p.f.issue(id)
	return i.Issue, err
}
func (p fakeProvider) Statuses(context.Context, string) ([]linearprovider.Status, error) {
	return nil, nil
}
func (p fakeProvider) Create(context.Context, linearprovider.CreateInput) (linearprovider.Issue, error) {
	return linearprovider.Issue{}, errors.New("no writes")
}
func (p fakeProvider) Update(context.Context, linearprovider.UpdateInput) (linearprovider.Issue, error) {
	return linearprovider.Issue{}, errors.New("no writes")
}

// Comment accepts the claim marker's note, as Linear would.
func (p fakeProvider) Comment(_ context.Context, issue, _ string) (linearprovider.Receipt, error) {
	p.f.notes++
	return linearprovider.Receipt{ID: fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012d", p.f.notes), IssueID: issue}, nil
}
func (p fakeProvider) SaveStatusUpdate(_ context.Context, project, body, health string) (string, error) {
	p.f.statusPosts = append(p.f.statusPosts, project+" "+health+" "+body)
	if p.f.statusErr != nil {
		return "", p.f.statusErr
	}
	return fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d", len(p.f.statusPosts)), nil
}
func (p fakeProvider) SetProjectStatus(_ context.Context, project, status string) error {
	p.f.saves = append(p.f.saves, project+" "+status)
	if p.f.saveLands {
		p.f.projectStatus = status
	}
	return nil
}
func (p fakeProvider) IssueV2(_ context.Context, id string) (linearprovider.IssueV2, error) {
	p.f.issueReads++
	return p.f.issue(id)
}
func (p fakeProvider) IssuesV2(_ context.Context, q linearprovider.IssueQueryV2) (linearprovider.Page[linearprovider.IssueV2], error) {
	out := linearprovider.Page[linearprovider.IssueV2]{Complete: true}
	if p.f.onLabelled != nil {
		p.f.onLabelled()
	}
	if q.LabelID != labelNeeds && p.f.carries == nil {
		return out, nil
	}
	// Like Linear, the list is filtered by the queried team, project and
	// label.
	for _, id := range p.f.labelled {
		i, _ := p.f.issue(id)
		if (i.TeamID != q.TeamID && !p.f.spill) || (q.ProjectID != "" && i.ProjectID != q.ProjectID) {
			continue
		}
		i.LabelIDs, i.LabelsKnown = []string{labelNeeds}, true
		if p.f.carries != nil {
			if !slices.Contains(p.f.carries[id], q.LabelID) {
				continue
			}
			i.LabelIDs = p.f.carries[id]
		}
		if p.f.unlisted[id] {
			i.ProjectID, i.ProjectKnown = "", false
		}
		out.Items = append(out.Items, i)
	}
	if n := p.f.labelledPage; n > 0 {
		from, _ := strconv.Atoi(q.Cursor)
		total := len(out.Items)
		out.Items = out.Items[from:min(from+n, total)]
		if from+n < total {
			out.NextCursor, out.Complete = strconv.Itoa(from+n), false
		}
	}
	return out, nil
}
func (p fakeProvider) Labels(_ context.Context, t string, _ linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	p.f.labelListings++
	if p.f.teamLabels != nil {
		return linearprovider.Page[linearprovider.Label]{Items: p.f.teamLabels[t], Complete: true}, nil
	}
	return linearprovider.Page[linearprovider.Label]{Items: []linearprovider.Label{{ID: labelNeeds, Name: needsAgentLabel, TeamID: t, EligibilityKnown: true}}, Complete: true}, nil
}
func (p fakeProvider) Members(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Member], error) {
	return linearprovider.Page[linearprovider.Member]{Complete: true}, nil
}
func (p fakeProvider) Capabilities(context.Context) (linearprovider.Capabilities, error) {
	return linearprovider.Capabilities{Labels: true}, nil
}
func (p fakeProvider) CreateV2(context.Context, linearprovider.CreateInputV2) (linearprovider.IssueV2, error) {
	return linearprovider.IssueV2{}, errors.New("no writes")
}
func (p fakeProvider) UpdateV2(context.Context, linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	return linearprovider.IssueV2{}, errors.New("no writes")
}
func (p fakeProvider) Relation(context.Context, linearprovider.RelationInput) (linearprovider.RelationReceipt, error) {
	return linearprovider.RelationReceipt{}, errors.New("no writes")
}
func (p fakeProvider) IssueComments(ctx context.Context, _ linearprovider.CommentsQuery) (linearprovider.Comments, error) {
	p.f.events = append(p.f.events, "comments")
	if p.f.onComments != nil {
		p.f.onComments(ctx)
	}
	return linearprovider.Comments{Items: p.f.comments, Complete: true}, nil
}

type fakeApp struct{ f *fakeLinear }

func (a fakeApp) Open(context.Context, bool) (linearboard.Provider, error) {
	return fakeProvider{a.f}, nil
}
func (a fakeApp) HasScope(_ context.Context, scope string) (bool, error) {
	return scope != "write" || !a.f.noWrite, nil
}
func (a fakeApp) SubscribeIssue(_ context.Context, issue, email, user string) (string, error) {
	who := email + user
	a.f.subscribed = append(a.f.subscribed, issue+" "+who)
	if class, ok := a.f.subscribeClass[who]; ok {
		return class, nil
	}
	return "subscribed", nil
}
func (a fakeApp) Identity(context.Context) (AppIdentity, error) {
	return AppIdentity{AppUserID: appUser, LinearOrgID: "org"}, nil
}
func (a fakeApp) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	a.f.queries = append(a.f.queries, query)
	if a.f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	var data any
	switch query {
	case userQuery:
		u, ok := a.f.users[vars["id"].(string)]
		if !ok {
			return errors.New("user unreadable")
		}
		data = map[string]any{"user": u}
	case historyQueryS1, historyQueryS2:
		if a.f.historyErr != nil && (a.f.historyOnly == "" || a.f.historyOnly == query) {
			return a.f.historyErr
		}
		issue := map[string]any{"history": map[string]any{"nodes": a.f.history[vars["id"].(string)], "pageInfo": map[string]any{"hasNextPage": a.f.historyMore, "endCursor": "more"}}}
		if a.f.creator != "" && query == historyQueryS1 {
			issue["creator"], issue["createdAt"] = map[string]any{"id": a.f.creator, "app": a.f.creatorApp}, a.f.createdAt
		}
		data = map[string]any{"issue": issue}
	case delegatedQuery:
		nodes := []any{}
		for _, id := range a.f.delegated {
			nodes = append(nodes, map[string]any{"id": id})
		}
		data = map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": a.f.delegatedMore, "endCursor": "next"}}}
	case labelledQuery, labelledOneQuery:
		if a.f.onLabelled != nil {
			a.f.onLabelled()
		}
		labels := []string{}
		if query == labelledQuery {
			if a.f.refuseIn {
				return &linearapp.Error{Code: "query_invalid"}
			}
			if a.f.labelledErr != nil {
				return a.f.labelledErr
			}
			labels = vars["labels"].([]string)
			if a.f.labelSets == nil {
				a.f.labelSets = map[string][]string{}
			}
			a.f.labelSets[vars["team"].(string)] = slices.Clone(labels)
		} else {
			labels = []string{vars["label"].(string)}
		}
		since, err := time.Parse(time.RFC3339Nano, vars["since"].(string))
		if err != nil {
			return err
		}
		// Like Linear, the list is filtered by the queried team (a parent's
		// filter may spill its sub-teams' and others' rows), the labels and
		// updatedAt.
		rows := []any{}
		for _, id := range a.f.labelled {
			i, _ := a.f.issue(id)
			if i.TeamID != vars["team"] && !a.f.spill {
				continue
			}
			carried := []string{labelNeeds}
			if a.f.carries != nil {
				carried = a.f.carries[id]
			}
			if !slices.ContainsFunc(carried, func(l string) bool { return slices.Contains(labels, l) }) {
				continue
			}
			updated := time.Now().UTC().Format(time.RFC3339Nano)
			if u, ok := a.f.updated[id]; ok {
				updated = u
			}
			if at, _ := time.Parse(time.RFC3339Nano, updated); !at.After(since) && !a.f.sinceIgnored {
				continue
			}
			if shown, ok := a.f.rowLabels[id]; ok {
				carried = shown
			}
			nodes := []any{}
			for _, l := range carried {
				nodes = append(nodes, map[string]any{"id": l})
			}
			var project any
			if i.ProjectID != "" && !a.f.unlisted[id] {
				project = map[string]any{"id": i.ProjectID}
			}
			rows = append(rows, map[string]any{"id": id, "updatedAt": updated, "team": map[string]any{"id": i.TeamID}, "project": project, "labels": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": a.f.rowLabelsMore[id]}}})
		}
		info := map[string]any{"hasNextPage": false}
		if n := a.f.labelledPage; n > 0 {
			from := 0
			if after, ok := vars["after"].(string); ok {
				from, _ = strconv.Atoi(after)
			}
			total := len(rows)
			rows = rows[from:min(from+n, total)]
			if from+n < total {
				info = map[string]any{"hasNextPage": true, "endCursor": strconv.Itoa(from + n)}
			}
		}
		data = map[string]any{"issues": map[string]any{"nodes": rows, "pageInfo": info}}
	case needsAgentQuery:
		if a.f.refuseLabelSet {
			return &linearapp.Error{Code: "query_invalid"}
		}
		if a.f.labelSetErr != nil {
			return a.f.labelSetErr
		}
		t := vars["team"].(string)
		if as, ok := a.f.answerAs[t]; ok {
			t = as
		}
		var node any
		if !a.f.hiddenTeams[t] {
			n := map[string]any{"id": t, "labels": a.f.labelNodes(t, t), "parent": nil}
			if p := a.f.parents[t]; p != "" {
				n["parent"] = map[string]any{"id": p, "labels": a.f.labelNodes(p, p)}
			}
			node = n
		}
		workspace := a.f.labelNodes(t, "")
		if p := a.f.parents[t]; p != "" {
			seen := map[string]bool{}
			nodes := []any{}
			for _, n := range append(workspace["nodes"].([]any), a.f.labelNodes(p, "")["nodes"].([]any)...) {
				if id := n.(map[string]any)["id"].(string); !seen[id] {
					seen[id] = true
					nodes = append(nodes, n)
				}
			}
			workspace["nodes"] = nodes
		}
		data = map[string]any{"team": node, "issueLabels": workspace}
	case documentsQuery:
		a.f.events = append(a.f.events, "docs-list")
		if a.f.refuseDocs {
			return &linearapp.Error{Code: "query_invalid"}
		}
		// Like Linear: one ordered list (newest first, or oldest first
		// with docAscending), first or last of it; docSince filters by the
		// query's since.
		refs := slices.Clone(a.f.docRefs)
		if a.f.docSince {
			since, err := time.Parse(time.RFC3339, vars["since"].(string))
			if err != nil {
				return err
			}
			refs = slices.DeleteFunc(refs, func(d linearprovider.DocRef) bool {
				at, _ := time.Parse(time.RFC3339Nano, d.UpdatedAt)
				return !at.After(since)
			})
		}
		slices.SortStableFunc(refs, func(x, y linearprovider.DocRef) int {
			xt, _ := time.Parse(time.RFC3339Nano, x.UpdatedAt)
			yt, _ := time.Parse(time.RFC3339Nano, y.UpdatedAt)
			if a.f.docAscending {
				return xt.Compare(yt)
			}
			return yt.Compare(xt)
		})
		n := len(refs)
		info := map[string]any{"hasNextPage": false, "hasPreviousPage": false}
		if last, ok := vars["last"].(int); ok && a.f.docLastAsFirst {
			// A server that serves last as first: the first rows, cut at
			// the end after them.
			refs = refs[:min(last, n)]
			info["hasNextPage"] = a.f.docMore || n > last
		} else if ok {
			refs = refs[max(n-last, 0):]
			info["hasPreviousPage"] = a.f.docMore || n > last
		} else {
			first := vars["first"].(int)
			refs = refs[:min(first, n)]
			info["hasNextPage"] = a.f.docMore || n > first
		}
		a.f.docEnds = append(a.f.docEnds, map[bool]string{true: "last", false: "first"}[vars["last"] != nil])
		nodes := []any{}
		for _, d := range refs {
			ref := func(id string) any {
				if id == "" {
					return nil
				}
				return map[string]any{"id": id}
			}
			nodes = append(nodes, map[string]any{"id": d.ID, "updatedAt": d.UpdatedAt, "team": ref(d.TeamID), "project": ref(d.ProjectID), "initiative": ref(d.InitiativeID), "issue": ref(d.IssueID)})
		}
		data = map[string]any{"documents": map[string]any{"nodes": nodes, "pageInfo": info}}
	case mentionsQuery:
		data = map[string]any{"notifications": map[string]any{"nodes": a.f.mentions, "pageInfo": map[string]any{"hasNextPage": false}}}
	case labelsQuery:
		ids, ok := a.f.issueLabels[vars["id"].(string)]
		if !ok {
			return errors.New("inaccessible issue")
		}
		nodes := []any{}
		for _, id := range ids {
			nodes = append(nodes, map[string]any{"id": id})
		}
		data = map[string]any{"issue": map[string]any{"id": vars["id"], "labels": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": a.f.labelsMore}}}}
	case healthProjectQuery:
		updates := []any{}
		for _, u := range a.f.updates {
			updates = append(updates, u)
		}
		data = map[string]any{"project": map[string]any{"id": vars["id"], "targetDate": nil, "startDate": nil, "createdAt": "2026-01-01T00:00:00.000Z", "projectUpdates": map[string]any{"nodes": updates}}}
	case healthIssuesQuery, flowIssuesQuery:
		data = map[string]any{"issues": map[string]any{"nodes": a.f.healthIssues, "pageInfo": map[string]any{"hasNextPage": a.f.healthMore, "endCursor": "next"}}}
	case flowProjectQuery:
		updates := []any{}
		for _, u := range a.f.updates {
			updates = append(updates, u)
		}
		data = map[string]any{"project": map[string]any{"id": vars["id"], "targetDate": nil, "startDate": nil, "createdAt": "2026-01-01T00:00:00.000Z", "projectUpdates": map[string]any{"nodes": updates}, "status": a.f.status(a.f.projectStatus)}, "projectStatuses": map[string]any{"nodes": a.f.statuses}}
	case flowReadBackQuery:
		data = map[string]any{"project": map[string]any{"id": vars["id"], "status": a.f.status(a.f.projectStatus)}}
	case healthActivityQuery:
		data = map[string]any{"issue": a.f.activity[vars["id"].(string)]}
	case subscribeCheckQuery:
		if a.f.subscribeErr != nil {
			return a.f.subscribeErr
		}
		if a.f.subscribeTeam == "" {
			data = map[string]any{"issue": nil}
			break
		}
		nodes := []any{}
		for _, id := range a.f.subscribeMembers {
			nodes = append(nodes, map[string]any{"id": id})
		}
		var project any
		if a.f.subscribeProject != "" {
			project = map[string]any{"id": a.f.subscribeProject}
		}
		data = map[string]any{"issue": map[string]any{"id": vars["id"], "project": project, "team": map[string]any{"id": a.f.subscribeTeam, "members": map[string]any{"nodes": nodes}}}}
	case dispatchIssueQuery, dispatchIssueTextQuery:
		issue, ok := a.f.dispatchIssues[vars["id"].(string)]
		if !ok {
			return errors.New("inaccessible issue")
		}
		data = map[string]any{"issue": issue}
	default:
		return errors.New("unexpected query")
	}
	raw, _ := json.Marshal(data)
	return json.Unmarshal(raw, out)
}

type env struct {
	t     *testing.T
	path  string
	s     *bus.Store
	f     *fakeLinear
	w     *Watcher
	pid   string
	insts map[string]string
}

func setup(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus.db")
	s, err := bus.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := &env{t: t, path: path, s: s, f: &fakeLinear{history: map[string][]map[string]any{}}, insts: map[string]string{}}
	for _, r := range []bus.Request{{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}, {Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}}, {Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "team_board", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{}}}} {
		if _, err = s.Execute(context.Background(), r); err != nil {
			t.Fatal(r.Operation, err)
		}
	}
	for _, name := range []string{"eng", "coord"} {
		out := e.call("register_session", name, map[string]any{"instance": name + "-i"})
		e.insts[name] = out["instance"].(string)
	}
	e.queryRow(`SELECT id FROM projects`, &e.pid)
	e.call("coordinator_set", "coord", map[string]any{"target_session": "coord"})
	e.w = &Watcher{Store: s, App: fakeApp{e.f}, LedgerDirectory: filepath.Join(t.TempDir(), "ledger")}
	return e
}

func (e *env) call(op, session string, a map[string]any) map[string]any {
	e.t.Helper()
	if inst, ok := e.insts[session]; ok {
		a["instance"] = inst
	}
	r, err := e.s.Execute(context.Background(), bus.Request{Operation: op, Workspace: "w", Project: "p", Session: session, Args: a})
	if err != nil {
		e.t.Fatal(op, err)
	}
	if m, ok := r.Data.(map[string]any); ok {
		return m
	}
	return map[string]any{"data": r.Data}
}

func (e *env) inbox(session, kind string) []map[string]any {
	e.t.Helper()
	out := []map[string]any{}
	for _, m := range e.call("inbox", session, map[string]any{})["messages"].([]map[string]any) {
		if m["kind"] == kind {
			out = append(out, m)
		}
	}
	return out
}

func (e *env) tick() ProjectTick {
	e.t.Helper()
	ticks := e.w.Tick(context.Background())
	if len(ticks) != 1 {
		e.t.Fatal("ticks:", ticks)
	}
	return ticks[0]
}

// dueNow makes every watch and intake cursor due.
func (e *env) dueNow() {
	e.t.Helper()
	e.sql(`UPDATE watch_cursors SET next_poll_at='2000-01-01T00:00:00.000000000Z'`)
}

// sql and queryRow reach the store's file directly, as a fixture.
func (e *env) sql(q string, args ...any) {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) queryRow(q string, dest ...any) {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow(q).Scan(dest...); err != nil {
		e.t.Fatal(err)
	}
}

// The server watcher runs the comment watch as the system session: the
// assignee is told, the tick is recorded, and a client watcher is refused.
func TestTickRunsTheCommentWatchAsTheSystemSession(t *testing.T) {
	e := setup(t)
	proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issueA, "issue_identifier": "FIX-3"})
	e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})
	e.tick() // the first poll starts the cursor
	e.f.comments = []linearprovider.Comment{{ID: humanNote, AuthorKind: "unknown", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), UntrustedBody: "a question"}}
	e.dueNow()
	tick := e.tick()
	if tick.Error != "" || tick.Comments == nil || len(tick.Comments.Notified) != 1 {
		t.Fatalf("tick: %+v %+v", tick, tick.Comments)
	}
	notices := e.inbox("eng", "linear-comment")
	if len(notices) != 1 || !strings.HasPrefix(notices[0]["sender_id"].(string), "sys_") {
		t.Fatal("the assignee was not told by the system session:", notices)
	}
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "comment_watch_lease", Workspace: "w", Project: "p", Session: "coord", Args: map[string]any{"instance": e.insts["coord"], "action": "acquire"}}); err == nil || !strings.Contains(err.Error(), "runs on the server") {
		t.Fatal("a client watcher took the lease while the server watches:", err)
	}
	// IN-M2: once the server's ticks are stale, a client watcher may take
	// over.
	e.sql(`UPDATE watch_ticks SET last_ok_at=?`, time.Now().Add(-20*time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "comment_watch_lease", Workspace: "w", Project: "p", Session: "coord", Args: map[string]any{"instance": e.insts["coord"], "action": "acquire"}}); err != nil {
		t.Fatal("no client failover after stale ticks:", err)
	}
}

func (e *env) queue() []map[string]any {
	e.t.Helper()
	return e.call("intake_list", "coord", map[string]any{})["data"].([]map[string]any)
}

func entry(at, actor string, fields map[string]any) map[string]any {
	n := map[string]any{"createdAt": at, "actorId": actor, "botActor": nil}
	for k, v := range fields {
		n[k] = v
	}
	return n
}

// S1: a needs-agent issue is proven in the binding and its label add's
// actor proven from history; a trusted actor may go furthest, a stranger
// or a bot only to notify. History before the cursor never triggers.
func TestIntakeS1ProvesTheBindingAndTheActor(t *testing.T) {
	e := setup(t)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.f.labelled = []string{issueA, issueB, issueOut}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// The newest history entry is a stranger's unrelated label change; the
	// needs-agent add before it is the trusted user's.
	e.f.history[issueA] = []map[string]any{entry(now, strangerID, map[string]any{"addedLabelIds": []string{"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}}), entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	first := e.tick()
	if first.Intake == nil || len(first.Intake.Queued) != 0 || len(e.f.queries) != 0 {
		t.Fatal("history triggered on the first poll:", first.Intake, e.f.queries)
	}
	e.f.history[issueB] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}, "botActor": map[string]any{"id": "bot"}})}
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 2 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	caps := map[string]string{}
	for _, row := range e.queue() {
		caps[row["issue_uuid"].(string)] = row["trigger_cap"].(string)
	}
	if caps[issueA] != "auto" || caps[issueB] != "notify" {
		t.Fatal("caps:", caps)
	}
	if n := len(e.inbox("coord", "intake")); n != 2 {
		t.Fatal("coordinator notices:", n)
	}
	var used int
	e.queryRow(`SELECT budget_used FROM watch_lease`, &used)
	if used < tick.Intake.Calls {
		t.Fatal("intake calls not charged:", used, tick.Intake.Calls)
	}
}

// A parent team's filter may also return its sub-teams' issues: a row of a
// team the binding doesn't list is skipped as other_team, never read or
// queued, and the bound team's own rows still are.
func TestIntakeS1SkipsAnotherTeamsRows(t *testing.T) {
	e := setup(t)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.f.labelled, e.f.spill = []string{issueA, issueOut}, true
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.tick()
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.f.history[issueOut] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.dueNow()
	reads := e.f.issueReads
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 1 || tick.Intake.Skipped["other_team"] != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if q := e.queue(); len(q) != 1 || q[0]["issue_uuid"] != issueA {
		t.Fatal("queue:", q)
	}
	if e.f.issueReads-reads != 1 {
		t.Fatal("another team's row was read:", e.f.issueReads-reads)
	}
}

// A bound sub-team's needs-agent set is its own copy and its bound
// parent's label (CROSS-TEAM-BINDING §3.2): S1 reads the sub-team with one
// query for the whole set, so an issue carrying either is queued. A label of
// the cached set that is merged away in Linear just matches nothing: the
// read stays readable and the rest of the set still reads.
func TestIntakeS1ReadsTheSubTeamsLabelSet(t *testing.T) {
	const subTeam = "abababab-abab-4bab-8bab-abababababab"
	const copyNeeds = "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"
	const subA = "a9a9a9a9-a9a9-4a9a-8a9a-a9a9a9a9a9a9"
	const subB = "b0b0b0b0-b0b0-4b0b-8b0b-b0b0b0b0b0b0"
	e := setup(t)
	e.call("taskboard_set_v2", "coord", map[string]any{"expected_revision": 1, "mode": "team_board", "linear_team_ids": []string{team, subTeam}, "linear_default_team_id": team, "linear_project_ids": []string{}})
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	needs := func(id, t string) linearprovider.Label {
		return linearprovider.Label{ID: id, Name: needsAgentLabel, TeamID: t, EligibilityKnown: true}
	}
	e.f.parents = map[string]string{subTeam: team}
	e.f.teamLabels = map[string][]linearprovider.Label{team: {needs(labelNeeds, team)}, subTeam: {needs(copyNeeds, subTeam)}}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.extra = map[string]linearprovider.IssueV2{}
	for _, id := range []string{subA, subB} {
		e.f.extra[id] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "SUB-" + id[:1], Title: "t", TeamID: subTeam}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: now}
	}
	e.f.labelled = []string{subA, subB}
	e.f.carries = map[string][]string{subA: {labelNeeds}, subB: {copyNeeds}}
	e.f.history[subA] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.f.history[subB] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{copyNeeds}})}
	e.tick()
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 2 || len(tick.Intake.Unreadable) != 0 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	// The owner merges the copy into the parent's label: the cached set
	// still names the copy, which no longer verifies.
	e.f.teamLabels[subTeam] = nil
	e.f.carries = map[string][]string{subA: {labelNeeds}, subB: {labelNeeds}}
	e.dueNow()
	if tick = e.tick(); tick.Intake == nil || len(tick.Intake.Unreadable) != 0 || len(tick.Intake.NoLabel) != 0 {
		t.Fatalf("a merged label failed the read: %+v", tick.Intake)
	}
}

// S2 is a delegation to the app, its actor the delegating human; S3 is a
// mention notification, its actor typed. Other notification types are
// ignored.
func TestIntakeS2AndS3(t *testing.T) {
	e := setup(t)
	e.tick()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.delegated = []string{issueA}
	// The newest entry is a trusted user's unrelated change; the delegation
	// before it is the stranger's.
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, map[string]any{"toDelegate": nil}), entry(now, strangerID, map[string]any{"toDelegate": map[string]any{"id": appUser}})}
	e.f.mentions = []map[string]any{
		{"id": "n1", "type": "issueCommentMention", "createdAt": now, "issue": map[string]any{"id": issueB}, "actor": map[string]any{"id": trustedUser}},
		{"id": "n2", "type": "issueSubscribed", "createdAt": now, "issue": map[string]any{"id": issueA}},
		{"id": "n3", "type": "issueCommentMention", "createdAt": now, "issue": map[string]any{"id": issueOut}, "actor": map[string]any{"id": trustedUser}},
	}
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 2 || tick.Intake.Skipped["not_in_binding"] != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	rows := map[string]map[string]any{}
	for _, row := range e.queue() {
		rows[row["issue_uuid"].(string)] = row
	}
	if rows[issueA]["signals"] != "S2" || rows[issueA]["trigger_cap"] != "notify" || rows[issueB]["signals"] != "S3" || rows[issueB]["trigger_cap"] != "auto" {
		t.Fatal(rows)
	}
}

// A transition at or before its issue's close does not re-queue it
// (IN-H2, IN2-L2); a rearm lets it.
func TestIntakeClosedRowIgnoresOldTransitions(t *testing.T) {
	e := setup(t)
	e.tick()
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	e.f.labelled = []string{issueA}
	e.f.history[issueA] = []map[string]any{entry(old, strangerID, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.dueNow()
	if tick := e.tick(); len(tick.Intake.Queued) != 1 {
		t.Fatalf("%+v", tick.Intake)
	}
	e.sql(`UPDATE intake_queue SET state='closed',closed_at=?`, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	e.dueNow()
	if tick := e.tick(); len(tick.Intake.Queued) != 0 || tick.Intake.Skipped["stale"] != 1 {
		t.Fatalf("an old transition re-queued a closed issue: %+v", tick.Intake)
	}
	e.call("intake_rearm", "coord", map[string]any{"issue_uuid": issueA})
	e.dueNow()
	if tick := e.tick(); len(tick.Intake.Queued) != 1 {
		t.Fatalf("a rearm did not let it re-queue: %+v", tick.Intake)
	}
}

// With intake off nothing is read.
func TestIntakeOffReadsNothing(t *testing.T) {
	e := setup(t)
	e.call("intake_policy_set", "coord", map[string]any{"mode": "off"})
	e.f.labelled = []string{issueA}
	e.tick()
	e.dueNow()
	if tick := e.tick(); tick.Intake == nil || !tick.Intake.Off || tick.Intake.Calls != 0 || len(e.f.queries) != 0 {
		t.Fatalf("%+v %v", tick.Intake, e.f.queries)
	}
}

// ledgerStore is a hosted server's shared ledger in memory.
type ledgerStore struct {
	mu      sync.Mutex
	records map[string]string
	inserts int
}

func (m *ledgerStore) LedgerInsert(_ context.Context, ns, op, record, _, _ string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.records[ns+"/"+op]; ok {
		return r, false, nil
	}
	m.records[ns+"/"+op] = record
	m.inserts++
	return "", true, nil
}
func (m *ledgerStore) LedgerGet(_ context.Context, ns, op string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[ns+"/"+op]
	return r, ok, nil
}
func (m *ledgerStore) LedgerReplace(_ context.Context, ns, op, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[ns+"/"+op] != from {
		return errors.New("changed")
	}
	m.records[ns+"/"+op] = to
	return nil
}
func (m *ledgerStore) LedgerDelete(_ context.Context, ns, op, record string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[ns+"/"+op] == record {
		delete(m.records, ns+"/"+op)
	}
	return nil
}
func (m *ledgerStore) LedgerMarkUnsettled(context.Context, string, string, bool) error { return nil }
func (m *ledgerStore) LedgerUnsettled(context.Context, string, int) ([]string, error) {
	return nil, nil
}

// W-M1: with a shared ledger the app's marker writes are recorded there,
// never in a local directory.
func TestMarkerWritesUseTheSharedLedger(t *testing.T) {
	e := setup(t)
	shared := &ledgerStore{records: map[string]string{}}
	e.w.Ledger, e.w.LedgerDirectory = linearboard.SharedLedger(shared, nil), ""
	proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issueA, "issue_identifier": "FIX-3"})
	e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})
	tick := e.tick()
	if tick.Error != "" || e.f.notes == 0 || shared.inserts == 0 {
		t.Fatalf("the claim marker was not written through the shared ledger: %+v notes=%d inserts=%d", tick, e.f.notes, shared.inserts)
	}
}

// W-L1: a source cut at intakePages keeps its cursor and says so; once a
// read completes, the cursor moves.
func TestIntakeTruncatedReadKeepsTheCursor(t *testing.T) {
	e := setup(t)
	e.tick()
	var started string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s2'`, &started)
	e.f.delegated, e.f.delegatedMore = []string{issueA}, true
	e.dueNow()
	before := len(e.f.queries)
	tick := e.tick()
	pages := 0
	for _, q := range e.f.queries[before:] {
		if q == delegatedQuery {
			pages++
		}
	}
	var kept string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s2'`, &kept)
	if tick.Intake == nil || !contains(tick.Intake.Truncated, "intake-s2") || kept != started || pages != intakePages {
		t.Fatalf("truncated read: %+v cursor %s→%s pages %d", tick.Intake, started, kept, pages)
	}
	e.f.delegatedMore = false
	e.dueNow()
	tick = e.tick()
	var moved string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s2'`, &moved)
	if tick.Intake == nil || len(tick.Intake.Truncated) != 0 || moved <= started {
		t.Fatalf("complete read: %+v cursor %s→%s", tick.Intake, started, moved)
	}
}

// A nudge's pass applies the due claim markers and nothing else: no
// comment poll, intake, health or document read, and it isn't the
// project's tick.
func TestMarkersPassAppliesOnlyMarkers(t *testing.T) {
	e := setup(t)
	e.tick()
	var ticked string
	e.queryRow(`SELECT last_tick_at FROM watch_ticks`, &ticked)
	proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issueA, "issue_identifier": "FIX-3"})
	e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})
	// Every comment watch and intake cursor is due: a tick would read them.
	e.dueNow()
	e.f.events, e.f.queries = nil, nil
	pass := e.w.Markers(context.Background(), e.pid)
	if pass.Error != "" || pass.Comments == nil || len(pass.Comments.Markers) != 1 || e.f.notes == 0 {
		t.Fatalf("the claim marker was not applied: %+v notes=%d", pass, e.f.notes)
	}
	if len(e.f.events) != 0 || len(e.f.queries) != 0 || pass.Intake != nil || pass.Health != nil || pass.Docs != nil || pass.Comments.Polled != 0 {
		t.Fatalf("a markers pass read more than markers: events=%v queries=%v %+v", e.f.events, e.f.queries, pass)
	}
	var after string
	e.queryRow(`SELECT last_tick_at FROM watch_ticks`, &after)
	if after != ticked {
		t.Fatal("a markers pass counted as the project's tick")
	}
}

// S1 reads each bound team once, across its projects, however many the
// binding pairs it with: a needs-agent issue in the last of more pairs
// than intakePages is queued, and the cursor moves. An issue whose list
// row shows a project outside the binding is skipped unread; one whose row
// hides its project is read, and its own read rules it out.
func TestIntakeS1ReadsTheTeamAcrossAllItsProjects(t *testing.T) {
	var logged bytes.Buffer
	old := logf
	logf = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }
	t.Cleanup(func() { logf = old })
	e := setup(t)
	n := intakePages + 2
	ids := []string{}
	e.f.projects = map[string]bool{}
	for i := 1; i <= n+1; i++ {
		e.f.projects[linearProjectID(i)] = true
	}
	for i := 1; i <= n; i++ {
		ids = append(ids, linearProjectID(i))
	}
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": ids}}); err != nil {
		t.Fatal(err)
	}
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	issue := func(id string, project int) linearprovider.IssueV2 {
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "FIX-" + id[:1], Title: "t", TeamID: team, ProjectID: linearProjectID(project)}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	last, outside, hidden := "d0000000-0000-4000-8000-000000000001", "d0000000-0000-4000-8000-000000000002", "d0000000-0000-4000-8000-000000000003"
	e.f.extra = map[string]linearprovider.IssueV2{last: issue(last, n), outside: issue(outside, n+1), hidden: issue(hidden, n+1)}
	e.f.unlisted = map[string]bool{hidden: true}
	e.f.labelled = []string{last, outside, hidden}
	e.f.labelledPage = 1
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.history[last] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.tick()
	var started string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &started)
	e.dueNow()
	e.f.issueReads = 0
	tick := e.tick()
	if tick.Intake == nil || !reflect.DeepEqual(tick.Intake.Queued, []string{last}) || len(tick.Intake.Truncated) != 0 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if tick.Intake.Skipped["not_in_binding"] != 2 || e.f.issueReads != 2 {
		t.Fatalf("skipped %v, %d issue reads", tick.Intake.Skipped, e.f.issueReads)
	}
	var moved string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &moved)
	if moved <= started {
		t.Fatal("the cursor did not move:", started, moved)
	}
	// Issues outside the binding leave no trace: only their count is kept,
	// in no table, log line or poll record. The store's files are read
	// whole, so every table counts.
	poll, _ := json.Marshal(tick)
	stored := []byte{}
	for _, suffix := range []string{"", "-wal"} {
		b, _ := os.ReadFile(e.path + suffix)
		stored = append(stored, b...)
	}
	for _, id := range []string{outside, hidden} {
		if strings.Contains(string(poll), id) || strings.Contains(logged.String(), id) || bytes.Contains(stored, []byte(id)) {
			t.Fatal("an out-of-binding issue was recorded:", id)
		}
	}
	if !bytes.Contains(stored, []byte(last)) {
		t.Fatal("the store's files hold no issue at all; the check above proves nothing")
	}
}

// A source is due again 15 minutes after its read started, and a tick up
// to 30 s early still polls it, rather than a whole tick later (the
// v0.3.5.1 smoke: due at :55:18.1, the tick at :55:14.97).
func TestIntakeSourceDueWithinTheSlack(t *testing.T) {
	e := setup(t)
	e.tick()
	var cursor, next string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s2'`, &cursor)
	e.dueNow()
	e.tick()
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s2'`, &cursor)
	e.queryRow(`SELECT next_poll_at FROM watch_cursors WHERE source='intake-s2'`, &next)
	read, _ := time.Parse(time.RFC3339Nano, cursor)
	due, _ := time.Parse(time.RFC3339Nano, next)
	if due.Sub(read) != 15*time.Minute {
		t.Fatalf("next poll %s is not 15 minutes after the read's start %s", next, cursor)
	}
	polls := func() int {
		n := 0
		for _, q := range e.f.queries {
			if q == delegatedQuery {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		in   time.Duration
		poll bool
	}{{3 * time.Second, true}, {25 * time.Second, true}, {2 * time.Minute, false}} {
		at := time.Now().UTC().Add(c.in).Format("2006-01-02T15:04:05.000000000Z")
		e.sql(`UPDATE watch_cursors SET next_poll_at='` + at + `' WHERE source='intake-s2'`)
		before := polls()
		e.tick()
		if polled := polls() > before; polled != c.poll {
			t.Fatalf("due in %s: polled %v", c.in, polled)
		}
	}
}
