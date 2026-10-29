package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	moveTeam  = "55555555-5555-4555-8555-555555555555"
	moveState = "66666666-6666-4666-8666-666666666666"
	moveLabel = "77777777-7777-4777-8777-777777777777"
)

// A move sends one official save_issue: the issue, the new team, its state,
// the full label set (sorted, an empty set as []), and null for the cycle
// and estimate it clears; nothing else.
func TestV2MoveSendsOneSaveIssue(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: map[string]any{"id": "NEW-1", "uuid": iid, "teamId": moveTeam, "projectId": nil}}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	in := MoveInputV2{ID: iid, TeamID: moveTeam, StateID: moveState, Labels: []string{moveLabel, uid}, ClearCycle: true, ClearEstimate: true}
	if err = p.ValidateMoveV2(in); err != nil || len(f.calls) != 0 {
		t.Fatal("validate", err, f.calls)
	}
	x, err := p.MoveV2(context.Background(), in)
	if err != nil || x.ID != iid || x.Identifier != "NEW-1" || x.TeamID != moveTeam {
		t.Fatal(x, err)
	}
	want := map[string]any{"id": iid, "team": moveTeam, "state": moveState, "labels": []string{moveLabel, uid}, "cycle": nil, "estimate": nil}
	if !reflect.DeepEqual(f.calls, []string{"save_issue"}) || !reflect.DeepEqual(f.args, []map[string]any{want}) {
		t.Fatal(f.calls, f.args)
	}
	f.calls, f.args = nil, nil
	if _, err = p.MoveV2(context.Background(), MoveInputV2{ID: iid, TeamID: moveTeam, StateID: moveState}); err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"id": iid, "team": moveTeam, "state": moveState, "labels": []string{}}); !reflect.DeepEqual(f.args, []map[string]any{want}) {
		t.Fatal("an empty label set is sent as [] and nothing is cleared unasked", f.args)
	}
}

// Bad IDs and an answer naming another issue are refused; a tool without a
// team field can't express a move.
func TestV2MoveRefuses(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: map[string]any{"id": "NEW-1", "uuid": uid, "teamId": moveTeam}}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []MoveInputV2{{ID: "ISS-1", TeamID: moveTeam, StateID: moveState}, {ID: iid, TeamID: "team", StateID: moveState}, {ID: iid, TeamID: moveTeam, StateID: "Todo"}, {ID: iid, TeamID: moveTeam, StateID: moveState, Labels: []string{"Bug"}}} {
		if _, err = p.MoveV2(context.Background(), in); err == nil {
			t.Fatal("invalid move sent", in)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal(f.calls)
	}
	if _, err = p.MoveV2(context.Background(), MoveInputV2{ID: iid, TeamID: moveTeam, StateID: moveState}); err == nil {
		t.Fatal("an answer for another issue was accepted")
	}
	tool := officialSaveIssue(t)
	delete(tool.InputSchema.(map[string]any)["properties"].(map[string]any), "team")
	g := &fake{tools: []*mcp.Tool{tool}}
	q, err := NewWithCaller(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.ValidateMoveV2(MoveInputV2{ID: iid, TeamID: moveTeam, StateID: moveState}); err == nil {
		t.Fatal("a tool without team expressed a move")
	}
}

// The cycle, estimate and sub-issues are known only from a shape that
// can't be misread.
func TestV2TeamFieldsParse(t *testing.T) {
	for name, c := range map[string]struct {
		m                       map[string]any
		cycleKnown, cycleSet    bool
		estimateKnown, estimate bool
		childrenKnown           bool
		children                int
	}{
		"absent":         {m: map[string]any{}},
		"null cycle":     {m: map[string]any{"cycle": nil}, cycleKnown: true},
		"cycle id":       {m: map[string]any{"cycleId": uid}, cycleKnown: true, cycleSet: true},
		"cycle object":   {m: map[string]any{"cycle": map[string]any{"id": uid, "number": 3.0}}, cycleKnown: true, cycleSet: true},
		"empty cycle":    {m: map[string]any{"cycle": map[string]any{}}, cycleKnown: true},
		"odd cycle":      {m: map[string]any{"cycle": 3.0}},
		"estimate":       {m: map[string]any{"estimate": 2.0}, estimateKnown: true, estimate: true},
		"null estimate":  {m: map[string]any{"estimate": nil}, estimateKnown: true},
		"odd estimate":   {m: map[string]any{"estimate": "2"}},
		"children":       {m: map[string]any{"children": []any{map[string]any{"id": uid}}}, childrenKnown: true, children: 1},
		"sub-issue conn": {m: map[string]any{"subIssues": map[string]any{"nodes": []any{}}}, childrenKnown: true},
		"null children":  {m: map[string]any{"children": nil}, childrenKnown: true},
		"odd children":   {m: map[string]any{"children": "none"}},
	} {
		var x IssueV2
		parseTeamFieldsV2(c.m, &x)
		if x.CycleKnown != c.cycleKnown || x.CycleSet != c.cycleSet || x.EstimateKnown != c.estimateKnown || (x.Estimate != nil) != c.estimate || x.ChildrenKnown != c.childrenKnown || x.Children != c.children {
			t.Fatal(name, x)
		}
	}
}

// Sub-issues are listed by parentId; any row counts, and a list tool
// without parentId can't prove none.
func TestV2SubIssues(t *testing.T) {
	list := &mcp.Tool{Name: "list_issues", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"parentId": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}, "cursor": map[string]any{"type": "string"}}}}
	f := &fake{tools: []*mcp.Tool{list}, result: map[string]any{"issues": []any{map[string]any{"id": "X-1"}}}}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := p.SubIssues(context.Background(), iid); err != nil || n != 1 || f.args[0]["parentId"] != iid {
		t.Fatal(n, err, f.args)
	}
	f.result = map[string]any{"issues": []any{}}
	if n, err := p.SubIssues(context.Background(), iid); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err := p.SubIssues(context.Background(), "X-1"); err == nil {
		t.Fatal("an identifier was sent as the parent")
	}
	delete(list.InputSchema.(map[string]any)["properties"].(map[string]any), "parentId")
	g := &fake{tools: []*mcp.Tool{list}, result: map[string]any{"issues": []any{}}}
	q, _ := NewWithCaller(context.Background(), g)
	if _, err := q.SubIssues(context.Background(), iid); err == nil {
		t.Fatal("a list without parentId proved none")
	}
}

// pagedFake answers each call with the next of its pages.
type pagedFake struct {
	fake
	pages []any
}

func (f *pagedFake) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.result = f.pages[0]
	if len(f.pages) > 1 {
		f.pages = f.pages[1:]
	}
	return f.fake.CallTool(ctx, p)
}

// Children pages list_issues {parentId} to its end; past max, or a page
// that says more without a cursor, is an error, never a short list.
func TestV2ChildrenPagesToTheEnd(t *testing.T) {
	list := &mcp.Tool{Name: "list_issues", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"parentId": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}, "cursor": map[string]any{"type": "string"}}}}
	row := func(id string) map[string]any {
		return map[string]any{"id": id, "teamId": tid, "projectId": nil, "parentId": iid}
	}
	const c1, c2 = "c1000000-0000-4000-8000-000000000001", "c1000000-0000-4000-8000-000000000002"
	f := &pagedFake{fake: fake{tools: []*mcp.Tool{list}}, pages: []any{
		map[string]any{"issues": []any{row(c1)}, "hasNextPage": true, "nextCursor": "n1"},
		map[string]any{"issues": []any{row(c2)}, "hasNextPage": false},
	}}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Children(context.Background(), iid, 50)
	if err != nil || len(got) != 2 || got[0].ID != c1 || got[1].ID != c2 || f.args[1]["cursor"] != "n1" || f.args[0]["parentId"] != iid {
		t.Fatal(got, err, f.args)
	}
	f.pages = []any{map[string]any{"issues": []any{row(c1), row(c2)}, "hasNextPage": false}}
	if _, err = p.Children(context.Background(), iid, 1); err == nil {
		t.Fatal("more than max children listed")
	}
	f.pages = []any{map[string]any{"issues": []any{row(c1)}}}
	if _, err = p.Children(context.Background(), iid, 50); err == nil {
		t.Fatal("a listing that can't prove its end passed")
	}
}
