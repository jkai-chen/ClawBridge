package linearprovider

import (
	"context"
	"unicode/utf8"
)

// DocParents are a document's parent fields as Linear returns them; a
// workspace-level document has none. Known is false when any of the four
// fields was missing from the response, so an absent parent is never
// taken for a null one.
type DocParents struct {
	TeamID       string `json:"team_id,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	InitiativeID string `json:"initiative_id,omitempty"`
	IssueID      string `json:"issue_id,omitempty"`
	Known        bool   `json:"-"`
}

// None reports a document with no parent at all.
func (p DocParents) None() bool {
	return p.TeamID == "" && p.ProjectID == "" && p.InitiativeID == "" && p.IssueID == ""
}

// ReadDocument is a document an agent reads: its parents, when it changed,
// and its content. Title and content are text written by others.
type ReadDocument struct {
	ID        string
	Title     string
	Content   string
	UpdatedAt string
	Archived  bool
	Parents   DocParents
}

// DocSummary is a document in a list: never its content, only its size.
type DocSummary struct {
	ID           string
	Title        string
	UpdatedAt    string
	ContentBytes int
	Parents      DocParents
}

var docParentKeys = []string{"team", "project", "initiative", "issue"}

var docSummaryFields = []string{"id", "title", "content", "updatedAt", "team", "project", "initiative", "issue"}

func parseDocParents(m map[string]any) (DocParents, error) {
	p := DocParents{Known: true}
	for _, k := range docParentKeys {
		if _, ok := m[k]; !ok {
			p.Known = false
		}
	}
	for dst, k := range map[*string]string{&p.TeamID: "team", &p.ProjectID: "project", &p.InitiativeID: "initiative", &p.IssueID: "issue"} {
		var e error
		if *dst, e = parentRef(m[k]); e != nil {
			return DocParents{}, e
		}
	}
	return p, nil
}

// ReadDocumentWithParents reads one document with its content and its
// parent fields, for a read that must prove the parent before showing
// any text.
func (p *Provider) ReadDocumentWithParents(ctx context.Context, id string) (ReadDocument, error) {
	if !ValidID(id) {
		return ReadDocument{}, fail("invalid")
	}
	if p.DocumentReason() != "" {
		return ReadDocument{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"get_document"}, a(id, "id"))
	if e != nil {
		return ReadDocument{}, e
	}
	if nested := obj(m, "document"); nested != nil {
		m = nested
	}
	d, e := parseDocument(m, true)
	if e != nil {
		return ReadDocument{}, e
	}
	if d.ID != id {
		return ReadDocument{}, fail("scope_mismatch")
	}
	parents, e := parseDocParents(m)
	if e != nil {
		return ReadDocument{}, e
	}
	return ReadDocument{ID: d.ID, Title: d.Title, Content: d.Content, UpdatedAt: str(m, "updatedAt"), Archived: d.Archived, Parents: parents}, nil
}

// DocumentSummaries reads one page of the documents of a team, a project
// or an initiative, archived ones left out, without a search query. Each
// summary carries the content's size, never the content. A row whose
// parent is not the one asked for (a team's filter may also return its
// projects' documents) is left out and counted in skipped.
func (p *Provider) DocumentSummaries(ctx context.Context, kind, parent, cursor string, limit int) (_ Page[DocSummary], skipped int, _ error) {
	var where arg
	switch {
	case !ValidID(parent) || limit < 1 || limit > 50 || !textOK(cursor, 2048):
		return Page[DocSummary]{}, 0, fail("invalid")
	case kind == "initiative":
		where = a(parent, "initiativeId")
	default:
		var ok bool
		if where, ok = parentArg(kind, parent, true); !ok {
			return Page[DocSummary]{}, 0, fail("invalid")
		}
	}
	if p.DocumentReason() != "" {
		return Page[DocSummary]{}, 0, fail("capability_unavailable")
	}
	args := []arg{where, {[]string{"limit"}, limit, "integer"}, {[]string{"fields"}, docSummaryFields, "array"}}
	if cursor != "" {
		args = append(args, a(cursor, "cursor"))
	}
	m, e := p.v2Call(ctx, []string{"list_documents"}, args...)
	if e != nil {
		return Page[DocSummary]{}, 0, e
	}
	rows, next, complete, e := list(m, "documents")
	if e != nil {
		return Page[DocSummary]{}, 0, e
	}
	out := Page[DocSummary]{NextCursor: next, Complete: complete}
	for _, row := range rows {
		d, e := parseDocument(row, true)
		if e != nil {
			return Page[DocSummary]{}, 0, e
		}
		parents, e := parseDocParents(row)
		if e != nil {
			return Page[DocSummary]{}, 0, e
		}
		got := map[string]string{"team": parents.TeamID, "project": parents.ProjectID, "initiative": parents.InitiativeID}[kind]
		if got != parent {
			skipped++
			continue
		}
		out.Items = append(out.Items, DocSummary{ID: d.ID, Title: d.Title, UpdatedAt: str(row, "updatedAt"), ContentBytes: len(d.Content), Parents: parents})
	}
	return out, skipped, nil
}

// InitiativeProjects reads an initiative's name and projects. Linear
// returns the projects inline, without paging (seen 2026-10-23); a list
// that says it has a next page is refused rather than read as complete.
func (p *Provider) InitiativeProjects(ctx context.Context, initiative string) (Initiative, []Project, error) {
	if !ValidID(initiative) {
		return Initiative{}, nil, fail("invalid")
	}
	if p.InitiativeReason() != "" {
		return Initiative{}, nil, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"get_initiative"}, a(initiative, "query"), arg{[]string{"includeProjects"}, true, "boolean"})
	if e != nil {
		return Initiative{}, nil, e
	}
	if nested := obj(m, "initiative"); nested != nil {
		m = nested
	}
	i, e := entityID(m, "initiativeId")
	if e != nil || i != initiative {
		return Initiative{}, nil, fail("scope_mismatch")
	}
	name := str(m, "name")
	if !textOK(name, 1024) || !utf8.ValidString(name) {
		return Initiative{}, nil, fail("invalid_response")
	}
	rows, _, complete, e := list(m, "projects")
	if e != nil {
		return Initiative{}, nil, e
	}
	if _, paged := m["hasNextPage"]; !paged && obj(m, "pageInfo") == nil {
		complete = true
	}
	if !complete {
		return Initiative{}, nil, fail("pagination_unavailable")
	}
	var projects []Project
	for _, row := range rows {
		id, e := entityID(row, "projectId")
		if e != nil {
			return Initiative{}, nil, e
		}
		projects = append(projects, Project{ID: id, Name: str(row, "name")})
	}
	return Initiative{ID: i, Name: name}, projects, nil
}
