package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Schema 16: the project owner's document policy (live/LINEAR-DOCS-DESIGN.md
// §2.1): the source root a publish reads from, the deny patterns it scans
// for, and the initiatives agents may read; and the publish ledger (§2.3),
// one row per published part. The same DDL serves SQLite and PostgreSQL.
const docPolicySchemaV16 = `
CREATE TABLE linear_doc_policies(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL,policy TEXT NOT NULL,updated_at TEXT NOT NULL,updated_by_principal TEXT NOT NULL,updated_by_session TEXT);
CREATE TABLE linear_doc_publications(project_id TEXT NOT NULL REFERENCES projects(id),parent_kind TEXT NOT NULL CHECK(parent_kind IN ('team','project','initiative')),parent_id TEXT NOT NULL,source_path TEXT NOT NULL,part INTEGER NOT NULL,parts INTEGER NOT NULL,generation INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','published')),publish_token TEXT NOT NULL,document_id TEXT NOT NULL DEFAULT '',published_sha256 TEXT NOT NULL DEFAULT '',source_commit TEXT NOT NULL,source_sha256 TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(project_id,parent_kind,parent_id,source_path,part));
`

const (
	maxDocDenyPatterns = 50
	maxDocInitiatives  = 20
	maxDocSourceRoot   = 1024
	maxDocPatternBytes = 1024
)

// DocPolicy is a project's document policy. With none, nothing is
// published and no initiative is in scope.
type DocPolicy struct {
	// SourceRoot is the one directory a publish reads from, stored as
	// written and expanded on the publishing machine.
	SourceRoot string `json:"source_root"`
	// DenyPatterns refuse a publish that matches any of them (RE2).
	DenyPatterns []DocDenyPattern `json:"deny_patterns"`
	// Initiatives are the initiative UUIDs agents may read and a publish
	// may target. Nothing is inferred from membership (LD-M1).
	Initiatives []string `json:"initiatives"`
}

// DocDenyPattern is one deny pattern; a hit names its ID, never the text.
type DocDenyPattern struct {
	ID    string `json:"id"`
	Regex string `json:"regex"`
}

var docPatternID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ParseDocPolicy decodes a policy strictly and checks its rules.
func ParseDocPolicy(v any) (DocPolicy, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 128<<10 {
		return DocPolicy{}, invalid("document policy must be a JSON object under 128 KiB")
	}
	var p DocPolicy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return DocPolicy{}, invalid(`document policy must be {"source_root", "deny_patterns": [{"id", "regex"}], "initiatives": [UUID]}`)
	}
	if len(p.SourceRoot) > maxDocSourceRoot || !utf8.ValidString(p.SourceRoot) || strings.ContainsAny(p.SourceRoot, "\x00\n\r") {
		return DocPolicy{}, invalid(fmt.Sprintf("source_root is one path of at most %d bytes", maxDocSourceRoot))
	}
	if p.SourceRoot != "" && !strings.HasPrefix(p.SourceRoot, "/") && p.SourceRoot != "~" && !strings.HasPrefix(p.SourceRoot, "~/") {
		return DocPolicy{}, invalid("source_root must be absolute or start with ~/")
	}
	if p.DenyPatterns == nil {
		p.DenyPatterns = []DocDenyPattern{}
	}
	if len(p.DenyPatterns) > maxDocDenyPatterns {
		return DocPolicy{}, invalid(fmt.Sprintf("document policy holds at most %d deny patterns", maxDocDenyPatterns))
	}
	ids := map[string]bool{}
	for _, d := range p.DenyPatterns {
		if !docPatternID.MatchString(d.ID) || ids[d.ID] {
			return DocPolicy{}, invalid("deny pattern IDs are unique, lower case, 1–64 of a-z, 0-9, _ and -")
		}
		ids[d.ID] = true
		if d.Regex == "" || len(d.Regex) > maxDocPatternBytes {
			return DocPolicy{}, invalid(fmt.Sprintf("deny pattern %s: a regex of 1–%d bytes", d.ID, maxDocPatternBytes))
		}
		if _, err := regexp.Compile(d.Regex); err != nil {
			return DocPolicy{}, invalid(fmt.Sprintf("deny pattern %s is not a valid RE2 regex", d.ID))
		}
	}
	if p.Initiatives == nil {
		p.Initiatives = []string{}
	}
	if len(p.Initiatives) > maxDocInitiatives {
		return DocPolicy{}, invalid(fmt.Sprintf("document policy lists at most %d initiatives", maxDocInitiatives))
	}
	seen := map[string]bool{}
	for _, id := range p.Initiatives {
		if !lowerUUIDPattern.MatchString(id) || seen[id] {
			return DocPolicy{}, invalid("initiatives are unique canonical initiative UUIDs")
		}
		seen[id] = true
	}
	return p, nil
}

func docPolicyOperation(op string) bool { return op == "doc_policy_set" }

// docPolicyOp sets the policy: the project owner only, without a session
// (the CLI) or from an active one. Reading it (doc_policy_get) is open to
// every member and handled before sessions.
func (s *Store) docPolicyOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid := w["id"].(string), p["id"].(string)
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	_, current, err := readDocPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	expected, err := integer(r.Args["expected_revision"])
	if err != nil || r.Args["expected_revision"] == nil {
		return nil, invalid("doc_policy_set requires expected_revision")
	}
	if current != int64(expected) {
		return nil, conflict(fmt.Sprintf("document policy changed: current revision is %d", current))
	}
	policy, err := ParseDocPolicy(r.Args["doc_policy"])
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(policy)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	var session any
	if actor != nil {
		session = actor["id"]
	}
	revision := int64(expected) + 1
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_doc_policies(project_id,revision,policy,updated_at,updated_by_principal,updated_by_session) VALUES(?,?,?,?,?,?)`, pid, revision, string(encoded), now, s.principal, session)
	} else {
		var result interface{ RowsAffected() (int64, error) }
		result, err = tx.ExecContext(ctx, `UPDATE linear_doc_policies SET revision=?,policy=?,updated_at=?,updated_by_principal=?,updated_by_session=? WHERE project_id=? AND revision=?`, revision, string(encoded), now, s.principal, session, pid, expected)
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				err = conflict("document policy changed concurrently; read it again")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	// The audit names what changed in size, never the patterns themselves.
	if err = s.audit(ctx, tx, wid, pid, "", "linear_doc_policy", pid, "set", map[string]any{"revision": revision, "source_root_set": policy.SourceRoot != "", "deny_patterns": len(policy.DenyPatterns), "initiatives": policy.Initiatives}); err != nil {
		return nil, err
	}
	return docPolicyView(ctx, tx, pid, true)
}

// readDocPolicy is the project's policy and its revision; with none, an
// empty policy at revision 0.
func readDocPolicy(ctx context.Context, tx *transaction, pid string) (DocPolicy, int64, error) {
	row, err := one(ctx, tx, `SELECT revision,policy FROM linear_doc_policies WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return DocPolicy{DenyPatterns: []DocDenyPattern{}, Initiatives: []string{}}, 0, nil
	}
	if err != nil {
		return DocPolicy{}, 0, err
	}
	var stored any
	if json.Unmarshal([]byte(fmt.Sprint(row["policy"])), &stored) != nil {
		return DocPolicy{}, 0, errors.New("stored document policy is unreadable")
	}
	policy, err := ParseDocPolicy(stored)
	if err != nil {
		return DocPolicy{}, 0, errors.New("stored document policy is unreadable")
	}
	return policy, int64(asInt(row["revision"])), nil
}

// docPolicyView is the policy as a member reads it. Only a project owner or
// lead, who may publish, sees the source root (a local path) and the deny
// patterns' regexes, which may name what must stay private; everyone else
// sees whether a source root is set, and the patterns' IDs.
func docPolicyView(ctx context.Context, tx *transaction, pid string, full bool) (map[string]any, error) {
	policy, revision, err := readDocPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	var patterns any = policy.DenyPatterns
	var root any = policy.SourceRoot
	key := "source_root"
	if !full {
		root, key = policy.SourceRoot != "", "source_root_set"
		ids := []string{}
		for _, d := range policy.DenyPatterns {
			ids = append(ids, d.ID)
		}
		patterns = ids
	}
	out := map[string]any{"revision": revision, key: root, "deny_patterns": patterns, "initiatives": policy.Initiatives, "updated_at": nil, "updated_by": nil}
	if revision > 0 {
		row, err := one(ctx, tx, `SELECT updated_at,updated_by_principal FROM linear_doc_policies WHERE project_id=?`, pid)
		if err != nil {
			return nil, err
		}
		out["updated_at"], out["updated_by"] = row["updated_at"], row["updated_by_principal"]
	}
	return out, nil
}
