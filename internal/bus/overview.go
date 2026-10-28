package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// project_overview (live/PROJECT-OVERVIEW-PLAN.md, TUI-UX-AUDIT PR C): one
// read for a project's detail pane and the Overview. It returns only what
// the reader's role can already read through list_sessions,
// taskboard_get_v2, status_policy_get and health_get, never an issue's
// text (the TUI fills titles in from the viewer's own Linear connection),
// and never a session's instance, transport, notes or notification
// settings.

// overviewSessions caps the rows; the counts are an aggregate over every
// session, so they never depend on the cap.
const overviewSessions = 200

// overviewSessionColumns are the session columns the overview shows: the
// row list_sessions shows, less instance tokens, transport, notes and the
// context watcher's own bookkeeping.
const overviewSessionColumns = `id,name,owner_principal,role,agent_kind,model,host,account,status,successor,last_seen,created_at,updated_at,context_tokens,context_window,context_level,context_compactions,context_at,alive_until,alive_state`

func (s *Store) projectOverview(ctx context.Context, tx *transaction, w, p map[string]any, role string) (any, error) {
	pid := p["id"].(string)
	coordinator := str(p, "coordinator_session")
	project := map[string]any{"id": pid, "slug": p["slug"], "name": p["name"], "git_remote": publicRemote(str(p, "git_remote")), "role": role}

	// Every session counts; the Sessions tab archives a retired or moved
	// session a day after its last change, and its default view shows
	// the rest (internal/tui/archive.go).
	now := hostClock().UTC()
	staleBefore := now.Add(-staleAfter).Format(time.RFC3339Nano)
	archivedBefore := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	c, err := one(ctx, tx, `SELECT COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN status='active' THEN 1 ELSE 0 END),0) AS active,
		COALESCE(SUM(CASE WHEN status='active' AND last_seen<? THEN 1 ELSE 0 END),0) AS stale,
		COALESCE(SUM(CASE WHEN status='retired' THEN 1 ELSE 0 END),0) AS retired,
		COALESCE(SUM(CASE WHEN status='moved' THEN 1 ELSE 0 END),0) AS moved,
		COALESCE(SUM(CASE WHEN status<>'active' AND updated_at<=? THEN 1 ELSE 0 END),0) AS archived
		FROM sessions WHERE project_id=? AND role<>'system'`, staleBefore, archivedBefore, pid)
	if err != nil {
		return nil, err
	}
	counts := map[string]any{}
	for _, k := range []string{"total", "active", "stale", "retired", "moved", "archived"} {
		counts[k] = asInt(c[k])
	}
	counts["current"] = asInt(c["total"]) - asInt(c["archived"])
	a, err := one(ctx, tx, `SELECT COALESCE(SUM(CASE WHEN state='confirmed' THEN 1 ELSE 0 END),0) AS confirmed,
		COALESCE(SUM(CASE WHEN state='proposed' THEN 1 ELSE 0 END),0) AS proposed
		FROM session_assignments WHERE project_id=? AND state<>'released'`, pid)
	if err != nil {
		return nil, err
	}
	counts["assignments_confirmed"], counts["assignments_proposed"] = asInt(a["confirmed"]), asInt(a["proposed"])

	rows, err := query(ctx, tx, `SELECT `+overviewSessionColumns+` FROM sessions WHERE project_id=? AND role<>'system'
		ORDER BY CASE WHEN status='active' THEN 0 ELSE 1 END,last_seen DESC,id LIMIT ?`, pid, overviewSessions+1)
	if err != nil {
		return nil, err
	}
	truncated := len(rows) > overviewSessions
	if truncated {
		rows = rows[:overviewSessions]
	}
	live, err := liveAssignments(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	bySession := map[string]map[string]any{}
	for _, l := range live {
		bySession[l["session_id"].(string)] = l
	}
	var coordinatorRow map[string]any
	for _, row := range rows {
		row["coordinator"] = coordinator != "" && row["id"] == coordinator
		row["assignment"] = overviewAssignment(bySession[row["id"].(string)])
		if row["coordinator"] == true {
			coordinatorRow = row
		}
	}
	var coord any
	if coordinator != "" {
		if coordinatorRow == nil {
			// Past the cap: read it on its own.
			coordinatorRow, err = one(ctx, tx, `SELECT id,name,status FROM sessions WHERE project_id=? AND id=?`, pid, coordinator)
			if err != nil && !errors.Is(err, errNotFound) {
				return nil, err
			}
		}
		if coordinatorRow != nil {
			coord = map[string]any{"session_id": coordinatorRow["id"], "name": coordinatorRow["name"], "status": coordinatorRow["status"]}
		}
	}
	linear, err := s.overviewLinear(ctx, tx, w["id"].(string), pid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"project": project, "coordinator": coord, "counts": counts, "sessions": rows,
		"sessions_truncated": truncated, "linear": linear}
	if role == "owner" {
		if out["project_leads"], err = projectLeadCounts(ctx, tx, pid, now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// projectLeadCounts is the owner's line on Linear project leads: in the
// last 7 days, how many ClawBridge set and how many of its notices reached
// someone, and how many wait for a session. Counts only, never who.
func projectLeadCounts(ctx context.Context, tx *transaction, pid string, now time.Time) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT details FROM history WHERE project_id=? AND entity='project_lead' AND action='lead' AND changed_at>=?`, pid, now.Add(-7*24*time.Hour).Format(timestampLayout))
	if err != nil {
		return nil, err
	}
	set := 0
	for _, r := range rows {
		// The details come back decoded or as JSON text, by dialect.
		d, ok := r["details"].(map[string]any)
		if !ok && json.Unmarshal([]byte(fmt.Sprint(r["details"])), &d) != nil {
			continue
		}
		if d["outcome"] != "accepted" {
			continue
		}
		set++
	}
	// Told: notices delivered in the window, whenever their write was;
	// waiting: notices no session has had yet.
	n, err := one(ctx, tx, `SELECT COALESCE(SUM(CASE WHEN delivered_at>=? THEN 1 ELSE 0 END),0) AS told,COALESCE(SUM(CASE WHEN delivered_at IS NULL AND created_at>=? THEN 1 ELSE 0 END),0) AS waiting FROM project_lead_notices WHERE project_id=?`, now.Add(-7*24*time.Hour).Format(timestampLayout), now.Add(-leadNoticeWaitDays*24*time.Hour).Format(timestampLayout), pid)
	if err != nil {
		return nil, err
	}
	return map[string]any{"set_7d": set, "told_7d": asInt(n["told"]), "waiting": asInt(n["waiting"])}, nil
}

// overviewAssignment is a session's live assignment: the issue's
// identifier and UUID, never its text.
func overviewAssignment(l map[string]any) any {
	if l == nil {
		return nil
	}
	return map[string]any{"state": l["state"], "issue_identifier": l["issue_identifier"], "issue_uuid": l["issue_uuid"],
		"task_role": l["task_role"], "scope_repo": l["scope_repo"], "scope_paths": decodePaths(l["scope_paths"])}
}

// overviewLinear sums up the board as the binding and the project's
// policies hold it. A binding that can't be read is a flag, never its
// error text.
func (s *Store) overviewLinear(ctx context.Context, tx *transaction, wid, pid string) (map[string]any, error) {
	raw, err := s.taskboard(ctx, tx, wid, pid, "", "taskboard_get_v2", map[string]any{})
	var se *serviceError
	if errors.As(err, &se) && se.code == "binding_unreadable" {
		return map[string]any{"configured": true, "unreadable": true}, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := ParseTaskboardBindingV2(raw)
	if err != nil {
		return nil, err
	}
	if !b.Configured {
		return map[string]any{"configured": false}, nil
	}
	policy, revision, err := readStatusPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	health := map[string]string{}
	hrows, err := query(ctx, tx, `SELECT linear_project_id,enabled,last_health FROM project_health WHERE project_id=?`, pid)
	if err != nil {
		return nil, err
	}
	for _, h := range hrows {
		if asInt(h["enabled"]) == 1 {
			health[str(h, "linear_project_id")] = str(h, "last_health")
		}
	}
	names, namesAt, err := bindingNames(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	teams := []map[string]any{}
	for _, id := range b.LinearTeamIDs {
		teams = append(teams, map[string]any{"id": id, "name": names.Teams[id], "default": id == b.LinearDefaultTeamID})
	}
	projects := []map[string]any{}
	for _, id := range b.LinearProjectIDs {
		closes := "human"
		if policy.AgentCloses(id) {
			closes = "agent"
		}
		row := map[string]any{"id": id, "name": names.Projects[id], "closes": closes}
		if h, ok := health[id]; ok {
			row["health"] = h
		}
		projects = append(projects, row)
	}
	return map[string]any{"configured": true, "revision": b.Revision, "mode": b.Mode, "teams": teams, "projects": projects,
		"names_updated_at": namesAt, "status_policy": map[string]any{"enabled": policy.Enabled, "revision": revision}}, nil
}

// publicRemote is a project's git remote without any user or password in
// it (PROJECT-OVERVIEW-PLAN-REVIEW L4). An scp-style remote names a user
// only, never a secret, and stays as it is.
func publicRemote(remote string) string {
	if !strings.Contains(remote, "://") {
		return remote
	}
	u, err := url.Parse(remote)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}
