package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toLocal sends the provider's fixed Linear endpoint to a local go-sdk server.
type toLocal struct {
	to   *url.URL
	base http.RoundTripper
}

func (r toLocal) RoundTrip(q *http.Request) (*http.Response, error) {
	x := q.Clone(q.Context())
	x.URL.Scheme, x.URL.Host, x.Host = r.to.Scheme, r.to.Host, r.to.Host
	return r.base.RoundTrip(x)
}

// realServer opens the production client against a real go-sdk MCP server.
func realServer(t *testing.T, stateless bool) (*Provider, *mcp.Server, *httptest.Server) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	h := httptest.NewTLSServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: stateless}))
	t.Cleanup(h.Close)
	u, _ := url.Parse(h.URL)
	p, e := New(context.Background(), &http.Client{Transport: toLocal{u, h.Client().Transport}}, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	return p, srv, h
}

func TestAliveAgainstRealStatefulAndStatelessServers(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		t.Run(fmt.Sprint("stateless=", stateless), func(t *testing.T) {
			p, _, _ := realServer(t, stateless)
			if !p.Alive(context.Background()) || !p.Alive(context.Background()) {
				t.Fatal("a healthy connection was not reused")
			}
		})
	}
}

func TestAliveReopensAnExpiredOrKilledSession(t *testing.T) {
	p, srv, _ := realServer(t, false)
	for s := range srv.Sessions() {
		s.Close()
	}
	if p.Alive(context.Background()) {
		t.Fatal("a session the server expired was reported alive")
	}

	p, _, h := realServer(t, false)
	h.CloseClientConnections()
	h.Close()
	if p.Alive(context.Background()) {
		t.Fatal("a killed server was reported alive")
	}
}

func TestAliveExcusesOnlyTheStatelessPingMetaRejection(t *testing.T) {
	exact := `missing or invalid _meta field "io.modelcontextprotocol/protocolVersion"`
	for name, c := range map[string]struct {
		err   error
		alive bool
	}{
		"stateless _meta rejection": {fmt.Errorf("sending %q: %w", "ping", &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: exact}), true},
		"other invalid params":      {&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid params"}, false},
		"same message, other code":  {&jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: exact}, false},
		"unknown session":           {&jsonrpc.Error{Code: -32001, Message: "session not found"}, false},
		"plain text of the message": {errors.New(exact), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &pingFake{fake: &fake{}, pingErr: c.err}
			p, e := NewWithCaller(context.Background(), f)
			if e != nil {
				t.Fatal(e)
			}
			if p.Alive(context.Background()) != c.alive || p.Alive(context.Background()) != c.alive {
				t.Fatalf("alive != %v", c.alive)
			}
		})
	}
}
