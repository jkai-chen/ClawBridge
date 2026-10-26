package linearprovider

import (
	"context"
	"reflect"
	"testing"
)

const (
	docProject    = "d4000000-0000-4000-8000-000000000004"
	docInitiative = "d5000000-0000-4000-8000-000000000005"
)

// A read with parents takes them from get_document's parent objects, and
// tells a missing field from a null one.
func TestReadDocumentWithParents(t *testing.T) {
	full := map[string]any{"id": docA, "title": "Plan", "content": "# Plan", "updatedAt": "2026-10-26T10:00:00Z", "archivedAt": nil,
		"team": nil, "project": map[string]any{"id": docProject, "name": "P"}, "initiative": nil, "issue": nil}
	partial := map[string]any{"id": docA, "title": "Plan", "content": "# Plan", "project": map[string]any{"id": docProject, "name": "P"}}
	workspace := map[string]any{"id": docA, "title": "Plan", "content": "x", "team": nil, "project": nil, "initiative": nil, "issue": nil}
	p, f := documentProvider(t, map[string][]any{"get_document": {full, partial, workspace, map[string]any{"id": docB, "content": "x"}}})
	d, e := p.ReadDocumentWithParents(context.Background(), docA)
	if e != nil || d.Parents != (DocParents{ProjectID: docProject, Known: true}) || d.Content != "# Plan" || d.UpdatedAt != "2026-10-26T10:00:00Z" {
		t.Fatal(d, e)
	}
	if !reflect.DeepEqual(f.args[0], map[string]any{"id": docA}) {
		t.Fatal(f.args[0])
	}
	if d, e = p.ReadDocumentWithParents(context.Background(), docA); e != nil || d.Parents.Known {
		t.Fatal("a response without every parent field was taken as complete", d, e)
	}
	if d, e = p.ReadDocumentWithParents(context.Background(), docA); e != nil || !d.Parents.Known || !d.Parents.None() {
		t.Fatal("workspace-level", d, e)
	}
	if _, e = p.ReadDocumentWithParents(context.Background(), docA); e == nil {
		t.Fatal("another document's answer was accepted")
	}
}

// A page of summaries filters by its parent, never searches, and carries
// each document's size instead of its content; a row under another parent
// is left out and counted.
func TestDocumentSummaries(t *testing.T) {
	row := func(id, project string) map[string]any {
		return map[string]any{"id": id, "title": "T", "content": "héllo", "updatedAt": "2026-10-26T10:00:00Z", "team": nil, "project": map[string]any{"id": project}, "initiative": nil, "issue": nil}
	}
	p, f := documentProvider(t, map[string][]any{"list_documents": {
		map[string]any{"documents": []any{row(docA, docProject)}, "hasNextPage": true, "cursor": "c2"},
		map[string]any{"documents": []any{row(docB, docTeam)}},
	}})
	page, skipped, e := p.DocumentSummaries(context.Background(), "project", docProject, "", 50)
	if e != nil || skipped != 0 || len(page.Items) != 1 || page.Items[0].ContentBytes != len("héllo") || page.NextCursor != "c2" || page.Complete {
		t.Fatal(page, e)
	}
	want := map[string]any{"projectId": docProject, "limit": 50, "fields": docSummaryFields}
	if !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("list arguments", f.args[0])
	}
	if page, skipped, e = p.DocumentSummaries(context.Background(), "project", docProject, "c2", 50); e != nil || skipped != 1 || len(page.Items) != 0 {
		t.Fatal("a row under another parent was listed", page, skipped, e)
	}
	if !reflect.DeepEqual(f.args[1]["cursor"], "c2") {
		t.Fatal(f.args[1])
	}
	for name, call := range map[string]func() error{
		"issue kind": func() error {
			_, _, e := p.DocumentSummaries(context.Background(), "issue", docProject, "", 50)
			return e
		},
		"limit":  func() error { _, _, e := p.DocumentSummaries(context.Background(), "team", docTeam, "", 51); return e },
		"parent": func() error { _, _, e := p.DocumentSummaries(context.Background(), "team", "FJA", "", 50); return e },
	} {
		if call() == nil {
			t.Fatal(name, "accepted")
		}
	}
	if len(f.args) != 2 {
		t.Fatal("a refused request reached Linear")
	}
}

func TestDocumentSummariesByInitiative(t *testing.T) {
	p, f := documentProvider(t, map[string][]any{"list_documents": {map[string]any{"documents": []any{
		map[string]any{"id": docA, "title": "T", "content": "x", "team": nil, "project": nil, "initiative": map[string]any{"id": docInitiative}, "issue": nil},
	}}}})
	page, _, e := p.DocumentSummaries(context.Background(), "initiative", docInitiative, "", 10)
	if e != nil || len(page.Items) != 1 || f.args[0]["initiativeId"] != docInitiative {
		t.Fatal(page, e, f.args)
	}
}

// An initiative's projects are read inline; a paged answer is refused.
func TestInitiativeProjects(t *testing.T) {
	p, f := initiativeProvider(t, map[string][]any{"get_initiative": {
		map[string]any{"id": docInitiative, "name": "Migration", "projects": []any{map[string]any{"id": docProject, "name": "P"}}},
		map[string]any{"id": docInitiative, "name": "Migration", "projects": []any{}, "hasNextPage": true, "cursor": "c"},
		map[string]any{"id": docA, "name": "Other", "projects": []any{}},
	}})
	i, projects, e := p.InitiativeProjects(context.Background(), docInitiative)
	if e != nil || i.Name != "Migration" || len(projects) != 1 || projects[0].ID != docProject || projects[0].Name != "P" {
		t.Fatal(i, projects, e)
	}
	if !reflect.DeepEqual(f.args[0], map[string]any{"query": docInitiative, "includeProjects": true}) {
		t.Fatal(f.args[0])
	}
	if _, _, e = p.InitiativeProjects(context.Background(), docInitiative); e == nil {
		t.Fatal("a paged project list was taken as complete")
	}
	if _, _, e = p.InitiativeProjects(context.Background(), docInitiative); e == nil {
		t.Fatal("another initiative's answer was accepted")
	}
}
