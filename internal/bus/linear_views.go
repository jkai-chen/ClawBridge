package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/principal"
)

// The Linear issue views ClawBridge wrote (live/LINEAR-VIEWS-PLAN.md §13.1,
// §14). A row is recorded only after the CLI's read-back proved the view's
// name, team, sharing and filter; the filter is stored as canonical text
// with its digest. Every view is a shared view on one bound team: scope is
// always "team" in this schema. Agents read the rows to filter issues by a
// view (taskboard_issues_v2 view_id); only the project's owner, without a
// session, records one.
const linearViewsSchemaV37 = `
CREATE TABLE linear_views(project_id TEXT NOT NULL REFERENCES projects(id),view_id TEXT NOT NULL,team_id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind='issue'),scope TEXT NOT NULL CHECK(scope='team'),name TEXT NOT NULL,filter TEXT NOT NULL,filter_sha256 TEXT NOT NULL,filter_as_of TEXT NOT NULL,agent_filter TEXT NOT NULL CHECK(agent_filter IN ('available','unavailable')),uses TEXT NOT NULL,binding_revision INTEGER NOT NULL,adopted INTEGER NOT NULL CHECK(adopted IN (0,1)),created_by TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(project_id,view_id));
`

const (
	maxViewFilterBytes = 16 << 10
	maxViewUses        = 20
	maxStoredViews     = 200
)

var (
	viewIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	viewUsePattern = regexp.MustCompile(`^[A-Za-z?]{1,40}(\.[A-Za-z?]{1,40}){0,3}$`)
	viewSHAPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func linearViewOperation(op string) bool {
	return op == "linear_view_record" || op == "linear_view_forget" || op == "linear_view_get" || op == "linear_views_list"
}

// linearViewsOp runs the stored views' operations. Any project member or
// session reads them; recording is the owner's, sessionless, and on the
// hosted authority only from a person's own device.
func (s *Store) linearViewsOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, errors.New("instance requires session")
	}
	switch r.Operation {
	case "linear_views_list":
		return s.listLinearViews(ctx, tx, pid)
	case "linear_view_get":
		id := str(a, "view_id")
		if !viewIDPattern.MatchString(id) {
			return nil, invalid("view_id must be a Linear view ID (a lower-case UUID)")
		}
		row, err := s.linearView(ctx, tx, pid, id, true)
		if errors.Is(err, errNotFound) {
			return nil, scoped("no view "+id+" is recorded for this project; ClawBridge filters only by views it wrote", "not_found")
		}
		return row, err
	}
	if r.Session != "" {
		return nil, scoped("a session can't write a Linear view; the project owner runs linear view create, update or delete in their own terminal", "forbidden")
	}
	if projectRole != "owner" {
		return nil, scoped("project owner permission required", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("a Linear view is recorded only from a person's own signed-in device", "forbidden")
	}
	if r.Operation == "linear_view_forget" {
		return s.forgetLinearView(ctx, tx, wid, pid, a)
	}
	return s.recordLinearView(ctx, tx, wid, pid, a)
}

// forgetLinearView drops a view the CLI deleted in Linear and read back as
// gone. It must still be the row the preview read; a row already gone
// answers as a replay.
func (s *Store) forgetLinearView(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	id, expected := str(a, "view_id"), str(a, "expected_filter_as_of")
	if !viewIDPattern.MatchString(id) || expected == "" {
		return nil, invalid("view_id and expected_filter_as_of (the preview's) are required")
	}
	prev, err := s.linearView(ctx, tx, pid, id, false)
	if errors.Is(err, errNotFound) {
		return map[string]any{"view_id": id, "forgotten": true, "replayed": true}, nil
	}
	if err != nil {
		return nil, err
	}
	if prev["filter_as_of"] != expected {
		return nil, &serviceError{code: "conflict", cause: errors.New("the recorded view changed since the preview; run the preview again")}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM linear_views WHERE project_id=? AND view_id=?`, pid, id); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_view", id, "view_deleted", map[string]any{"scope": "team:" + prev["team_id"].(string), "view_id": id}); err != nil {
		return nil, err
	}
	return map[string]any{"view_id": id, "forgotten": true, "replayed": false}, nil
}

// viewRecord is a checked linear_view_record request.
type viewRecord struct {
	action, viewID, teamID, name, filter, sha, asOf, agentFilter, uses, expectedAsOf string
	revision                                                                         int64
}

func parseViewRecord(a map[string]any) (viewRecord, error) {
	v := viewRecord{action: str(a, "action"), viewID: str(a, "view_id"), teamID: str(a, "team_id"), name: strings.TrimSpace(str(a, "name")), filter: str(a, "filter"), sha: str(a, "filter_sha256"), asOf: str(a, "filter_as_of"), agentFilter: str(a, "agent_filter"), expectedAsOf: str(a, "expected_filter_as_of")}
	if v.action != "create" && v.action != "update" {
		return v, invalid("action must be create or update")
	}
	if !viewIDPattern.MatchString(v.viewID) || !viewIDPattern.MatchString(v.teamID) {
		return v, invalid("view_id and team_id must be lower-case UUIDs")
	}
	if !LinearNameOK(v.name) {
		return v, invalid("name must be 1 to 80 characters with no control or direction characters")
	}
	sum := sha256.Sum256([]byte(v.filter))
	if v.filter == "" || len(v.filter) > maxViewFilterBytes || !viewSHAPattern.MatchString(v.sha) || hex.EncodeToString(sum[:]) != v.sha {
		return v, invalid("filter must be the canonical filter text, at most 16 KiB, with its SHA-256 as filter_sha256")
	}
	if _, err := time.Parse(time.RFC3339Nano, v.asOf); err != nil {
		return v, invalid("filter_as_of must be Linear's updatedAt for the view (RFC 3339)")
	}
	if (v.action == "update") != (v.expectedAsOf != "") {
		return v, invalid("expected_filter_as_of goes with an update, and only with one")
	}
	uses, isArray, ok := stringList(a["uses"])
	if (a["uses"] != nil && (!isArray || !ok)) || len(uses) > maxViewUses {
		return v, invalid("uses must be at most 20 filter keys")
	}
	for _, use := range uses {
		if !viewUsePattern.MatchString(use) {
			return v, invalid("uses must name filter keys, like or or priority.gt")
		}
	}
	switch {
	case v.agentFilter == "available" && len(uses) == 0, v.agentFilter == "unavailable" && len(uses) > 0:
	default:
		return v, invalid("agent_filter is available with no uses, unavailable with them")
	}
	v.uses = strings.Join(uses, ",")
	n, err := integer(a["expected_binding_revision"])
	if err != nil || n < 1 {
		return v, invalid("expected_binding_revision must be the binding revision the preview read")
	}
	v.revision = int64(n)
	return v, nil
}

// recordLinearView stores a view the CLI wrote and read back. The binding
// must be the one the preview read and still hold the view's team; an
// update must follow the stored row (compare-and-set on filter_as_of) and
// keep its team. A replay of the same record answers the stored row.
func (s *Store) recordLinearView(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	v, err := parseViewRecord(a)
	if err != nil {
		return nil, err
	}
	b, err := s.viewBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if b.Revision != v.revision {
		return nil, &serviceError{code: "conflict", cause: errors.New("the task board binding changed since the preview; run the preview again")}
	}
	if !containsString(b.LinearTeamIDs, v.teamID) {
		return nil, invalid("team " + v.teamID + " isn't bound to this project; a view is recorded only on a bound team")
	}
	prev, err := s.linearView(ctx, tx, pid, v.viewID, true)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	exists := err == nil
	same := exists && prev["team_id"] == v.teamID && prev["name"] == v.name && prev["filter_sha256"] == v.sha && prev["filter_as_of"] == v.asOf
	if same {
		return viewAnswer(prev, true), nil
	}
	now := stamp()
	switch {
	case v.action == "create" && exists:
		return nil, &serviceError{code: "conflict", cause: errors.New("view " + v.viewID + " is already recorded; update it instead")}
	case v.action == "create":
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_views WHERE project_id=?`, pid).Scan(&count); err != nil {
			return nil, err
		}
		if count >= maxStoredViews {
			return nil, invalid("a project records at most 200 views")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO linear_views(project_id,view_id,team_id,kind,scope,name,filter,filter_sha256,filter_as_of,agent_filter,uses,binding_revision,adopted,created_by,created_at,updated_at) VALUES(?,?,?,'issue','team',?,?,?,?,?,?,?,0,?,?,?)`,
			pid, v.viewID, v.teamID, v.name, v.filter, v.sha, v.asOf, v.agentFilter, v.uses, v.revision, s.principal, now, now); err != nil {
			return nil, err
		}
	case !exists:
		return nil, scoped("no view "+v.viewID+" is recorded for this project; ClawBridge updates only views it wrote", "not_found")
	case prev["team_id"] != v.teamID:
		return nil, invalid("an update keeps the view's team")
	default:
		res, err := tx.ExecContext(ctx, `UPDATE linear_views SET name=?,filter=?,filter_sha256=?,filter_as_of=?,agent_filter=?,uses=?,binding_revision=?,updated_at=? WHERE project_id=? AND view_id=? AND filter_as_of=?`,
			v.name, v.filter, v.sha, v.asOf, v.agentFilter, v.uses, v.revision, now, pid, v.viewID, v.expectedAsOf)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, &serviceError{code: "conflict", cause: errors.New("the recorded view changed since the preview; run the preview again")}
		}
	}
	// History names the scope, the view, its sharing and the filter's
	// digest: never the filter or the names (§14.1).
	action := "view_created"
	if v.action == "update" {
		action = "view_updated"
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_view", v.viewID, action, map[string]any{"scope": "team:" + v.teamID, "view_id": v.viewID, "shared": true, "filter_sha256": v.sha, "agent_filter": v.agentFilter}); err != nil {
		return nil, err
	}
	row, err := s.linearView(ctx, tx, pid, v.viewID, true)
	if err != nil {
		return nil, err
	}
	return viewAnswer(row, false), nil
}

// viewBinding is the project's readable, linked binding.
func (s *Store) viewBinding(ctx context.Context, tx *transaction, pid string) (TaskboardBindingV2, error) {
	row, err := one(ctx, tx, `SELECT revision,mapping,mapping_version FROM taskboard_bindings WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return TaskboardBindingV2{}, invalid("link the task board first; a view belongs to a bound team")
	}
	if err != nil {
		return TaskboardBindingV2{}, err
	}
	mapping, ok := row["mapping"].(string)
	if !ok {
		return TaskboardBindingV2{}, invalid("link the task board first; a view belongs to a bound team")
	}
	b, err := readStoredMapping(mapping, int(row["mapping_version"].(int64)))
	if err != nil {
		return TaskboardBindingV2{}, bindingUnreadable(row["revision"].(int64), errors.Is(err, errMappingFormat))
	}
	b.Revision = row["revision"].(int64)
	return b, nil
}

// linearView reads one stored view; withFilter adds the filter text.
func (s *Store) linearView(ctx context.Context, tx *transaction, pid, id string, withFilter bool) (map[string]any, error) {
	row, err := one(ctx, tx, `SELECT * FROM linear_views WHERE project_id=? AND view_id=?`, pid, id)
	if err != nil {
		return nil, err
	}
	bound, err := s.viewTeamBound(ctx, tx, pid, row["team_id"].(string))
	if err != nil {
		return nil, err
	}
	return viewRow(row, bound, withFilter), nil
}

func (s *Store) listLinearViews(ctx context.Context, tx *transaction, pid string) (any, error) {
	rows, err := query(ctx, tx, `SELECT * FROM linear_views WHERE project_id=? ORDER BY name,view_id`, pid)
	if err != nil {
		return nil, err
	}
	b, err := s.viewBinding(ctx, tx, pid)
	var unreadable *serviceError
	if err != nil && !errors.As(err, &unreadable) {
		return nil, err
	}
	views := make([]any, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewRow(row, err == nil && containsString(b.LinearTeamIDs, row["team_id"].(string)), false))
	}
	return map[string]any{"views": views}, nil
}

// viewTeamBound reports whether team is still bound; an unlinked or
// unreadable binding binds nothing.
func (s *Store) viewTeamBound(ctx context.Context, tx *transaction, pid, team string) (bool, error) {
	b, err := s.viewBinding(ctx, tx, pid)
	var refused *serviceError
	if errors.As(err, &refused) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return containsString(b.LinearTeamIDs, team), nil
}

// viewRow is the answer's shape: uses as a list, adopted as a boolean, and
// team_bound saying whether the view's team is still in the binding.
func viewRow(row map[string]any, bound, withFilter bool) map[string]any {
	uses := []string{}
	if text, _ := row["uses"].(string); text != "" {
		uses = strings.Split(text, ",")
	}
	out := map[string]any{"view_id": row["view_id"], "team_id": row["team_id"], "kind": row["kind"], "scope": row["scope"], "name": row["name"], "filter_sha256": row["filter_sha256"], "filter_as_of": row["filter_as_of"], "agent_filter": row["agent_filter"], "uses": uses, "binding_revision": asInt(row["binding_revision"]), "adopted": asInt(row["adopted"]) == 1, "created_at": row["created_at"], "updated_at": row["updated_at"], "team_bound": bound}
	if withFilter {
		out["filter"] = row["filter"]
	}
	return out
}

func viewAnswer(row map[string]any, replay bool) map[string]any {
	delete(row, "filter")
	return map[string]any{"view": row, "replayed": replay}
}
