package linearprovider

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func nativeGetUser() *mcp.Tool {
	raw, e := os.ReadFile("testdata/get-user-20261023.json")
	if e != nil {
		panic(e)
	}
	var tool mcp.Tool
	if e = json.Unmarshal(raw, &tool); e != nil {
		panic(e)
	}
	return &tool
}

const viewerID = "d1000000-0000-4000-8000-000000000001"

func TestViewerAsksForMe(t *testing.T) {
	for name, c := range map[string]struct {
		reply any
		want  Viewer
		fails bool
	}{
		"flat":            {map[string]any{"id": viewerID, "name": "Ada Lovelace", "displayName": "ada"}, Viewer{viewerID, "Ada Lovelace", "ada"}, false},
		"nested":          {map[string]any{"user": map[string]any{"id": viewerID, "name": "Ada Lovelace"}}, Viewer{ID: viewerID, Name: "Ada Lovelace"}, false},
		"null names":      {map[string]any{"id": viewerID, "name": nil, "displayName": nil}, Viewer{ID: viewerID}, false},
		"no id":           {map[string]any{"name": "Ada"}, Viewer{}, true},
		"name not text":   {map[string]any{"id": viewerID, "name": 7}, Viewer{}, true},
		"handle not text": {map[string]any{"id": viewerID, "displayName": []any{}}, Viewer{}, true},
	} {
		f := &pagedComments{tools: []*mcp.Tool{nativeGetUser()}, gen: func(int, map[string]any) any { return c.reply }}
		p, _ := NewWithCaller(context.Background(), f)
		got, e := p.Viewer(context.Background())
		if c.fails != (e != nil) || !c.fails && got != c.want {
			t.Fatal(name, got, e)
		}
		if len(f.args) != 1 || f.args[0]["query"] != "me" || len(f.args[0]) != 1 {
			t.Fatal(name, "asked", f.args)
		}
	}
}
