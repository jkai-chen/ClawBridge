package linearapp

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// An issue's history as the app reads it (FR21): who changed what, typed
// fields only. No title, description or comment text is read, and no
// email: a user is named by ClawBridge's stored name, else Linear's
// display name, else the user ID.

// History page bounds: a request asks for 1–HistoryMaxLimit entries,
// HistoryDefaultLimit when it names none.
const (
	HistoryDefaultLimit = 20
	HistoryMaxLimit     = 50
)

// HistoryAppName names ClawBridge's own app as an actor.
const HistoryAppName = "ClawBridge app"

// The read-only GraphQL documents. The scope read comes first, so nothing
// of an issue outside the binding is read; the history read takes the
// issue's team and project again, so a move between the two reads is
// caught.
const (
	issueScopeQuery   = `query IssueHistoryScope($id: String!) { issue(id: $id) { id team { id } project { id } } }`
	issueHistoryQuery = `query IssueHistory($id: String!, $first: Int!, $after: String) { issue(id: $id) { id team { id } project { id } history(first: $first, after: $after) { nodes { createdAt actorId actor { id displayName app } botActor { id name } fromState { id name type } toState { id name type } fromAssignee { id displayName } toAssignee { id displayName } addedLabels { id name } removedLabels { id name } addedLabelIds removedLabelIds fromTeam { id key } toTeam { id key } fromProject { id name } toProject { id name } fromPriority toPriority fromParent { id identifier } toParent { id identifier } archived trashed } pageInfo { hasNextPage endCursor } } } }`
)

// GraphQLFunc runs one read-only query: App.GraphQL's shape.
type GraphQLFunc func(ctx context.Context, query string, variables map[string]any, out any) error

// IssueScope is an issue's team and project as the app reads them. Found
// is false when Linear returned no such issue.
type IssueScope struct {
	ID, TeamID, ProjectID string
	Found                 bool
}

type scopeNode struct {
	ID      string  `json:"id"`
	Team    *idNode `json:"team"`
	Project *idNode `json:"project"`
}

type idNode struct {
	ID string `json:"id"`
}

func (n *scopeNode) scope(issue string) IssueScope {
	if n == nil || !strings.EqualFold(n.ID, issue) || n.Team == nil || n.Team.ID == "" {
		return IssueScope{}
	}
	s := IssueScope{ID: strings.ToLower(n.ID), TeamID: strings.ToLower(n.Team.ID), Found: true}
	if n.Project != nil {
		s.ProjectID = strings.ToLower(n.Project.ID)
	}
	return s
}

// ReadIssueScope reads an issue's team and project.
func ReadIssueScope(ctx context.Context, gql GraphQLFunc, issue string) (IssueScope, error) {
	var out struct {
		Issue *scopeNode `json:"issue"`
	}
	if err := gql(ctx, issueScopeQuery, map[string]any{"id": issue}, &out); err != nil {
		return IssueScope{}, err
	}
	return out.Issue.scope(issue), nil
}

type userNode struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	App         bool   `json:"app"`
}

type namedNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type stateNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type teamNode struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type parentNode struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

type historyNode struct {
	CreatedAt     string      `json:"createdAt"`
	ActorID       string      `json:"actorId"`
	Actor         *userNode   `json:"actor"`
	BotActor      *namedNode  `json:"botActor"`
	FromState     *stateNode  `json:"fromState"`
	ToState       *stateNode  `json:"toState"`
	FromAssignee  *userNode   `json:"fromAssignee"`
	ToAssignee    *userNode   `json:"toAssignee"`
	AddedLabels   []namedNode `json:"addedLabels"`
	RemovedLabels []namedNode `json:"removedLabels"`
	// The label IDs stand in for labels the app's token cannot read,
	// which Linear answers as null.
	AddedLabelIDs   []string    `json:"addedLabelIds"`
	RemovedLabelIDs []string    `json:"removedLabelIds"`
	FromTeam        *teamNode   `json:"fromTeam"`
	ToTeam          *teamNode   `json:"toTeam"`
	FromProject     *namedNode  `json:"fromProject"`
	ToProject       *namedNode  `json:"toProject"`
	FromPriority    *float64    `json:"fromPriority"`
	ToPriority      *float64    `json:"toPriority"`
	FromParent      *parentNode `json:"fromParent"`
	ToParent        *parentNode `json:"toParent"`
	Archived        *bool       `json:"archived"`
	Trashed         *bool       `json:"trashed"`
}

// HistoryPage is one page of an issue's history as Linear answered it.
type HistoryPage struct {
	Scope       IssueScope
	HasNextPage bool
	EndCursor   string
	nodes       []historyNode
}

// ReadIssueHistory reads up to first entries of an issue's history after
// a Linear cursor ("" for the newest), with the issue's scope.
func ReadIssueHistory(ctx context.Context, gql GraphQLFunc, issue string, first int, after string) (HistoryPage, error) {
	if first < 1 || first > HistoryMaxLimit {
		return HistoryPage{}, fail("invalid")
	}
	var out struct {
		Issue *struct {
			scopeNode
			History struct {
				Nodes    []historyNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"history"`
		} `json:"issue"`
	}
	vars := map[string]any{"id": issue, "first": first, "after": nil}
	if after != "" {
		vars["after"] = after
	}
	if err := gql(ctx, issueHistoryQuery, vars, &out); err != nil {
		return HistoryPage{}, err
	}
	if out.Issue == nil {
		return HistoryPage{}, nil
	}
	page := HistoryPage{Scope: out.Issue.scope(issue), nodes: out.Issue.History.Nodes, HasNextPage: out.Issue.History.PageInfo.HasNextPage, EndCursor: out.Issue.History.PageInfo.EndCursor}
	if !page.Scope.Found {
		return HistoryPage{}, nil
	}
	if len(page.nodes) > first {
		return HistoryPage{}, fail("invalid_response")
	}
	// Linear's default order is newest first, which the watcher relies
	// on too: a page out of that order is refused, not reordered.
	var last time.Time
	for i, n := range page.nodes {
		at, err := time.Parse(time.RFC3339Nano, n.CreatedAt)
		if err != nil || (i > 0 && at.After(last)) {
			return HistoryPage{}, fail("invalid_response")
		}
		last = at
	}
	return page, nil
}

// UserIDs are the Linear users the page names, actors and assignees, for
// looking up the names ClawBridge stores.
func (p HistoryPage) UserIDs() []string {
	seen, out := map[string]bool{}, []string{}
	add := func(id string) {
		if id = strings.ToLower(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, n := range p.nodes {
		if n.Actor != nil {
			add(n.Actor.ID)
		} else {
			add(n.ActorID)
		}
		for _, u := range []*userNode{n.FromAssignee, n.ToAssignee} {
			if u != nil {
				add(u.ID)
			}
		}
	}
	return out
}

// HistoryActor is who made a change: a user, an integration (a bot or
// another app), ClawBridge's own app, or the system when Linear names no
// one.
type HistoryActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// HistoryState is a workflow state.
type HistoryState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// HistoryNamed is a user, label or project. Outside is a project outside
// the project's binding, whose ID and name the server hid: it reads as
// {"outside":true} only.
type HistoryNamed struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Outside bool   `json:"outside,omitempty"`
}

// HistoryTeam is a team. Outside is a team outside the project's binding,
// whose ID and key the server hid: it reads as {"outside":true} only.
type HistoryTeam struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Outside bool   `json:"outside,omitempty"`
}

// outsideJSON is a side outside the binding, as it goes on the wire.
var outsideJSON = []byte(`{"outside":true}`)

// MarshalJSON writes an outside project as {"outside":true}, with no ID
// or name; any other as before.
func (n HistoryNamed) MarshalJSON() ([]byte, error) {
	if n.Outside {
		return outsideJSON, nil
	}
	type plain HistoryNamed
	return json.Marshal(plain(n))
}

// MarshalJSON writes an outside team as {"outside":true}, with no ID or
// key; any other as before.
func (t HistoryTeam) MarshalJSON() ([]byte, error) {
	if t.Outside {
		return outsideJSON, nil
	}
	type plain HistoryTeam
	return json.Marshal(plain(t))
}

// HistoryParent is a parent issue.
type HistoryParent struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// HistoryChange is a field's value before and after; null is none.
type HistoryChange[T any] struct {
	From *T `json:"from"`
	To   *T `json:"to"`
}

// HistoryLabel is a label; Name is empty, and left out, when the app's
// token could read only its ID.
type HistoryLabel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// HistoryLabels are the labels a change added and removed.
type HistoryLabels struct {
	Added   []HistoryLabel `json:"added"`
	Removed []HistoryLabel `json:"removed"`
}

// HistoryEntry is one change. Only the fields it changed are set; Other is
// true when it changed none of them (a title or description edit, say).
type HistoryEntry struct {
	At       string                        `json:"at"`
	Actor    HistoryActor                  `json:"actor"`
	State    *HistoryChange[HistoryState]  `json:"state,omitempty"`
	Assignee *HistoryChange[HistoryNamed]  `json:"assignee,omitempty"`
	Labels   *HistoryLabels                `json:"labels,omitempty"`
	Team     *HistoryChange[HistoryTeam]   `json:"team,omitempty"`
	Project  *HistoryChange[HistoryNamed]  `json:"project,omitempty"`
	Priority *HistoryChange[float64]       `json:"priority,omitempty"`
	Parent   *HistoryChange[HistoryParent] `json:"parent,omitempty"`
	Archived *bool                         `json:"archived,omitempty"`
	Trashed  *bool                         `json:"trashed,omitempty"`
	Other    bool                          `json:"other,omitempty"`
}

// Entries maps the page's nodes, in Linear's order (newest first). appUser
// is the app's own Linear user; stored are the names ClawBridge keeps, by
// lower-case Linear user ID.
func (p HistoryPage) Entries(appUser string, stored map[string]string) []HistoryEntry {
	appUser = strings.ToLower(appUser)
	out := make([]HistoryEntry, 0, len(p.nodes))
	for _, n := range p.nodes {
		at, _ := time.Parse(time.RFC3339Nano, n.CreatedAt)
		e := HistoryEntry{At: at.UTC().Format(time.RFC3339Nano), Actor: actorOf(n, appUser, stored)}
		if n.FromState != nil || n.ToState != nil {
			e.State = &HistoryChange[HistoryState]{From: stateOf(n.FromState), To: stateOf(n.ToState)}
		}
		if n.FromAssignee != nil || n.ToAssignee != nil {
			e.Assignee = &HistoryChange[HistoryNamed]{From: userOf(n.FromAssignee, stored), To: userOf(n.ToAssignee, stored)}
		}
		if added, removed := labelList(n.AddedLabels, n.AddedLabelIDs), labelList(n.RemovedLabels, n.RemovedLabelIDs); len(added) > 0 || len(removed) > 0 {
			e.Labels = &HistoryLabels{Added: added, Removed: removed}
		}
		if n.FromTeam != nil || n.ToTeam != nil {
			e.Team = &HistoryChange[HistoryTeam]{From: teamOf(n.FromTeam), To: teamOf(n.ToTeam)}
		}
		if n.FromProject != nil || n.ToProject != nil {
			e.Project = &HistoryChange[HistoryNamed]{From: namedOf(n.FromProject), To: namedOf(n.ToProject)}
		}
		// A priority is a change only when Linear names both sides and they
		// differ: it may send the unchanged priority on every entry.
		if n.FromPriority != nil && n.ToPriority != nil && *n.FromPriority != *n.ToPriority {
			e.Priority = &HistoryChange[float64]{From: n.FromPriority, To: n.ToPriority}
		}
		if n.FromParent != nil || n.ToParent != nil {
			e.Parent = &HistoryChange[HistoryParent]{From: parentOf(n.FromParent), To: parentOf(n.ToParent)}
		}
		// Only true is shown: Linear may send false on ordinary entries
		// (unverified), so false says nothing.
		if n.Archived != nil && *n.Archived {
			e.Archived = n.Archived
		}
		if n.Trashed != nil && *n.Trashed {
			e.Trashed = n.Trashed
		}
		e.Other = e.State == nil && e.Assignee == nil && e.Labels == nil && e.Team == nil && e.Project == nil && e.Priority == nil && e.Parent == nil && e.Archived == nil && e.Trashed == nil
		out = append(out, e)
	}
	return out
}

// actorOf classifies a change's actor. The app is its own Linear user,
// as the watcher's intake proof recognises it; any other bot, or a user
// Linear marks as an app, is an integration.
func actorOf(n historyNode, appUser string, stored map[string]string) HistoryActor {
	id := strings.ToLower(n.ActorID)
	if n.Actor != nil {
		id = strings.ToLower(n.Actor.ID)
	}
	switch {
	case appUser != "" && (id == appUser || (n.BotActor != nil && strings.EqualFold(n.BotActor.ID, appUser))):
		return HistoryActor{Kind: "app", ID: appUser, Name: HistoryAppName}
	case n.BotActor != nil:
		bot := strings.ToLower(n.BotActor.ID)
		return HistoryActor{Kind: "integration", ID: bot, Name: shownName(bot, "", n.BotActor.Name)}
	case n.Actor != nil && n.Actor.App:
		return HistoryActor{Kind: "integration", ID: id, Name: shownName(id, "", n.Actor.DisplayName)}
	case id != "":
		display := ""
		if n.Actor != nil {
			display = n.Actor.DisplayName
		}
		return HistoryActor{Kind: "user", ID: id, Name: shownName(id, stored[id], display)}
	}
	return HistoryActor{Kind: "system"}
}

// shownName is the first of ClawBridge's stored name and Linear's name
// that is set and holds no "@" (never an email or a handle), else the ID.
func shownName(id string, names ...string) string {
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" && !strings.Contains(name, "@") && !strings.ContainsFunc(name, isControl) {
			return name
		}
	}
	return id
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func userOf(u *userNode, stored map[string]string) *HistoryNamed {
	if u == nil {
		return nil
	}
	id := strings.ToLower(u.ID)
	return &HistoryNamed{ID: id, Name: shownName(id, stored[id], u.DisplayName)}
}

func stateOf(s *stateNode) *HistoryState {
	if s == nil {
		return nil
	}
	return &HistoryState{ID: strings.ToLower(s.ID), Name: s.Name, Type: s.Type}
}

func namedOf(n *namedNode) *HistoryNamed {
	if n == nil {
		return nil
	}
	return &HistoryNamed{ID: strings.ToLower(n.ID), Name: n.Name}
}

// labelList is the labels Linear named, then any it gave only by ID (a
// label the app's token cannot read), without a name.
func labelList(list []namedNode, ids []string) []HistoryLabel {
	out, seen := make([]HistoryLabel, 0, len(list)), map[string]bool{}
	for _, l := range list {
		id := strings.ToLower(l.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, HistoryLabel{ID: id, Name: l.Name})
		}
	}
	for _, id := range ids {
		if id = strings.ToLower(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, HistoryLabel{ID: id})
		}
	}
	return out
}

func teamOf(t *teamNode) *HistoryTeam {
	if t == nil {
		return nil
	}
	return &HistoryTeam{ID: strings.ToLower(t.ID), Key: t.Key}
}

func parentOf(p *parentNode) *HistoryParent {
	if p == nil {
		return nil
	}
	return &HistoryParent{ID: strings.ToLower(p.ID), Identifier: p.Identifier}
}
