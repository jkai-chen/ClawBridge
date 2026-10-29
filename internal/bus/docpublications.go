package bus

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The publish ledger (live/LINEAR-DOCS-DESIGN.md §2.3): one row per
// (project, parent, source path, part), in linear_doc_publications
// (schema 16). A row is pending from just before a create until the
// document is read back; published_sha256 is the digest of the content as
// Linear returned it, so an untouched document can be told from one a
// person edited.

const maxDocParts = 20

var publishOperationID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func docPublicationOperation(op string) bool {
	return op == "doc_publication_get" || op == "doc_publication_put" || op == "doc_publish_override"
}

// generatedSourcePrefix starts the ledger key of a document ClawBridge
// generates rather than publishes from git (team roles, design §13): a git
// publish never addresses one, and its rows have their own ops.
const generatedSourcePrefix = "generated:"

// docSourcePath checks a repo-relative path as the ledger keys it.
func docSourcePath(p string) error {
	if strings.HasPrefix(p, generatedSourcePrefix) {
		return invalid("source_path " + generatedSourcePrefix + "... is a generated document's; it is never published from git")
	}
	if p == "" || len(p) > 1024 || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\n\r\\") || strings.HasPrefix(p, "/") {
		return invalid("source_path is a repository-relative path with forward slashes")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return invalid("source_path is a repository-relative path with forward slashes")
		}
	}
	return nil
}

// docPublicationOp reads and records published documents: a project lead
// or owner only, with or without a session, for a parent the project may
// publish to. The deny-scan override is the owner's alone.
func (s *Store) docPublicationOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project lead or owner permission required")
	}
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	wid, pid, a := w["id"].(string), p["id"].(string), r.Args
	kind, parent, path := str(a, "parent_kind"), str(a, "parent_id"), str(a, "source_path")
	if err := s.docPublishParent(ctx, tx, wid, pid, kind, parent); err != nil {
		return nil, err
	}
	if err := docSourcePath(path); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "doc_publication_get":
		rows, err := query(ctx, tx, `SELECT part,parts,generation,state,publish_token,document_id,published_sha256,source_commit,source_sha256,updated_at FROM linear_doc_publications WHERE project_id=? AND parent_kind=? AND parent_id=? AND source_path=? ORDER BY part`, pid, kind, parent, path)
		if err != nil {
			return nil, err
		}
		// A row's age by the authority's own clock (as doc seeds).
		now, err := dbStamp(ctx, tx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			age := -1
			if updated, e := time.Parse(timestampLayout, str(row, "updated_at")); e == nil {
				if current, e := time.Parse(timestampLayout, now); e == nil && !current.Before(updated) {
					age = int(current.Sub(updated) / time.Second)
				}
			}
			row["age_seconds"] = age
		}
		return rows, nil
	case "doc_publish_override":
		if projectRole != "owner" {
			return nil, errors.New("project owner permission required to publish over a deny pattern")
		}
		op := str(a, "operation_id")
		if !publishOperationID.MatchString(op) {
			return nil, invalid("doc_publish_override requires the publish's operation_id")
		}
		ids, isArray, ok := stringList(a["pattern_ids"])
		if !isArray || !ok || len(ids) == 0 || len(ids) > maxDocDenyPatterns {
			return nil, invalid("doc_publish_override names the overridden pattern IDs")
		}
		for _, id := range ids {
			if !docPatternID.MatchString(id) {
				return nil, invalid("doc_publish_override names the overridden pattern IDs")
			}
		}
		lines, ok := intList(a["lines"], 1000)
		if !ok {
			return nil, invalid("doc_publish_override lists the matched line numbers")
		}
		commit := str(a, "source_commit")
		if !commitIDPattern.MatchString(commit) {
			return nil, invalid("doc_publish_override names the source commit")
		}
		details := map[string]any{"operation_id": op, "pattern_ids": ids, "lines": lines, "source_commit": commit, "source_path": path, "parent_kind": kind, "parent_id": parent}
		if err := s.audit(ctx, tx, wid, pid, "", "linear_doc_publish_override", op, "allow", details); err != nil {
			return nil, err
		}
		return map[string]any{"operation_id": op, "recorded": true}, nil
	}
	part, err1 := integer(a["part_number"])
	parts, err2 := integer(a["part_count"])
	generation, err3 := integer(a["generation"])
	state, token, expect := str(a, "state"), str(a, "publish_token"), str(a, "expect_token")
	if err1 != nil || err2 != nil || err3 != nil || parts < 1 || parts > maxDocParts || part < 1 || part > parts || generation < 1 {
		return nil, invalid("doc_publication_put requires part_number 1..part_count (at most 20) and a generation")
	}
	if (state != "pending" && state != "published") || !seedTokenPattern.MatchString(token) || (expect != "" && !seedTokenPattern.MatchString(expect)) {
		return nil, invalid("doc_publication_put requires state pending or published, a 12-hex publish_token, and expect_token as the row's token or empty")
	}
	document, digest := str(a, "document_id"), str(a, "published_sha256")
	commit, source := str(a, "source_commit"), str(a, "source_sha256")
	if !commitIDPattern.MatchString(commit) || !sha256Pattern.MatchString(source) {
		return nil, invalid("doc_publication_put names the source commit and the source digest")
	}
	if state == "published" && (!issueUUIDPattern.MatchString(document) || !sha256Pattern.MatchString(digest)) {
		return nil, invalid("a published row names its document UUID and the read-back content digest")
	}
	if state == "pending" && (document != "" || digest != "") {
		return nil, invalid("a pending row has no document yet")
	}
	// A generated document is never recorded as a git publish (T4).
	if document != "" {
		if held, e := generatedDocument(ctx, tx, pid, document); e != nil || held {
			if e != nil {
				return nil, e
			}
			return nil, invalid("that document is generated by ClawBridge; a git publish never writes it")
		}
	}
	// Compare-and-swap on the part's token: a second run that read the
	// same row cannot overwrite the first one's record.
	rows, err := query(ctx, tx, `SELECT publish_token FROM linear_doc_publications WHERE project_id=? AND parent_kind=? AND parent_id=? AND source_path=? AND part=?`, pid, kind, parent, path, part)
	if err != nil {
		return nil, err
	}
	current := ""
	if len(rows) == 1 {
		current = str(rows[0], "publish_token")
	}
	if current != expect {
		return nil, conflict("the published document record changed; run the publish again")
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_doc_publications(project_id,parent_kind,parent_id,source_path,part,parts,generation,state,publish_token,document_id,published_sha256,source_commit,source_sha256,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, pid, kind, parent, path, part, parts, generation, state, token, document, digest, commit, source, now, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE linear_doc_publications SET parts=?,generation=?,state=?,publish_token=?,document_id=?,published_sha256=?,source_commit=?,source_sha256=?,updated_at=? WHERE project_id=? AND parent_kind=? AND parent_id=? AND source_path=? AND part=?`, parts, generation, state, token, document, digest, commit, source, now, pid, kind, parent, path, part)
	}
	if err != nil {
		return nil, err
	}
	details := map[string]any{"parent_kind": kind, "parent_id": parent, "source_path": path, "part": part, "parts": parts, "generation": generation, "source_commit": commit, "publish_token": token}
	if document != "" {
		details["document_id"] = document
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_doc_publication", kind+":"+parent+":"+path+"#"+strconv.Itoa(part), state, details); err != nil {
		return nil, err
	}
	return map[string]any{"part": part, "state": state, "publish_token": token, "document_id": document}, nil
}

var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// docPublishParent checks that the project may publish to the parent: a
// bound team, a bound project (on a team board, any project; the board
// proves it belongs to a bound team), or an initiative the document policy
// lists.
func (s *Store) docPublishParent(ctx context.Context, tx *transaction, wid, pid, kind, parent string) error {
	if !issueUUIDPattern.MatchString(parent) {
		return invalid("a publication names a parent_kind (team, project or initiative) and a parent_id UUID")
	}
	if kind == "initiative" {
		policy, _, err := readDocPolicy(ctx, tx, pid)
		if err != nil {
			return err
		}
		if !containsString(policy.Initiatives, parent) {
			return invalid("that initiative is not listed in the project's document policy")
		}
		return nil
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
	switch {
	case kind == "team" && containsString(b.LinearTeamIDs, parent):
	case kind == "project" && (b.Mode == "team_board" || containsString(b.LinearProjectIDs, parent)):
	case kind != "team" && kind != "project":
		return invalid("a publication names a parent_kind (team, project or initiative) and a parent_id UUID")
	default:
		return invalid("that parent is not bound to this project's task board")
	}
	return nil
}

func intList(v any, max int) ([]int, bool) {
	var out []int
	switch t := v.(type) {
	case []int:
		out = t
	case []any:
		for _, x := range t {
			n, err := integer(x)
			if err != nil {
				return nil, false
			}
			out = append(out, n)
		}
	default:
		return nil, false
	}
	for _, n := range out {
		if n < 1 {
			return nil, false
		}
	}
	return out, len(out) <= max
}
