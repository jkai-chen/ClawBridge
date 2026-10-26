package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	docTeam = "d1000000-0000-4000-8000-000000000001"
	docA    = "d2000000-0000-4000-8000-000000000002"
	docB    = "d3000000-0000-4000-8000-000000000003"
)

func documentProvider(t *testing.T, answers map[string][]any) (*Provider, *scripted) {
	t.Helper()
	tools := []*mcp.Tool{fixtureTool(t, "save-document-20261023.json"), fixtureTool(t, "get-document-20261023.json"), fixtureTool(t, "list-documents-20261023.json")}
	f := &scripted{fake: &fake{tools: tools}, answers: answers}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	if reason := p.DocumentReason(); reason != "" {
		t.Fatal("native document schemas refused:", reason)
	}
	return p, f
}

// A create sends the title, the content and the parent only; an update
// sends the ID and the content only.
func TestDocumentCreateAndUpdateSendOnlyWhatTheySay(t *testing.T) {
	p, f := documentProvider(t, map[string][]any{"save_document": {
		map[string]any{"document": map[string]any{"id": docA, "title": "Guide"}},
		map[string]any{"id": docA, "title": "Guide"},
		map[string]any{"id": docB},
	}})
	d, e := p.CreateDocument(context.Background(), "team", docTeam, "Guide", "# Hello")
	if e != nil || d.ID != docA {
		t.Fatal(d, e)
	}
	if want := map[string]any{"title": "Guide", "content": "# Hello", "team": docTeam}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("create arguments", f.args[0])
	}
	if d, e = p.UpdateDocumentContent(context.Background(), docA, "# New"); e != nil || d.ID != docA {
		t.Fatal(d, e)
	}
	if want := map[string]any{"id": docA, "content": "# New"}; !reflect.DeepEqual(f.args[1], want) {
		t.Fatal("update arguments", f.args[1])
	}
	if _, e = p.UpdateDocumentContent(context.Background(), docA, "# New"); e == nil {
		t.Fatal("an update answered for another document")
	}
	for name, call := range map[string]func() error{
		"parent kind": func() error { _, e := p.CreateDocument(context.Background(), "cycle", docTeam, "G", "x"); return e },
		"parent id":   func() error { _, e := p.CreateDocument(context.Background(), "team", "FJA", "G", "x"); return e },
		"no title":    func() error { _, e := p.CreateDocument(context.Background(), "team", docTeam, "", "x"); return e },
		"too long": func() error {
			_, e := p.CreateDocument(context.Background(), "team", docTeam, "G", string(make([]byte, MaxDocumentContent+1)))
			return e
		},
	} {
		if call() == nil {
			t.Fatal(name, "accepted")
		}
	}
	if len(f.args) != 3 {
		t.Fatal("a refused write was sent", f.args)
	}
}

// A read needs the content; an archived document says so.
func TestDocumentGetAndArchived(t *testing.T) {
	p, _ := documentProvider(t, map[string][]any{"get_document": {
		map[string]any{"id": docA, "title": "Guide", "content": "body", "archivedAt": nil},
		map[string]any{"id": docA, "content": "body", "archivedAt": "2026-10-26T10:00:00.000Z"},
		map[string]any{"id": docA, "title": "Guide"},
		map[string]any{"id": docB, "content": "body"},
	}})
	if d, e := p.GetDocument(context.Background(), docA); e != nil || d.Content != "body" || d.Archived {
		t.Fatal(d, e)
	}
	if d, e := p.GetDocument(context.Background(), docA); e != nil || !d.Archived {
		t.Fatal("archived", d, e)
	}
	if _, e := p.GetDocument(context.Background(), docA); e == nil {
		t.Fatal("a read without content answered")
	}
	var pe *Error
	if _, e := p.GetDocument(context.Background(), docA); !errors.As(e, &pe) || pe.Kind != "scope_mismatch" {
		t.Fatal("another document answered", e)
	}
}

// The list walks a parent's documents without a query, archived ones
// included, and follows the cursor.
func TestDocumentsUnderWalksTheParent(t *testing.T) {
	p, f := documentProvider(t, map[string][]any{"list_documents": {
		map[string]any{"documents": []any{map[string]any{"id": docA, "title": "A", "content": "a"}}, "hasNextPage": true, "cursor": "c1"},
		map[string]any{"documents": []any{map[string]any{"id": docB, "title": "B", "content": "b", "archivedAt": "2026-10-26T10:00:00.000Z"}}, "hasNextPage": false},
	}})
	docs, complete, e := p.DocumentsUnder(context.Background(), "team", docTeam)
	if e != nil || !complete || len(docs) != 2 || docs[1].Content != "b" || !docs[1].Archived {
		t.Fatal(docs, complete, e)
	}
	if f.args[0]["teamId"] != docTeam || f.args[0]["includeArchived"] != true || f.args[0]["query"] != nil || f.args[1]["cursor"] != "c1" {
		t.Fatal(f.args)
	}
	p, f = documentProvider(t, map[string][]any{"list_documents": {map[string]any{"documents": []any{}, "hasNextPage": false}}})
	if _, complete, e = p.DocumentsUnder(context.Background(), "project", docTeam); e != nil || !complete || f.args[0]["projectId"] != docTeam {
		t.Fatal(f.args, e)
	}
}
