package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/docpublish"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Publishing a file from git as a Linear document (live/LINEAR-DOCS-
// DESIGN.md §2): one way, and git wins only over an untouched document. A
// project owner or lead runs it from the CLI; it is never on MCP.

// publishPendingFor is how long an uncertain create may still appear in
// the parent's list before it is taken as not made (as doc seeds).
const publishPendingFor = 10 * time.Minute

var publishOperationID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// PublishInput is one publish. Without Confirm it previews.
type PublishInput struct {
	File, Commit, Title  string
	ParentKind, ParentID string
	OperationID          string
	// AllowPatterns are deny pattern IDs the owner publishes over, for
	// this operation only; they need Confirm.
	AllowPatterns []string
	Split         bool
	// Recreate creates the documents again: over an edited, archived or
	// unreadable one, or to change the number of parts.
	Recreate bool
	Confirm  bool
	// Home expands the policy's source_root; Git runs in the source
	// repository.
	Home string
	Git  docpublish.Git
}

// PublishHit is a deny pattern's match: in the content (by the file's
// line), the title or the footer. Never the matched text.
type PublishHit struct {
	Pattern string `json:"pattern"`
	Where   string `json:"where"`
	Line    int    `json:"line,omitempty"`
}

// PublishPartResult is one part of a publish. Action is what the publish
// does (or would with --confirm): create, settle, update, none, edited,
// gone, unreadable or pending_recent. Outcome is what a confirmed run did:
// created, settled, updated, drifted, uncertain or failed.
type PublishPartResult struct {
	Part       int    `json:"part"`
	Title      string `json:"title"`
	Bytes      int    `json:"bytes"`
	Action     string `json:"action"`
	Outcome    string `json:"outcome,omitempty"`
	DocumentID string `json:"document_id,omitempty"`
	Note       string `json:"note,omitempty"`
}

// PublishResult is what a publish found and, with Confirm, did. Refused,
// when set, says why nothing was written.
type PublishResult struct {
	ParentKind     string `json:"parent_kind"`
	ParentID       string `json:"parent_id"`
	SourcePath     string `json:"source_path"`
	SourceCommit   string `json:"source_commit"`
	SourceSHA256   string `json:"source_sha256"`
	WorkingDiffers string `json:"warning,omitempty"`
	// Warnings never stop a publish: they name what Linear will show
	// differently from the file.
	Warnings   []string            `json:"warnings,omitempty"`
	Limit      int                 `json:"limit"`
	Parts      []PublishPartResult `json:"parts"`
	Mixed      bool                `json:"mixed_set,omitempty"`
	Hits       []PublishHit        `json:"deny_hits,omitempty"`
	Overridden []string            `json:"overridden_patterns,omitempty"`
	Refused    string              `json:"refused,omitempty"`
	Confirmed  bool                `json:"confirmed"`
}

type publishRow struct {
	part, parts, generation        int
	state, token, document, digest string
	sourceSHA                      string
	age                            int
}

// docWriterV2 is the provider support a publish needs.
type docWriterV2 interface {
	docReaderV2
	docProvider
}

// PublishV2 previews, or with Confirm carries out, a publish.
func (s *Service) PublishV2(ctx context.Context, scope bus.Request, in PublishInput) (PublishResult, error) {
	out := PublishResult{ParentKind: in.ParentKind, ParentID: in.ParentID, Limit: docpublish.MaxContent, Parts: []PublishPartResult{}}
	if !publishOperationID.MatchString(in.OperationID) {
		return out, errors.New("a publish needs --operation-id (1–128 of A-Z, a-z, 0-9, . _ -)")
	}
	if in.ParentKind != "team" && in.ParentKind != "project" && in.ParentKind != "initiative" || !linearprovider.ValidID(in.ParentID) {
		return out, errors.New("a publish names one parent: --team, --linear-project or --initiative, by UUID")
	}
	if len(in.AllowPatterns) > 0 && !in.Confirm {
		return out, errors.New("--allow-pattern needs --confirm")
	}
	if in.Git == nil {
		return out, errors.New("a publish needs git")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return out, e
	}
	if !b.Configured {
		return out, errors.New("task-board is not linked")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return out, e
	}
	if access.ProjectRole != "owner" && access.ProjectRole != "lead" {
		return out, errors.New("publishing documents is for a project lead or owner")
	}
	policy, e := s.publishPolicy(ctx, scope)
	if e != nil {
		return out, e
	}
	// The checks before any Linear call (LD-M3).
	src, e := docpublish.Read(in.Git, policy.SourceRoot, in.Home, in.File, in.Commit)
	if e != nil {
		return out, e
	}
	out.SourcePath, out.SourceCommit, out.SourceSHA256 = src.Path, src.Commit, src.SHA256
	if src.WorkingDiffers {
		out.WorkingDiffers = "your working copy differs from what is published (commit " + src.Short + ")"
	}
	if e = docpublish.CheckContent(src.Content); e != nil {
		return out, e
	}
	for _, line := range docpublish.MergedLists(src.Content) {
		out.Warnings = append(out.Warnings, fmt.Sprintf("line %d: an ordered list restarts right after another list, with nothing between them; Linear merges the two into one list and renumbers it. Put a line of text or a heading between them.", line))
	}
	title := docpublish.Title(in.Title, src.Content, src.Path)
	if title == "" || len(title) > 200 {
		return out, errors.New("the title must be 1–200 characters")
	}
	if e = mentionGuard(&title, &src.Content); e != nil {
		return out, e
	}
	rows, e := s.publishRows(ctx, scope, in.ParentKind, in.ParentID, src.Path)
	if e != nil {
		return out, e
	}
	// The footer's size is the same for every token.
	footer := docpublish.Footer(src, "000000000000")
	parts := []docpublish.Part{{Title: title, Body: src.Content}}
	if len(src.Content)+len(footer) > docpublish.MaxContent {
		if !in.Split {
			out.Refused = fmt.Sprintf("the document is %d bytes with its footer, over the %d-byte limit; split it in git, or pass --split to publish it in parts at its ## headings", len(src.Content)+len(footer), docpublish.MaxContent)
			return out, nil
		}
		if parts, e = docpublish.Split(title, src.Content, docpublish.MaxContent, len(footer)); e != nil {
			return out, e
		}
		if len(parts) > 20 {
			return out, errors.New("the document would take more than 20 parts")
		}
	}
	// The deny scan: the content by its own lines, every title, and the
	// footer as rendered.
	patterns := make([]docpublish.Pattern, len(policy.DenyPatterns))
	for i, p := range policy.DenyPatterns {
		patterns[i] = docpublish.Pattern{ID: p.ID, Regex: p.Regex}
	}
	scan := func(where, text string, lines bool) error {
		hits, e := docpublish.Scan(text, patterns)
		for _, h := range hits {
			hit := PublishHit{Pattern: h.Pattern, Where: where}
			if lines {
				hit.Line = h.Line
			}
			out.Hits = append(out.Hits, hit)
		}
		return e
	}
	if e = scan("content", src.Content, true); e != nil {
		return out, e
	}
	for _, p := range parts {
		if e = scan("title", p.Title, false); e != nil {
			return out, e
		}
	}
	if e = scan("footer", footer, false); e != nil {
		return out, e
	}
	if len(out.Hits) > 0 {
		hit := map[string]bool{}
		for _, h := range out.Hits {
			hit[h.Pattern] = true
		}
		allowed := map[string]bool{}
		for _, p := range in.AllowPatterns {
			allowed[p] = true
		}
		var missing []string
		for p := range hit {
			if !allowed[p] {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			out.Refused = fmt.Sprintf("the document matches deny patterns %v; nothing is published. Remove the match in git; only the project owner may publish over a pattern, with --allow-pattern ID --confirm for this operation", missing)
			return out, nil
		}
		for p := range hit {
			out.Overridden = append(out.Overridden, p)
		}
		sort.Strings(out.Overridden)
	}
	d, done, e := s.openDocScopeFor(ctx, scope, in.Confirm)
	if e != nil {
		return out, e
	}
	defer done()
	docs, ok := d.p.(docWriterV2)
	if !ok {
		return out, errors.New("native provider lacks the document tools")
	}
	if reason := docs.DocumentReason(); reason != "" {
		return out, errors.New("Linear document writes are unavailable: " + reason)
	}
	switch in.ParentKind {
	case "team":
		e = d.proveTeam(in.ParentID)
	case "project":
		e = d.proveProject(ctx, in.ParentID)
	case "initiative":
		_, _, _, e = d.proveInitiative(ctx, in.ParentID)
	}
	if e != nil {
		return out, e
	}
	plan := s.planPublish(ctx, docs, in, src, rows, parts, &out)
	// Nothing is written while any part blocks the publish: an edit in
	// Linear to any part blocks the whole re-publish.
	for _, p := range out.Parts {
		switch p.Action {
		case "edited", "gone", "unreadable", "pending_recent":
			if out.Refused == "" {
				out.Refused = "part " + fmt.Sprint(p.Part) + " cannot be published (" + p.Action + "); see its note. Nothing is written"
			}
		}
	}
	if out.Refused != "" || !in.Confirm {
		return out, nil
	}
	if e = s.unchangedV2(ctx, scope, d.b); e != nil {
		return out, e
	}
	if len(out.Overridden) > 0 {
		if e = s.publishOverride(ctx, scope, in, src, out.Hits); e != nil {
			out.Refused = "the override could not be recorded, so nothing was published: " + e.Error()
			return out, nil
		}
	}
	generation := 1
	for _, r := range rows {
		if r.generation >= generation {
			generation = r.generation + 1
		}
	}
	for i := range out.Parts {
		s.publishPart(ctx, scope, docs, in, src, parts, plan[i], generation, &out.Parts[i])
	}
	out.Confirmed = true
	return out, nil
}

// partPlan is what a part found: its ledger row, if any, and a document
// found by its token.
type partPlan struct {
	row   publishRow
	seen  bool
	found linearprovider.Document
}

// planPublish decides each part's action (§2.3, §2.4).
func (s *Service) planPublish(ctx context.Context, docs docWriterV2, in PublishInput, src docpublish.Source, rows []publishRow, parts []docpublish.Part, out *PublishResult) []partPlan {
	byPart := map[int]publishRow{}
	recorded := 0
	if len(rows) > 0 {
		for _, r := range rows {
			byPart[r.part] = r
		}
		recorded = byPart[1].parts
	}
	if recorded > 0 && recorded != len(parts) && !in.Recreate {
		out.Refused = fmt.Sprintf("this file was published in %d parts and now takes %d; pass --recreate --confirm to publish it again as new documents", recorded, len(parts))
	}
	generations := map[int]bool{}
	for p := 1; p <= recorded; p++ {
		if r, ok := byPart[p]; ok && r.state == "published" {
			generations[r.generation] = true
		}
	}
	out.Mixed = len(generations) > 1
	listed, listErr := []linearprovider.Document(nil), error(nil)
	listOnce := func() ([]linearprovider.Document, error) {
		if listed == nil && listErr == nil {
			var complete bool
			listed, complete, listErr = docs.DocumentsUnder(ctx, in.ParentKind, in.ParentID)
			if listErr == nil && !complete {
				listErr = errors.New("the parent has more documents than one walk reads")
			}
			if listed == nil {
				listed = []linearprovider.Document{}
			}
		}
		return listed, listErr
	}
	plan := make([]partPlan, len(parts))
	for i, part := range parts {
		r := PublishPartResult{Part: i + 1, Title: part.Title, Bytes: len(part.Body) + len(docpublish.Footer(src, "000000000000"))}
		// A row past the recorded part count is left from an earlier,
		// larger set: the part is created over it.
		row, ok := byPart[i+1]
		seen := ok && i+1 <= recorded
		plan[i] = partPlan{row: row, seen: seen}
		switch {
		case !seen:
			r.Action = "create"
		case in.Recreate:
			r.Action, r.Note = "create", "--recreate: a new document; the earlier one is left as it is"
		case row.state == "pending":
			list, e := listOnce()
			if e != nil {
				r.Action, r.Note = "unreadable", "could not list the parent's documents"
				break
			}
			for _, d := range list {
				if docpublish.HasToken(d.Content, row.token) {
					plan[i].found = d
				}
			}
			switch {
			case plan[i].found.ID != "":
				r.Action, r.DocumentID = "settle", plan[i].found.ID
			case row.age < 0:
				r.Action, r.Note = "pending_recent", "could not tell when the earlier create was reserved; publish again later"
			case time.Duration(row.age)*time.Second < publishPendingFor:
				r.Action, r.Note = "pending_recent", "an earlier create may still appear; publish again in a few minutes"
			default:
				r.Action, r.Note = "create", "an earlier create did not appear; it is created again"
			}
		default:
			r.DocumentID = row.document
			doc, e := docs.GetDocument(ctx, row.document)
			switch {
			case e != nil:
				r.Action, r.Note = "unreadable", "could not read the published document; only if you know it was deleted, publish with --recreate --confirm"
			case doc.Archived:
				r.Action, r.Note = "gone", "the published document is archived; publish with --recreate --confirm to create it again"
			case contentDigest(doc.Content) != row.digest:
				r.Action, r.Note = "edited", "edited in Linear since it was published; bring the edit into git, or publish with --recreate --confirm"
			case row.sourceSHA == src.SHA256:
				r.Action = "none"
			default:
				r.Action = "update"
			}
		}
		out.Parts = append(out.Parts, r)
	}
	return plan
}

// publishPart carries out one part's action.
func (s *Service) publishPart(ctx context.Context, scope bus.Request, docs docWriterV2, in PublishInput, src docpublish.Source, parts []docpublish.Part, plan partPlan, generation int, r *PublishPartResult) {
	fail := func(outcome, note string) { r.Outcome, r.Note = outcome, note }
	part := parts[r.Part-1]
	n := len(parts)
	// The CAS expects whatever row the part has, current or left over.
	expect := plan.row.token
	switch r.Action {
	case "settle":
		if e := s.publishPut(ctx, scope, in, src, r.Part, n, plan.row.generation, "published", plan.row.token, expect, plan.found.ID, contentDigest(plan.found.Content)); e != nil {
			fail("failed", "found the document but could not record it: "+e.Error())
			return
		}
		r.Outcome = "settled"
	case "create":
		token, e := docpublish.NewToken()
		if e != nil {
			fail("failed", e.Error())
			return
		}
		// Reserve before sending: a create whose answer is lost is found
		// again by its token, never sent twice blindly.
		if e = s.publishPut(ctx, scope, in, src, r.Part, n, generation, "pending", token, expect, "", ""); e != nil {
			fail("failed", "nothing was sent: "+e.Error())
			return
		}
		created, e := docs.CreateDocument(ctx, in.ParentKind, in.ParentID, part.Title, part.Body+docpublish.Footer(src, token))
		if e != nil {
			fail("uncertain", "the create may or may not have happened; publish again to find or redo it")
			return
		}
		r.DocumentID = created.ID
		s.publishReadBack(ctx, scope, docs, in, src, r.Part, n, generation, token, created.ID, r, "created")
	case "update":
		// Read again right before the write: an edit since the plan
		// refuses it (LD-L1). An edit landing between this read and the
		// write is overwritten; Linear has no compare-and-swap.
		current, e := docs.GetDocument(ctx, plan.row.document)
		if e != nil || current.Archived || contentDigest(current.Content) != plan.row.digest {
			fail("drifted", "the document changed in Linear just now; nothing was written to it")
			return
		}
		if _, e = docs.UpdateDocumentContent(ctx, plan.row.document, part.Body+docpublish.Footer(src, plan.row.token)); e != nil {
			fail("uncertain", "the update may or may not have happened; the next publish will treat the document as edited")
			return
		}
		s.publishReadBack(ctx, scope, docs, in, src, r.Part, n, generation, plan.row.token, plan.row.document, r, "updated")
	}
}

// publishReadBack reads a written document and records the digest of the
// content as Linear returned it.
func (s *Service) publishReadBack(ctx context.Context, scope bus.Request, docs docWriterV2, in PublishInput, src docpublish.Source, part, parts, generation int, token, id string, r *PublishPartResult, outcome string) {
	d, e := docs.GetDocument(ctx, id)
	if e != nil || !docpublish.HasToken(d.Content, token) {
		r.Outcome, r.Note = "uncertain", "written, but the read-back failed; publish again to record it"
		return
	}
	if e = s.publishPut(ctx, scope, in, src, part, parts, generation, "published", token, token, id, contentDigest(d.Content)); e != nil {
		r.Outcome, r.Note = "uncertain", "written, but not recorded: "+e.Error()+"; publish again"
		return
	}
	r.Outcome = outcome
}

// docPolicyFull is the document policy as an owner or lead reads it.
type docPolicyFull struct {
	SourceRoot   string               `json:"source_root"`
	DenyPatterns []bus.DocDenyPattern `json:"deny_patterns"`
	Initiatives  []string             `json:"initiatives"`
}

func (s *Service) publishPolicy(ctx context.Context, scope bus.Request) (docPolicyFull, error) {
	data, e := s.DocPolicyV2(ctx, scope)
	if e != nil {
		return docPolicyFull{}, e
	}
	raw, _ := json.Marshal(data)
	var p docPolicyFull
	if json.Unmarshal(raw, &p) != nil {
		return p, errors.New("the document policy is unreadable")
	}
	return p, nil
}

func (s *Service) publishRows(ctx context.Context, scope bus.Request, kind, parent, path string) ([]publishRow, error) {
	scope.Operation = "doc_publication_get"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["parent_kind"], scope.Args["parent_id"], scope.Args["source_path"] = kind, parent, path
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	if r.Source != "server" && r.Source != "local-authority" {
		return nil, errors.New("the published documents require authoritative settings")
	}
	var out []publishRow
	for _, m := range anyRows(r.Data) {
		row := publishRow{part: number(m["part"]), parts: number(m["parts"]), generation: number(m["generation"]), age: -1}
		if _, ok := m["age_seconds"]; ok {
			row.age = number(m["age_seconds"])
		}
		for dst, k := range map[*string]string{&row.state: "state", &row.token: "publish_token", &row.document: "document_id", &row.digest: "published_sha256", &row.sourceSHA: "source_sha256"} {
			*dst, _ = m[k].(string)
		}
		if row.part < 1 || row.parts < 1 {
			return nil, errors.New("invalid published document record")
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Service) publishPut(ctx context.Context, scope bus.Request, in PublishInput, src docpublish.Source, part, parts, generation int, state, token, expect, document, digest string) error {
	scope.Operation = "doc_publication_put"
	scope.Args = instanceArgs(scope.Args)
	for k, v := range map[string]any{"parent_kind": in.ParentKind, "parent_id": in.ParentID, "source_path": src.Path, "part_number": part, "part_count": parts, "generation": generation, "state": state, "publish_token": token, "expect_token": expect, "source_commit": src.Commit, "source_sha256": src.SHA256} {
		scope.Args[k] = v
	}
	if document != "" {
		scope.Args["document_id"], scope.Args["published_sha256"] = document, digest
	}
	_, e := s.cfg.Executor.Execute(ctx, scope)
	return e
}

func (s *Service) publishOverride(ctx context.Context, scope bus.Request, in PublishInput, src docpublish.Source, hits []PublishHit) error {
	scope.Operation = "doc_publish_override"
	scope.Args = instanceArgs(scope.Args)
	lines := []int{}
	for _, h := range hits {
		if h.Line > 0 {
			lines = append(lines, h.Line)
		}
	}
	for k, v := range map[string]any{"parent_kind": in.ParentKind, "parent_id": in.ParentID, "source_path": src.Path, "operation_id": in.OperationID, "pattern_ids": in.AllowPatterns, "lines": lines, "source_commit": src.Commit} {
		scope.Args[k] = v
	}
	_, e := s.cfg.Executor.Execute(ctx, scope)
	return e
}

// openDocScopeFor opens the document scope, with a write connection when
// the publish is confirmed.
func (s *Service) openDocScopeFor(ctx context.Context, scope bus.Request, write bool) (*docScope, func(), error) {
	if !write {
		return s.openDocScope(ctx, scope)
	}
	return s.openDocScopeWith(ctx, scope, func(ctx context.Context) (linearconn.Profile, Provider, func(), error) {
		profile, p, done, e := s.openForWrite(ctx)
		if e == nil && profile.Access != linearconn.Write {
			done()
			return profile, nil, nil, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
		}
		return profile, p, done, e
	})
}
