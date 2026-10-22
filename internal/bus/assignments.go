package bus

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Schema 5: structured session assignments and the project's designated
// coordinator session. The same DDL serves SQLite and PostgreSQL.
const assignmentSchemaV5 = `
CREATE TABLE session_assignments(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),session_id TEXT NOT NULL,issue_uuid TEXT NOT NULL DEFAULT '',issue_identifier TEXT NOT NULL DEFAULT '',task_role TEXT NOT NULL DEFAULT '',scope_repo TEXT NOT NULL DEFAULT '',scope_paths TEXT NOT NULL DEFAULT '[]',revision TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('proposed','confirmed','released')),proposed_by_session TEXT NOT NULL,proposed_at TEXT NOT NULL,confirmed_by_session TEXT,confirmed_by_principal TEXT,confirmed_at TEXT,released_at TEXT,release_reason TEXT,transferred_from TEXT,FOREIGN KEY(project_id,session_id) REFERENCES sessions(project_id,id));
CREATE UNIQUE INDEX live_session_assignment ON session_assignments(project_id,session_id) WHERE state<>'released';
CREATE INDEX project_assignments ON session_assignments(project_id,state,issue_uuid);
ALTER TABLE projects ADD COLUMN coordinator_session TEXT;
`

// An assignment describes who owns a slice. It is never a lock and grants no
// project rights: only the designated coordinator session may confirm one.
const (
	maxScopePaths     = 32
	maxScopePathBytes = 256
)

var (
	issueUUIDPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	issueIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

func assignmentOperation(op string) bool {
	return op == "assignment_propose" || op == "assignment_confirm" || op == "assignment_release" || op == "coordinator_set"
}

// scopePaths validates repository-relative path patterns. A pattern covers
// the paths it matches and everything beneath them; "**" is allowed only as
// the whole pattern or as a final "/**".
func scopePaths(v any) ([]string, error) {
	var raw []string
	switch list := v.(type) {
	case nil:
		return []string{}, nil
	case []string:
		raw = list
	case []any:
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, invalid("scope_paths must be strings")
			}
			raw = append(raw, s)
		}
	default:
		return nil, invalid("scope_paths must be an array of strings")
	}
	if len(raw) > maxScopePaths {
		return nil, invalid("scope_paths allows at most 32 patterns")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range raw {
		if err := scopePathOK(p); err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}
func scopePathOK(p string) error {
	bad := invalid("scope paths must be clean repository-relative patterns of at most 256 bytes")
	if p == "" || len(p) > maxScopePathBytes || strings.ContainsAny(p, "\\") || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return bad
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return bad
		}
	}
	base := strings.TrimSuffix(p, "/**")
	if p == "**" {
		base = "*"
	}
	if strings.Contains(base, "**") {
		return invalid(`"**" is allowed only as a whole pattern or a final "/**"`)
	}
	if _, err := path.Match(base, ""); err != nil {
		return bad
	}
	return nil
}

// scopeCovers reports whether pattern p covers the repository path x.
func scopeCovers(p, x string) bool {
	if p == "**" {
		return true
	}
	base := strings.TrimSuffix(p, "/**")
	for at := x; at != "." && at != "/" && at != ""; at = path.Dir(at) {
		if ok, _ := path.Match(base, at); ok {
			return true
		}
	}
	return false
}

// scopeOverlap is conservative: it may report patterns that never share a
// path, and never misses two that do.
func scopeOverlap(a, b string) bool {
	literal := func(p string) string {
		if i := strings.IndexAny(p, "*?[\\"); i >= 0 {
			return p[:i]
		}
		return p
	}
	x, y := literal(a), literal(b)
	return strings.HasPrefix(x, y) || strings.HasPrefix(y, x)
}

func decodePaths(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := []string{}
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		if json.Unmarshal([]byte(list), &out) == nil {
			return out
		}
	}
	return []string{}
}

// overlaps reports whether two live assignments share an issue or scope.
func overlaps(a, b map[string]any) bool {
	if issue := str(a, "issue_uuid"); issue != "" && issue == str(b, "issue_uuid") {
		return true
	}
	ra, rb := str(a, "scope_repo"), str(b, "scope_repo")
	if ra != "" && rb != "" && ra != rb {
		return false
	}
	for _, x := range decodePaths(a["scope_paths"]) {
		for _, y := range decodePaths(b["scope_paths"]) {
			if scopeOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

func liveAssignment(ctx context.Context, tx *transaction, pid, sid string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT * FROM session_assignments WHERE project_id=? AND session_id=? AND state<>'released'`, pid, sid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}
func liveAssignments(ctx context.Context, tx *transaction, pid string) ([]map[string]any, error) {
	return query(ctx, tx, `SELECT * FROM session_assignments WHERE project_id=? AND state<>'released' ORDER BY session_id`, pid)
}

// conflicts lists the other live assignments overlapping a; confirmedOnly
// limits them to confirmed ones.
func conflicts(ctx context.Context, tx *transaction, pid string, a map[string]any, confirmedOnly bool) ([]map[string]any, error) {
	rows, err := liveAssignments(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, other := range rows {
		if other["session_id"] == a["session_id"] || (confirmedOnly && other["state"] != "confirmed") || !overlaps(a, other) {
			continue
		}
		out = append(out, map[string]any{"assignment_id": other["id"], "session_id": other["session_id"], "state": other["state"], "revision": other["revision"], "issue_uuid": other["issue_uuid"], "scope_repo": other["scope_repo"], "scope_paths": decodePaths(other["scope_paths"])})
	}
	return out, nil
}
func conflictIDs(rows []map[string]any) []any {
	ids := []any{}
	for _, c := range rows {
		ids = append(ids, c["assignment_id"])
	}
	return ids
}

// releaseAssignment releases a session's live assignment, if any.
func (s *Store) releaseAssignment(ctx context.Context, tx *transaction, wid, pid, sid, reason string) (map[string]any, error) {
	live, err := liveAssignment(ctx, tx, pid, sid)
	if err != nil || live == nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE session_assignments SET state='released',revision=?,released_at=?,release_reason=? WHERE id=? AND project_id=?`, newID("rev_"), stamp(), reason, live["id"], pid); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "assignment", live["id"].(string), "released", map[string]any{"session_id": sid, "reason": reason}); err != nil {
		return nil, err
	}
	return live, nil
}

// endSessionAssignment runs when a session stops being active: it releases the
// live assignment, hands a moved session's assignment to its successor as a
// proposal, and clears the session's coordinator designation.
func (s *Store) endSessionAssignment(ctx context.Context, tx *transaction, wid, pid, sid, reason string, successor any) (map[string]any, error) {
	result := map[string]any{}
	released, err := s.releaseAssignment(ctx, tx, wid, pid, sid, reason)
	if err != nil {
		return nil, err
	}
	if released != nil {
		result["released"] = released["id"]
		if next, ok := successor.(string); ok && next != "" {
			existing, err := liveAssignment(ctx, tx, pid, next)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				result["transfer_skipped"] = "successor already has a live assignment"
			} else {
				id := newID("asg_")
				if _, err = tx.ExecContext(ctx, `INSERT INTO session_assignments(id,project_id,session_id,issue_uuid,issue_identifier,task_role,scope_repo,scope_paths,revision,state,proposed_by_session,proposed_at,transferred_from) VALUES(?,?,?,?,?,?,?,?,?,'proposed',?,?,?)`, id, pid, next, released["issue_uuid"], released["issue_identifier"], released["task_role"], released["scope_repo"], asJSON(decodePaths(released["scope_paths"])), newID("rev_"), sid, stamp(), released["id"]); err != nil {
					return nil, err
				}
				if err = s.audit(ctx, tx, wid, pid, "", "assignment", id, "transferred", map[string]any{"from": released["id"], "session_id": next}); err != nil {
					return nil, err
				}
				result["transferred_to"] = id
			}
		}
	}
	if _, err = s.clearCoordinator(ctx, tx, wid, pid, sid); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) clearCoordinator(ctx context.Context, tx *transaction, wid, pid, sid string) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE projects SET coordinator_session=NULL WHERE id=? AND coordinator_session=?`, pid, sid)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	return true, s.audit(ctx, tx, wid, pid, "", "project", pid, "coordinator_set", map[string]any{"old": sid, "new": nil, "reason": "session_ended"})
}

// assignmentView is the assignment as list_sessions and whoami show it.
func assignmentView(a map[string]any, session map[string]any) map[string]any {
	if a == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{"id", "issue_uuid", "issue_identifier", "task_role", "scope_repo", "state", "revision", "proposed_by_session", "proposed_at", "confirmed_by_session", "confirmed_at", "transferred_from"} {
		out[k] = a[k]
	}
	out["scope_paths"] = decodePaths(a["scope_paths"])
	stale, _ := session["stale"].(bool)
	out["assignment_stale"] = a["state"] == "confirmed" && stale
	return out
}

func scoped(message, code string) error {
	return &serviceError{code: code, cause: errors.New(message), public: true}
}

// coordinatorMay authorizes the designated coordinator session to
// confirm or release another session's assignment.
func (s *Store) coordinatorMay(ctx context.Context, tx *transaction, w map[string]any, p map[string]any, projectRole string, actor, target map[string]any) error {
	if projectRole != "owner" && projectRole != "lead" {
		return scoped("project lead or owner permission required", "forbidden")
	}
	if str(p, "coordinator_session") == "" || p["coordinator_session"] != actor["id"] {
		return scoped("only the project's designated coordinator session may do this", "forbidden")
	}
	if projectRole == "lead" && target["owner_principal"] != s.principal {
		return s.leadMayAdminister(ctx, tx, w, p["id"].(string), target, map[string]any{})
	}
	return nil
}

func (s *Store) assignment(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	a := r.Args
	wid, pid, sid := w["id"].(string), p["id"].(string), actor["id"].(string)
	if actor["status"] != "active" {
		return nil, invalid("session is retired or moved; it cannot perform this operation")
	}
	target := actor
	if selector := str(a, "target_session"); selector != "" {
		match, args := sessionAddress(selector)
		var err error
		target, err = one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match, append([]any{pid}, args...)...)
		if err != nil {
			return nil, scopedError(err, "target session must exist in the same project", "invalid")
		}
	}
	tid := target["id"].(string)
	switch r.Operation {
	case "coordinator_set":
		if projectRole != "owner" && projectRole != "lead" {
			return nil, scoped("project lead or owner permission required", "forbidden")
		}
		old := p["coordinator_session"]
		var next any
		if str(a, "target_session") != "" {
			if target["status"] != "active" {
				return nil, invalid("coordinator session must be active")
			}
			next = tid
		}
		for _, id := range []any{old, next} {
			key, ok := id.(string)
			if !ok || key == "" || projectRole != "lead" {
				continue
			}
			row, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND id=?`, pid, key)
			if err != nil {
				return nil, err
			}
			if row["owner_principal"] != s.principal {
				if err = s.leadMayAdminister(ctx, tx, w, pid, row, map[string]any{}); err != nil {
					return nil, err
				}
			}
		}
		if old != next {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET coordinator_session=? WHERE id=?`, next, pid); err != nil {
				return nil, err
			}
			if err := s.audit(ctx, tx, wid, pid, sid, "project", pid, "coordinator_set", map[string]any{"old": old, "new": next}); err != nil {
				return nil, err
			}
		}
		return map[string]any{"project_id": pid, "coordinator_session": next}, nil
	case "assignment_propose":
		if tid != sid {
			return nil, invalid("a session proposes only its own assignment")
		}
		if projectRole == "reader" {
			return nil, scoped("project member permission required", "forbidden")
		}
		issue, identifier, role := str(a, "issue_uuid"), str(a, "issue_identifier"), str(a, "task_role")
		if issue != "" && !issueUUIDPattern.MatchString(issue) {
			return nil, invalid("issue_uuid must be a lowercase Linear issue UUID")
		}
		if identifier != "" && (issue == "" || !issueIdentifierPattern.MatchString(identifier)) {
			return nil, invalid("issue_identifier needs issue_uuid and must be a short display identifier such as ABC-12")
		}
		if len(role) > 256 || strings.IndexFunc(role, unicode.IsControl) >= 0 {
			return nil, invalid("task_role must be at most 256 bytes without control characters")
		}
		repo, err := NormalizeRemote(str(a, "scope_repo"))
		if err != nil {
			return nil, err
		}
		paths, err := scopePaths(a["scope_paths"])
		if err != nil {
			return nil, err
		}
		if issue == "" && len(paths) == 0 {
			return nil, invalid("an assignment needs issue_uuid or scope_paths")
		}
		if _, err = s.releaseAssignment(ctx, tx, wid, pid, sid, "replaced"); err != nil {
			return nil, err
		}
		id := newID("asg_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO session_assignments(id,project_id,session_id,issue_uuid,issue_identifier,task_role,scope_repo,scope_paths,revision,state,proposed_by_session,proposed_at) VALUES(?,?,?,?,?,?,?,?,?,'proposed',?,?)`, id, pid, sid, issue, identifier, role, repo, asJSON(paths), newID("rev_"), sid, stamp()); err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, wid, pid, sid, "assignment", id, "proposed", map[string]any{"session_id": sid, "issue_uuid": issue, "task_role": role, "scope_repo": repo, "scope_paths": paths}); err != nil {
			return nil, err
		}
		live, err := liveAssignment(ctx, tx, pid, sid)
		if err != nil {
			return nil, err
		}
		found, err := conflicts(ctx, tx, pid, live, false)
		if err != nil {
			return nil, err
		}
		return map[string]any{"assignment": assignmentView(live, actor), "conflicts": found, "coordinator_session": p["coordinator_session"]}, nil
	case "assignment_confirm", "assignment_release":
		revision := str(a, "revision")
		if revision == "" {
			return nil, invalid("revision is required; pass the assignment revision you reviewed")
		}
		if r.Operation == "assignment_confirm" || tid != sid {
			if err := s.coordinatorMay(ctx, tx, w, p, projectRole, actor, target); err != nil {
				return nil, err
			}
		}
		live, err := liveAssignment(ctx, tx, pid, tid)
		if err != nil {
			return nil, err
		}
		if live == nil {
			return nil, scoped("session has no live assignment", "not_found")
		}
		if live["revision"] != revision {
			return nil, scoped("assignment changed; review its current revision", "conflict")
		}
		if r.Operation == "assignment_release" {
			reason := "self"
			if tid != sid {
				reason = "coordinator"
			}
			if _, err = s.releaseAssignment(ctx, tx, wid, pid, tid, reason); err != nil {
				return nil, err
			}
			return map[string]any{"released": live["id"], "session_id": tid, "reason": reason}, nil
		}
		if live["state"] == "confirmed" {
			return nil, errors.New("assignment is already confirmed")
		}
		found, err := conflicts(ctx, tx, pid, live, true)
		if err != nil {
			return nil, err
		}
		override, _ := a["allow_overlap"].(bool)
		if len(found) > 0 && !override {
			sessions := []string{}
			for _, c := range found {
				sessions = append(sessions, c["session_id"].(string))
			}
			return nil, scoped("assignment overlaps the confirmed assignment of "+strings.Join(sessions, ", ")+"; pass allow_overlap to confirm both owners", "conflict")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE session_assignments SET state='confirmed',revision=?,confirmed_by_session=?,confirmed_by_principal=?,confirmed_at=? WHERE id=? AND project_id=?`, newID("rev_"), sid, s.principal, stamp(), live["id"], pid); err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, wid, pid, sid, "assignment", live["id"].(string), "confirmed", map[string]any{"session_id": tid}); err != nil {
			return nil, err
		}
		if len(found) > 0 {
			if err = s.audit(ctx, tx, wid, pid, sid, "assignment", live["id"].(string), "conflict_overridden", map[string]any{"session_id": tid, "conflicts": conflictIDs(found)}); err != nil {
				return nil, err
			}
		}
		live, err = liveAssignment(ctx, tx, pid, tid)
		if err != nil {
			return nil, err
		}
		return map[string]any{"confirmed": true, "assignment": assignmentView(live, target), "conflicts": found}, nil
	}
	return nil, invalid("unsupported operation")
}

// listSessions pages a project's sessions with their live assignments. The
// assignment filters select sessions whose live assignment matches: issue by
// UUID or display identifier, scope_path by a pattern covering that path.
func (s *Store) listSessions(ctx context.Context, tx *transaction, pid, status, role string, a map[string]any, after int64, n int) (any, error) {
	issue, scopePath, state := str(a, "issue"), str(a, "scope_path"), str(a, "assignment_state")
	if state != "" && state != "proposed" && state != "confirmed" {
		return nil, invalid("assignment_state must be proposed or confirmed")
	}
	if scopePath != "" && (len(scopePath) > maxScopePathBytes || path.IsAbs(scopePath) || path.Clean(scopePath) != scopePath || scopePath == "." || strings.HasPrefix(scopePath, "../") || scopePath == ".." || strings.ContainsAny(scopePath, "\\")) {
		return nil, invalid("scope_path must be a clean repository-relative path")
	}
	filtered := issue != "" || scopePath != "" || state != ""
	q := `SELECT rowid AS sequence,* FROM sessions WHERE project_id=? AND (?='' OR status=?) AND (?='' OR role=?) AND rowid>?`
	args := []any{pid, status, status, role, role, after}
	if filtered {
		q += ` AND EXISTS(SELECT 1 FROM session_assignments g WHERE g.project_id=sessions.project_id AND g.session_id=sessions.id AND g.state<>'released' AND (?='' OR g.state=?) AND (?='' OR g.issue_uuid=? OR g.issue_identifier=?))`
		args = append(args, state, state, issue, issue, issue)
	}
	q += ` ORDER BY rowid`
	if scopePath == "" {
		q += ` LIMIT ?`
		args = append(args, n+1)
	}
	rows, err := query(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	live, err := liveAssignments(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	bySession := map[string]map[string]any{}
	for _, l := range live {
		bySession[l["session_id"].(string)] = l
	}
	out := []map[string]any{}
	for _, row := range rows {
		l := bySession[row["id"].(string)]
		if scopePath != "" {
			covered := false
			for _, p := range decodePaths(l["scope_paths"]) {
				covered = covered || scopeCovers(p, scopePath)
			}
			if !covered {
				continue
			}
		}
		row["assignment"] = assignmentView(l, row)
		out = append(out, row)
	}
	return page("sessions", out, n, after), nil
}
