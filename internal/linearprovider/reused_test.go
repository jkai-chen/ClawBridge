package linearprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// staleFake answers its first failures with err, then results.
type staleFake struct {
	*fake
	failures int
	err      error
	closed   int
}

func (f *staleFake) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if f.failures > 0 {
		f.failures--
		f.calls = append(f.calls, p.Name)
		return nil, f.err
	}
	return f.fake.CallTool(ctx, p)
}

func (f *staleFake) Close() error {
	f.closed++
	return nil
}

func teamFake(failures int, err error) *staleFake {
	return &staleFake{fake: &fake{tools: []*mcp.Tool{tool("get_team", map[string]string{"query": "string"}, "query")}, result: map[string]any{"id": tid, "name": "team"}}, failures: failures, err: err}
}

func reusedProvider(t *testing.T, f *staleFake) *Provider {
	t.Helper()
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	p.closer = f
	return p
}

// reopener opens providers over fakes in turn and counts the opens; past
// limit it fails the test, so a reopen loop ends.
func reopener(t *testing.T, limit int, fakes ...*staleFake) (func(context.Context) (*Provider, error), *int) {
	n := 0
	return func(context.Context) (*Provider, error) {
		n++
		if n > limit || n > len(fakes) {
			t.Error("reopened", n, "times")
			return nil, fail("unavailable")
		}
		p, e := NewWithCaller(context.Background(), fakes[n-1])
		if p != nil {
			p.closer = fakes[n-1]
		}
		return p, e
	}, &n
}

var unsent = &Error{Kind: "unavailable", NotSent: true}

// A kept session whose first call fails unsent is reopened once, and the
// call runs on the new session.
func TestReusedReopensOnceAndSends(t *testing.T) {
	for name, err := range map[string]error{"unsent": unsent, "session gone": &Error{Kind: "unavailable", gone: true}, "401": &Error{Kind: "unauthorized", Status: 401, NotSent: true}} {
		t.Run(name, func(t *testing.T) {
			old, fresh := teamFake(1, err), teamFake(0, nil)
			p := reusedProvider(t, old)
			reopen, opened := reopener(t, 1, fresh)
			p.Reused(reopen)
			if team, e := p.GetTeam(context.Background(), tid); e != nil || team.ID != tid {
				t.Fatal(e)
			}
			if *opened != 1 || old.closed != 1 || len(fresh.calls) != 1 || p.Broken() {
				t.Fatal("reopen", *opened, old.closed, fresh.calls, p.Broken())
			}
			// The retry was the only one: a later failure is the call's own.
			fresh.failures, fresh.err = 1, unsent
			if _, e := p.GetTeam(context.Background(), tid); e == nil || *opened != 1 {
				t.Fatal("a later call reopened", e, *opened)
			}
		})
	}
}

// A second failure is the call's: no loop, and the first error, which says
// nothing was sent, is returned.
func TestReusedSecondFailureFailsTheCall(t *testing.T) {
	old, fresh := teamFake(1, unsent), teamFake(5, unsent)
	p := reusedProvider(t, old)
	reopen, opened := reopener(t, 1, fresh)
	p.Reused(reopen)
	_, e := p.GetTeam(context.Background(), tid)
	var pe *Error
	if !errors.As(e, &pe) || !pe.NotSent || *opened != 1 || len(fresh.calls) != 1 || !p.Broken() {
		t.Fatal("second failure", e, *opened, fresh.calls, p.Broken())
	}
	// A failed reopen sends nothing and fails the call too.
	q := reusedProvider(t, teamFake(1, unsent))
	q.Reused(func(context.Context) (*Provider, error) { return nil, fail("unavailable") })
	if _, e := q.GetTeam(context.Background(), tid); !errors.As(e, &pe) || !pe.NotSent {
		t.Fatal("a failed reopen", e)
	}
}

// Only a stale session is reopened: a rate limit, a 403, a tool's own
// error or a call that may have been sent is the call's.
func TestReusedReopensOnlyAStaleSession(t *testing.T) {
	for name, err := range map[string]error{
		"429":        &Error{Kind: "rate_limited", Status: 429, NotSent: true},
		"403":        &Error{Kind: "unauthorized", Status: 403, NotSent: true},
		"maybe sent": errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			p := reusedProvider(t, teamFake(1, err))
			reopen, opened := reopener(t, 0)
			p.Reused(reopen)
			if _, e := p.GetTeam(context.Background(), tid); e == nil || *opened != 0 || !p.Broken() {
				t.Fatal("reopened", e, *opened)
			}
		})
	}
	// A first call that succeeds uses up the reopen.
	f := teamFake(0, nil)
	p := reusedProvider(t, f)
	reopen, opened := reopener(t, 0)
	p.Reused(reopen)
	if _, e := p.GetTeam(context.Background(), tid); e != nil {
		t.Fatal(e)
	}
	f.failures, f.err = 1, unsent
	if _, e := p.GetTeam(context.Background(), tid); e == nil || *opened != 0 {
		t.Fatal("a second call reopened", e, *opened)
	}
}

// A reopened session whose tool changed shape is not sent the arguments
// negotiated for the old one.
func TestReusedRefusesAChangedTool(t *testing.T) {
	old := teamFake(1, unsent)
	changed := teamFake(0, nil)
	changed.tools = []*mcp.Tool{tool("get_team", map[string]string{"id": "string"}, "id")}
	p := reusedProvider(t, old)
	reopen, _ := reopener(t, 1, changed)
	p.Reused(reopen)
	if _, e := p.GetTeam(context.Background(), tid); e == nil || len(changed.calls) != 0 || changed.closed != 1 {
		t.Fatal("the call went to a changed tool", e, changed.calls)
	}
}
