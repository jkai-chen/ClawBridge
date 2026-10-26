package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/docseed"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Seeding the Linear guide documents (live/DOC-SEED-DESIGN.md r1.1). A
// project lead or owner seeds them from the CLI, as their own Linear
// account, under the binding's parent. ClawBridge never overwrites a
// document a person edited, never recreates a vanished one without
// --reseed, and never creates one twice for an uncertain create.

// docProvider is a provider with the document tools.
type docProvider interface {
	DocumentReason() string
	CreateDocument(ctx context.Context, kind, parent, title, content string) (linearprovider.Document, error)
	UpdateDocumentContent(ctx context.Context, id, content string) (linearprovider.Document, error)
	GetDocument(ctx context.Context, id string) (linearprovider.Document, error)
	DocumentsUnder(ctx context.Context, kind, parent string) ([]linearprovider.Document, bool, error)
}

// seedPendingFor is how long an uncertain create may still appear in the
// parent's list before it is taken as not made.
const seedPendingFor = 10 * time.Minute

// SeedInput says what seed-docs may do. Without Confirm it only reports.
type SeedInput struct {
	Confirm bool
	// Update rewrites an untouched document whose template is newer.
	Update bool
	// Reseed creates a document again when the seeded one is archived or
	// cannot be read.
	Reseed bool
	// ServiceURL and Version fill the templates.
	ServiceURL, Version string
}

// SeedResult is what seed-docs found and, with Confirm, did.
type SeedResult struct {
	ParentKind string    `json:"parent_kind"`
	ParentID   string    `json:"parent_id"`
	Documents  []SeedDoc `json:"documents"`
	Confirmed  bool      `json:"confirmed"`
}

// SeedDoc is one guide document. Action is what seed-docs does (or would
// do with --confirm): create, settle, update, none, update_available,
// edited, gone, unreadable or pending_recent. Outcome is what a confirmed
// run did: created, settled, updated, uncertain or failed.
type SeedDoc struct {
	Template        string `json:"template"`
	Title           string `json:"title"`
	TemplateVersion int    `json:"template_version"`
	Action          string `json:"action"`
	Outcome         string `json:"outcome,omitempty"`
	DocumentID      string `json:"document_id,omitempty"`
	Note            string `json:"note,omitempty"`
}

type seedRow struct {
	state, token, document, digest, updatedAt string
	version                                   int
}

// SeedDocs plans the guide documents for the project's binding and, with
// in.Confirm, creates, settles or updates them.
func (s *Service) SeedDocs(ctx context.Context, scope bus.Request, in SeedInput) (SeedResult, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return SeedResult{}, e
	}
	if !b.Configured {
		return SeedResult{}, errors.New("task-board is not linked")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return SeedResult{}, e
	}
	if access.ProjectRole != "owner" && access.ProjectRole != "lead" {
		return SeedResult{}, errors.New("seeding the guide documents is for a project lead or owner")
	}
	kind, parent := bus.DocSeedParent(b)
	result := SeedResult{ParentKind: kind, ParentID: parent}
	rows, e := s.seedRows(ctx, scope, kind, parent)
	if e != nil {
		return SeedResult{}, e
	}
	var docs docProvider
	if len(rows) > 0 || in.Confirm {
		open := s.open
		if in.Confirm {
			open = s.openForWrite
		}
		profile, p, done, e := open(ctx)
		if e != nil {
			return SeedResult{}, e
		}
		defer done()
		if in.Confirm && profile.Access != linearconn.Write {
			return SeedResult{}, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
		}
		d, ok := p.(docProvider)
		if !ok {
			return SeedResult{}, errors.New("native provider lacks the document tools")
		}
		if reason := d.DocumentReason(); reason != "" {
			return SeedResult{}, errors.New("Linear document writes are unavailable: " + reason)
		}
		if in.Confirm {
			if _, e = verifyBindingV2(ctx, p, b); e != nil {
				return SeedResult{}, e
			}
		}
		docs = d
	}
	listed, listErr := []linearprovider.Document(nil), error(nil)
	listOnce := func() ([]linearprovider.Document, error) {
		if listed == nil && listErr == nil {
			var complete bool
			listed, complete, listErr = docs.DocumentsUnder(ctx, kind, parent)
			if listErr == nil && !complete {
				listErr = errors.New("the parent has more documents than one walk reads")
			}
			if listed == nil {
				listed = []linearprovider.Document{}
			}
		}
		return listed, listErr
	}
	now := time.Now
	if s.cfg.Now != nil {
		now = s.cfg.Now
	}
	for _, t := range docseed.Templates {
		doc := SeedDoc{Template: t.Name, Title: t.Title, TemplateVersion: t.Version}
		row, seen := rows[t.Name]
		var found linearprovider.Document
		switch {
		case !seen:
			doc.Action = "create"
		case row.state == "pending":
			list, e := listOnce()
			if e != nil {
				doc.Action, doc.Note = "unreadable", "could not list the parent's documents; nothing written"
				break
			}
			for _, d := range list {
				if docseed.HasToken(d.Content, row.token) {
					found = d
				}
			}
			at, _ := time.Parse(time.RFC3339Nano, row.updatedAt)
			switch {
			case found.ID != "":
				doc.Action, doc.DocumentID = "settle", found.ID
			case now().Sub(at) < seedPendingFor:
				doc.Action, doc.Note = "pending_recent", "an earlier create may still appear; run seed-docs again in a few minutes"
			default:
				doc.Action, doc.Note = "create", "an earlier create did not appear; it is created again"
			}
		default:
			doc.DocumentID = row.document
			d, e := docs.GetDocument(ctx, row.document)
			switch {
			case e != nil && in.Reseed:
				doc.Action, doc.Note = "create", "the seeded document could not be read; --reseed creates it again"
			case e != nil:
				doc.Action, doc.Note = "unreadable", "could not read the seeded document; nothing written. If it was deleted, run seed-docs --reseed"
			case d.Archived && in.Reseed:
				doc.Action, doc.Note = "create", "the seeded document is archived; --reseed creates it again"
			case d.Archived:
				doc.Action, doc.Note = "gone", "the seeded document is archived; run seed-docs --reseed to create it again"
			case contentDigest(d.Content) != row.digest:
				doc.Action, doc.Note = "edited", "edited by your team; left as is"
			case row.version < t.Version && in.Update:
				doc.Action = "update"
			case row.version < t.Version:
				doc.Action, doc.Note = "update_available", fmt.Sprintf("template %d is newer; run seed-docs --update to replace the untouched document", t.Version)
			default:
				doc.Action = "none"
			}
		}
		if in.Confirm {
			s.seedOne(ctx, scope, docs, kind, parent, t, row, seen, found, &doc, in)
		}
		result.Documents = append(result.Documents, doc)
	}
	result.Confirmed = in.Confirm
	return result, nil
}

// seedOne carries out one document's action.
func (s *Service) seedOne(ctx context.Context, scope bus.Request, docs docProvider, kind, parent string, t docseed.Template, row seedRow, seen bool, found linearprovider.Document, doc *SeedDoc, in SeedInput) {
	fail := func(outcome, note string) { doc.Outcome, doc.Note = outcome, note }
	switch doc.Action {
	case "settle":
		if e := s.seedPut(ctx, scope, kind, parent, t.Name, row.version, "seeded", row.token, row.token, found.ID, contentDigest(found.Content)); e != nil {
			fail("failed", "found the document but could not record it: "+e.Error())
			return
		}
		doc.Outcome = "settled"
	case "create":
		token, e := docseed.NewToken()
		if e != nil {
			fail("failed", e.Error())
			return
		}
		content, e := docseed.Render(t, docseed.Values{ServiceURL: in.ServiceURL, Version: in.Version}, token)
		if e == nil {
			e = mentionGuard(&content)
		}
		if e != nil {
			fail("failed", "the template did not render: "+e.Error())
			return
		}
		// Reserve before sending: a create whose answer is lost is found
		// again by its token, never sent twice blindly.
		if e = s.seedPut(ctx, scope, kind, parent, t.Name, t.Version, "pending", token, row.token, "", ""); e != nil {
			fail("failed", "nothing was sent: "+e.Error())
			return
		}
		created, e := docs.CreateDocument(ctx, kind, parent, t.Title, content)
		if e != nil {
			fail("uncertain", "the create may or may not have happened; run seed-docs again to find or redo it")
			return
		}
		doc.DocumentID = created.ID
		s.seedReadBack(ctx, scope, docs, kind, parent, t, token, created.ID, doc, "created")
	case "update":
		content, e := docseed.Render(t, docseed.Values{ServiceURL: in.ServiceURL, Version: in.Version}, row.token)
		if e == nil {
			e = mentionGuard(&content)
		}
		if e != nil {
			fail("failed", "the template did not render: "+e.Error())
			return
		}
		if _, e = docs.UpdateDocumentContent(ctx, row.document, content); e != nil {
			fail("uncertain", "the update may or may not have happened; seed-docs will treat the document as edited and leave it")
			return
		}
		s.seedReadBack(ctx, scope, docs, kind, parent, t, row.token, row.document, doc, "updated")
	}
}

// seedReadBack reads a written document and records the digest of the
// content as Linear returned it.
func (s *Service) seedReadBack(ctx context.Context, scope bus.Request, docs docProvider, kind, parent string, t docseed.Template, token, id string, doc *SeedDoc, outcome string) {
	d, e := docs.GetDocument(ctx, id)
	if e != nil || !docseed.HasToken(d.Content, token) {
		doc.Outcome, doc.Note = "uncertain", "written, but the read-back failed; run seed-docs again to record it"
		return
	}
	if e = s.seedPut(ctx, scope, kind, parent, t.Name, t.Version, "seeded", token, token, id, contentDigest(d.Content)); e != nil {
		doc.Outcome, doc.Note = "uncertain", "written, but not recorded: "+e.Error()+"; run seed-docs again"
		return
	}
	doc.Outcome = outcome
}

func (s *Service) seedRows(ctx context.Context, scope bus.Request, kind, parent string) (map[string]seedRow, error) {
	scope.Operation = "doc_seed_get"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["parent_kind"], scope.Args["parent_id"] = kind, parent
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	if r.Source != "server" && r.Source != "local-authority" {
		return nil, errors.New("the seeded documents require authoritative settings")
	}
	out := map[string]seedRow{}
	for _, m := range anyRows(r.Data) {
		name, _ := m["template"].(string)
		version := number(m["template_version"])
		if name == "" || version < 1 {
			return nil, errors.New("invalid seeded document record")
		}
		row := seedRow{version: version}
		for dst, k := range map[*string]string{&row.state: "state", &row.token: "seed_token", &row.document: "document_id", &row.digest: "seeded_sha256", &row.updatedAt: "updated_at"} {
			*dst, _ = m[k].(string)
		}
		out[name] = row
	}
	return out, nil
}

// anyRows reads a list of records from a local or a decoded server answer.
func anyRows(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func (s *Service) seedPut(ctx context.Context, scope bus.Request, kind, parent, template string, version int, state, token, expect, document, digest string) error {
	scope.Operation = "doc_seed_put"
	scope.Args = instanceArgs(scope.Args)
	for k, v := range map[string]any{"parent_kind": kind, "parent_id": parent, "template": template, "template_version": version, "state": state, "seed_token": token, "expect_token": expect} {
		scope.Args[k] = v
	}
	if document != "" {
		scope.Args["document_id"], scope.Args["seeded_sha256"] = document, digest
	}
	_, e := s.cfg.Executor.Execute(ctx, scope)
	return e
}

func contentDigest(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}
