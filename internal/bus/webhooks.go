package bus

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// Linear webhooks (FR14, live/FR14-WEBHOOKS-DESIGN.md §5, §6, §10): a
// verified delivery is a doorbell. It marks existing watch cursors due and
// nothing else; polling and the cursors stay the only record of progress.
// A delivery is stored as IDs, kinds and times only, never text (R4, C5).

// webhookSchemaVersion is the webhook deliveries, team states, counters and
// cursor hints.
const webhookSchemaVersion = 51

// webhookSchema is valid for SQLite and Postgres: TEXT times in layout().
const webhookSchema = `
CREATE TABLE webhook_deliveries(delivery_id TEXT PRIMARY KEY,team_id TEXT NOT NULL,webhook_id TEXT NOT NULL DEFAULT '',event_type TEXT NOT NULL,action TEXT NOT NULL,outcome TEXT NOT NULL CHECK(outcome IN ('applied','inert','unbound_team','ignored_type')),linear_at TEXT NOT NULL,received_at TEXT NOT NULL,updated_keys TEXT NOT NULL DEFAULT '',size INTEGER NOT NULL DEFAULT 0);
CREATE INDEX webhook_deliveries_team ON webhook_deliveries(team_id,received_at);
CREATE TABLE webhook_teams(team_id TEXT PRIMARY KEY,configured_at TEXT,first_delivery_at TEXT,last_delivery_at TEXT,last_missed_at TEXT);
CREATE TABLE webhook_counts(team_id TEXT NOT NULL,hour TEXT NOT NULL,reason TEXT NOT NULL,n INTEGER NOT NULL,PRIMARY KEY(team_id,hour,reason));
CREATE TABLE webhook_server(id INTEGER PRIMARY KEY CHECK(id=1),gate_on_since TEXT,drain_ended_at TEXT);
ALTER TABLE watch_cursors ADD COLUMN hinted_at TEXT;
`

// WebhookWindow is how far a delivery's webhookTimestamp may be from the
// server's clock.
const WebhookWindow = 60 * time.Second

// webhookRetention is how long deliveries (the dedup keys) and counters
// are kept. It must outlast WebhookWindow, or a replay inside the window
// could pass dedup.
const webhookRetention = 7 * 24 * time.Hour

// webhookUpdateMarksAll: until the probe shows updatedFrom carries the
// labelIds and delegateId keys (Q11), every Issue update or create marks
// both S1 and S2.
const webhookUpdateMarksAll = true

// webhookMissAge and webhookDegrade are the miss rule's thresholds (Q13).
const (
	webhookMissAge = 10 * time.Minute
	webhookDegrade = 24 * time.Hour
)

// webhookReasons are the counters' reasons.
var webhookReasons = map[string]bool{
	"delivered": true, "duplicate": true, "unbound_team": true, "ignored_type": true, "inert": true,
	"signature": true, "too_large": true, "stale": true, "org_mismatch": true, "team_mismatch": true,
	"rate_limited": true, "verified_throttled": true, "missed": true, "bad_payload": true,
}

// webhookRejects are the reasons webhook_status reports as a team's
// rejects.
var webhookRejects = map[string]bool{
	"signature": true, "too_large": true, "stale": true, "org_mismatch": true, "team_mismatch": true,
	"rate_limited": true, "verified_throttled": true, "bad_payload": true,
}

// WebhookDelivery is one verified delivery's typed fields. It has no
// title, body, actor or URL field, and must never get one
// (TestWebhookDeliveryHoldsNoText).
type WebhookDelivery struct {
	DeliveryID, TeamID, WebhookID, EventType, Action string   // EventType "Issue"|"Comment"|other
	EntityID, IssueID                                string   // data.id; data.issueId for comments (lower-case UUIDs)
	UpdatedKeys                                      []string // updatedFrom key names only
	LinearAt, ReceivedAt                             time.Time
	// Size is the raw body's length in bytes, for the probe's evidence on
	// the body cap (Q2); the body itself is never kept.
	Size int
}

// WebhookResult is a delivery's outcome.
type WebhookResult struct {
	Outcome  string   // applied | inert | unbound_team | ignored_type | duplicate
	Projects []string // project IDs whose cursors were marked (applied only), for the nudge
}

// SetWebhooksProven is the webhooks gate: off, a delivery is recorded
// inert and marks nothing.
func (s *Store) SetWebhooksProven(on bool) { s.webhooksProven = on }

func webhookHour(t time.Time) string { return layout(t.UTC().Truncate(time.Hour)) }

func webhookCount(ctx context.Context, tx *transaction, team, reason string, n int, now time.Time) error {
	if !webhookReasons[reason] {
		return invalid("unsupported webhook count reason")
	}
	if n <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO webhook_counts(team_id,hour,reason,n) VALUES(?,?,?,?) ON CONFLICT(team_id,hour,reason) DO UPDATE SET n=webhook_counts.n+excluded.n`, strings.ToLower(team), webhookHour(now), reason, n)
	return err
}

// projectsForTeam lists the projects whose binding holds team as a write
// team, whatever its case. An unreadable binding is skipped: a hint is an
// optimisation, and its project's polling still runs.
func projectsForTeam(ctx context.Context, tx *transaction, team string) ([]string, error) {
	team = strings.ToLower(team)
	rows, err := query(ctx, tx, `SELECT project_id,mapping FROM taskboard_bindings WHERE mapping IS NOT NULL ORDER BY project_id`)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range rows {
		var m struct {
			Teams []string `json:"linear_team_ids"`
		}
		if json.Unmarshal([]byte(str(row, "mapping")), &m) != nil {
			continue
		}
		for _, t := range m.Teams {
			if strings.ToLower(t) == team {
				out = append(out, str(row, "project_id"))
				break
			}
		}
	}
	return out, nil
}

// webhookKeys joins a delivery's updatedFrom key names for the probe's
// evidence (Q11): names of up to 64 letters only, never values (C5).
func webhookKeys(keys []string) string {
	out := []string{}
	for _, k := range keys {
		ok := k != "" && len(k) <= 64
		for _, c := range k {
			ok = ok && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z')
		}
		if ok && len(out) < 64 {
			out = append(out, k)
		}
	}
	return strings.Join(out, ",")
}

// ProjectsForTeam lists the projects whose binding holds team.
func (s *Store) ProjectsForTeam(ctx context.Context, team string) ([]string, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return projectsForTeam(ctx, tx, team)
}

// webhookTargets are the cursors a handled event marks: intake (skipped
// while a project's intake is off) and comment watches. handled is false
// for an event that is not handled.
func webhookTargets(d WebhookDelivery) (intake [][2]string, comments []string, handled bool) {
	has := func(k string) bool {
		for _, u := range d.UpdatedKeys {
			if u == k {
				return true
			}
		}
		return false
	}
	team := strings.ToLower(d.TeamID)
	switch {
	case d.EventType == "Issue" && (d.Action == "create" || d.Action == "update"):
		// A create carries its set keys in UpdatedKeys.
		if webhookUpdateMarksAll || has("labelIds") {
			intake = append(intake, [2]string{"intake-s1", team})
		}
		if webhookUpdateMarksAll || has("delegateId") {
			intake = append(intake, [2]string{"intake-s2", "app"})
		}
		return intake, nil, true
	case d.EventType == "Comment" && d.Action == "create":
		intake = append(intake, [2]string{"intake-s3", "app"})
		if d.IssueID != "" {
			comments = append(comments, strings.ToLower(d.IssueID))
		}
		return intake, comments, true
	}
	return nil, nil, false
}

// WebhookRecord records one verified delivery and, when the gate is on and
// a bound project concerns it, marks that project's existing cursors due.
// It runs in one server-wide transaction.
func (s *Store) WebhookRecord(ctx context.Context, d WebhookDelivery) (WebhookResult, error) {
	if d.DeliveryID == "" || d.TeamID == "" {
		return WebhookResult{}, invalid("a delivery needs its ID and team")
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return WebhookResult{}, err
	}
	defer tx.Rollback()
	received := d.ReceivedAt.UTC()
	if received.IsZero() {
		raw, e := dbStamp(ctx, tx)
		if e != nil {
			return WebhookResult{}, e
		}
		received = at(raw)
	}
	now := layout(received)
	team := strings.ToLower(d.TeamID)
	projects, err := projectsForTeam(ctx, tx, team)
	if err != nil {
		return WebhookResult{}, err
	}
	intake, comments, handled := webhookTargets(d)
	outcome := "applied"
	switch {
	case len(projects) == 0:
		outcome = "unbound_team"
	case !s.webhooksProven:
		outcome = "inert"
	case !handled:
		outcome = "ignored_type"
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(delivery_id,team_id,webhook_id,event_type,action,outcome,linear_at,received_at,updated_keys,size) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(delivery_id) DO NOTHING`, d.DeliveryID, team, d.WebhookID, d.EventType, d.Action, outcome, layout(d.LinearAt), now, webhookKeys(d.UpdatedKeys), d.Size)
	if err != nil {
		return WebhookResult{}, err
	}
	if n, e := res.RowsAffected(); e != nil {
		return WebhookResult{}, e
	} else if n == 0 {
		if err = webhookCount(ctx, tx, team, "duplicate", 1, received); err != nil {
			return WebhookResult{}, err
		}
		return WebhookResult{Outcome: "duplicate"}, tx.Commit()
	}
	if s.webhookAfterInsert != nil {
		if err = s.webhookAfterInsert(); err != nil {
			return WebhookResult{}, err
		}
	}
	out := WebhookResult{Outcome: outcome}
	if outcome == "unbound_team" {
		if err = webhookCount(ctx, tx, team, "unbound_team", 1, received); err != nil {
			return WebhookResult{}, err
		}
		return out, tx.Commit()
	}
	if outcome == "applied" {
		// A hint always records hinted_at, but moves next_poll_at only on a
		// cursor that isn't backing off a failure: a hint never cuts a
		// back-off short. A project is listed for the nudge only when one
		// of its cursors moved. cond adds to the not-backing-off test, with
		// its arguments first in args.
		mark := func(cond, q string, args ...any) (int, error) {
			rows, e := query(ctx, tx, `UPDATE watch_cursors SET hinted_at=?,next_poll_at=CASE WHEN failures=0 AND last_error=''`+cond+` THEN ? ELSE next_poll_at END WHERE `+q+` RETURNING next_poll_at`, append([]any{now}, args...)...)
			moved := 0
			for _, r := range rows {
				if str(r, "next_poll_at") == now {
					moved++
				}
			}
			return moved, e
		}
		for _, pid := range projects {
			moved := 0
			policy, e := intakePolicy(ctx, tx, pid)
			if e != nil {
				return WebhookResult{}, e
			}
			if str(policy, "mode") != "off" {
				for _, c := range intake {
					n, e := mark("", `project_id=? AND source=? AND subject=?`, now, pid, c[0], c[1])
					if e != nil {
						return WebhookResult{}, e
					}
					moved += n
				}
			}
			for _, issue := range comments {
				// A rate_limited or auth failure backs off every comment
				// watch of the project for its hour, but marks only the
				// failing issue: no hint pulls another one back meanwhile.
				n, e := mark(` AND NOT EXISTS (SELECT 1 FROM watch_cursors f WHERE f.project_id=watch_cursors.project_id AND f.source='comments' AND f.last_error IN ('rate_limited','auth') AND f.next_poll_at>?)`, `project_id=? AND source='comments' AND subject=? AND active_until IS NULL`, now, now, pid, issue)
				if e != nil {
					return WebhookResult{}, e
				}
				moved += n
			}
			if moved > 0 {
				out.Projects = append(out.Projects, pid)
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_teams(team_id,first_delivery_at,last_delivery_at) VALUES(?,?,?) ON CONFLICT(team_id) DO UPDATE SET first_delivery_at=COALESCE(webhook_teams.first_delivery_at,excluded.first_delivery_at),last_delivery_at=excluded.last_delivery_at`, team, now, now); err != nil {
		return WebhookResult{}, err
	}
	if outcome != "applied" {
		if err = webhookCount(ctx, tx, team, outcome, 1, received); err != nil {
			return WebhookResult{}, err
		}
	}
	if err = webhookCount(ctx, tx, team, "delivered", 1, received); err != nil {
		return WebhookResult{}, err
	}
	return out, tx.Commit()
}

// WebhookTeamsConfigured records the teams a start found a secret for:
// configured_at is set the first time a start sees a team's secret, and
// cleared when a start sees none. A newly configured team waits for its
// first delivery again.
func (s *Store) WebhookTeamsConfigured(ctx context.Context, teams []string, now time.Time) error {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	for _, t := range teams {
		t = strings.ToLower(t)
		seen[t] = true
		if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_teams(team_id,configured_at) VALUES(?,?) ON CONFLICT(team_id) DO UPDATE SET configured_at=excluded.configured_at,first_delivery_at=NULL WHERE webhook_teams.configured_at IS NULL`, t, layout(now)); err != nil {
			return err
		}
	}
	rows, err := query(ctx, tx, `SELECT team_id FROM webhook_teams WHERE configured_at IS NOT NULL`)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !seen[str(row, "team_id")] {
			if _, err = tx.ExecContext(ctx, `UPDATE webhook_teams SET configured_at=NULL WHERE team_id=?`, row["team_id"]); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// WebhookCount adds n to a team's counter for reason in now's hour; team
// "" is a server-wide (unattributed) count.
func (s *Store) WebhookCount(ctx context.Context, team, reason string, n int, now time.Time) error {
	if !webhookReasons[reason] {
		return invalid("unsupported webhook count reason")
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = webhookCount(ctx, tx, team, reason, n, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) webhookServerSet(ctx context.Context, column string, now time.Time) error {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_server(id,`+column+`) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET `+column+`=excluded.`+column, layout(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// WebhookGateOn records when the gate went on (a start with the gate on).
func (s *Store) WebhookGateOn(ctx context.Context, now time.Time) error {
	return s.webhookServerSet(ctx, "gate_on_since", now)
}

// WebhookDrainEnded records the last drain's end.
func (s *Store) WebhookDrainEnded(ctx context.Context, now time.Time) error {
	return s.webhookServerSet(ctx, "drain_ended_at", now)
}

// webhookTeam is a team's stored state.
type webhookTeam struct {
	configured, first, last, missed string
}

func readWebhookTeam(ctx context.Context, tx *transaction, team string) (webhookTeam, error) {
	rows, err := query(ctx, tx, `SELECT configured_at,first_delivery_at,last_delivery_at,last_missed_at FROM webhook_teams WHERE team_id=?`, strings.ToLower(team))
	if err != nil || len(rows) == 0 {
		return webhookTeam{}, err
	}
	r := rows[0]
	return webhookTeam{str(r, "configured_at"), str(r, "first_delivery_at"), str(r, "last_delivery_at"), str(r, "last_missed_at")}, nil
}

// liveSince is when the team's webhook last became live: the latest of its
// first delivery after configured_at, the gate's going on and the last
// drain's end. ok is false while the gate is off, the team is not
// configured or no delivery arrived since.
func (s *Store) liveSince(ctx context.Context, tx *transaction, w webhookTeam) (time.Time, bool, error) {
	if !s.webhooksProven || w.configured == "" || w.first == "" || w.first < w.configured {
		return time.Time{}, false, nil
	}
	since := w.first
	rows, err := query(ctx, tx, `SELECT gate_on_since,drain_ended_at FROM webhook_server WHERE id=1`)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(rows) == 1 {
		for _, k := range []string{"gate_on_since", "drain_ended_at"} {
			if v := str(rows[0], k); v > since {
				since = v
			}
		}
	}
	return at(since), true, nil
}

// WebhookTeamLive reports whether the team's webhook is live at t: live
// since before t, with no miss in the 24 hours before t.
func (s *Store) WebhookTeamLive(ctx context.Context, team string, t time.Time) (bool, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return s.webhookTeamLiveTx(ctx, tx, team, t)
}

// webhookTeamLiveTx is WebhookTeamLive inside tx. With the gate off it
// reads nothing.
func (s *Store) webhookTeamLiveTx(ctx context.Context, tx *transaction, team string, t time.Time) (bool, error) {
	if !s.webhooksProven {
		return false, nil
	}
	w, err := readWebhookTeam(ctx, tx, team)
	if err != nil {
		return false, err
	}
	since, ok, err := s.liveSince(ctx, tx, w)
	if err != nil || !ok || since.After(t) {
		return false, err
	}
	return w.missed == "" || w.missed <= layout(t.Add(-webhookDegrade)), nil
}

// webhookLiveWait is an intake source's or a comment watch's wait while
// the webhooks it depends on are live (§6 P4).
const webhookLiveWait = 60 * time.Minute

// boundTeams are the project's bound write teams, lower-cased; none
// without a readable binding.
func boundTeams(ctx context.Context, tx *transaction, pid string) ([]string, error) {
	rows, err := query(ctx, tx, `SELECT mapping FROM taskboard_bindings WHERE project_id=? AND mapping IS NOT NULL`, pid)
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	var m struct {
		Teams []string `json:"linear_team_ids"`
	}
	if json.Unmarshal([]byte(str(rows[0], "mapping")), &m) != nil {
		return nil, nil
	}
	out := make([]string, 0, len(m.Teams))
	for _, t := range m.Teams {
		out = append(out, strings.ToLower(t))
	}
	return out, nil
}

// webhookProjectLiveTx reports whether every bound write team of the
// project is live at t (Q14); a project with none is not.
func (s *Store) webhookProjectLiveTx(ctx context.Context, tx *transaction, pid string, t time.Time) (bool, error) {
	if !s.webhooksProven {
		return false, nil
	}
	teams, err := boundTeams(ctx, tx, pid)
	if err != nil || len(teams) == 0 {
		return false, err
	}
	for _, team := range teams {
		if live, e := s.webhookTeamLiveTx(ctx, tx, team, t); e != nil || !live {
			return false, e
		}
	}
	return true, nil
}

// intakeSourceLive reports whether an intake source's webhooks are live at
// t: S1's own team, or for a subject app source every bound write team.
func (s *Store) intakeSourceLive(ctx context.Context, tx *transaction, pid, source, subject string, t time.Time) (bool, error) {
	if !s.webhooksProven {
		return false, nil
	}
	if source == "intake-s1" {
		return s.webhookTeamLiveTx(ctx, tx, subject, t)
	}
	if subject == "app" {
		return s.webhookProjectLiveTx(ctx, tx, pid, t)
	}
	return false, nil
}

// WebhookCatchUp marks every live team's cursors due once after a drain
// (§6 P6): each of its projects' intake sources (unless intake is off) and
// active comment watches, with WebhookRecord's not-backing-off rule. It
// returns the projects whose cursors moved, for the nudge. With the gate
// off it marks nothing.
func (s *Store) WebhookCatchUp(ctx context.Context, now time.Time) ([]string, error) {
	if !s.webhooksProven {
		return nil, nil
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp := layout(now)
	teams, err := query(ctx, tx, `SELECT team_id FROM webhook_teams WHERE configured_at IS NOT NULL ORDER BY team_id`)
	if err != nil {
		return nil, err
	}
	mark := func(cond, q string, args ...any) (int, error) {
		rows, e := query(ctx, tx, `UPDATE watch_cursors SET hinted_at=?,next_poll_at=CASE WHEN failures=0 AND last_error=''`+cond+` THEN ? ELSE next_poll_at END WHERE `+q+` RETURNING next_poll_at`, append([]any{stamp}, args...)...)
		moved := 0
		for _, r := range rows {
			if str(r, "next_poll_at") == stamp {
				moved++
			}
		}
		return moved, e
	}
	listed := map[string]bool{}
	out := []string{}
	for _, row := range teams {
		team := str(row, "team_id")
		live, e := s.webhookTeamLiveTx(ctx, tx, team, now)
		if e != nil {
			return nil, e
		}
		if !live {
			continue
		}
		projects, e := projectsForTeam(ctx, tx, team)
		if e != nil {
			return nil, e
		}
		for _, pid := range projects {
			moved := 0
			policy, e := intakePolicy(ctx, tx, pid)
			if e != nil {
				return nil, e
			}
			if str(policy, "mode") != "off" {
				for _, c := range [][2]string{{"intake-s1", team}, {"intake-s2", "app"}, {"intake-s3", "app"}} {
					n, e := mark("", `project_id=? AND source=? AND subject=?`, stamp, pid, c[0], c[1])
					if e != nil {
						return nil, e
					}
					moved += n
				}
			}
			n, e := mark(` AND NOT EXISTS (SELECT 1 FROM watch_cursors f WHERE f.project_id=watch_cursors.project_id AND f.source='comments' AND f.last_error IN ('rate_limited','auth') AND f.next_poll_at>?)`, `project_id=? AND source='comments' AND active_until IS NULL`, stamp, stamp, pid)
			if e != nil {
				return nil, e
			}
			moved += n
			if moved > 0 && !listed[pid] {
				listed[pid] = true
				out = append(out, pid)
			}
		}
	}
	sort.Strings(out)
	return out, tx.Commit()
}

// WebhookMiss counts an event a poll found (queued, or notified) that the
// team's webhook should have rung for: older than 10 minutes when found,
// and from a time the team was live. It reports whether it counted.
func (s *Store) WebhookMiss(ctx context.Context, team string, linearAt, foundAt time.Time) (bool, error) {
	if !s.webhooksProven || foundAt.Sub(linearAt) <= webhookMissAge {
		return false, nil
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	counted, err := s.webhookMissTx(ctx, tx, team, linearAt, foundAt)
	if err != nil || !counted {
		return false, err
	}
	return true, tx.Commit()
}

// webhookMissTx is WebhookMiss inside tx, under the same rules.
func (s *Store) webhookMissTx(ctx context.Context, tx *transaction, team string, linearAt, foundAt time.Time) (bool, error) {
	if !s.webhooksProven || foundAt.Sub(linearAt) <= webhookMissAge {
		return false, nil
	}
	w, err := readWebhookTeam(ctx, tx, team)
	if err != nil {
		return false, err
	}
	since, ok, err := s.liveSince(ctx, tx, w)
	if err != nil || !ok || since.After(linearAt) {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_teams SET last_missed_at=? WHERE team_id=?`, layout(foundAt), strings.ToLower(team)); err != nil {
		return false, err
	}
	if err = webhookCount(ctx, tx, team, "missed", 1, foundAt); err != nil {
		return false, err
	}
	return true, nil
}

// PurgeWebhooks deletes deliveries and counters older than the retention.
func (s *Store) PurgeWebhooks(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	horizon := layout(now.Add(-webhookRetention))
	total := int64(0)
	for _, q := range []string{`DELETE FROM webhook_deliveries WHERE received_at<?`, `DELETE FROM webhook_counts WHERE hour<?`} {
		r, e := tx.ExecContext(ctx, q, horizon)
		if e != nil {
			return 0, e
		}
		n, _ := r.RowsAffected()
		total += n
	}
	return total, tx.Commit()
}

// webhookStatus is webhook_status: each bound write team's webhook state
// and 24-hour counts, and the server-wide rejects. It holds no secret and
// no text; unbound teams' IDs are listed for the project owner only.
func (s *Store) webhookStatus(ctx context.Context, tx *transaction, pid, projectRole string) (any, error) {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := at(nowRaw)
	dayAgo := layout(now.Add(-24 * time.Hour))
	since := webhookHour(now.Add(-23 * time.Hour))
	rows, err := query(ctx, tx, `SELECT mapping FROM taskboard_bindings WHERE project_id=? AND mapping IS NOT NULL`, pid)
	if err != nil {
		return nil, err
	}
	teams := []string{}
	if len(rows) == 1 {
		var m struct {
			Teams []string `json:"linear_team_ids"`
		}
		if json.Unmarshal([]byte(str(rows[0], "mapping")), &m) != nil {
			return nil, errors.New("the task board binding is unreadable")
		}
		for _, t := range m.Teams {
			teams = append(teams, strings.ToLower(t))
		}
	}
	sort.Strings(teams)
	counts, err := query(ctx, tx, `SELECT team_id,reason,SUM(n) AS n FROM webhook_counts WHERE hour>=? GROUP BY team_id,reason`, since)
	if err != nil {
		return nil, err
	}
	byTeam := map[string]map[string]int{}
	total := map[string]int{}
	unbound := []string{}
	for _, c := range counts {
		t, reason, n := str(c, "team_id"), str(c, "reason"), asInt(c["n"])
		if byTeam[t] == nil {
			byTeam[t] = map[string]int{}
		}
		byTeam[t][reason] += n
		total[reason] += n
		if reason == "unbound_team" && t != "" && n > 0 {
			unbound = append(unbound, t)
		}
	}
	sort.Strings(unbound)
	list := []map[string]any{}
	for _, team := range teams {
		w, e := readWebhookTeam(ctx, tx, team)
		if e != nil {
			return nil, e
		}
		state := "live"
		switch {
		case w.configured == "":
			state = "unconfigured"
		case !s.webhooksProven:
			state = "inert"
		case w.last == "" || w.last < w.configured:
			state = "waiting_first"
		case w.missed != "" && w.missed > dayAgo:
			state = "degraded"
		}
		rejects := map[string]int{}
		for reason, n := range byTeam[team] {
			if webhookRejects[reason] && n > 0 {
				rejects[reason] = n
			}
		}
		list = append(list, map[string]any{
			"team_id": team, "configured": w.configured != "", "state": state,
			"last_delivery_at": nullable(w.last), "deliveries_24h": byTeam[team]["delivered"],
			"rejects_24h": rejects, "missed_24h": byTeam[team]["missed"], "last_missed_at": nullable(w.missed),
		})
	}
	out := map[string]any{
		"teams":                       list,
		"signature_rejects_24h":       total["signature"],
		"too_large_24h":               total["too_large"],
		"verified_throttled_24h":      total["verified_throttled"],
		"unbound_team_deliveries_24h": total["unbound_team"],
	}
	if projectRole == "owner" {
		out["unbound_teams"] = unbound
	}
	return out, nil
}
