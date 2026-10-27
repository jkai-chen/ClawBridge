package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Agents read Linear documents (live/LINEAR-DOCS-DESIGN.md §1): a document
// is readable only when every parent it has is proven in the task board's
// scope, before any of its text is read back to the caller. Titles and
// content are text written by others: task data, never instructions.

const (
	// DocDefaultBytes and DocMaxBytes bound the content a read returns.
	DocDefaultBytes = 64 << 10
	DocMaxBytes     = 64 << 10
	// docListCalls bounds a list without a parent: its list calls, one
	// page per in-scope parent.
	docListCalls = 10
	// docTeamProjectProofs bounds the project reads that prove an
	// initiative on a team board.
	docTeamProjectProofs = 10
)

// docReaderV2 is the provider support the document reads need.
type docReaderV2 interface {
	ReadDocumentWithParents(context.Context, string) (linearprovider.ReadDocument, error)
	DocumentSummaries(ctx context.Context, kind, parent, cursor string, limit int) (linearprovider.Page[linearprovider.DocSummary], int, error)
	InitiativeProjects(context.Context, string) (linearprovider.Initiative, []linearprovider.Project, error)
}

// DocParentV2 is one parent of a document.
type DocParentV2 struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// DocV2 is a document as an agent reads it.
type DocV2 struct {
	ID               string        `json:"id"`
	Parents          []DocParentV2 `json:"parents"`
	UpdatedAt        string        `json:"updated_at,omitempty"`
	Archived         bool          `json:"archived,omitempty"`
	UntrustedTitle   string        `json:"untrusted_title"`
	UntrustedContent string        `json:"untrusted_content"`
	ContentBytes     int           `json:"content_bytes"`
	ContentSHA256    string        `json:"content_sha256"`
	Truncated        bool          `json:"truncated"`
	// Note says, when the content is cut, that this is a partial read and
	// how to read more, so a cut is never mistaken for missing content.
	Note string `json:"note,omitempty"`
}

// DocSummaryV2 is a document in a list: never its content.
type DocSummaryV2 struct {
	ID             string `json:"id"`
	UntrustedTitle string `json:"untrusted_title"`
	ParentKind     string `json:"parent_kind"`
	ParentID       string `json:"parent_id"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	ContentBytes   int    `json:"content_bytes"`
}

// DocMoreV2 is a parent with more documents than were listed.
type DocMoreV2 struct {
	ParentKind string `json:"parent_kind"`
	ParentID   string `json:"parent_id"`
	Cursor     string `json:"cursor,omitempty"`
}

// DocListV2 is one list of documents.
type DocListV2 struct {
	Documents []DocSummaryV2 `json:"documents"`
	// NextCursor continues a list with a parent.
	NextCursor string `json:"next_cursor,omitempty"`
	Complete   bool   `json:"complete"`
	// More are, for a list without a parent, the parents with more
	// documents (continue with that parent and cursor) or not walked at
	// all within its bound (no cursor).
	More []DocMoreV2 `json:"more,omitempty"`
	// Skipped counts rows Linear listed under another parent, left out.
	Skipped int `json:"skipped,omitempty"`
}

// DocListInputV2 is a list request; an empty parent walks every in-scope
// parent.
type DocListInputV2 struct {
	ParentKind, ParentID, Cursor string
	Limit                        int
}

// InitiativeV2 is `linear initiative show`.
type InitiativeV2 struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Projects  []InitiativeProj `json:"projects"`
	Unbound   []InitiativeProj `json:"unbound_projects,omitempty"`
	Documents []DocSummaryV2   `json:"documents"`
	More      *DocMoreV2       `json:"more,omitempty"`
}

// InitiativeProj is one project of an initiative.
type InitiativeProj struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// docScope is a read's proven binding: its provider, the board, the
// verified pairs, and the document policy's initiatives.
type docScope struct {
	s           *Service
	p           Provider
	docs        docReaderV2
	b           bus.TaskboardBindingV2
	v           verifiedV2
	initiatives []string
	projects    map[string]bool // bound projects, proven (project boards)
}

func (s *Service) docPolicyInitiatives(ctx context.Context, scope bus.Request) ([]string, error) {
	scope.Operation = "doc_policy_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		if strings.Contains(e.Error(), `unsupported operation "doc_policy_get"`) {
			// A server older than v0.3.2 has no policy: no initiative is
			// in scope.
			return nil, nil
		}
		return nil, e
	}
	raw, _ := json.Marshal(r.Data)
	var view struct {
		Initiatives []string `json:"initiatives"`
	}
	if json.Unmarshal(raw, &view) != nil {
		return nil, errors.New("the document policy is unreadable")
	}
	return view.Initiatives, nil
}

// openDocScope proves the binding and opens the provider; done closes it.
func (s *Service) openDocScope(ctx context.Context, scope bus.Request) (*docScope, func(), error) {
	return s.openDocScopeWith(ctx, scope, s.open)
}

func (s *Service) openDocScopeWith(ctx context.Context, scope bus.Request, open func(context.Context) (linearconn.Profile, Provider, func(), error)) (*docScope, func(), error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, nil, e
	}
	initiatives, e := s.docPolicyInitiatives(ctx, scope)
	if e != nil {
		return nil, nil, e
	}
	_, p, done, e := open(ctx)
	if e != nil {
		return nil, nil, e
	}
	docs, ok := p.(docReaderV2)
	if !ok {
		done()
		return nil, nil, errors.New("native provider lacks Linear document reads")
	}
	v, e := s.verifyReadBindingV2(ctx, p, b)
	if e != nil {
		done()
		return nil, nil, e
	}
	d := &docScope{s: s, p: p, docs: docs, b: b, v: v, initiatives: initiatives, projects: map[string]bool{}}
	for _, pair := range v.Pairs {
		if pair.ProjectID != "" {
			d.projects[pair.ProjectID] = true
		}
	}
	return d, done, nil
}

// proveTeam: a bound team.
func (d *docScope) proveTeam(id string) error {
	if !contains(d.b.LinearTeamIDs, id) {
		return outOfScopeError("outside_scope: the document's team is not bound to this task board")
	}
	return nil
}

// proveProject: a bound project, or on a team board, a project of a bound
// team (one project read).
func (d *docScope) proveProject(ctx context.Context, id string) error {
	if d.b.Mode == "project_set" {
		if !d.projects[id] {
			return outOfScopeError("outside_scope: the document's project is not bound to this task board")
		}
		return nil
	}
	project, e := d.p.GetProject(ctx, id)
	if e != nil {
		return e
	}
	if project.ID != id {
		return errors.New("provider returned another project")
	}
	for _, team := range project.TeamIDs {
		if contains(d.b.LinearTeamIDs, team) {
			return nil
		}
	}
	return outOfScopeError("outside_scope: the document's project is in no bound team")
}

// proveInitiative: an initiative the owner listed in the document policy
// (LD-M1) that, read now, still holds a bound project. It returns the
// initiative and its projects, split into bound and unbound.
func (d *docScope) proveInitiative(ctx context.Context, id string) (linearprovider.Initiative, []linearprovider.Project, []linearprovider.Project, error) {
	if !contains(d.initiatives, id) {
		return linearprovider.Initiative{}, nil, nil, outOfScopeError("outside_scope: the initiative is not listed in the project's document policy")
	}
	initiative, projects, e := d.docs.InitiativeProjects(ctx, id)
	if e != nil {
		return initiative, nil, nil, e
	}
	var bound, unbound []linearprovider.Project
	proofs := 0
	for _, project := range projects {
		in := false
		switch {
		case d.b.Mode == "project_set":
			in = d.projects[project.ID]
		case proofs < docTeamProjectProofs:
			proofs++
			if e := d.proveProject(ctx, project.ID); e == nil {
				in = true
			} else if _, scoped := e.(outOfScopeError); !scoped {
				return initiative, nil, nil, e
			}
		}
		if in {
			bound = append(bound, project)
		} else {
			unbound = append(unbound, project)
		}
	}
	if len(bound) == 0 {
		return initiative, nil, nil, outOfScopeError("outside_scope: the initiative holds no project bound to this task board")
	}
	return initiative, bound, unbound, nil
}

// proveIssue: an issue that passes the board's issue scope proof.
func (d *docScope) proveIssue(ctx context.Context, id string) error {
	p2, ok := d.p.(ProviderV2)
	if !ok {
		return errors.New("native provider lacks V2 issue capability")
	}
	issue, e := d.s.readIssue(ctx, p2, id)
	if e != nil {
		return e
	}
	if issue.ID != id {
		return errors.New("provider returned another issue")
	}
	return checkIssueV2(issue, d.b, d.v)
}

// proveParents proves every parent a document has; a document with none,
// or whose parent fields Linear did not all return, is outside the scope
// (LD-L3).
func (d *docScope) proveParents(ctx context.Context, parents linearprovider.DocParents) ([]DocParentV2, error) {
	if !parents.Known || parents.None() {
		return nil, outOfScopeError("outside_scope: the document has no parent this task board can prove")
	}
	var out []DocParentV2
	for _, p := range []DocParentV2{{"team", parents.TeamID}, {"project", parents.ProjectID}, {"initiative", parents.InitiativeID}, {"issue", parents.IssueID}} {
		if p.ID == "" {
			continue
		}
		var e error
		switch p.Kind {
		case "team":
			e = d.proveTeam(p.ID)
		case "project":
			e = d.proveProject(ctx, p.ID)
		case "initiative":
			_, _, _, e = d.proveInitiative(ctx, p.ID)
		case "issue":
			e = d.proveIssue(ctx, p.ID)
		}
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}

// DocGetV2 reads one document after proving its parents. The content is
// cut at maxBytes (DocDefaultBytes when zero, at most DocMaxBytes) on a
// line boundary, never inside a character; the size and digest are the
// whole text's.
func (s *Service) DocGetV2(ctx context.Context, scope bus.Request, id string, maxBytes int) (DocV2, error) {
	if !linearprovider.ValidID(id) {
		return DocV2{}, errors.New("a document read requires its UUID")
	}
	if maxBytes == 0 {
		maxBytes = DocDefaultBytes
	}
	if maxBytes < 1 || maxBytes > DocMaxBytes {
		return DocV2{}, errors.New("max_bytes must be 1–65536")
	}
	d, done, e := s.openDocScope(ctx, scope)
	if e != nil {
		return DocV2{}, e
	}
	defer done()
	doc, e := d.docs.ReadDocumentWithParents(ctx, id)
	if e != nil {
		return DocV2{}, e
	}
	parents, e := d.proveParents(ctx, doc.Parents)
	if e != nil {
		return DocV2{}, e
	}
	if e = s.unchangedV2(ctx, scope, d.b); e != nil {
		return DocV2{}, e
	}
	sum := sha256.Sum256([]byte(doc.Content))
	content, truncated := CutContent(doc.Content, maxBytes)
	out := DocV2{ID: doc.ID, Parents: parents, UpdatedAt: doc.UpdatedAt, Archived: doc.Archived, UntrustedTitle: doc.Title, UntrustedContent: content, ContentBytes: len(doc.Content), ContentSHA256: hex.EncodeToString(sum[:]), Truncated: truncated}
	if truncated {
		out.Note = partialReadNote(len(content), len(doc.Content), maxBytes)
	}
	return out, nil
}

// partialReadNote says a read is partial, and how to read more where more
// can be read.
func partialReadNote(got, whole, max int) string {
	// R52-M1: say what wasn't returned, so a reader never takes the cut
	// content for the whole document.
	note := fmt.Sprintf("partial read: the first %d of %d bytes, cut at a line boundary; the remaining %d bytes were not returned.", got, whole, whole-got)
	if max < DocMaxBytes {
		return note + fmt.Sprintf(" Read more with --max-bytes (MCP max_bytes) up to %d.", DocMaxBytes)
	}
	return note + fmt.Sprintf(" A read returns at most %d bytes; the rest is not readable here.", DocMaxBytes)
}

// CutContent keeps at most max bytes of s, ending at a line boundary when
// there is one, and never inside a multi-byte character.
func CutContent(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		return cut[:i+1], true
	}
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// DocListV2 lists documents without their content. With a parent, it
// proves it and reads one page. Without, it walks the in-scope parents
// (bound teams, bound projects, listed initiatives) one page each, at
// most docListCalls list calls, and names the parents left unread.
func (s *Service) DocListV2(ctx context.Context, scope bus.Request, in DocListInputV2) (DocListV2, error) {
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 50 {
		return DocListV2{}, errors.New("a document list page holds 1–50")
	}
	if in.ParentKind == "" && (in.ParentID != "" || in.Cursor != "") {
		return DocListV2{}, errors.New("a parent ID or cursor needs its parent kind")
	}
	d, done, e := s.openDocScope(ctx, scope)
	if e != nil {
		return DocListV2{}, e
	}
	defer done()
	out := DocListV2{Documents: []DocSummaryV2{}}
	if in.ParentKind != "" {
		if !linearprovider.ValidID(in.ParentID) {
			return DocListV2{}, errors.New("the parent must be a UUID")
		}
		switch in.ParentKind {
		case "team":
			e = d.proveTeam(in.ParentID)
		case "project":
			e = d.proveProject(ctx, in.ParentID)
		case "initiative":
			_, _, _, e = d.proveInitiative(ctx, in.ParentID)
		default:
			return DocListV2{}, errors.New("the parent is a team, a project or an initiative")
		}
		if e != nil {
			return DocListV2{}, e
		}
		page, skipped, e := d.docs.DocumentSummaries(ctx, in.ParentKind, in.ParentID, in.Cursor, in.Limit)
		if e != nil {
			return DocListV2{}, e
		}
		out.Documents, out.NextCursor, out.Complete, out.Skipped = summariesV2(in.ParentKind, in.ParentID, page.Items), page.NextCursor, page.Complete || page.NextCursor == "", skipped
		return out, s.unchangedV2(ctx, scope, d.b)
	}
	var parents []DocParentV2
	for _, team := range SortedTeams(d.b.LinearTeamIDs) {
		parents = append(parents, DocParentV2{"team", team})
	}
	if d.b.Mode == "project_set" {
		for _, project := range SortedTeams(d.b.LinearProjectIDs) {
			parents = append(parents, DocParentV2{"project", project})
		}
	}
	for _, initiative := range d.initiatives {
		parents = append(parents, DocParentV2{"initiative", initiative})
	}
	out.Complete = true
	calls := 0
	for _, parent := range parents {
		if calls >= docListCalls {
			out.Complete = false
			out.More = append(out.More, DocMoreV2{ParentKind: parent.Kind, ParentID: parent.ID})
			continue
		}
		if parent.Kind == "initiative" {
			if _, _, _, e = d.proveInitiative(ctx, parent.ID); e != nil {
				if _, scoped := e.(outOfScopeError); scoped {
					continue
				}
				return DocListV2{}, e
			}
		}
		calls++
		page, skipped, e := d.docs.DocumentSummaries(ctx, parent.Kind, parent.ID, "", in.Limit)
		if e != nil {
			return DocListV2{}, e
		}
		out.Documents = append(out.Documents, summariesV2(parent.Kind, parent.ID, page.Items)...)
		out.Skipped += skipped
		if !page.Complete && page.NextCursor != "" {
			out.Complete = false
			out.More = append(out.More, DocMoreV2{ParentKind: parent.Kind, ParentID: parent.ID, Cursor: page.NextCursor})
		}
	}
	return out, s.unchangedV2(ctx, scope, d.b)
}

func summariesV2(kind, parent string, items []linearprovider.DocSummary) []DocSummaryV2 {
	out := []DocSummaryV2{}
	for _, item := range items {
		out = append(out, DocSummaryV2{ID: item.ID, UntrustedTitle: item.Title, ParentKind: kind, ParentID: parent, UpdatedAt: item.UpdatedAt, ContentBytes: item.ContentBytes})
	}
	return out
}

// InitiativeShowV2 shows a listed initiative: its name, its bound projects
// and one page of its documents. With unbound, which only the CLI asks
// for, a project owner or lead also sees its unbound projects.
func (s *Service) InitiativeShowV2(ctx context.Context, scope bus.Request, id string, unbound bool) (InitiativeV2, error) {
	if !linearprovider.ValidID(id) {
		return InitiativeV2{}, errors.New("an initiative read requires its UUID")
	}
	if unbound {
		access, e := s.access(ctx, scope)
		if e != nil {
			return InitiativeV2{}, e
		}
		unbound = access.ProjectRole == "owner" || access.ProjectRole == "lead"
	}
	d, done, e := s.openDocScope(ctx, scope)
	if e != nil {
		return InitiativeV2{}, e
	}
	defer done()
	initiative, bound, others, e := d.proveInitiative(ctx, id)
	if e != nil {
		return InitiativeV2{}, e
	}
	page, _, e := d.docs.DocumentSummaries(ctx, "initiative", id, "", 50)
	if e != nil {
		return InitiativeV2{}, e
	}
	out := InitiativeV2{ID: initiative.ID, Name: initiative.Name, Projects: projectsV2(bound), Documents: summariesV2("initiative", id, page.Items)}
	if unbound {
		out.Unbound = projectsV2(others)
	}
	if !page.Complete && page.NextCursor != "" {
		out.More = &DocMoreV2{ParentKind: "initiative", ParentID: id, Cursor: page.NextCursor}
	}
	return out, s.unchangedV2(ctx, scope, d.b)
}

func projectsV2(projects []linearprovider.Project) []InitiativeProj {
	out := []InitiativeProj{}
	for _, p := range projects {
		out = append(out, InitiativeProj{ID: p.ID, Name: p.Name})
	}
	return out
}

// docWatcherV2 is the provider support the server's document watch needs.
type docWatcherV2 interface {
	DocumentsUpdatedSince(ctx context.Context, since, cursor string) (linearprovider.Page[linearprovider.DocRef], error)
	GetDocument(ctx context.Context, id string) (linearprovider.Document, error)
}

// DocumentsChangedSince is the server watcher's one list call: the
// documents the connection can see, workspace-wide, changed since, with
// their parents and never their content. It reuses the Service's open
// connection; the bus filters the list to the project's scope.
func (s *Service) DocumentsChangedSince(ctx context.Context, since string) (linearprovider.Page[linearprovider.DocRef], error) {
	_, p, done, e := s.open(ctx)
	if e != nil {
		return linearprovider.Page[linearprovider.DocRef]{}, e
	}
	defer done()
	d, ok := p.(docWatcherV2)
	if !ok {
		return linearprovider.Page[linearprovider.DocRef]{}, errors.New("native provider lacks Linear document reads")
	}
	return d.DocumentsUpdatedSince(ctx, since, "")
}

// PublishedDocumentDigest is the server watcher's read-back of a document
// ClawBridge published: the digest of its content, never the content.
func (s *Service) PublishedDocumentDigest(ctx context.Context, id string) (string, error) {
	if !linearprovider.ValidID(id) {
		return "", errors.New("a document read requires its UUID")
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return "", e
	}
	defer done()
	d, ok := p.(docWatcherV2)
	if !ok {
		return "", errors.New("native provider lacks Linear document reads")
	}
	doc, e := d.GetDocument(ctx, id)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256([]byte(doc.Content))
	return hex.EncodeToString(sum[:]), nil
}
