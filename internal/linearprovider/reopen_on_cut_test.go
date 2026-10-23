package linearprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const projectB = "33333333-3333-4333-8333-333333333333"

// cutServer is a real go-sdk MCP server, reached through New(): get_project
// answers project uid with a response larger than MaxResultBytes and any
// other project normally; save_comment always answers too large. It answers
// in JSON, where go-sdk closes the client session when a body is cut (an SSE
// stream can survive a cut), and it counts initialize requests, so a test
// can tell that the provider opened a new session.
type cutServer struct {
	mu          sync.Mutex
	calls       map[string]int
	initialized int
}

func newCutServer(t *testing.T) (*Provider, *cutServer) {
	t.Helper()
	cs := &cutServer{calls: map[string]int{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	huge := strings.Repeat("x", MaxResultBytes+1024)
	count := func(name, key string) {
		cs.mu.Lock()
		cs.calls[name+":"+key]++
		cs.mu.Unlock()
	}
	text := func(v any) *mcp.CallToolResult {
		raw, _ := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
	}
	srv.AddTool(&mcp.Tool{Name: "get_project", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		count("get_project", args.Query)
		if args.Query == uid {
			return text(map[string]any{"id": uid, "name": huge, "teams": []any{map[string]any{"id": tid}}}), nil
		}
		return text(map[string]any{"id": args.Query, "name": "p", "teams": []any{map[string]any{"id": tid}}}), nil
	})
	srv.AddTool(&mcp.Tool{Name: "save_comment", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"issueId": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}, "required": []any{"issueId", "body"}}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		count("save_comment", "")
		return text(map[string]any{"id": commentID, "issueId": iid, "body": huge}), nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"initialize"`)) {
			cs.mu.Lock()
			cs.initialized++
			cs.mu.Unlock()
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	u, _ := url.Parse(h.URL)
	p, e := New(context.Background(), &http.Client{Transport: toLocal{u, h.Client().Transport}}, false)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	return p, cs
}

func (cs *cutServer) sessions() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.initialized
}

func (cs *cutServer) count(key string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.calls[key]
}

func isKind(e error, kind string) bool {
	var pe *Error
	return errors.As(e, &pe) && pe.Kind == kind
}

// After a read the response cap cut, the next call runs on a fresh session
// (go-sdk closed the old one), and the cut read is not sent again.
func TestReopenOnCutServesTheNextRead(t *testing.T) {
	p, cs := newCutServer(t)
	if _, e := p.GetProject(context.Background(), uid); !isKind(e, "result_limit") {
		t.Fatal("cut read", e)
	}
	if cs.sessions() != 1 {
		t.Fatal("a session opened before the next call", cs.sessions())
	}
	project, e := p.GetProject(context.Background(), projectB)
	if cs.sessions() != 2 {
		t.Fatal("the next call did not open a new session", cs.sessions())
	}
	if e != nil || project.ID != projectB {
		t.Fatal("the read after a cut ran on the closed session", e)
	}
	if cs.count("get_project:"+uid) != 1 || cs.count("get_project:"+projectB) != 1 {
		t.Fatal("a read was sent again", cs.calls)
	}
	if p.Alive(context.Background()) {
		t.Fatal("a connection that hit the cap offered to the pool")
	}
}

// A write whose receipt the cap cut is never sent again; it stays
// uncertain for its caller, and the next call still gets a fresh session.
func TestReopenOnCutNeverResendsAWrite(t *testing.T) {
	p, cs := newCutServer(t)
	if _, e := p.Comment(context.Background(), iid, "body"); e == nil {
		t.Fatal("cut write receipt accepted")
	}
	if project, e := p.GetProject(context.Background(), projectB); e != nil || project.ID != projectB || cs.sessions() != 2 {
		t.Fatal("the call after a cut write ran on the closed session", e, cs.sessions())
	}
	if n := cs.count("save_comment:"); n != 1 {
		t.Fatal("the cut write was sent", n, "times")
	}
}

// A reopen that fails sends nothing and says so (NotSent), so a write
// caller knows it was not dispatched; the next call tries again.
func TestReopenFailureSendsNothing(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{tool("get_project", map[string]string{"query": "string"}, "query")}, result: map[string]any{"id": projectB, "name": "p", "teams": []any{map[string]any{"id": tid}}}}
	p, _ := NewWithCaller(context.Background(), f)
	opened := 0
	p.reopen = func(context.Context) (ToolCaller, interface{ Close() error }, error) {
		opened++
		if opened == 1 {
			return nil, nil, errors.New("dial failed")
		}
		return f, nil, nil
	}
	// A call that ends at the cap leaves the session to be reopened.
	f.err = &Error{Kind: "result_limit"}
	if _, e := p.GetProject(context.Background(), projectB); !isKind(e, "result_limit") || opened != 0 {
		t.Fatal("cut call", e, opened)
	}
	f.err = nil
	_, e := p.GetProject(context.Background(), projectB)
	var pe *Error
	if !errors.As(e, &pe) || pe.Kind != "unavailable" || !pe.NotSent || opened != 1 || len(f.calls) != 1 {
		t.Fatal("failed reopen", e, opened, f.calls)
	}
	if project, e := p.GetProject(context.Background(), projectB); e != nil || project.ID != projectB || opened != 2 || len(f.calls) != 2 {
		t.Fatal("reopen not retried on the next call", e, opened, f.calls)
	}
	if _, e := p.GetProject(context.Background(), projectB); e != nil || opened != 2 || len(f.calls) != 3 {
		t.Fatal("a healthy session reopened again", e, opened)
	}
}
