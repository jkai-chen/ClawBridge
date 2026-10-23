package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// End to end on Postgres: linear_write and linear_app_status run as the
// ClawBridge app against a real bus store, a faked api.linear.app and a real
// go-sdk MCP server standing in for mcp.linear.app.

const (
	awAppUser      = "a7000000-0000-4000-8000-0000000000a1"
	awOrg          = "b7000000-0000-4000-8000-0000000000b1"
	awOtherOrg     = "b7000000-0000-4000-8000-0000000000b2"
	awTeam         = "c7000000-0000-4000-8000-0000000000c1"
	awOtherTeam    = "c7000000-0000-4000-8000-0000000000c2"
	awIssue        = "d7000000-0000-4000-8000-0000000000d1"
	awForeignIssue = "d7000000-0000-4000-8000-0000000000d2"
	awComment      = "e7000000-0000-4000-8000-0000000000e1"
	awToken        = "lin_oauth_app_token_e2e"
	awSession      = "agent"
	awMarker       = "— 🤖 ClawBridge agent"
)

func awUnique(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// awLinearAPI fakes api.linear.app: the client-credentials token endpoint
// and the GraphQL viewer, which answers only for the minted token.
type awLinearAPI struct {
	mu    sync.Mutex
	mints int
}

func (l *awLinearAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	reply := func(status int, v any) (*http.Response, error) {
		raw, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{"Content-Type": {"application/json"}}, Request: r}, nil
	}
	switch r.URL.String() {
	case "https://api.linear.app/oauth/token":
		l.mints++
		return reply(200, map[string]any{"access_token": awToken, "token_type": "Bearer", "expires_in": 2591999, "scope": "read comments:create issues:create app:assignable app:mentionable"})
	case "https://api.linear.app/graphql":
		if r.Header.Get("Authorization") != "Bearer "+awToken {
			return reply(401, map[string]any{})
		}
		return reply(200, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": awAppUser, "app": true, "organization": map[string]any{"id": awOrg}}}})
	}
	return nil, errors.New("unexpected Linear API request " + r.URL.String())
}

// awToLocal sends what the app addressed to mcp.linear.app to the test server.
type awToLocal struct {
	to   *url.URL
	base http.RoundTripper
}

func (r awToLocal) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.URL.Host != "mcp.linear.app" {
		return nil, errors.New("unexpected MCP destination " + q.URL.String())
	}
	x := q.Clone(q.Context())
	x.URL.Scheme, x.URL.Host, x.Host = r.to.Scheme, r.to.Host, r.to.Host
	return r.base.RoundTrip(x)
}

// awMCP is a real go-sdk MCP server with native-shaped get_team, get_issue,
// save_comment (the verbatim live schema) and list_comments. The app "sees
// everything": get_issue answers any issue it knows, in any team.
type awMCP struct {
	mu       sync.Mutex
	auths    []string
	calls    map[string]int
	saved    []map[string]any
	comments []map[string]any
	issues   map[string]string
}

func (m *awMCP) count(tool string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[tool]
}

func (m *awMCP) authorizations() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.auths...)
}

func (m *awMCP) savedBodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for _, s := range m.saved {
		b, _ := s["body"].(string)
		out = append(out, b)
	}
	return out
}

func awSaveCommentTool(t *testing.T) *mcp.Tool {
	t.Helper()
	raw, err := os.ReadFile("../linearprovider/testdata/save-comment-20261022.json")
	if err != nil {
		t.Fatal(err)
	}
	var tool mcp.Tool
	if err = json.Unmarshal(raw, &tool); err != nil {
		t.Fatal(err)
	}
	return &tool
}

func awObject(props map[string]any, required ...string) map[string]any {
	req := []any{}
	for _, r := range required {
		req = append(req, r)
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

func newAWMCP(t *testing.T) (*awMCP, *httptest.Server) {
	t.Helper()
	m := &awMCP{calls: map[string]int{}, issues: map[string]string{awIssue: awTeam, awForeignIssue: awOtherTeam}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic-linear", Version: "1"}, nil)
	text := func(v any) *mcp.CallToolResult {
		raw, _ := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
	}
	notFound := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: `{"error":"Entity not found"}`}}}
	args := func(req *mcp.CallToolRequest) map[string]any {
		var a map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &a)
		return a
	}
	str := func(a map[string]any, k string) string { s, _ := a[k].(string); return s }
	stringProp := map[string]any{"type": "string"}
	srv.AddTool(&mcp.Tool{Name: "get_team", InputSchema: awObject(map[string]any{"query": stringProp}, "query")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := args(req)
		m.mu.Lock()
		m.calls["get_team"]++
		m.mu.Unlock()
		q := str(a, "query")
		if q != awTeam && q != awOtherTeam {
			return notFound, nil
		}
		return text(map[string]any{"id": q, "name": "Synthetic", "key": "SYN"}), nil
	})
	srv.AddTool(&mcp.Tool{Name: "get_issue", InputSchema: awObject(map[string]any{"id": stringProp}, "id")}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := args(req)
		m.mu.Lock()
		m.calls["get_issue"]++
		team, ok := m.issues[str(a, "id")]
		m.mu.Unlock()
		if !ok {
			return notFound, nil
		}
		identifier := "SYN-6"
		if team == awOtherTeam {
			identifier = "OTH-9"
		}
		return text(map[string]any{"id": identifier, "uuid": str(a, "id"), "teamId": team, "projectId": nil, "title": "synthetic", "status": "Backlog", "statusType": "backlog", "assignee": nil, "url": "https://linear.example/issue/" + identifier, "parentId": nil, "relations": map[string]any{"blocks": []any{}, "blockedBy": []any{}, "relatedTo": []any{}, "duplicateOf": nil}, "labels": []any{}, "assigneeId": nil}), nil
	})
	srv.AddTool(awSaveCommentTool(t), func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := args(req)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		row := map[string]any{"id": awComment, "body": str(a, "body"), "createdAt": now, "updatedAt": now, "author": map[string]any{"id": awAppUser, "name": "ClawBridge"}, "onBehalfOf": nil, "parentId": nil, "resolvedAt": nil, "quotedText": nil, "attachments": []any{}}
		m.mu.Lock()
		m.calls["save_comment"]++
		m.saved = append(m.saved, a)
		m.comments = append(m.comments, row)
		m.mu.Unlock()
		// Native receipts name no issue, so the board reads the comment back.
		return text(row), nil
	})
	srv.AddTool(&mcp.Tool{Name: "list_comments", Description: "List comments on a Linear issue.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"cursor":     stringProp,
		"issueId":    stringProp,
		"projectId":  stringProp,
		"documentId": stringProp,
		"limit":      map[string]any{"type": "number", "default": 50, "maximum": 250},
		"orderBy":    map[string]any{"type": "string", "default": "updatedAt", "enum": []any{"createdAt", "updatedAt"}},
	}}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := args(req)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.calls["list_comments"]++
		rows := []any{}
		if str(a, "issueId") == awIssue {
			for i := len(m.comments) - 1; i >= 0; i-- {
				rows = append(rows, m.comments[i])
			}
		}
		return text(map[string]any{"comments": rows, "hasNextPage": false}), nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.auths = append(m.auths, r.Header.Get("Authorization"))
		m.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	return m, h
}

type awFixture struct {
	t         *testing.T
	store     *bus.Store
	owner     string
	workspace string
	writer    *AppWriter
	mcp       *awMCP
	api       *awLinearAPI
}

func (f *awFixture) identity() bus.Identity {
	return bus.Identity{Principal: f.owner, Issuer: bus.GoogleIssuer, Subject: "sub-owner"}
}

func (f *awFixture) must(r bus.Request) bus.Result {
	f.t.Helper()
	out, err := f.store.ExecuteAsIdentity(context.Background(), f.identity(), r)
	if err != nil {
		f.t.Fatal(r.Operation, err)
	}
	return out
}

// newAWFixture opens the disposable Postgres with a fresh owner, workspace
// and project, registers the owner's session, links a team board pinned to
// pin (nil leaves it unpinned) and configures the app against the fakes.
func newAWFixture(t *testing.T, pin any) *awFixture {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	f := &awFixture{t: t, owner: awUnique("owner") + "@appwrite-test.example", workspace: awUnique("awws")}
	store, err := bus.OpenPostgres(ctx, dsn, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	f.must(bus.Request{Operation: "workspace_init", Workspace: f.workspace, Args: map[string]any{"mode": "server"}})
	f.must(bus.Request{Operation: "project_create", Workspace: f.workspace, Args: map[string]any{"slug": "p"}})
	f.must(bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i", "role": "worker"}})
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{
		"instance":               awSession + "-i",
		"expected_revision":      0,
		"mode":                   "team_board",
		"linear_team_ids":        []string{awTeam},
		"linear_default_team_id": awTeam,
		"linear_project_ids":     []string{},
		"linear_workspace_id":    pin,
	}})
	m, h := newAWMCP(t)
	u, _ := url.Parse(h.URL)
	f.mcp, f.api = m, &awLinearAPI{}
	app, err := linearapp.New(linearapp.Config{
		ClientID:     awUnique("client"),
		Secret:       "s3cret-e2e",
		Key:          []byte("0123456789abcdef0123456789abcdef"),
		Store:        store,
		HTTP:         &http.Client{Transport: f.api},
		MCPTransport: awToLocal{u, h.Client().Transport},
	})
	if err != nil || app == nil {
		t.Fatal(app, err)
	}
	f.writer = &AppWriter{App: app, Store: store}
	return f
}

func (f *awFixture) write(operation string, args map[string]any) (bus.Result, error) {
	return f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_write", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i", "operation": operation, "args": args}})
}

func awJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(string(raw), err)
	}
	return out
}

// A comment on an issue of the bound team is posted once, as the app, with
// the agent marker; a retry of its operation_id answers the same receipt
// and sends nothing.
func TestPostgresAppWriteCommentAsTheApp(t *testing.T) {
	f := newAWFixture(t, awOrg)
	args := map[string]any{"id": awIssue, "body": "hello from the app", "operation_id": "e2e-comment-1"}
	first, err := f.write("comment", args)
	if err != nil {
		t.Fatal("comment refused:", err)
	}
	data, _ := first.Data.(map[string]any)
	if _, uncertain := data["outcome"]; uncertain || data["receipt"] == nil {
		t.Fatal("comment not accepted", first.Data)
	}
	receipt := awJSON(t, data["receipt"])
	if receipt["id"] != awComment || receipt["issue_id"] != awIssue {
		t.Fatal("receipt", receipt)
	}
	if n := f.mcp.count("save_comment"); n != 1 {
		t.Fatal("save_comment sent", n, "times")
	}
	if f.mcp.count("list_comments") == 0 {
		t.Fatal("the receipt was not read back through list_comments")
	}
	body := f.mcp.savedBodies()[0]
	last := body[strings.LastIndex(body, "\n\n")+2:]
	if !strings.HasPrefix(body, "hello from the app\n\n") || !strings.HasPrefix(last, awMarker) || body != "hello from the app\n\n"+awMarker+" · session "+awSession+" · "+bus.EngineerName("", f.owner) {
		t.Fatalf("posted body lacks the agent marker naming the session and its engineer: %q", body)
	}
	auths := f.mcp.authorizations()
	if len(auths) == 0 {
		t.Fatal("the MCP server saw no request")
	}
	for _, a := range auths {
		if a != "Bearer "+awToken {
			t.Fatalf("MCP request authorized as %q, not the app token", a)
		}
	}
	// The same operation_id again: the recorded receipt, nothing sent.
	again, err := f.write("comment", args)
	if err != nil {
		t.Fatal("retry refused:", err)
	}
	againData, _ := again.Data.(map[string]any)
	if _, uncertain := againData["outcome"]; uncertain || !reflect.DeepEqual(awJSON(t, againData["receipt"]), receipt) {
		t.Fatal("retry answered another receipt", again.Data, receipt)
	}
	if n := f.mcp.count("save_comment"); n != 1 {
		t.Fatal("the retry sent save_comment again:", n)
	}
	if f.api.mints != 1 {
		t.Fatal("the app token was minted", f.api.mints, "times")
	}
}

// The app sees every team, so an issue outside the bound team is readable;
// the board still refuses it and sends nothing.
func TestPostgresAppWriteRefusesAnIssueOutsideTheBinding(t *testing.T) {
	f := newAWFixture(t, awOrg)
	_, err := f.write("comment", map[string]any{"id": awForeignIssue, "body": "not here", "operation_id": "e2e-foreign-1"})
	if err == nil {
		t.Fatal("a comment outside the bound team was accepted")
	}
	if code, _ := serviceError(err); code != "invalid" || !strings.Contains(responseMessage(code, err), "outside the selected task-board scope") {
		t.Fatal("refusal", code, responseMessage(code, err), err)
	}
	if f.mcp.count("get_issue") == 0 {
		t.Fatal("the refusal did not come from the issue's team")
	}
	if n := f.mcp.count("save_comment"); n != 0 {
		t.Fatal("save_comment sent", n, "times for an out-of-bound issue")
	}
}

// A binding not pinned to the app's organisation is refused before any
// Linear write: unpinned, or pinned to another organisation.
func TestPostgresAppWriteRefusesAnUnpinnedOrForeignBinding(t *testing.T) {
	for name, c := range map[string]struct {
		pin  any
		want string
	}{
		"unpinned": {nil, "workspace_unpinned"},
		"foreign":  {awOtherOrg, "workspace_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAWFixture(t, c.pin)
			_, err := f.write("comment", map[string]any{"id": awIssue, "body": "pinned?", "operation_id": "e2e-pin-" + name})
			if err == nil {
				t.Fatal("accepted")
			}
			code, _ := serviceError(err)
			if message := responseMessage(code, err); !strings.Contains(message, c.want) {
				t.Fatalf("refusal %q (%s) does not name %s: %v", message, code, c.want, err)
			}
			if n := f.mcp.count("save_comment"); n != 0 {
				t.Fatal("save_comment sent", n, "times")
			}
		})
	}
}

// linear_app_status names the app user, its organisation and, for the
// project's binding, which bound teams the app can see.
func TestPostgresAppStatusReportsTheBoundTeams(t *testing.T) {
	f := newAWFixture(t, awOrg)
	out, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_app_status", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := out.Data.(map[string]any)
	if data["app_user_id"] != awAppUser || data["linear_org_id"] != awOrg {
		t.Fatal("status identity", data)
	}
	if _, failed := data["binding"]; failed {
		t.Fatal("binding unread", data["binding"])
	}
	teams, _ := data["teams_visible"].(map[string]bool)
	if !reflect.DeepEqual(teams, map[string]bool{awTeam: true}) {
		t.Fatal("teams_visible", data["teams_visible"])
	}
	if n := f.mcp.count("save_comment"); n != 0 {
		t.Fatal("status wrote", n)
	}
}
