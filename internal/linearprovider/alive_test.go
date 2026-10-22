package linearprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pingFake struct {
	*fake
	pings   int
	pingErr error
}

func (f *pingFake) Ping(context.Context, *mcp.PingParams) error {
	f.pings++
	return f.pingErr
}

func TestAliveTracksFailedCallsMissingCapabilitiesAndDeadSessions(t *testing.T) {
	healthy := func() (*pingFake, *Provider) {
		f := &pingFake{fake: &fake{tools: []*mcp.Tool{tool("get_team", map[string]string{"query": "string"}, "query")}, result: map[string]any{"id": tid, "name": "team"}}}
		p, e := NewWithCaller(context.Background(), f)
		if e != nil {
			t.Fatal(e)
		}
		return f, p
	}
	f, p := healthy()
	if _, e := p.GetTeam(context.Background(), tid); e != nil || !p.Alive(context.Background()) || f.pings != 1 {
		t.Fatalf("healthy connection: %v pings=%d", e, f.pings)
	}

	f, p = healthy()
	f.err = errors.New("transport closed")
	if _, e := p.GetTeam(context.Background(), tid); e == nil || p.Alive(context.Background()) || f.pings != 0 {
		t.Fatalf("failed call left the connection alive: %v pings=%d", e, f.pings)
	}

	_, p = healthy()
	if _, e := p.GetProject(context.Background(), iid); e == nil || e.(*Error).Kind != "capability_unavailable" || p.Alive(context.Background()) {
		t.Fatalf("capability_unavailable left the connection alive: %v", e)
	}

	f, p = healthy()
	f.pingErr = errors.New("session expired")
	if p.Alive(context.Background()) || p.Alive(context.Background()) || f.pings != 1 {
		t.Fatalf("dead session reported alive, pings=%d", f.pings)
	}
}
