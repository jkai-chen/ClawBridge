package linearapp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	hIssue   = "d2100000-0000-4000-8000-000000000001"
	hTeam    = "c2100000-0000-4000-8000-000000000001"
	hProject = "e2100000-0000-4000-8000-000000000001"
	hApp     = "a2100000-0000-4000-8000-0000000000a1"
	hAda     = "b2100000-0000-4000-8000-000000000001"
	hBen     = "b2100000-0000-4000-8000-000000000002"
	hMail    = "b2100000-0000-4000-8000-000000000003"
	hBot     = "f2100000-0000-4000-8000-000000000001"
	hOther   = "b2100000-0000-4000-8000-000000000004"
)

// fakeGraphQL answers every query with data, recording what it was sent.
type fakeGraphQL struct {
	data    string
	queries []string
	vars    []map[string]any
}

func (f *fakeGraphQL) run(_ context.Context, query string, vars map[string]any, out any) error {
	f.queries = append(f.queries, query)
	f.vars = append(f.vars, vars)
	return json.Unmarshal([]byte(f.data), out)
}

func historyData(nodes string, more bool, end string) string {
	page, _ := json.Marshal(map[string]any{"hasNextPage": more, "endCursor": end})
	return `{"issue":{"id":"` + strings.ToUpper(hIssue) + `","team":{"id":"` + hTeam + `"},"project":{"id":"` + hProject + `"},"history":{"nodes":[` + nodes + `],"pageInfo":` + string(page) + `}}}`
}

// The documents are read-only queries App.GraphQL takes, and ask for no
// email, title, description or comment text.
func TestHistoryQueriesAreTypedReads(t *testing.T) {
	for _, q := range []string{issueScopeQuery, issueHistoryQuery} {
		if len(q) > 8192 || mutationWord.MatchString(q) || !strings.HasPrefix(q, "query ") {
			t.Fatal("not a read-only query:", q)
		}
		for _, banned := range []string{"email", "title", "Title", "description", "Description", "body", "comments"} {
			if strings.Contains(q, banned) {
				t.Fatalf("query reads %q: %s", banned, q)
			}
		}
	}
}

// Every field kind decodes, newest first as Linear gives it; an entry
// that changes none of them is other.
func TestHistoryDecodesEveryFieldKind(t *testing.T) {
	nodes := strings.Join([]string{
		`{"createdAt":"2026-10-30T10:00:05.000Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"fromState":{"id":"S1","name":"Backlog","type":"backlog"},"toState":{"id":"s2","name":"Todo","type":"unstarted"}}`,
		`{"createdAt":"2026-10-30T10:00:04Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"fromAssignee":null,"toAssignee":{"id":"` + hBen + `","displayName":"ben"}}`,
		`{"createdAt":"2026-10-30T10:00:03Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"addedLabels":[{"id":"L1","name":"Bug"}],"removedLabels":[]}`,
		`{"createdAt":"2026-10-30T10:00:02Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"fromTeam":{"id":"T1","key":"ENG"},"toTeam":{"id":"t2","key":"OPS"},"fromProject":null,"toProject":{"id":"P1","name":"Launch"}}`,
		`{"createdAt":"2026-10-30T10:00:01Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"fromPriority":0,"toPriority":2,"fromParent":null,"toParent":{"id":"I1","identifier":"ENG-1"}}`,
		`{"createdAt":"2026-10-30T10:00:00Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"archived":true,"trashed":false}`,
		`{"createdAt":"2026-10-29T10:00:00Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"}}`,
		// Linear may send false and an unchanged priority on ordinary
		// entries: false is no change, nor is a priority equal on both
		// sides or named on one side only, so these are other.
		`{"createdAt":"2026-10-29T10:00:00Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"archived":false,"trashed":false,"fromPriority":3,"toPriority":3}`,
		`{"createdAt":"2026-10-29T10:00:00Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"fromPriority":null,"toPriority":2}`,
		`{"createdAt":"2026-10-28T10:00:00Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"},"archived":false,"trashed":true}`,
	}, ",")
	f := &fakeGraphQL{data: historyData(nodes, true, "linear-cursor-1")}
	page, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasNextPage || page.EndCursor != "linear-cursor-1" || page.Scope != (IssueScope{ID: hIssue, TeamID: hTeam, ProjectID: hProject, Found: true}) {
		t.Fatal(page.HasNextPage, page.EndCursor, page.Scope)
	}
	if f.vars[0]["first"] != 20 || f.vars[0]["after"] != nil || f.vars[0]["id"] != hIssue {
		t.Fatal(f.vars[0])
	}
	got, _ := json.Marshal(page.Entries(hApp, nil))
	want := `[` +
		`{"at":"2026-10-30T10:00:05Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"state":{"from":{"id":"s1","name":"Backlog","type":"backlog"},"to":{"id":"s2","name":"Todo","type":"unstarted"}}},` +
		`{"at":"2026-10-30T10:00:04Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"assignee":{"from":null,"to":{"id":"` + hBen + `","name":"ben"}}},` +
		`{"at":"2026-10-30T10:00:03Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"labels":{"added":[{"id":"l1","name":"Bug"}],"removed":[]}},` +
		`{"at":"2026-10-30T10:00:02Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"team":{"from":{"id":"t1","key":"ENG"},"to":{"id":"t2","key":"OPS"}},"project":{"from":null,"to":{"id":"p1","name":"Launch"}}},` +
		`{"at":"2026-10-30T10:00:01Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"priority":{"from":0,"to":2},"parent":{"from":null,"to":{"id":"i1","identifier":"ENG-1"}}},` +
		`{"at":"2026-10-30T10:00:00Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"archived":true},` +
		`{"at":"2026-10-29T10:00:00Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"other":true},` +
		`{"at":"2026-10-29T10:00:00Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"other":true},` +
		`{"at":"2026-10-29T10:00:00Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"other":true},` +
		`{"at":"2026-10-28T10:00:00Z","actor":{"kind":"user","id":"` + hAda + `","name":"ada"},"trashed":true}]`
	if string(got) != want {
		t.Fatalf("entries\n got %s\nwant %s", got, want)
	}
	if ids := page.UserIDs(); !reflect.DeepEqual(ids, []string{hAda, hBen}) {
		t.Fatal(ids)
	}
	// A cursor is passed through to Linear as it is.
	if _, err = ReadIssueHistory(context.Background(), f.run, hIssue, 10, "linear-cursor-1"); err != nil || f.vars[1]["after"] != "linear-cursor-1" || f.vars[1]["first"] != 10 {
		t.Fatal(err, f.vars[1])
	}
}

// Each actor kind, and the name rule: ClawBridge's stored name, else
// Linear's display name, else the ID; never a name with an "@".
func TestHistoryActorKindsAndNames(t *testing.T) {
	nodes := strings.Join([]string{
		// A user ClawBridge stores a name for.
		`{"createdAt":"2026-10-30T10:00:09Z","actorId":"` + hAda + `","actor":{"id":"` + hAda + `","displayName":"ada"}}`,
		// A user with only Linear's display name.
		`{"createdAt":"2026-10-30T10:00:08Z","actorId":"` + hBen + `","actor":{"id":"` + hBen + `","displayName":"ben"}}`,
		// A user whose names are all emails: the ID.
		`{"createdAt":"2026-10-30T10:00:07Z","actorId":"` + hMail + `","actor":{"id":"` + hMail + `","displayName":"someone@example.com"}}`,
		// The app, by its own user.
		`{"createdAt":"2026-10-30T10:00:06Z","actorId":"` + strings.ToUpper(hApp) + `","actor":{"id":"` + hApp + `","displayName":"clawbridge","app":true}}`,
		// The app, named as a bot.
		`{"createdAt":"2026-10-30T10:00:05Z","botActor":{"id":"` + hApp + `","name":"ClawBridge"}}`,
		// Another integration: a bot.
		`{"createdAt":"2026-10-30T10:00:04Z","botActor":{"id":"` + hBot + `","name":"GitHub"}}`,
		// Another app acting as its own user.
		`{"createdAt":"2026-10-30T10:00:03Z","actorId":"` + hOther + `","actor":{"id":"` + hOther + `","displayName":"Zapier","app":true}}`,
		// Linear names no one.
		`{"createdAt":"2026-10-30T10:00:02Z","actorId":null,"actor":null,"botActor":null}`,
		// A user Linear names only by ID (the user is gone).
		`{"createdAt":"2026-10-30T10:00:01Z","actorId":"` + hOther + `","actor":null}`,
	}, ",")
	f := &fakeGraphQL{data: historyData(nodes, false, "")}
	page, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]string{hAda: "Ada Example", hMail: "mail@example.com"}
	var actors []HistoryActor
	for _, e := range page.Entries(hApp, stored) {
		actors = append(actors, e.Actor)
	}
	want := []HistoryActor{
		{"user", hAda, "Ada Example"},
		{"user", hBen, "ben"},
		{"user", hMail, hMail},
		{"app", hApp, HistoryAppName},
		{"app", hApp, HistoryAppName},
		{"integration", hBot, "GitHub"},
		{"integration", hOther, "Zapier"},
		{"system", "", ""},
		{"user", hOther, hOther},
	}
	if !reflect.DeepEqual(actors, want) {
		t.Fatalf("actors\n got %+v\nwant %+v", actors, want)
	}
	// Assignees follow the same rule.
	f.data = historyData(`{"createdAt":"2026-10-30T10:00:00Z","fromAssignee":{"id":"`+hAda+`","displayName":"ada"},"toAssignee":{"id":"`+hMail+`","displayName":"x@example.com"}}`, false, "")
	page, _ = ReadIssueHistory(context.Background(), f.run, hIssue, 20, "")
	e := page.Entries(hApp, stored)[0]
	if e.Actor.Kind != "system" || *e.Assignee.From != (HistoryNamed{hAda, "Ada Example"}) || *e.Assignee.To != (HistoryNamed{hMail, hMail}) {
		t.Fatal(e.Actor, e.Assignee.From, e.Assignee.To)
	}
}

// An issue Linear doesn't return is not found; a page longer than asked,
// or an entry without a time, is refused.
func TestHistoryRefusesAnUnusablePage(t *testing.T) {
	f := &fakeGraphQL{data: `{"issue":null}`}
	if page, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, ""); err != nil || page.Scope.Found {
		t.Fatal(page, err)
	}
	if s, err := ReadIssueScope(context.Background(), f.run, hIssue); err != nil || s.Found {
		t.Fatal(s, err)
	}
	f.data = `{"issue":{"id":"` + hBen + `","team":{"id":"` + hTeam + `"}}}`
	if s, err := ReadIssueScope(context.Background(), f.run, hIssue); err != nil || s.Found {
		t.Fatal("another issue's answer was taken:", s, err)
	}
	f.data = `{"issue":{"id":"` + hIssue + `","team":{"id":"` + strings.ToUpper(hTeam) + `"},"project":null}}`
	if s, err := ReadIssueScope(context.Background(), f.run, hIssue); err != nil || s != (IssueScope{ID: hIssue, TeamID: hTeam, Found: true}) {
		t.Fatal(s, err)
	}
	two := `{"createdAt":"2026-10-30T10:00:00Z"},{"createdAt":"2026-10-30T09:00:00Z"}`
	f.data = historyData(two, false, "")
	if _, err := ReadIssueHistory(context.Background(), f.run, hIssue, 1, ""); err == nil {
		t.Fatal("a page longer than asked was taken")
	}
	f.data = historyData(`{"createdAt":"yesterday"}`, false, "")
	if _, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, ""); err == nil {
		t.Fatal("an entry without a time was taken")
	}
	for _, n := range []int{0, 51} {
		if _, err := ReadIssueHistory(context.Background(), f.run, hIssue, n, ""); err == nil {
			t.Fatal("first", n)
		}
	}
}

// A label the app's token cannot read comes back null in the object list:
// its ID stands in, without a name; IDs already named aren't repeated.
func TestHistoryLabelsFallBackToIDs(t *testing.T) {
	nodes := strings.Join([]string{
		`{"createdAt":"2026-10-30T10:00:02Z","addedLabels":null,"removedLabels":null,"addedLabelIds":["L1"],"removedLabelIds":["L2"]}`,
		`{"createdAt":"2026-10-30T10:00:01Z","addedLabels":[{"id":"L1","name":"Bug"}],"removedLabels":[],"addedLabelIds":["L1","L3"],"removedLabelIds":[]}`,
		`{"createdAt":"2026-10-30T10:00:00Z","addedLabels":[],"removedLabels":[],"addedLabelIds":[],"removedLabelIds":[]}`,
	}, ",")
	f := &fakeGraphQL{data: historyData(nodes, false, "")}
	page, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(page.Entries(hApp, nil))
	want := `[` +
		`{"at":"2026-10-30T10:00:02Z","actor":{"kind":"system"},"labels":{"added":[{"id":"l1"}],"removed":[{"id":"l2"}]}},` +
		`{"at":"2026-10-30T10:00:01Z","actor":{"kind":"system"},"labels":{"added":[{"id":"l1","name":"Bug"},{"id":"l3"}],"removed":[]}},` +
		`{"at":"2026-10-30T10:00:00Z","actor":{"kind":"system"},"other":true}]`
	if string(got) != want {
		t.Fatalf("labels\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(issueHistoryQuery, "addedLabelIds removedLabelIds") {
		t.Fatal("the query reads no label IDs")
	}
}

// A page must be newest first, as Linear's default order gives it (two
// changes may share a time); one out of order is refused, not reordered.
func TestHistoryRefusesAPageOutOfOrder(t *testing.T) {
	f := &fakeGraphQL{data: historyData(`{"createdAt":"2026-10-30T10:00:00Z"},{"createdAt":"2026-10-30T10:00:00Z"},{"createdAt":"2026-10-30T09:00:00Z"}`, false, "")}
	if _, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, ""); err != nil {
		t.Fatal("equal times refused:", err)
	}
	f.data = historyData(`{"createdAt":"2026-10-30T09:00:00Z"},{"createdAt":"2026-10-30T10:00:00Z"}`, false, "")
	var e *Error
	if _, err := ReadIssueHistory(context.Background(), f.run, hIssue, 20, ""); !errors.As(err, &e) || e.Code != "invalid_response" {
		t.Fatal("an oldest-first page was taken:", err)
	}
	if strings.Contains(issueHistoryQuery, "orderBy") {
		t.Fatal("the history read names an order")
	}
}
