package linearprovider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	msA = "f5000000-0000-4000-8000-00000000000a"
	msB = "f5000000-0000-4000-8000-00000000000b"
	msC = "f5000000-0000-4000-8000-00000000000c"
)

func msTools(t *testing.T) []*mcp.Tool {
	return []*mcp.Tool{officialSaveIssue(t), fixtureTool(t, "list-milestones-20261102.json")}
}

func f64(v float64) *float64 { return &v }

// The recorded catalogs give both capabilities; without list_milestones, or
// with a project that isn't a string, neither is on.
func TestMilestoneCapabilities(t *testing.T) {
	caps := func(tools ...*mcp.Tool) Capabilities {
		p, e := NewWithCaller(context.Background(), &fake{tools: tools, result: issueResult()})
		if e != nil {
			t.Fatal(e)
		}
		c, e := p.Capabilities(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	if c := caps(msTools(t)...); !c.Milestone || !c.MilestoneRead {
		t.Fatal("recorded catalogs", c)
	}
	if c := caps(officialSaveIssue(t)); c.Milestone || c.MilestoneRead {
		t.Fatal("no list_milestones", c)
	}
	if c := caps(fixtureTool(t, "list-milestones-20261102.json")); c.Milestone || !c.MilestoneRead {
		t.Fatal("no save_issue", c)
	}
	if c := caps(officialSaveIssue(t), tool("list_milestones", map[string]string{"project": "integer"})); c.Milestone || c.MilestoneRead {
		t.Fatal("project not a string", c)
	}
	mod := func(f func(props map[string]any)) *mcp.Tool {
		tl := officialSaveIssue(t)
		root := schemaProperty(tl.InputSchema)
		f(root["properties"].(map[string]any))
		tl.InputSchema = root
		return tl
	}
	list := fixtureTool(t, "list-milestones-20261102.json")
	if c := caps(mod(func(m map[string]any) { delete(m, "milestone") }), list); c.Milestone || !c.MilestoneRead {
		t.Fatal("milestone missing", c)
	}
	if c := caps(mod(func(m map[string]any) { m["milestone"] = map[string]any{"type": "integer"} }), list); c.Milestone {
		t.Fatal("milestone mistyped", c)
	}
	// A nullable milestone is still a settable one.
	if c := caps(mod(func(m map[string]any) { m["milestone"] = map[string]any{"type": []any{"string", "null"}} }), list); !c.Milestone {
		t.Fatal("nullable milestone", c)
	}
}

func msProvider(t *testing.T, tools []*mcp.Tool, list any) (*Provider, *fake) {
	f := &fake{tools: tools, result: issueResult(), results: map[string]any{"list_milestones": list}}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	return p, f
}

// The list parses the recorded shape, orders by sort order and then name,
// and sends the project ID only.
func TestListMilestones(t *testing.T) {
	list := map[string]any{"milestones": []any{
		map[string]any{"id": msB, "name": "Beta", "progress": 0.25, "sortOrder": 2.0, "targetDate": "2026-12-01"},
		map[string]any{"id": msC, "name": "alpha", "progress": 0.0, "sortOrder": 2.0, "targetDate": nil},
		map[string]any{"id": strings.ToUpper(msA), "name": "Zed", "progress": 1.0, "sortOrder": 1.0, "targetDate": "2026-11-20"},
	}}
	p, f := msProvider(t, msTools(t), list)
	got, e := p.ListMilestones(context.Background(), tid)
	if e != nil {
		t.Fatal(e)
	}
	want := []Milestone{
		{ID: msA, Name: "Zed", TargetDate: ptr("2026-11-20"), TargetDateKnown: true, Progress: f64(1), ProgressKnown: true, SortOrder: f64(1)},
		{ID: msC, Name: "alpha", TargetDateKnown: true, Progress: f64(0), ProgressKnown: true, SortOrder: f64(2)},
		{ID: msB, Name: "Beta", TargetDate: ptr("2026-12-01"), TargetDateKnown: true, Progress: f64(0.25), ProgressKnown: true, SortOrder: f64(2)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if len(f.calls) != 1 || f.calls[0] != "list_milestones" || !reflect.DeepEqual(f.args[0], map[string]any{"project": tid}) {
		t.Fatal(f.calls, f.args)
	}
}

// A progress or date of another shape is unknown, never guessed; a row with
// a non-UUID ID or an empty name refuses the whole list.
func TestListMilestonesUnknownShapes(t *testing.T) {
	list := map[string]any{"milestones": []any{
		map[string]any{"id": msA, "name": "Odd", "progress": "half", "targetDate": 20261120},
		map[string]any{"id": msB, "name": "Over", "progress": 1.5, "targetDate": "soon"},
		map[string]any{"id": msC, "name": "Absent"},
	}}
	p, _ := msProvider(t, msTools(t), list)
	got, e := p.ListMilestones(context.Background(), tid)
	if e != nil || len(got) != 3 {
		t.Fatal(got, e)
	}
	for _, m := range got {
		if m.ProgressKnown || m.Progress != nil || m.TargetDateKnown || m.TargetDate != nil || m.SortOrder != nil {
			t.Fatal("guessed", m)
		}
	}
	for name, row := range map[string]map[string]any{
		"non-uuid id": {"id": "not-a-uuid", "name": "x"},
		"empty name":  {"id": msA, "name": " "},
		"no id":       {"name": "x"},
	} {
		p, _ := msProvider(t, msTools(t), map[string]any{"milestones": []any{row}})
		if _, e := p.ListMilestones(context.Background(), tid); e == nil {
			t.Fatal(name, "was accepted")
		}
	}
	dup := map[string]any{"milestones": []any{map[string]any{"id": msA, "name": "x"}, map[string]any{"id": msA, "name": "y"}}}
	p, _ = msProvider(t, msTools(t), dup)
	if _, e := p.ListMilestones(context.Background(), tid); e == nil {
		t.Fatal("duplicate id accepted")
	}
	p, _ = msProvider(t, msTools(t), map[string]any{"milestones": "nope"})
	if _, e := p.ListMilestones(context.Background(), tid); e == nil {
		t.Fatal("bad envelope accepted")
	}
	if _, e := p.ListMilestones(context.Background(), "name"); e == nil {
		t.Fatal("a project name was sent")
	}
}

// Without list_milestones nothing is called.
func TestListMilestonesNeedsTheTool(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: issueResult()}
	p, _ := NewWithCaller(context.Background(), f)
	if _, e := p.ListMilestones(context.Background(), tid); e == nil || len(f.calls) != 0 {
		t.Fatal(e, f.calls)
	}
}

// An issue's projectMilestone is its milestone ID, null for none, and
// unknown for an absent key or any other shape.
func TestIssueMilestoneParse(t *testing.T) {
	type want struct {
		id    *string
		known bool
	}
	cases := map[string]struct {
		row  map[string]any
		want want
	}{
		"object":     {map[string]any{"projectMilestone": map[string]any{"id": msA, "name": "M"}}, want{ptr(msA), true}},
		"null":       {map[string]any{"projectMilestone": nil}, want{nil, true}},
		"absent":     {map[string]any{}, want{nil, false}},
		"string":     {map[string]any{"projectMilestone": msA}, want{nil, false}},
		"non-uuid":   {map[string]any{"projectMilestone": map[string]any{"id": "M-1"}}, want{nil, false}},
		"no id":      {map[string]any{"projectMilestone": map[string]any{"name": "M"}}, want{nil, false}},
		"number id":  {map[string]any{"projectMilestone": map[string]any{"id": 7}}, want{nil, false}},
		"list":       {map[string]any{"projectMilestone": []any{}}, want{nil, false}},
		"empty text": {map[string]any{"projectMilestone": ""}, want{nil, false}},
	}
	for name, c := range cases {
		row := nativeGetIssue([]any{}, emptyNativeRelations())
		for k, v := range c.row {
			row[k] = v
		}
		p, _ := NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"})}, result: row})
		x, e := p.IssueV2(context.Background(), iid)
		if e != nil {
			t.Fatal(name, e)
		}
		if x.MilestoneKnown != c.want.known || !reflect.DeepEqual(x.MilestoneID, c.want.id) {
			t.Fatal(name, x.MilestoneID, x.MilestoneKnown)
		}
	}
}

// Create and update send the milestone ID only. A clear is refused before
// any call, even on a catalog where the milestone is nullable; a set
// without the capability is refused too; a name is never sent.
func TestMilestoneWrites(t *testing.T) {
	p, f := msProvider(t, msTools(t), map[string]any{"milestones": []any{}})
	up := func(m Field[*string]) UpdateInputV2 {
		return UpdateInputV2{UpdateInput: UpdateInput{ID: iid, Title: ptr("t")}, Milestone: m}
	}
	if _, e := p.UpdateV2(context.Background(), up(Field[*string]{Present: true, Value: ptr(msA)})); e != nil {
		t.Fatal(e)
	}
	if _, e := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Milestone: Field[*string]{Present: true, Value: ptr(msB)}}); e != nil {
		t.Fatal(e)
	}
	if _, e := p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}, Milestone: Field[*string]{Present: true, Value: ptr(msC)}}); e != nil {
		t.Fatal(e)
	}
	if _, e := p.UpdateV2(context.Background(), up(Field[*string]{})); e != nil {
		t.Fatal(e)
	}
	if f.args[0]["milestone"] != msA || f.args[1]["milestone"] != msB || f.args[2]["milestone"] != msC {
		t.Fatal(f.args)
	}
	if _, ok := f.args[3]["milestone"]; ok {
		t.Fatal("sent when not Present", f.args[3])
	}
	n := len(f.calls)
	for _, bad := range []string{"Sprint 1", "", "M-1"} {
		if _, e := p.UpdateV2(context.Background(), up(Field[*string]{Present: true, Value: ptr(bad)})); e == nil || len(f.calls) != n {
			t.Fatal(bad, e)
		}
	}
	// A clear, on the recorded schema and on a nullable one.
	nullable := officialSaveIssue(t)
	root := schemaProperty(nullable.InputSchema)
	root["properties"].(map[string]any)["milestone"] = map[string]any{"type": []any{"string", "null"}}
	nullable.InputSchema = root
	for name, tools := range map[string][]*mcp.Tool{"recorded": msTools(t), "nullable": {nullable, fixtureTool(t, "list-milestones-20261102.json")}} {
		p, f := msProvider(t, tools, map[string]any{"milestones": []any{}})
		_, e := p.UpdateV2(context.Background(), up(Field[*string]{Present: true}))
		if e == nil || e.Error() != "clearing a milestone isn't available yet; nothing was sent" || len(f.calls) != 0 {
			t.Fatal(name, e, f.calls)
		}
		_, e = p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}, Milestone: Field[*string]{Present: true}})
		if e == nil || e.Error() != MilestoneClearRefused || len(f.calls) != 0 {
			t.Fatal(name, e, f.calls)
		}
	}
	// No list_milestones: a set is refused, nothing sent.
	f2 := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: issueResult()}
	p2, _ := NewWithCaller(context.Background(), f2)
	_, e := p2.UpdateV2(context.Background(), up(Field[*string]{Present: true, Value: ptr(msA)}))
	if e == nil || e.Error() != "this Linear connection can't set a milestone; nothing was sent" || len(f2.calls) != 0 {
		t.Fatal(e, f2.calls)
	}
}
