package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mpProject   = "f6000000-0000-4000-8000-000000000001"
	mpMilestone = "f6000000-0000-4000-8000-000000000002"
)

func msSaveTool(t *testing.T, edit func(props map[string]any, root map[string]any)) *mcp.Tool {
	tl := fixtureTool(t, "save-milestone-20261102.json")
	root := schemaProperty(tl.InputSchema)
	edit(root["properties"].(map[string]any), root)
	tl.InputSchema = root
	return tl
}

func msSaveProvider(t *testing.T, tl *mcp.Tool) (*Provider, *scripted) {
	return statusProvider(t, []*mcp.Tool{tl}, map[string][]any{"save_milestone": {map[string]any{"id": mpMilestone}, map[string]any{"milestone": map[string]any{"id": strings.ToUpper(mpMilestone)}}, map[string]any{"ok": true}, map[string]any{"id": "Roadmap"}, map[string]any{"id": mpMilestone}, map[string]any{"id": mpMilestone}, map[string]any{"id": mpMilestone}, map[string]any{"id": mpMilestone}}})
}

func msDate(s string) Field[*string] { return Field[*string]{Present: true, Value: ptr(s)} }

// The recorded save_milestone gives both capabilities; an unknown field,
// or no tool, gives none.
func TestMilestonePlanReasonAgainstTheCatalog(t *testing.T) {
	p, _ := msSaveProvider(t, fixtureTool(t, "save-milestone-20261102.json"))
	for _, update := range []bool{false, true} {
		for _, fields := range [][]string{{"name"}, {"targetDate"}, MilestonePlanFields} {
			if r := p.MilestonePlanReason(update, fields); r != "" {
				t.Fatal(update, fields, r)
			}
		}
	}
	if r := p.MilestonePlanReason(false, []string{"description"}); r == "" {
		t.Fatal("description accepted")
	}
	if !reflect.DeepEqual(MilestonePlanFields, []string{"name", "targetDate"}) {
		t.Fatal(MilestonePlanFields)
	}
	none, _ := statusProvider(t, nil, nil)
	if none.MilestonePlanReason(false, []string{"name"}) == "" {
		t.Fatal("no tool accepted")
	}
}

// Only the given fields are sent; the milestone goes as its UUID in id; the
// project always; a description never. The ID the answer names is returned
// (top level or under milestone, lower-cased), and none when it names no
// UUID.
func TestSaveMilestoneSendsOnlyTheGivenFields(t *testing.T) {
	p, f := msSaveProvider(t, fixtureTool(t, "save-milestone-20261102.json"))
	ctx := context.Background()
	name := "Beta"
	type step struct {
		plan MilestonePlan
		id   string
	}
	for _, c := range []step{
		{MilestonePlan{Project: mpProject, Name: &name}, mpMilestone},
		{MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("2026-12-31")}, mpMilestone},
		{MilestonePlan{Project: mpProject, ID: mpMilestone, TargetDate: msDate("2027-01-15")}, ""},
		{MilestonePlan{Project: mpProject, ID: mpMilestone, TargetDate: Field[*string]{Present: true}}, ""},
		{MilestonePlan{Project: mpProject, ID: mpMilestone, Name: &name}, mpMilestone},
	} {
		got, e := p.SaveMilestone(ctx, c.plan)
		if e != nil {
			t.Fatal(c.plan, e)
		}
		_ = got
	}
	want := []map[string]any{
		{"project": mpProject, "name": "Beta"},
		{"project": mpProject, "name": "Beta", "targetDate": "2026-12-31"},
		{"project": mpProject, "id": mpMilestone, "targetDate": "2027-01-15"},
		{"project": mpProject, "id": mpMilestone, "targetDate": nil},
		{"project": mpProject, "id": mpMilestone, "name": "Beta"},
	}
	if !reflect.DeepEqual(f.args, want) {
		t.Fatal(f.args)
	}
	for _, a := range f.args {
		if _, has := a["description"]; has {
			t.Fatal("a description was sent", a)
		}
	}
	// The answer's ID, from either place, lower-cased; an answer naming no
	// UUID names none.
	p, _ = msSaveProvider(t, fixtureTool(t, "save-milestone-20261102.json"))
	for i, wantID := range []string{mpMilestone, mpMilestone, "", ""} {
		if got, e := p.SaveMilestone(ctx, MilestonePlan{Project: mpProject, Name: &name}); e != nil || got != wantID {
			t.Fatal(i, got, e)
		}
	}
}

func TestSaveMilestoneRefusals(t *testing.T) {
	good := fixtureTool(t, "save-milestone-20261102.json")
	name := "Beta"
	long := strings.Repeat("x", 81)
	bidi := "Be\u202eta"
	cases := map[string]struct {
		tool *mcp.Tool
		plan MilestonePlan
		kind string
	}{
		"no field":          {good, MilestonePlan{Project: mpProject, ID: mpMilestone}, "invalid"},
		"create no name":    {good, MilestonePlan{Project: mpProject, TargetDate: msDate("2026-12-31")}, "invalid"},
		"project by name":   {good, MilestonePlan{Project: "P-ENG-1", Name: &name}, "invalid"},
		"milestone by name": {good, MilestonePlan{Project: mpProject, ID: "Beta", Name: &name}, "invalid"},
		"untrimmed name":    {good, MilestonePlan{Project: mpProject, Name: ptr(" Beta ")}, "invalid"},
		"long name":         {good, MilestonePlan{Project: mpProject, Name: &long}, "invalid"},
		"bidi name":         {good, MilestonePlan{Project: mpProject, Name: &bidi}, "invalid"},
		"bad month":         {good, MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("2026-13-01")}, "invalid"},
		"too early":         {good, MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("1999-12-31")}, "invalid"},
		"too late":          {good, MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("2101-01-01")}, "invalid"},
		"datetime":          {good, MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("2026-12-01T00:00:00Z")}, "invalid"},
		"no project":        {msSaveTool(t, func(m, _ map[string]any) { delete(m, "project") }), MilestonePlan{Project: mpProject, Name: &name}, "capability_unavailable"},
		"project integer":   {msSaveTool(t, func(m, _ map[string]any) { m["project"] = map[string]any{"type": "integer"} }), MilestonePlan{Project: mpProject, Name: &name}, "capability_unavailable"},
		"no id":             {msSaveTool(t, func(m, _ map[string]any) { delete(m, "id") }), MilestonePlan{Project: mpProject, ID: mpMilestone, Name: &name}, "capability_unavailable"},
		"id integer":        {msSaveTool(t, func(m, _ map[string]any) { m["id"] = map[string]any{"type": "integer"} }), MilestonePlan{Project: mpProject, ID: mpMilestone, Name: &name}, "capability_unavailable"},
		"no name":           {msSaveTool(t, func(m, _ map[string]any) { delete(m, "name") }), MilestonePlan{Project: mpProject, Name: &name}, "capability_unavailable"},
		"name integer":      {msSaveTool(t, func(m, _ map[string]any) { m["name"] = map[string]any{"type": "integer"} }), MilestonePlan{Project: mpProject, Name: &name}, "capability_unavailable"},
		"date not nullable": {msSaveTool(t, func(m, _ map[string]any) { m["targetDate"] = map[string]any{"type": "string"} }), MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("2026-12-31")}, "capability_unavailable"},
		"date integer":      {msSaveTool(t, func(m, _ map[string]any) { m["targetDate"] = map[string]any{"type": []any{"integer", "null"}} }), MilestonePlan{Project: mpProject, Name: &name, TargetDate: msDate("2026-12-31")}, "capability_unavailable"},
		"extra required":    {msSaveTool(t, func(_, r map[string]any) { r["required"] = []any{"project", "name"} }), MilestonePlan{Project: mpProject, Name: &name}, "capability_unavailable"},
	}
	for label, c := range cases {
		p, f := msSaveProvider(t, c.tool)
		var refused *Error
		if _, e := p.SaveMilestone(context.Background(), c.plan); !errors.As(e, &refused) || refused.Kind != c.kind || len(f.args) != 0 || len(f.calls) != 0 {
			t.Fatal(label, e, f.args)
		}
	}
	// A schema lacking only the date still lets a name through.
	p, f := msSaveProvider(t, msSaveTool(t, func(m, _ map[string]any) { delete(m, "targetDate") }))
	if _, e := p.SaveMilestone(context.Background(), MilestonePlan{Project: mpProject, Name: &name}); e != nil || len(f.args) != 1 {
		t.Fatal(e, f.args)
	}
}

// The name rules (N2): trimmed, 1-80 characters, no control, bidi or
// zero-width character; the comparison key folds case and normalizes to NFC.
func TestMilestoneNameRules(t *testing.T) {
	for name, ok := range map[string]bool{
		"Beta": true, " Beta ": true, "测试 milestone": true, strings.Repeat("é", 80): true, strings.Repeat("x", 81): false,
		"": false, "   ": false, "a\nb": false, "a\tb": false, "a\x00b": false, "a\x1bb": false, "a\u007fb": false, "a\u0085b": false,
		"a\u2028b": false, "a\u2029b": false,
		"a\u202ab": false, "a\u202bb": false, "a\u202cb": false, "a\u202db": false, "a\u202eb": false,
		"a\u2066b": false, "a\u2067b": false, "a\u2068b": false, "a\u2069b": false, "a\u200eb": false, "a\u200fb": false, "a\u061cb": false,
		"a\u200bb": false, "a\u200cb": false, "a\u200db": false, "a\u2060b": false, "a\ufeffb": false,
		"a\xffb": false,
	} {
		got, e := CleanMilestoneName(name)
		if (e == nil) != ok {
			t.Errorf("%q: %q %v", name, got, e)
		}
		if ok && got != strings.TrimSpace(name) {
			t.Errorf("%q: not trimmed: %q", name, got)
		}
	}
	same := [][2]string{{"Beta", "beta"}, {"BETA", "beta"}, {"caf\u00e9", "cafe\u0301"}, {"CAF\u00c9", "cafe\u0301"}, {"Stra\u00dfe", "STRASSE"}, {" Beta", "beta "}}
	for _, c := range same {
		if MilestoneNameKey(c[0]) != MilestoneNameKey(c[1]) || MilestoneNameHash(c[0]) != MilestoneNameHash(c[1]) {
			t.Errorf("%q and %q differ", c[0], c[1])
		}
	}
	if MilestoneNameKey("Beta") == MilestoneNameKey("Beta 2") || MilestoneNameHash("Beta") == MilestoneNameHash("Alpha") {
		t.Error("different names matched")
	}
	if h := MilestoneNameHash("Beta"); len(h) != 64 || strings.ToLower(h) != h {
		t.Error(h)
	}
}
