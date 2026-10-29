package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ptr(s string) *string { return &s }

// A due date is read as a date, an ISO datetime's date part, none for null
// or empty, and unknown when absent or of any other shape.
func TestV2DueDateParse(t *testing.T) {
	type want struct {
		date  *string
		known bool
	}
	cases := map[string]struct {
		row  map[string]any
		want want
	}{
		"date":           {map[string]any{"dueDate": "2026-11-14"}, want{ptr("2026-11-14"), true}},
		"datetime":       {map[string]any{"dueDate": "2026-11-14T00:00:00.000Z"}, want{ptr("2026-11-14"), true}},
		"datetime zone":  {map[string]any{"dueDate": "2026-11-14T10:00:00+02:00"}, want{ptr("2026-11-14"), true}},
		"null":           {map[string]any{"dueDate": nil}, want{nil, true}},
		"empty":          {map[string]any{"dueDate": ""}, want{nil, true}},
		"absent":         {map[string]any{}, want{nil, false}},
		"number":         {map[string]any{"dueDate": 20261114}, want{nil, false}},
		"object":         {map[string]any{"dueDate": map[string]any{"value": "2026-11-14"}}, want{nil, false}},
		"not a date":     {map[string]any{"dueDate": "tomorrow"}, want{nil, false}},
		"impossible":     {map[string]any{"dueDate": "2026-13-01"}, want{nil, false}},
		"bad datetime":   {map[string]any{"dueDate": "2026-11-14Tnonsense"}, want{nil, false}},
		"trailing":       {map[string]any{"dueDate": "2026-11-14 "}, want{nil, false}},
		"datetime no tz": {map[string]any{"dueDate": "2026-11-14T10:00:00"}, want{nil, false}},
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
		if x.DueDateKnown != c.want.known || !reflect.DeepEqual(x.DueDate, c.want.date) {
			t.Fatal(name, x.DueDate, x.DueDateKnown)
		}
	}
}

// Create and update send dueDate exactly: the string, or null to clear, and
// nothing when it is not Present. A date outside the plan's range is
// refused before a call.
func TestV2DueDateWrites(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: issueResult()}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := p.Capabilities(context.Background())
	if err != nil || !caps.DueDate || len(f.calls) != 0 {
		t.Fatal(caps, err, f.calls)
	}
	up := func(d Field[*string]) UpdateInputV2 {
		return UpdateInputV2{UpdateInput: UpdateInput{ID: iid, Title: ptr("t")}, DueDate: d}
	}
	if _, err := p.UpdateV2(context.Background(), up(Field[*string]{Present: true, Value: ptr("2026-11-14")})); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateV2(context.Background(), up(Field[*string]{Present: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateV2(context.Background(), up(Field[*string]{})); err != nil {
		t.Fatal(err)
	}
	// A due date alone is a change.
	if _, err := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, DueDate: Field[*string]{Present: true, Value: ptr("2026-11-15")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}, DueDate: Field[*string]{Present: true, Value: ptr("2026-12-01")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.args) != 6 {
		t.Fatal(f.args)
	}
	if v, ok := f.args[0]["dueDate"]; !ok || v != "2026-11-14" {
		t.Fatal(f.args[0])
	}
	if v, ok := f.args[1]["dueDate"]; !ok || v != nil {
		t.Fatal("clear is not null", f.args[1])
	}
	if _, ok := f.args[2]["dueDate"]; ok {
		t.Fatal("sent when not Present", f.args[2])
	}
	if f.args[3]["dueDate"] != "2026-11-15" || f.args[4]["dueDate"] != "2026-12-01" {
		t.Fatal(f.args[3], f.args[4])
	}
	if _, ok := f.args[5]["dueDate"]; ok {
		t.Fatal("create sent a due date it was not given", f.args[5])
	}
	for _, bad := range []string{"2026-13-01", "1999-12-31", "2101-01-01", "2026-11-14T00:00:00Z", "11/14/2026", ""} {
		n := len(f.calls)
		if _, err := p.UpdateV2(context.Background(), up(Field[*string]{Present: true, Value: ptr(bad)})); err == nil || len(f.calls) != n {
			t.Fatal(bad, err)
		}
	}
}

// A schema whose dueDate is missing or not nullable, or on one tool only,
// is no capability; asked anyway, nothing is sent.
func TestV2DueDateNeedsTheSchema(t *testing.T) {
	mod := func(f func(props map[string]any)) *mcp.Tool {
		tl := officialSaveIssue(t)
		root := schemaProperty(tl.InputSchema)
		f(root["properties"].(map[string]any))
		tl.InputSchema = root
		return tl
	}
	cases := map[string]*mcp.Tool{
		"missing":      mod(func(m map[string]any) { delete(m, "dueDate") }),
		"string only":  mod(func(m map[string]any) { m["dueDate"] = map[string]any{"type": "string"} }),
		"null only":    mod(func(m map[string]any) { m["dueDate"] = map[string]any{"type": "null"} }),
		"integer type": mod(func(m map[string]any) { m["dueDate"] = map[string]any{"type": []any{"integer", "null"}} }),
	}
	for name, tl := range cases {
		f := &fake{tools: []*mcp.Tool{tl}, result: issueResult()}
		p, err := NewWithCaller(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if caps, err := p.Capabilities(context.Background()); err != nil || caps.DueDate {
			t.Fatal(name, caps, err)
		}
		d := Field[*string]{Present: true, Value: ptr("2026-11-14")}
		for label, run := range map[string]func() error{
			"update": func() error {
				_, e := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid, Title: ptr("t")}, DueDate: d})
				return e
			},
			"create": func() error {
				_, e := p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}, DueDate: d})
				return e
			},
		} {
			var refused *Error
			if e := run(); !errors.As(e, &refused) || refused.Kind != "capability_unavailable" || len(f.calls) != 0 {
				t.Fatal(name, label, e, f.calls)
			}
		}
	}
	// Separate create_issue and update_issue: update must take null too.
	create := tool("create_issue", map[string]string{"team": "string", "title": "string", "description": "string", "dueDate": "string"}, "team", "title")
	updateStr := tool("update_issue", map[string]string{"id": "string", "title": "string", "dueDate": "string"}, "id")
	p, _ := NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{create, updateStr}})
	if caps, _ := p.Capabilities(context.Background()); caps.DueDate {
		t.Fatal("update without null accepted")
	}
}
