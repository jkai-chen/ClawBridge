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
	// bindingErr fails every binding read.
	bindingErr error
}

func (a *appServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	switch r.Operation {
	case "linear_app_status":
		a.statuses++
		return a.status()
	case "linear_write":
		a.writes = append(a.writes, r)
		return a.write(r)
	case "taskboard_get_v2":
		if a.bindingErr != nil {
			return bus.Result{}, a.bindingErr
		}
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

// Every write to the app says whether an agent's environment runs it, so
// the server credits only a person's own write to them (intake attribution).
func TestHostedAgentWritesSayWhetherAnAgentRunsThem(t *testing.T) {
	server := &appServer{status: appOn, write: func(bus.Request) (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"receipt": map[string]any{"kind": "comment"}}}, nil
	}}
	s, _, scope := routedV2(t, server)
	defer func(old func() []string) { agentEnviron = old }(agentEnviron)
	for i, env := range [][]string{{"HOME=/h", "TERM=xterm"}, {"HOME=/h", "CLAUDECODE=1"}, {"CODEX_HOME=/c"}, {"CLAWBRIDGE_SESSION=s"}} {
		agentEnviron = func() []string { return env }
		in := commentIn
		in.OperationID = "c-env-" + string(rune('a'+i))
		if _, e := s.WriteV2(context.Background(), scope, in); e != nil {
			t.Fatal(e)
		}
	}
	got := []any{}
	for _, w := range server.writes {
		v, ok := w.Args["agent_marker"]
		if !ok {
			t.Fatal("a write without agent_marker", w.Args)
		}
		got = append(got, v)
	}
	if !reflect.DeepEqual(got, []any{"", "CLAUDECODE", "CODEX_HOME", "CLAWBRIDGE_SESSION"}) {
		t.Fatal(got)
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
		// Linear down: the server proved nothing was sent, so it is a
		// refusal, never uncertain, and says so.
		"linear down": {func() (bus.Result, error) {
			return bus.Result{}, &remote.HTTPError{Status: 503, Code: "linear_unavailable", Message: "Linear is unavailable; nothing was sent; safe to retry with the same operation ID"}
		}, func(_ any, e error) bool {
			return e != nil && !IsUncertain(e) && !IsInProgress(e) && strings.Contains(e.Error(), "Linear is unavailable; nothing was sent")
		}},
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

// §3: a server in maintenance refused before anything reached Linear, so
// the answer is definitive, never uncertain. The CLI's service doesn't
// retry; the MCP server's retries once, after Retry-After, with the same
// request, within the call's deadline.
func TestAppWriteInMaintenance(t *testing.T) {
	drain := func() (bus.Result, error) {
		return bus.Result{}, &remote.HTTPError{Status: 503, Code: "maintenance", RetryAfter: time.Second}
	}
	receipt := func() (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"receipt": map[string]any{"id": "c"}}}, nil
	}
	answers := []func() (bus.Result, error){drain, receipt}
	server := &appServer{status: appOn, write: func(bus.Request) (bus.Result, error) {
		next := answers[0]
		answers = answers[1:]
		return next()
	}}
	s, _, scope := routedV2(t, server)
	_, e := s.WriteV2(context.Background(), scope, commentIn)
	if e == nil || IsUncertain(e) || !strings.Contains(e.Error(), "nothing was sent to Linear; retry the same operation_id after 1 s") || len(server.writes) != 1 {
		t.Fatal(e, len(server.writes))
	}
	answers = []func() (bus.Result, error){drain, receipt}
	server.writes = nil
	s.RetryMaintenanceOnce()
	started := time.Now()
	if got, e := s.WriteV2(context.Background(), scope, commentIn); e != nil || got == nil || len(server.writes) != 2 || time.Since(started) < time.Second {
		t.Fatal(got, e, len(server.writes))
	}
	if !reflect.DeepEqual(server.writes[0].Args, server.writes[1].Args) {
		t.Fatal("the retry changed the request")
	}
	// Once only, and only when the deadline leaves room.
	answers = []func() (bus.Result, error){drain, drain, receipt}
	server.writes = nil
	if _, e = s.WriteV2(context.Background(), scope, commentIn); e == nil || IsUncertain(e) || len(server.writes) != 2 {
		t.Fatal(e, len(server.writes))
	}
	answers = []func() (bus.Result, error){drain, receipt}
	server.writes = nil
	short, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, e = s.WriteV2(short, scope, commentIn); e == nil || len(server.writes) != 1 {
		t.Fatal(e, len(server.writes))
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
		{WriteInput: WriteInput{Operation: "update", OperationID: "u-3", IssueID: issueID}, DueDate: linearprovider.Field[*string]{Present: true, Value: text("2026-11-14")}},
		{WriteInput: WriteInput{Operation: "update", OperationID: "u-4", IssueID: issueID}, DueDate: linearprovider.Field[*string]{Present: true}},
		{WriteInput: WriteInput{Operation: "create", OperationID: "n-2", Title: text("t")}, TeamID: teamID, DueDate: linearprovider.Field[*string]{Present: true, Value: text("2026-11-14")}},
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
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
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
	if _, e := s.WriteV2(context.Background(), scope, commentIn); e == nil || !strings.Contains(e.Error(), "already sent with your own Linear profile") || len(server.writes) != 0 {
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
	// A binding that cannot be read cannot prove the ID is new: nothing is sent.
	server.bindingErr = errors.New("authority unavailable")
	fresh := commentIn
	fresh.OperationID = "c-2"
	if _, e := s.WriteV2(context.Background(), scope, fresh); e == nil || len(server.writes) != 0 {
		t.Fatal("routed without reading the binding", e)
	}
	server.bindingErr = nil
	if _, e := s.WriteV2(context.Background(), scope, fresh); e != nil || len(server.writes) != 1 || len(p.bodies) != 1 {
		t.Fatal("a new operation", e)
	}
}

// A write is sent with accept_queued only when the server says it queues
// writes and the caller handles a queued answer (the MCP tools, the CLI): a
// claim note or an older server never sees it. A queued answer is a result,
// not an error; a queued write that ended unsent is an error.
func TestQueuedAppWrites(t *testing.T) {
	queues := func() (bus.Result, error) {
		r, _ := appOn()
		r.Data.(map[string]any)["queues_writes"] = true
		return r, nil
	}
	answer := map[string]any{"outcome": "queued", "operation_id": "c-1", "reason": "linear_rate_limit", "position": 1}
	for name, c := range map[string]struct {
		status func() (bus.Result, error)
		ctx    context.Context
		want   bool
	}{
		"queues, caller accepts":  {queues, AcceptQueued(context.Background()), true},
		"queues, caller doesn't":  {queues, context.Background(), false},
		"older server, accepting": {appOn, AcceptQueued(context.Background()), false},
	} {
		server := &appServer{status: c.status, write: func(bus.Request) (bus.Result, error) {
			return bus.Result{Source: "server", Data: answer}, nil
		}}
		s, _, scope := routedV2(t, server)
		got, e := s.WriteV2(c.ctx, scope, commentIn)
		if sent := server.writes[0].Args["accept_queued"] == true; sent != c.want {
			t.Fatal(name, "accept_queued sent:", sent)
		}
		if c.want && (e != nil || !reflect.DeepEqual(got, answer)) {
			t.Fatal(name, "queued answer:", got, e)
		}
		if !c.want && !IsUncertain(e) {
			// Without accept_queued, a receiptless answer stays uncertain.
			t.Fatal(name, got, e)
		}
	}
	for outcome, check := range map[string]func(error) bool{
		"failed": func(e error) bool {
			return e != nil && !IsUncertain(e) && strings.Contains(e.Error(), "nothing was sent")
		},
		"dead": func(e error) bool { return e != nil && !IsUncertain(e) && strings.Contains(e.Error(), "ended dead") },
		"expired": func(e error) bool {
			return e != nil && !IsUncertain(e) && strings.Contains(e.Error(), "new operation_id")
		},
		"uncertain": IsUncertain,
	} {
		server := &appServer{status: queues, write: func(bus.Request) (bus.Result, error) {
			return bus.Result{Source: "server", Data: map[string]any{"outcome": outcome, "reason": "linear_rate_limit"}}, nil
		}}
		s, _, scope := routedV2(t, server)
		if got, e := s.WriteV2(AcceptQueued(context.Background()), scope, commentIn); got != nil || !check(e) {
			t.Fatal(outcome, got, e)
		}
	}
}

// Only Linear's rate limit, or the pacer's, reads as a write to queue: a
// write not sent for another reason is not, nor one that may have gone.
func TestRateLimitedNotSent(t *testing.T) {
	limited := writeFailure(nil, 0, false, &linearprovider.Error{Kind: "rate_limited", RetryAfter: "5", NotSent: true}, func(e error) error { return e })
	if wait, ok := RateLimitedNotSent(notDispatchedError{errors.New("nothing was sent"), &linearprovider.Error{Kind: "rate_limited", RetryAfter: "5", NotSent: true}}); !ok || wait != 5*time.Second {
		t.Fatal(wait, ok)
	}
	for name, err := range map[string]error{
		"down":     notDispatchedError{errors.New("nothing was sent"), &linearprovider.Error{Kind: "unavailable", NotSent: true}},
		"no cause": notDispatched(errors.New("nothing was sent")),
		"maybe":    limited,
	} {
		if _, ok := RateLimitedNotSent(err); ok {
			t.Fatal(name, "read as rate-limited and unsent")
		}
	}
}
