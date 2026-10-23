package linearboard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/remote"
)

const appUser = "a1000000-0000-4000-8000-000000000001"

// appServer is a hosted server in front of authorityV2: it answers
// linear_app_status and linear_write as told, and passes the rest on.
type appServer struct {
	*authorityV2
	status   func() (bus.Result, error)
	write    func(bus.Request) (bus.Result, error)
	statuses int
	writes   []bus.Request
}

func (a *appServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "linear_app_status":
		a.statuses++
		return a.status()
	case "linear_write":
		a.writes = append(a.writes, r)
		return a.write(r)
	}
	return a.authorityV2.Execute(ctx, r)
}

func appOn() (bus.Result, error) {
	return bus.Result{Source: "server", Data: map[string]any{"configured": true, "app_user_id": appUser, "linear_org_id": "o"}}, nil
}

func routedV2(t *testing.T, server *appServer) (*Service, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	server.authorityV2 = a
	s.cfg.Executor, s.cfg.AppRoute = server, true
	return s, p, scope
}

var commentIn = WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "c-1", IssueID: issueID, Body: "hello"}}

// A server with the app gets every agent write as linear_write, with the
// caller's arguments; nothing is sent with the member's own profile.
func TestHostedAgentWritesGoToTheServerAsTheApp(t *testing.T) {
	receipt := map[string]any{"kind": "comment", "outcome": "accepted", "id": "c1000000-0000-4000-8000-000000000001"}
	server := &appServer{status: appOn, write: func(bus.Request) (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"receipt": receipt}}, nil
	}}
	s, p, scope := routedV2(t, server)
	scope.Session = "ses"
	got, e := s.WriteV2(context.Background(), scope, commentIn)
	if e != nil || !reflect.DeepEqual(got, receipt) || len(p.bodies) != 0 {
		t.Fatal(got, e, p.bodies)
	}
	w := server.writes[0]
	if w.Workspace != scope.Workspace || w.Project != scope.Project || w.Session != "ses" || w.Args["instance"] != "secret-instance" || w.Args["operation"] != "comment" || !reflect.DeepEqual(w.Args["args"], map[string]any{"operation_id": "c-1", "id": issueID, "body": "hello"}) {
		t.Fatal("linear_write request", w)
	}
	if _, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "r-1", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}); e != nil || len(server.writes) != 2 || server.writes[1].Args["operation"] != "relation_add" {
		t.Fatal("relation", e, server.writes)
	}
	if server.statuses != 1 {
		t.Fatal("the app status was asked more than once", server.statuses)
	}
	if s.appUserID(context.Background()) != appUser {
		t.Fatal("app user")
	}
	// A claim note is the bus's own write and keeps its path.
	marker := commentIn
	marker.marker = true
	marker.OperationID = "m-1"
	if _, e = s.WriteV2(context.Background(), scope, marker); len(server.writes) != 2 {
		t.Fatal("a claim note went to linear_write", e)
	}
}

// A server without the app, or older than v0.2.8, refuses the status as
// invalid: writes stay with the member's profile. An unanswered status
// sends nothing and is asked again.
func TestHostedWritesWithoutTheAppUseTheProfile(t *testing.T) {
	server := &appServer{status: func() (bus.Result, error) {
		return bus.Result{}, &remote.HTTPError{Status: 400, Code: "invalid"}
	}}
	s, p, scope := routedV2(t, server)
	if _, e := s.WriteV2(context.Background(), scope, commentIn); e != nil || len(p.bodies) != 1 || len(server.writes) != 0 {
		t.Fatal(e, p.bodies)
	}
	if s.appUserID(context.Background()) != "" || server.statuses != 1 {
		t.Fatal("no app", server.statuses)
	}
	down := &appServer{status: func() (bus.Result, error) { return bus.Result{}, &remote.HTTPError{Status: 503, Code: "unavailable"} }}
	s, p, scope = routedV2(t, down)
	for i := 0; i < 2; i++ {
		if _, e := s.WriteV2(context.Background(), scope, commentIn); e == nil || len(p.bodies) != 0 {
			t.Fatal("wrote without knowing the route", e)
		}
	}
	if down.statuses != 2 {
		t.Fatal("an unanswered status was cached", down.statuses)
	}
	bad := &appServer{status: func() (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"configured": true, "app_user_id": "not-a-uuid"}}, nil
	}}
	s, p, scope = routedV2(t, bad)
	if _, e := s.WriteV2(context.Background(), scope, commentIn); e == nil || len(p.bodies) != 0 {
		t.Fatal("an invalid status routed a write", e)
	}
}

// The server's answers keep their types: in progress, uncertain, refused,
// and an accepted write with a receipt outcome.
func TestAppWriteAnswersKeepTheirTypes(t *testing.T) {
	for name, c := range map[string]struct {
		answer func() (bus.Result, error)
		check  func(any, error) bool
	}{
		"in progress": {func() (bus.Result, error) {
			return bus.Result{}, &remote.HTTPError{Status: 409, Code: "in_progress", Message: "in_progress: retry"}
		}, func(_ any, e error) bool { return IsInProgress(e) && !IsUncertain(e) }},
		"uncertain": {func() (bus.Result, error) {
			return bus.Result{}, &remote.HTTPError{Status: 409, Code: "conflict", Message: "outcome uncertain"}
		}, func(_ any, e error) bool { return IsUncertain(e) && !IsInProgress(e) }},
		"refused": {func() (bus.Result, error) {
			return bus.Result{}, &remote.HTTPError{Status: 400, Code: "invalid", Message: "workspace_unpinned"}
		}, func(_ any, e error) bool { return e != nil && !IsUncertain(e) && !IsInProgress(e) }},
		"server down": {func() (bus.Result, error) {
			return bus.Result{}, &remote.HTTPError{Status: 503, Code: "unavailable"}
		}, func(_ any, e error) bool { return IsUncertain(e) }},
		"transport": {func() (bus.Result, error) {
			return bus.Result{}, errors.New("cloud write failed or its outcome is uncertain")
		}, func(_ any, e error) bool { return IsUncertain(e) }},
		"no receipt": {func() (bus.Result, error) { return bus.Result{Source: "server", Data: map[string]any{}}, nil }, func(_ any, e error) bool { return IsUncertain(e) }},
		"outcome": {func() (bus.Result, error) {
			return bus.Result{Source: "server", Data: map[string]any{"receipt": map[string]any{"id": "x"}, "outcome": "accepted_ledger_unrecorded"}}, nil
		}, func(r any, e error) bool {
			var re *ReceiptError
			return r != nil && errors.As(e, &re) && re.Outcome == "accepted_ledger_unrecorded"
		}},
	} {
		answer := c.answer
		server := &appServer{status: appOn, write: func(bus.Request) (bus.Result, error) { return answer() }}
		s, _, scope := routedV2(t, server)
		if got, e := s.WriteV2(context.Background(), scope, commentIn); !c.check(got, e) {
			t.Fatal(name, got, e)
		}
	}
}

// What a client sends is exactly what the server reads back, and an absent
// field is never sent.
func TestWriteArgsRoundTrip(t *testing.T) {
	text := func(s string) *string { return &s }
	absent := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "u-0", IssueID: issueID}, Project: linearprovider.Field[*string]{Value: text(projectA)}, Labels: linearprovider.Field[[]string]{Value: []string{labelA}}}
	if got := writeArgsV2(absent); len(got) != 2 {
		t.Fatal("absent fields sent", got)
	}
	for _, in := range []WriteInputV2{
		commentIn,
		{WriteInput: WriteInput{Operation: "comment", OperationID: "c-2", IssueID: issueID, Body: "reply"}, Parent: linearprovider.Field[*string]{Present: true, Value: text(parentA)}, InResponseTo: parentA},
		{WriteInput: WriteInput{Operation: "create", OperationID: "n-1", Title: text("t"), Description: text(""), StateID: text(memberA)}, TeamID: teamID, Project: linearprovider.Field[*string]{Present: true, Value: text(projectA)}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}, Assignee: linearprovider.Field[*string]{Present: true, Value: text(memberA)}},
		{WriteInput: WriteInput{Operation: "update", OperationID: "u-1", IssueID: issueID, Title: text("new")}, ExpectedTitle: text("old"), ExpectedDescriptionSHA256: text("abc"), Project: linearprovider.Field[*string]{Present: true}, Parent: linearprovider.Field[*string]{Present: true}, Assignee: linearprovider.Field[*string]{Present: true}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}},
		{WriteInput: WriteInput{Operation: "update", OperationID: "u-2", IssueID: issueID}, AddLabels: []string{labelA}, RemoveLabels: []string{}},
	} {
		got, e := parseWriteArgsV2(in.Operation, writeArgsV2(in))
		if e != nil || !reflect.DeepEqual(got, in) {
			t.Fatalf("%+v\n%+v %v", in, got, e)
		}
	}
}

// A comment the ClawBridge app authored reads as ClawBridge's, even without
// a ledger row; one the ledger recorded keeps its session; anyone else's
// stays as the ledger says.
func TestAppAuthoredCommentsReadAsClawBridges(t *testing.T) {
	server := &appServer{status: appOn}
	s, p, scope := routedV2(t, server)
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	result := linearprovider.Comments{Items: []linearprovider.Comment{
		{ID: "c1000000-0000-4000-8000-000000000001", AuthorID: appUser, AuthorKind: "unknown", CreatedAt: "2026-10-22T08:00:00Z", UpdatedAt: "2026-10-22T08:00:00Z", UntrustedBody: "from the app"},
		{ID: "c1000000-0000-4000-8000-000000000002", AuthorID: memberA, AuthorKind: "unknown", CreatedAt: "2026-10-22T08:01:00Z", UpdatedAt: "2026-10-22T08:01:00Z", UntrustedBody: "from a person"},
		{ID: "c1000000-0000-4000-8000-000000000003", AuthorID: appUser, AuthorKind: "unknown", CreatedAt: "2026-10-22T08:02:00Z", UpdatedAt: "2026-10-22T08:02:00Z", UntrustedBody: "recorded"},
	}, Complete: true, Watermark: "cw1.x"}
	server.authorityV2.comments.flags = map[string]any{"c1000000-0000-4000-8000-000000000003": map[string]any{"session_id": "ses", "session_name": "worker"}}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: &result}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	out, e := s.IssueV2WithComments(context.Background(), scope, issueID, CommentsInputV2{})
	if e != nil || len(out.Items) != 3 {
		t.Fatal(out, e)
	}
	app, person, recorded := out.Items[0], out.Items[1], out.Items[2]
	if recorded.ClawBridgeMatch != "ledger" || recorded.ClawBridgeSession == nil || recorded.ClawBridgeSession.Name != "worker" {
		t.Fatal("a recorded app comment lost its ledger match", recorded)
	}
	if app.PostedByClawBridge == nil || !*app.PostedByClawBridge || app.ClawBridgeMatch != "app" {
		t.Fatal("app comment", app)
	}
	if person.PostedByClawBridge != nil && *person.PostedByClawBridge || person.ClawBridgeMatch != "" {
		t.Fatal("a person's comment read as ClawBridge's", person)
	}
}

// v1App is a hosted server with the app in front of the V1 fake.
type v1App struct{ *fakeAuthority }

func (v v1App) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_app_status" {
		return appOn()
	}
	return v.fakeAuthority.Execute(ctx, r)
}

// A V1 write, made with the member's profile, is refused when the server
// writes as the app: nothing is sent.
func TestV1WritesAreRefusedWhenTheServerWritesAsTheApp(t *testing.T) {
	s, a, p, scope := testService(t)
	s.cfg.Executor, s.cfg.AppRoute = v1App{a}, true
	if _, e := s.Write(context.Background(), scope, WriteInput{Operation: "comment", OperationID: "v1-c", IssueID: issueID, Body: "hello"}); !errors.Is(e, errV1AppWrites) || len(p.bodies) != 0 {
		t.Fatal(e, p.bodies)
	}
}

// An operation_id this client already wrote with the member's profile is
// refused once the server writes as the app, never sent again; a new one
// goes through.
func TestAnOperationFromBeforeTheAppIsNeverResent(t *testing.T) {
	receipt := map[string]any{"kind": "comment", "outcome": "accepted", "id": "c1000000-0000-4000-8000-000000000009"}
	server := &appServer{status: appOn, write: func(bus.Request) (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"receipt": receipt}}, nil
	}}
	s, p, scope := routedV2(t, server)
	s.cfg.AppRoute = false
	if _, e := s.WriteV2(context.Background(), scope, commentIn); e != nil || len(p.bodies) != 1 {
		t.Fatal("the personal write", e)
	}
	s.cfg.AppRoute = true
	if _, e := s.WriteV2(context.Background(), scope, commentIn); e == nil || !strings.Contains(e.Error(), "start a new operation_id") || len(server.writes) != 0 {
		t.Fatal("a pre-app operation was sent as the app", e, server.writes)
	}
	// An unreadable record is no proof it was never sent: nothing is sent.
	records, _ := filepath.Glob(filepath.Join(s.cfg.LedgerDirectory, "*", "*.json"))
	if len(records) != 1 || os.WriteFile(records[0], []byte("{"), 0o600) != nil {
		t.Fatal("ledger records", records)
	}
	if _, e := s.WriteV2(context.Background(), scope, commentIn); e == nil || !strings.Contains(e.Error(), "cannot read") || len(server.writes) != 0 {
		t.Fatal("an unreadable record", e)
	}
	fresh := commentIn
	fresh.OperationID = "c-2"
	if _, e := s.WriteV2(context.Background(), scope, fresh); e != nil || len(server.writes) != 1 || len(p.bodies) != 1 {
		t.Fatal("a new operation", e)
	}
}
