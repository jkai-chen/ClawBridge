package linearboard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// historyServer answers linear_issue_history as a hosted server would and
// records each request; every other operation goes to the authority.
type historyServer struct {
	*authorityV2
	requests []bus.Request
	answer   func(bus.Request) (bus.Result, error)
}

func (h *historyServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "linear_issue_history" {
		return h.authorityV2.Execute(ctx, r)
	}
	h.requests = append(h.requests, r)
	return h.answer(r)
}

// historyRejection is a hosted server's refusal (remote.HTTPError).
type historyRejection struct {
	status        int
	code, message string
}

func (e historyRejection) Error() string {
	return "cloud request rejected (" + e.code + "): " + e.message + "; no write queued"
}
func (e historyRejection) Rejection() (int, string, string) { return e.status, e.code, e.message }

// historyPage is the server's answer for issueID: one entry, more to come.
func historyPage(r bus.Request) (bus.Result, error) {
	return bus.Result{Source: "server", Data: map[string]any{
		"issue_uuid": r.Args["issue_uuid"], "limit": 20, "has_more": true, "next_cursor": "c2",
		"entries": []any{map[string]any{"at": "2026-10-30T03:12:09Z", "actor": map[string]any{"kind": "integration", "id": "f1000000-0000-4000-8000-000000000001", "name": "GitHub"},
			"state": map[string]any{"from": map[string]any{"id": "c1000000-0000-4000-8000-0000000000a1", "name": "In Progress", "type": "started"}, "to": map[string]any{"id": "c1000000-0000-4000-8000-0000000000a2", "name": "Todo", "type": "unstarted"}}}},
	}}, nil
}

func historyTest(t *testing.T, answer func(bus.Request) (bus.Result, error)) (*Service, *historyServer, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	i := p.issues[issueID]
	i.Identifier = "ENG-88"
	p.issues[issueID] = i
	h := &historyServer{authorityV2: a, answer: answer}
	s.cfg.Executor = h
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return refProvider{providerV2: p}, nil }
	return s, h, p, scope
}

// FR21: the issue is proved in the client's own scope first, by UUID or
// identifier; only its UUID, the page size, the cursor and the session's
// instance reach the server, and the page comes back as the server wrote
// it, with the identifier the client read.
func TestIssueHistoryProvesTheIssueThenAsksTheServer(t *testing.T) {
	for _, ref := range []string{issueID, "ENG-88"} {
		s, h, p, scope := historyTest(t, historyPage)
		page, e := s.IssueHistoryV2(context.Background(), scope, ref, IssueHistoryInputV2{Limit: 5, Cursor: "c1"})
		if e != nil {
			t.Fatal(ref, e)
		}
		if p.reads != 1 || len(h.requests) != 1 {
			t.Fatal(ref, "reads", p.reads, "server requests", len(h.requests))
		}
		r := h.requests[0]
		want := map[string]any{"issue_uuid": issueID, "limit": 5, "cursor": "c1", "instance": "secret-instance"}
		if r.Workspace != "ws" || r.Project != "prj" || r.Session != "ses" || len(r.Args) != len(want) {
			t.Fatal(ref, r)
		}
		for k, v := range want {
			if r.Args[k] != v {
				t.Fatal(ref, k, r.Args[k])
			}
		}
		if page.IssueUUID != issueID || page.Identifier != "ENG-88" || !page.HasMore || page.NextCursor != "c2" || len(page.Entries) != 1 {
			t.Fatalf("%s: %+v", ref, page)
		}
		entry := page.Entries[0]
		if entry.Actor.Kind != "integration" || entry.Actor.Name != "GitHub" || entry.State == nil || entry.State.From.Name != "In Progress" || entry.State.To.Name != "Todo" {
			t.Fatalf("%s: %+v", ref, entry)
		}
	}
	// Without a limit, a cursor or a session, only the issue is sent.
	s, h, _, scope := historyTest(t, historyPage)
	scope.Session = ""
	if _, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{}); e != nil {
		t.Fatal(e)
	}
	if args := h.requests[0].Args; len(args) != 1 || args["issue_uuid"] != issueID {
		t.Fatal(args)
	}
}

// An issue the client's own scope check refuses never reaches the server,
// and is refused as `linear issue` refuses it: out of scope.
func TestIssueHistoryOutOfScopeNeverAsksTheServer(t *testing.T) {
	s, h, p, scope := historyTest(t, historyPage)
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, Identifier: "OPS-5", TeamID: "99999999-aaaa-4aaa-8aaa-999999999999"}, ProjectKnown: true}
	_, eIssue := s.IssueV2(context.Background(), scope, parentA)
	for _, ref := range []string{parentA, "OPS-5"} {
		_, e := s.IssueHistoryV2(context.Background(), scope, ref, IssueHistoryInputV2{})
		if e == nil || eIssue == nil || e.Error() != eIssue.Error() || !IsOutOfScope(e) {
			t.Fatalf("%s: %v (issue read: %v)", ref, e, eIssue)
		}
	}
	if len(h.requests) != 0 {
		t.Fatal("an out-of-scope issue reached the server")
	}
}

// The server's own scope refusals are the board's out-of-scope texts and
// are shown as the client's are; its other refusals show their text.
func TestIssueHistoryServerRefusals(t *testing.T) {
	for _, text := range []string{
		"issue is outside the selected task-board scope",
		"issue is outside the selected task-board scope: no issue UUID in this binding",
		"linked_only: this binding shows the issue's team only as linked issues; nothing was read in full",
		"issue project/team pair is outside the verified scope",
	} {
		s, _, _, scope := historyTest(t, func(bus.Request) (bus.Result, error) {
			return bus.Result{}, historyRejection{400, "invalid", text}
		})
		_, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{})
		if e == nil || e.Error() != text || !IsOutOfScope(e) {
			t.Fatal(text, e)
		}
	}
	for _, refusal := range []historyRejection{
		{400, "invalid", "app_unavailable: issue history needs the ClawBridge Linear app on the server"},
		{400, "invalid", "invalid_cursor: the cursor is not for this issue or this binding revision; read the history again without one"},
		{429, "rate_limited", "the ClawBridge Linear app is rate limited; nothing more was read; retry at 2026-10-30T03:13:00Z"},
	} {
		s, _, _, scope := historyTest(t, func(bus.Request) (bus.Result, error) { return bus.Result{}, refusal })
		_, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{})
		var rejected rejection
		if e == nil || e.Error() != refusal.message || IsOutOfScope(e) || !errors.As(e, &rejected) {
			t.Fatal(refusal, e)
		}
	}
	// A local store refuses with the text itself.
	local := errors.New("app_unavailable: issue history needs the ClawBridge Linear app on the server")
	s, _, _, scope := historyTest(t, func(bus.Request) (bus.Result, error) { return bus.Result{}, local })
	if _, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{}); e != local {
		t.Fatal(e)
	}
}

// A server older than the read: a local or in-process authority names the
// operation it doesn't know; a hosted one refuses it generically, and a
// probe with an argument the read never takes tells it from a server that
// has the read.
func TestIssueHistoryOlderServer(t *testing.T) {
	const want = "the ClawBridge server is older than this client and has no issue history read; upgrade the server"
	s, _, _, scope := historyTest(t, func(bus.Request) (bus.Result, error) {
		return bus.Result{}, errors.New(`unsupported operation "linear_issue_history"`)
	})
	if _, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{}); e == nil || e.Error() != want {
		t.Fatal(e)
	}
	generic := historyRejection{400, "invalid", "request rejected; verify scope, session instance and operation arguments; no write queued"}
	s, h, _, scope := historyTest(t, func(bus.Request) (bus.Result, error) { return bus.Result{}, generic })
	if _, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{Limit: 3}); e == nil || e.Error() != want {
		t.Fatal(e)
	}
	if len(h.requests) != 2 || h.requests[1].Args["probe"] != 1 || h.requests[1].Args["issue_uuid"] != issueID || h.requests[1].Args["limit"] != nil {
		t.Fatal(h.requests)
	}
	// A server that has the read refuses the probe by name: the first
	// refusal stands.
	s, h, _, scope = historyTest(t, func(r bus.Request) (bus.Result, error) {
		if r.Args["probe"] != nil {
			return bus.Result{}, historyRejection{400, "invalid", "linear_issue_history takes instance, issue_uuid, limit and cursor only"}
		}
		return bus.Result{}, generic
	})
	if _, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{}); e == nil || e.Error() != generic.message || len(h.requests) != 2 {
		t.Fatal(e)
	}
}

func TestIssueHistoryRefusesBadInputAndAnswers(t *testing.T) {
	s, h, p, scope := historyTest(t, historyPage)
	for _, in := range []struct {
		ref string
		in  IssueHistoryInputV2
	}{
		{"eng-88", IssueHistoryInputV2{}},
		{"", IssueHistoryInputV2{}},
		{issueID, IssueHistoryInputV2{Limit: 51}},
		{issueID, IssueHistoryInputV2{Limit: -1}},
		{issueID, IssueHistoryInputV2{Cursor: strings.Repeat("c", 8193)}},
	} {
		if _, e := s.IssueHistoryV2(context.Background(), scope, in.ref, in.in); e == nil {
			t.Fatal("accepted", in)
		}
	}
	if p.tools != 0 || p.reads != 0 || len(h.requests) != 0 {
		t.Fatal("bad input reached Linear or the server")
	}
	for name, answer := range map[string]func(bus.Request) (bus.Result, error){
		"another issue": func(bus.Request) (bus.Result, error) {
			return bus.Result{Source: "server", Data: map[string]any{"issue_uuid": parentA, "entries": []any{}}}, nil
		},
		"not the server": func(r bus.Request) (bus.Result, error) {
			return bus.Result{Source: "local-authority", Data: map[string]any{"issue_uuid": r.Args["issue_uuid"], "entries": []any{}}}, nil
		},
		"more without a cursor": func(r bus.Request) (bus.Result, error) {
			return bus.Result{Source: "server", Data: map[string]any{"issue_uuid": r.Args["issue_uuid"], "has_more": true, "entries": []any{}}}, nil
		},
		"unreadable": func(r bus.Request) (bus.Result, error) {
			return bus.Result{Source: "server", Data: map[string]any{"issue_uuid": r.Args["issue_uuid"], "entries": "nope"}}, nil
		},
	} {
		s, _, _, scope := historyTest(t, answer)
		if _, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{}); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	// A last page has no cursor and an empty one lists no entries.
	s, _, _, scope = historyTest(t, func(r bus.Request) (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"issue_uuid": r.Args["issue_uuid"], "limit": 20, "has_more": false, "next_cursor": "stale"}}, nil
	})
	page, e := s.IssueHistoryV2(context.Background(), scope, issueID, IssueHistoryInputV2{})
	if e != nil || page.NextCursor != "" || page.Entries == nil || len(page.Entries) != 0 {
		t.Fatalf("%+v %v", page, e)
	}
}

// The MCP adapter takes id, limit (1–50) and cursor only.
func TestIssueHistoryExecuteV2Arguments(t *testing.T) {
	s, h, _, scope := historyTest(t, historyPage)
	if _, e := s.ExecuteV2(context.Background(), scope, "issue_history", map[string]any{"id": "ENG-88", "limit": float64(7), "cursor": "c1"}); e != nil {
		t.Fatal(e)
	}
	if args := h.requests[0].Args; args["limit"] != 7 || args["cursor"] != "c1" || args["issue_uuid"] != issueID {
		t.Fatal(args)
	}
	for _, bad := range []map[string]any{
		{"id": issueID, "limit": float64(60)},
		{"id": issueID, "comments": true},
		{"id": issueID, "cursor": 5},
	} {
		if _, e := s.ExecuteV2(context.Background(), scope, "issue_history", bad); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	if len(h.requests) != 1 {
		t.Fatal("a refused call reached the server")
	}
}
