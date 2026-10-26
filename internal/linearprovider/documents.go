package linearprovider

import (
	"context"
	"unicode/utf8"
)

// Document is a Linear document as the seeding reads it. Content is the
// Markdown Linear returns, which it re-serialises from rich text, so it may
// differ from what was sent.
type Document struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Content  string `json:"-"`
	Archived bool   `json:"archived,omitempty"`
}

// MaxDocumentContent bounds a document ClawBridge writes.
const MaxDocumentContent = 64 << 10

// documentPages bounds a walk of a parent's documents.
const documentPages = 10

// DocumentReason is why the document tools cannot be used, or "": the
// seeding needs a create under a team or a project, a content-only update,
// a read by ID and a paged list by parent with content.
func (p *Provider) DocumentReason() string {
	sample := "00000000-0000-4000-8000-000000000001"
	checks := []struct {
		tool, field string
		args        []arg
	}{
		{"save_document", "title, content and team", []arg{a("x", "title"), a("x", "content"), a(sample, "team")}},
		{"save_document", "title, content and project", []arg{a("x", "title"), a("x", "content"), a(sample, "project")}},
		{"save_document", "id and content", []arg{a(sample, "id"), a("x", "content")}},
		{"get_document", "id", []arg{a(sample, "id")}},
		{"list_documents", "teamId, cursor, limit, fields and includeArchived", []arg{a(sample, "teamId"), a("c", "cursor"), {[]string{"limit"}, 50, "integer"}, {[]string{"fields"}, documentFields, "array"}, {[]string{"includeArchived"}, true, "boolean"}}},
		{"list_documents", "projectId", []arg{a(sample, "projectId")}},
		{"list_documents", "updatedAt and parent fields", []arg{a("-P1D", "updatedAt"), {[]string{"fields"}, docRefFields, "array"}}},
		{"list_documents", "initiativeId", []arg{a(sample, "initiativeId")}},
	}
	for _, c := range checks {
		if _, _, e := p.v2Negotiation([]string{c.tool}, c.args); e != nil {
			return c.tool + " no longer accepts " + c.field
		}
	}
	return ""
}

var documentFields = []string{"id", "title", "content", "archivedAt"}

// parseDocument reads a document; withContent requires its content.
func parseDocument(m map[string]any, withContent bool) (Document, error) {
	if nested := obj(m, "document"); nested != nil {
		m = nested
	}
	i, e := entityID(m, "documentId")
	if e != nil {
		return Document{}, e
	}
	d := Document{ID: i, Title: str(m, "title")}
	content, ok := m["content"].(string)
	if withContent && !ok {
		return Document{}, fail("invalid_response")
	}
	if len(content) > 4*MaxDocumentContent || !utf8.ValidString(content) {
		return Document{}, fail("invalid_response")
	}
	d.Content = content
	switch v := m["archivedAt"].(type) {
	case nil:
	case string:
		d.Archived = v != ""
	default:
		return Document{}, fail("invalid_response")
	}
	return d, nil
}

func parentArg(kind, parent string, list bool) (arg, bool) {
	if !ValidID(parent) {
		return arg{}, false
	}
	switch {
	case kind == "team" && list:
		return a(parent, "teamId"), true
	case kind == "team":
		return a(parent, "team"), true
	case kind == "project" && list:
		return a(parent, "projectId"), true
	case kind == "project":
		return a(parent, "project"), true
	}
	return arg{}, false
}

// CreateDocument creates a document under a team or a project. It sends
// the title, the content and the parent only.
func (p *Provider) CreateDocument(ctx context.Context, kind, parent, title, content string) (Document, error) {
	where, ok := parentArg(kind, parent, false)
	if !ok || title == "" || !textOK(title, 256) || !textOK(content, MaxDocumentContent) || !utf8.ValidString(content) {
		return Document{}, fail("invalid")
	}
	if p.DocumentReason() != "" {
		return Document{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"save_document"}, a(title, "title"), a(content, "content"), where)
	if e != nil {
		return Document{}, e
	}
	return parseDocument(m, false)
}

// UpdateDocumentContent replaces a document's content and nothing else.
func (p *Provider) UpdateDocumentContent(ctx context.Context, id, content string) (Document, error) {
	if !ValidID(id) || !textOK(content, MaxDocumentContent) || !utf8.ValidString(content) {
		return Document{}, fail("invalid")
	}
	if p.DocumentReason() != "" {
		return Document{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"save_document"}, a(id, "id"), a(content, "content"))
	if e != nil {
		return Document{}, e
	}
	d, e := parseDocument(m, false)
	if e == nil && d.ID != id {
		return Document{}, fail("scope_mismatch")
	}
	return d, e
}

// GetDocument reads one document with its content. Linear answers a
// missing document like any other refusal, so a failure here never proves
// the document is gone.
func (p *Provider) GetDocument(ctx context.Context, id string) (Document, error) {
	if !ValidID(id) {
		return Document{}, fail("invalid")
	}
	if p.DocumentReason() != "" {
		return Document{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"get_document"}, a(id, "id"))
	if e != nil {
		return Document{}, e
	}
	d, e := parseDocument(m, true)
	if e == nil && d.ID != id {
		return Document{}, fail("scope_mismatch")
	}
	return d, e
}

// DocumentsUnder walks the documents of a team or a project, archived ones
// included, with their content, without a search query: a document created
// moments ago is found whatever the search index holds. complete is false
// when the walk stopped at its bound.
func (p *Provider) DocumentsUnder(ctx context.Context, kind, parent string) ([]Document, bool, error) {
	where, ok := parentArg(kind, parent, true)
	if !ok {
		return nil, false, fail("invalid")
	}
	if p.DocumentReason() != "" {
		return nil, false, fail("capability_unavailable")
	}
	var out []Document
	cursor := ""
	for page := 0; page < documentPages; page++ {
		args := []arg{where, {[]string{"limit"}, 50, "integer"}, {[]string{"fields"}, documentFields, "array"}, {[]string{"includeArchived"}, true, "boolean"}}
		if cursor != "" {
			args = append(args, a(cursor, "cursor"))
		}
		m, e := p.v2Call(ctx, []string{"list_documents"}, args...)
		if e != nil {
			return nil, false, e
		}
		rows, next, complete, e := list(m, "documents")
		if e != nil {
			return nil, false, e
		}
		for _, row := range rows {
			d, e := parseDocument(row, true)
			if e != nil {
				return nil, false, e
			}
			out = append(out, d)
		}
		if complete || next == "" {
			return out, complete, nil
		}
		cursor = next
	}
	return out, false, nil
}

// DocRef is a document as the watcher sees it in a workspace-wide list:
// its ID, when it changed, and its parent. It never carries the title or
// the content.
type DocRef struct {
	ID           string `json:"id"`
	UpdatedAt    string `json:"updated_at"`
	TeamID       string `json:"team_id,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	InitiativeID string `json:"initiative_id,omitempty"`
	IssueID      string `json:"issue_id,omitempty"`
}

var docRefFields = []string{"id", "updatedAt", "team", "project", "initiative", "issue"}

// DocumentsUpdatedSince lists the documents the connection can see, across
// the workspace, changed after since (an ISO-8601 time or duration), with
// their parents and no content: one call per page.
func (p *Provider) DocumentsUpdatedSince(ctx context.Context, since, cursor string) (Page[DocRef], error) {
	if since == "" || !textOK(since, 64) || !textOK(cursor, 2048) {
		return Page[DocRef]{}, fail("invalid")
	}
	if p.DocumentReason() != "" {
		return Page[DocRef]{}, fail("capability_unavailable")
	}
	args := []arg{a(since, "updatedAt"), {[]string{"limit"}, 50, "integer"}, {[]string{"fields"}, docRefFields, "array"}, {[]string{"includeArchived"}, true, "boolean"}}
	if cursor != "" {
		args = append(args, a(cursor, "cursor"))
	}
	m, e := p.v2Call(ctx, []string{"list_documents"}, args...)
	if e != nil {
		return Page[DocRef]{}, e
	}
	rows, next, complete, e := list(m, "documents")
	if e != nil {
		return Page[DocRef]{}, e
	}
	out := Page[DocRef]{NextCursor: next, Complete: complete}
	for _, row := range rows {
		i, e := entityID(row, "documentId")
		if e != nil {
			return Page[DocRef]{}, e
		}
		ref := DocRef{ID: i, UpdatedAt: str(row, "updatedAt")}
		for dst, k := range map[*string]string{&ref.TeamID: "team", &ref.ProjectID: "project", &ref.InitiativeID: "initiative", &ref.IssueID: "issue"} {
			if *dst, e = parentRef(row[k]); e != nil {
				return Page[DocRef]{}, e
			}
		}
		out.Items = append(out.Items, ref)
	}
	return out, nil
}

// parentRef reads a parent as Linear lists it: absent or null, an ID, or
// an object with an id.
func parentRef(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		if t == "" {
			return "", nil
		}
		if ValidID(t) {
			return t, nil
		}
	case map[string]any:
		if id, _ := t["id"].(string); ValidID(id) {
			return id, nil
		}
	}
	return "", fail("invalid_response")
}
