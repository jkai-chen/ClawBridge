package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func planTool(t *testing.T, edit func(props map[string]any, root map[string]any)) *mcp.Tool {
	tl := fixtureTool(t, "save-project-20261022.json")
	root := schemaProperty(tl.InputSchema)
	edit(root["properties"].(map[string]any), root)
	tl.InputSchema = root
	return tl
}

func planProvider(t *testing.T, tl *mcp.Tool) (*Provider, *scripted) {
	return statusProvider(t, []*mcp.Tool{tl}, map[string][]any{"save_project": {map[string]any{"id": suProject}, map[string]any{"id": suProject}, map[string]any{"id": suProject}}})
}

func TestProjectPlanReasonAgainstTheCatalog(t *testing.T) {
	p, _ := planProvider(t, fixtureTool(t, "save-project-20261022.json"))
	for _, f := range ProjectPlanFields {
		if r := p.ProjectPlanReason([]string{f}); r != "" {
			t.Fatal(f, r)
		}
	}
	if r := p.ProjectPlanReason(ProjectPlanFields); r != "" {
		t.Fatal(r)
	}
	if r := p.ProjectPlanReason([]string{"health"}); r == "" {
		t.Fatal("unknown field accepted")
	}
	if !reflect.DeepEqual(ProjectPlanFields, []string{"priority", "targetDate", "startDate"}) {
		t.Fatal(ProjectPlanFields)
	}
	none, _ := statusProvider(t, nil, nil)
	if none.ProjectPlanReason([]string{"priority"}) == "" {
		t.Fatal("no tool accepted")
	}
}

func TestSaveProjectPlanSendsOnlyTheGivenFields(t *testing.T) {
	p, f := planProvider(t, fixtureTool(t, "save-project-20261022.json"))
	high := 2
	zero := 0
	for _, plan := range []ProjectPlan{
		{Priority: &high},
		{TargetDate: Field[*string]{Present: true, Value: ptr("2026-12-31")}},
		{StartDate: Field[*string]{Present: true}},
	} {
		if e := p.SaveProjectPlan(context.Background(), suProject, plan); e != nil {
			t.Fatal(e)
		}
	}
	want := []map[string]any{
		{"id": suProject, "priority": 2},
		{"id": suProject, "targetDate": "2026-12-31"},
		{"id": suProject, "startDate": nil},
	}
	if !reflect.DeepEqual(f.args, want) {
		t.Fatal(f.args)
	}
	// Priority 0 (none) is a value, not an omission.
	p, f = planProvider(t, fixtureTool(t, "save-project-20261022.json"))
	if e := p.SaveProjectPlan(context.Background(), suProject, ProjectPlan{Priority: &zero}); e != nil || !reflect.DeepEqual(f.args, []map[string]any{{"id": suProject, "priority": 0}}) {
		t.Fatal(e, f.args)
	}
	// All three together.
	if e := p.SaveProjectPlan(context.Background(), suProject, ProjectPlan{Priority: &high, TargetDate: Field[*string]{Present: true, Value: ptr("2026-12-31")}, StartDate: Field[*string]{Present: true, Value: ptr("2026-11-01")}}); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.args[1], map[string]any{"id": suProject, "priority": 2, "targetDate": "2026-12-31", "startDate": "2026-11-01"}) {
		t.Fatal(f.args[1])
	}
}

func TestSaveProjectPlanRefusals(t *testing.T) {
	five := 5
	one := 1
	date := func(s string) Field[*string] { return Field[*string]{Present: true, Value: ptr(s)} }
	good := fixtureTool(t, "save-project-20261022.json")
	cases := map[string]struct {
		tool *mcp.Tool
		id   string
		plan ProjectPlan
		kind string
	}{
		"no field":          {good, suProject, ProjectPlan{}, "invalid"},
		"priority 5":        {good, suProject, ProjectPlan{Priority: &five}, "invalid"},
		"bad month":         {good, suProject, ProjectPlan{TargetDate: date("2026-13-01")}, "invalid"},
		"too early":         {good, suProject, ProjectPlan{StartDate: date("1999-12-31")}, "invalid"},
		"too late":          {good, suProject, ProjectPlan{StartDate: date("2101-01-01")}, "invalid"},
		"datetime":          {good, suProject, ProjectPlan{TargetDate: date("2026-12-01T00:00:00Z")}, "invalid"},
		"project by name":   {good, "P-ENG-1", ProjectPlan{Priority: &one}, "invalid"},
		"no resolution":     {planTool(t, func(m, _ map[string]any) { delete(m, "targetDateResolution") }), suProject, ProjectPlan{TargetDate: date("2026-12-01")}, "capability_unavailable"},
		"no start res":      {planTool(t, func(m, _ map[string]any) { delete(m, "startDateResolution") }), suProject, ProjectPlan{StartDate: date("2026-12-01")}, "capability_unavailable"},
		"date not nullable": {planTool(t, func(m, _ map[string]any) { m["targetDate"] = map[string]any{"type": "string"} }), suProject, ProjectPlan{TargetDate: date("2026-12-01")}, "capability_unavailable"},
		"no minimum":        {planTool(t, func(m, _ map[string]any) { delete(m["priority"].(map[string]any), "minimum") }), suProject, ProjectPlan{Priority: &one}, "capability_unavailable"},
		"no maximum":        {planTool(t, func(m, _ map[string]any) { delete(m["priority"].(map[string]any), "maximum") }), suProject, ProjectPlan{Priority: &one}, "capability_unavailable"},
		"priority string":   {planTool(t, func(m, _ map[string]any) { m["priority"] = map[string]any{"type": "string"} }), suProject, ProjectPlan{Priority: &one}, "capability_unavailable"},
		"no priority":       {planTool(t, func(m, _ map[string]any) { delete(m, "priority") }), suProject, ProjectPlan{Priority: &one}, "capability_unavailable"},
		"no id":             {planTool(t, func(m, _ map[string]any) { delete(m, "id") }), suProject, ProjectPlan{Priority: &one}, "capability_unavailable"},
		"extra required":    {planTool(t, func(_, r map[string]any) { r["required"] = []any{"id", "name"} }), suProject, ProjectPlan{Priority: &one}, "capability_unavailable"},
	}
	for name, c := range cases {
		p, f := planProvider(t, c.tool)
		var refused *Error
		if e := p.SaveProjectPlan(context.Background(), c.id, c.plan); !errors.As(e, &refused) || refused.Kind != c.kind || len(f.args) != 0 || len(f.calls) != 0 {
			t.Fatal(name, e, f.args)
		}
	}
	// A schema lacking only the other date's resolution still lets a
	// priority and the date it has through.
	p, f := planProvider(t, planTool(t, func(m, _ map[string]any) { delete(m, "startDateResolution") }))
	if e := p.SaveProjectPlan(context.Background(), suProject, ProjectPlan{TargetDate: date("2026-12-01")}); e != nil || len(f.args) != 1 {
		t.Fatal(e, f.args)
	}
}

func TestProjectSnapshotPlanAccessors(t *testing.T) {
	get := tool("get_project", map[string]string{"query": "string"})
	read := func(extra map[string]any) ProjectSnapshot {
		p, _ := statusProvider(t, []*mcp.Tool{get}, map[string][]any{"get_project": {snapshotProject([]string{ptTeamA}, nil, nil, extra)}})
		s, e := p.ProjectSnapshot(context.Background(), suProject)
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	s := read(map[string]any{"priority": map[string]any{"value": 2, "name": "High"}, "targetDate": "2026-12-31", "startDate": nil, "targetDateResolution": "quarter", "startDateResolution": nil})
	if v, ok := s.Priority(); !ok || v != 2 {
		t.Fatal(v, ok)
	}
	if v, ok := s.TargetDate(); !ok || v != "2026-12-31" {
		t.Fatal(v, ok)
	}
	if v, ok := s.StartDate(); !ok || v != "" {
		t.Fatal("null start date", v, ok)
	}
	for field, want := range map[string]string{"targetDate": "quarter", "targetDateResolution": "quarter", "startDate": "", "startDateResolution": ""} {
		if v, ok := s.Resolution(field); !ok || v != want {
			t.Fatal(field, v, ok)
		}
	}
	if _, ok := s.Resolution("priority"); ok {
		t.Fatal("resolution of a non-date")
	}
	// Bare priority; empty date; datetime date.
	s = read(map[string]any{"priority": 4, "targetDate": "", "startDate": "2026-11-01T00:00:00Z"})
	if v, ok := s.Priority(); !ok || v != 4 {
		t.Fatal(v, ok)
	}
	if v, ok := s.TargetDate(); !ok || v != "" {
		t.Fatal(v, ok)
	}
	if v, ok := s.StartDate(); !ok || v != "2026-11-01" {
		t.Fatal(v, ok)
	}
	// Absent and odd are unknown (snapshotProject carries a startDate; drop it).
	s = read(map[string]any{"priority": "high", "targetDate": 7, "startDate": "soon", "targetDateResolution": 3})
	if _, ok := s.Priority(); ok {
		t.Fatal("odd priority")
	}
	for _, f := range []func() (string, bool){s.TargetDate, s.StartDate} {
		if _, ok := f(); ok {
			t.Fatal("odd date")
		}
	}
	if _, ok := s.Resolution("targetDate"); ok {
		t.Fatal("odd resolution")
	}
	if _, ok := s.Resolution("startDate"); ok {
		t.Fatal("absent resolution")
	}
	// The resolution fields are compared by the team-add changes.
	b := read(map[string]any{"targetDate": "2026-12-31", "targetDateResolution": "day"})
	a := read(map[string]any{"targetDate": "2026-12-31", "targetDateResolution": "quarter"})
	if got := TeamAddChanges(b, a, ptTeamB); !reflect.DeepEqual(got, []string{"targetDateResolution"}) {
		t.Fatal(got)
	}
}
