package bus

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The generated "Team roles" document (live/TEAM-ROLE-MAPPING-DESIGN.md
// §4, §13): one ledger row in linear_doc_publications under the reserved
// source path, whatever its parent. The mapping is its only source, so a
// publish overwrites an edit (§4's accepted exception, T4), but only of
// the document this row names: never one found by title, never a git
// publish's.

// teamRolesDocSource is the one generated source path team roles use.
const teamRolesDocSource = generatedSourcePrefix + "team_roles"

// teamRolesDocGet is the row, if any: its parent, state, token, document,
// digests, the mapping revision it was rendered from, and its age by the
// authority's clock; and the mapping's current revision.
func (s *Store) teamRolesDocGet(ctx context.Context, tx *transaction, pid string) (any, error) {
	_, revision, err := readTeamRoles(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"revision": revision, "published": nil}
	rows, err := query(ctx, tx, `SELECT parent_kind,parent_id,state,publish_token,document_id,published_sha256,source_commit,source_sha256,updated_at FROM linear_doc_publications WHERE project_id=? AND source_path=?`, pid, teamRolesDocSource)
	if err != nil || len(rows) == 0 {
		return out, err
	}
	row := rows[0]
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	age := -1
	if updated, e := time.Parse(timestampLayout, str(row, "updated_at")); e == nil {
		if current, e := time.Parse(timestampLayout, now); e == nil && !current.Before(updated) {
			age = int(current.Sub(updated) / time.Second)
		}
	}
	rendered, replaced := teamRolesDocMarker(str(row, "source_commit"))
	out["published"] = map[string]any{"parent_kind": row["parent_kind"], "parent_id": row["parent_id"], "state": row["state"], "publish_token": row["publish_token"], "document_id": row["document_id"], "published_sha256": row["published_sha256"], "source_sha256": row["source_sha256"], "revision": rendered, "replaced_document_id": replaced, "age_seconds": age}
	return out, nil
}

// teamRolesDocMarker reads the row's source_commit, which for a generated
// document holds "team_roles:r<revision>", and after a --recreate the
// document it replaced (";replaced:<uuid>"), kept until the next publish
// so doctor can name it for archiving by hand (review V2).
func teamRolesDocMarker(v string) (int, string) {
	head, replaced, _ := strings.Cut(v, ";replaced:")
	n, err := strconv.Atoi(strings.TrimPrefix(head, "team_roles:r"))
	if err != nil || !strings.HasPrefix(head, "team_roles:r") {
		n = -1
	}
	return n, replaced
}

// teamRolesDocPut records one step of a publish, with the same token
// compare-and-swap as doc_publication_put. Moving the document to another
// parent replaces the row, so there is only ever one.
func (s *Store) teamRolesDocPut(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	if str(a, "source_path") != teamRolesDocSource {
		return nil, invalid("team_roles_doc_put records only " + teamRolesDocSource)
	}
	kind, parent := str(a, "parent_kind"), str(a, "parent_id")
	if kind != "team" && kind != "project" {
		return nil, invalid("the team roles document's parent is a bound team or project")
	}
	if err := s.docPublishParent(ctx, tx, wid, pid, kind, parent); err != nil {
		return nil, err
	}
	state, token, expect := str(a, "state"), str(a, "publish_token"), str(a, "expect_token")
	if (state != "pending" && state != "published") || !seedTokenPattern.MatchString(token) || (expect != "" && !seedTokenPattern.MatchString(expect)) {
		return nil, invalid("team_roles_doc_put requires state pending or published, a 12-hex publish_token, and expect_token as the row's token or empty")
	}
	revision, err := integer(a["mapping_revision"])
	if err != nil || revision < 1 {
		return nil, invalid("team_roles_doc_put names the mapping revision it rendered")
	}
	document, digest, source, replaced := str(a, "document_id"), str(a, "published_sha256"), str(a, "source_sha256"), str(a, "replaced_document_id")
	if !sha256Pattern.MatchString(source) {
		return nil, invalid("team_roles_doc_put names the rendered content's digest")
	}
	if state == "published" && (!issueUUIDPattern.MatchString(document) || !sha256Pattern.MatchString(digest)) {
		return nil, invalid("a published row names its document UUID and the read-back content digest")
	}
	if state == "pending" && (document != "" || digest != "") {
		return nil, invalid("a pending row has no document yet")
	}
	if replaced != "" && !issueUUIDPattern.MatchString(replaced) {
		return nil, invalid("replaced_document_id is a document UUID")
	}
	// T4, the other way: a git publish's document is never overwritten
	// as the generated one.
	if document != "" {
		held, e := one(ctx, tx, `SELECT count(*) AS n FROM linear_doc_publications WHERE project_id=? AND document_id=? AND source_path<>?`, pid, document, teamRolesDocSource)
		if e != nil {
			return nil, e
		}
		if asInt(held["n"]) > 0 {
			return nil, invalid("that document is published from git; the team roles document never writes it")
		}
	}
	rows, err := query(ctx, tx, `SELECT publish_token FROM linear_doc_publications WHERE project_id=? AND source_path=?`, pid, teamRolesDocSource)
	if err != nil {
		return nil, err
	}
	current := ""
	if len(rows) > 0 {
		current = str(rows[0], "publish_token")
	}
	if current != expect {
		return nil, conflict("the team roles document's record changed; publish again")
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	marker := fmt.Sprintf("team_roles:r%d", revision)
	if replaced != "" {
		marker += ";replaced:" + replaced
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM linear_doc_publications WHERE project_id=? AND source_path=?`, pid, teamRolesDocSource); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO linear_doc_publications(project_id,parent_kind,parent_id,source_path,part,parts,generation,state,publish_token,document_id,published_sha256,source_commit,source_sha256,created_at,updated_at) VALUES(?,?,?,?,1,1,1,?,?,?,?,?,?,?,?)`, pid, kind, parent, teamRolesDocSource, state, token, document, digest, marker, source, now, now); err != nil {
		return nil, err
	}
	// The document ID names no one; the content's names never reach history.
	details := map[string]any{"parent_kind": kind, "parent_id": parent, "revision": revision, "publish_token": token}
	if document != "" {
		details["document_id"] = document
	}
	if err = s.audit(ctx, tx, wid, pid, "", "team_roles_doc", pid, state, details); err != nil {
		return nil, err
	}
	return map[string]any{"state": state, "publish_token": token, "document_id": document, "revision": revision}, nil
}

// generatedDocument is whether a generated row holds the document.
func generatedDocument(ctx context.Context, tx *transaction, pid, document string) (bool, error) {
	row, err := one(ctx, tx, `SELECT count(*) AS n FROM linear_doc_publications WHERE project_id=? AND document_id=? AND source_path LIKE ?`, pid, document, generatedSourcePrefix+"%")
	if err != nil {
		return false, err
	}
	return asInt(row["n"]) > 0, nil
}
