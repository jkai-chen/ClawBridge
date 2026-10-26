package bus

import (
	"context"
	"errors"
	"regexp"
)

// Schema 13: the Linear guide documents ClawBridge seeded (live/DOC-SEED-
// DESIGN.md). One row per parent (the binding's default team, or its one
// project) and template, shared by every ClawBridge project bound there. A
// row is pending from just before the create until the document is read
// back; seeded_sha256 is the digest of the content as Linear returned it,
// so an untouched document can be told from one a person edited. The same
// DDL serves SQLite and PostgreSQL.
const docSeedSchemaV13 = `
CREATE TABLE linear_doc_seeds(workspace_id TEXT NOT NULL REFERENCES workspaces(id),parent_kind TEXT NOT NULL CHECK(parent_kind IN ('team','project')),parent_id TEXT NOT NULL,template TEXT NOT NULL,template_version INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','seeded')),seed_token TEXT NOT NULL,document_id TEXT NOT NULL DEFAULT '',seeded_sha256 TEXT NOT NULL DEFAULT '',project_id TEXT NOT NULL REFERENCES projects(id),updated_by TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(workspace_id,parent_kind,parent_id,template));
`

var (
	docSeedTemplates = map[string]bool{"developer": true, "reviewer": true}
	seedTokenPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

func docSeedOperation(op string) bool { return op == "doc_seed_get" || op == "doc_seed_put" }

// docSeedOp reads and records seeded documents: a project lead or owner
// only, with or without a session (linear link and linear seed-docs are
// CLI commands), and only for a parent in the project's own binding.
func (s *Store) docSeedOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project lead or owner permission required")
	}
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	wid, pid, a := w["id"].(string), p["id"].(string), r.Args
	kind, parent := str(a, "parent_kind"), str(a, "parent_id")
	if err := s.docSeedParentBound(ctx, tx, wid, pid, kind, parent); err != nil {
		return nil, err
	}
	if r.Operation == "doc_seed_get" {
		return query(ctx, tx, `SELECT template,template_version,state,seed_token,document_id,seeded_sha256,project_id,updated_by,updated_at FROM linear_doc_seeds WHERE workspace_id=? AND parent_kind=? AND parent_id=? ORDER BY template`, wid, kind, parent)
	}
	template, state, token := str(a, "template"), str(a, "state"), str(a, "seed_token")
	version, err := integer(a["template_version"])
	if !docSeedTemplates[template] || (state != "pending" && state != "seeded") || !seedTokenPattern.MatchString(token) || err != nil || version < 1 || version > 1000 {
		return nil, invalid("doc_seed_put requires a known template, state pending or seeded, a 12-hex seed_token and a template_version")
	}
	expect := str(a, "expect_token")
	if expect != "" && !seedTokenPattern.MatchString(expect) {
		return nil, invalid("expect_token must be the row's current seed_token, or empty for no row")
	}
	document, digest := str(a, "document_id"), str(a, "seeded_sha256")
	if state == "seeded" && (!issueUUIDPattern.MatchString(document) || !sha256Pattern.MatchString(digest)) {
		return nil, invalid("a seeded row names its document UUID and the read-back content digest")
	}
	if state == "pending" && (document != "" || digest != "") {
		return nil, invalid("a pending row has no document yet")
	}
	// Compare-and-swap on the seed token: a second run that read the same
	// row cannot overwrite the first one's record.
	rows, err := query(ctx, tx, `SELECT seed_token FROM linear_doc_seeds WHERE workspace_id=? AND parent_kind=? AND parent_id=? AND template=?`, wid, kind, parent, template)
	if err != nil {
		return nil, err
	}
	current := ""
	if len(rows) == 1 {
		current = str(rows[0], "seed_token")
	}
	if current != expect {
		return nil, conflict("the seeded document record changed; run seed-docs again")
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_doc_seeds(workspace_id,parent_kind,parent_id,template,template_version,state,seed_token,document_id,seeded_sha256,project_id,updated_by,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, wid, kind, parent, template, version, state, token, document, digest, pid, s.principal, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE linear_doc_seeds SET template_version=?,state=?,seed_token=?,document_id=?,seeded_sha256=?,project_id=?,updated_by=?,updated_at=? WHERE workspace_id=? AND parent_kind=? AND parent_id=? AND template=?`, version, state, token, document, digest, pid, s.principal, now, wid, kind, parent, template)
	}
	if err != nil {
		return nil, err
	}
	details := map[string]any{"parent_kind": kind, "parent_id": parent, "template": template, "template_version": version, "seed_token": token}
	if document != "" {
		details["document_id"] = document
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_doc_seed", kind+":"+parent+":"+template, state, details); err != nil {
		return nil, err
	}
	return map[string]any{"template": template, "state": state, "seed_token": token, "document_id": document}, nil
}

// DocSeedParent is where a binding's guide documents live: its one project
// for a one-project set, otherwise its default team (owner decision 2).
func DocSeedParent(b TaskboardBindingV2) (kind, id string) {
	if b.Mode == "project_set" && len(b.LinearProjectIDs) == 1 {
		return "project", b.LinearProjectIDs[0]
	}
	return "team", b.LinearDefaultTeamID
}

// docSeedParentBound checks that the parent is where the project's binding
// seeds its documents.
func (s *Store) docSeedParentBound(ctx context.Context, tx *transaction, wid, pid, kind, parent string) error {
	if (kind != "team" && kind != "project") || !issueUUIDPattern.MatchString(parent) {
		return invalid("doc seeds name a parent_kind (team or project) and a parent_id UUID")
	}
	raw, err := s.taskboard(ctx, tx, wid, pid, "", "taskboard_get_v2", map[string]any{})
	if err != nil {
		return err
	}
	b, err := ParseTaskboardBindingV2(raw)
	if err != nil {
		return err
	}
	if !b.Configured {
		return invalid("the project has no task board binding")
	}
	if k, id := DocSeedParent(b); k != kind || id != parent {
		return invalid("that parent is not where this project's binding seeds its documents")
	}
	return nil
}
