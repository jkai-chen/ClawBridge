package linearprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// classifyServer serves a real go-sdk MCP server whose tools/call POSTs can
// be answered by refuse instead, and whose tool reports rejected.
func classifyServer(t *testing.T, refuse func(http.ResponseWriter) bool, rejected string) (*Provider, *httptest.Server) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_team", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		if rejected != "" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: rejected}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"id":"` + tid + `","name":"team"}`}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if strings.Contains(string(body), `"tools/call"`) && refuse != nil && refuse(w) {
			return
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
	return p, h
}

func status(code int, header ...string) func(http.ResponseWriter) bool {
	return func(w http.ResponseWriter) bool {
		for n := 0; n+1 < len(header); n += 2 {
			w.Header().Set(header[n], header[n+1])
		}
		w.WriteHeader(code)
		io.WriteString(w, "free text that must not surface")
		return true
	}
}

// reset drops the connection after the request arrived: possibly applied.
func reset(w http.ResponseWriter) bool {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
	return true
}

func TestProviderFailuresCarryOnlySafeDetailAndProveUnsentRequests(t *testing.T) {
	for name, c := range map[string]struct {
		refuse   func(http.ResponseWriter) bool
		rejected string
		kill     bool
		want     Error
	}{
		"rate limited":      {refuse: status(429, "Retry-After", "30"), want: Error{Kind: "rate_limited", Status: 429, RetryAfter: "30", NotSent: true}},
		"hostile retry":     {refuse: status(429, "Retry-After", "30; rm -rf /tmp/x"), want: Error{Kind: "rate_limited", Status: 429, NotSent: true}},
		"unauthorized":      {refuse: status(401), want: Error{Kind: "unauthorized", Status: 401, NotSent: true}},
		"forbidden":         {refuse: status(403), want: Error{Kind: "unauthorized", Status: 403, NotSent: true}},
		"server error":      {refuse: status(500), want: Error{Kind: "unavailable"}},
		"server killed":     {kill: true, want: Error{Kind: "unavailable", NotSent: true}},
		"reset after write": {refuse: reset, want: Error{Kind: "unavailable"}},
		"rejected, code":    {rejected: `{"errors":[{"message":"secret title","extensions":{"code":"RATELIMITED"}}]}`, want: Error{Kind: "provider_rejected", LinearCode: "RATELIMITED"}},
		"rejected, no code": {rejected: `{"code":"not a code; secret"}`, want: Error{Kind: "provider_rejected"}},
		"rejected, text":    {rejected: "Rate limited: secret title", want: Error{Kind: "provider_rejected"}},
	} {
		t.Run(name, func(t *testing.T) {
			p, h := classifyServer(t, c.refuse, c.rejected)
			if c.kill {
				h.CloseClientConnections()
				h.Close()
			}
			before := p.Sent()
			_, err := p.GetTeam(context.Background(), tid)
			var got *Error
			if !errors.As(err, &got) || *got != c.want {
				t.Fatalf("got %#v (%v), want %#v", got, err, c.want)
			}
			if sent := p.Sent() - before; (sent == 0) != c.want.NotSent {
				t.Fatalf("sent count moved by %d for NotSent=%v", sent, c.want.NotSent)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "free text") || strings.Contains(err.Error(), "\x1b") {
				t.Fatal("unsafe provider text surfaced", err)
			}
		})
	}
}

func TestANegotiationFailureSendsNothing(t *testing.T) {
	p, _ := classifyServer(t, nil, "")
	before := p.Sent()
	if _, err := p.GetProject(context.Background(), iid); err == nil || p.Sent() != before {
		t.Fatal("a missing tool must fail without sending", err, p.Sent()-before)
	}
}
