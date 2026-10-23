package linearprovider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// flakyServer is a real go-sdk MCP server that answers the first fail
// requests whose body holds method with HTTP status, and counts calls.
type flakyServer struct {
	mu      sync.Mutex
	method  string
	fail    int
	status  int
	counts  map[string]int
	refused int
}

func (f *flakyServer) start(t *testing.T) *http.Client {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "get_project", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"id":"` + uid + `","name":"p","teams":[{"id":"` + tid + `"}]}`}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		for _, m := range []string{"initialize", "tools/list", "tools/call"} {
			if bytes.Contains(body, []byte(`"method":"`+m+`"`)) {
				f.counts[m]++
			}
		}
		refuse := f.refused < f.fail && bytes.Contains(body, []byte(`"method":"`+f.method+`"`))
		if refuse {
			f.refused++
		}
		f.mu.Unlock()
		if refuse {
			http.Error(w, "unavailable", f.status)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	u, _ := url.Parse(h.URL)
	return &http.Client{Transport: toLocal{u, h.Client().Transport}}
}

func (f *flakyServer) count(m string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[m]
}

func quickRetry(t *testing.T) {
	t.Helper()
	old := connectRetryDelay
	connectRetryDelay = func() time.Duration { return time.Millisecond }
	t.Cleanup(func() { connectRetryDelay = old })
}

// One 5xx while connecting, at initialize or the first tools/list, is
// retried once; a second fails.
func TestConnectRetriesOneTransientFailure(t *testing.T) {
	quickRetry(t)
	for _, method := range []string{"initialize", "tools/list"} {
		f := &flakyServer{method: method, fail: 1, status: http.StatusInternalServerError, counts: map[string]int{}}
		p, e := New(context.Background(), f.start(t), false)
		if e != nil {
			t.Fatal(method, "one 5xx was not retried", e)
		}
		p.Close()
		if f.count(method) != 2 {
			t.Fatal(method, "sent", f.count(method), "times")
		}
		f = &flakyServer{method: method, fail: 2, status: http.StatusBadGateway, counts: map[string]int{}}
		if _, e = New(context.Background(), f.start(t), false); !isKind(e, "unavailable") || f.count(method) != 2 {
			t.Fatal(method, "two 5xx: want unavailable after one retry", e, f.count(method))
		}
	}
}

// A refusal is not transient, and a tool call is never retried.
func TestConnectNeverRetriesARefusalOrAToolCall(t *testing.T) {
	quickRetry(t)
	f := &flakyServer{method: "initialize", fail: 1, status: http.StatusUnauthorized, counts: map[string]int{}}
	if _, e := New(context.Background(), f.start(t), false); e == nil || f.count("initialize") != 1 {
		t.Fatal("a 401 at initialize was retried", e, f.count("initialize"))
	}
	f = &flakyServer{method: "tools/call", fail: 1, status: http.StatusInternalServerError, counts: map[string]int{}}
	p, e := New(context.Background(), f.start(t), false)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if _, e = p.GetProject(context.Background(), uid); e == nil || f.count("tools/call") != 1 {
		t.Fatal("a 5xx on a tool call was retried or hidden", e, f.count("tools/call"))
	}
}
