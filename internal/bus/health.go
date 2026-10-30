package bus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Schema 15: project health (live/PROJECT-HEALTH-DESIGN.md r2). The
// server watcher computes each enabled Linear project's health from Linear
// facts once a day, in a slot staggered by the project's ID, at most
// healthRunsPerTick runs a tick, after the comment watch and markers. This
// version takes the fallback path (HP-M2): a confirmed health goes to the
// coordinator session as a bus notice and to `linear health show`; nothing
// is written to Linear. The same DDL serves SQLite and PostgreSQL.
const healthSchemaV15 = `
CREATE TABLE project_health(project_id TEXT NOT NULL REFERENCES projects(id),linear_project_id TEXT NOT NULL,enabled INTEGER NOT NULL,policy TEXT NOT NULL,revision INTEGER NOT NULL,last_health TEXT NOT NULL DEFAULT '',pending_health TEXT NOT NULL DEFAULT '',pending_since TEXT,last_acted_at TEXT,last_run_at TEXT,next_health_at TEXT NOT NULL,last_error TEXT NOT NULL DEFAULT '',last_report TEXT NOT NULL DEFAULT '',updated_at TEXT NOT NULL,updated_by_principal TEXT NOT NULL,PRIMARY KEY(project_id,linear_project_id));
CREATE INDEX project_health_due ON project_health(project_id,enabled,next_health_at);
`

// Schema 19: posting project health to Linear (design §4, path A). One row
// per Linear project and UTC day is the post's ledger: it is inserted
// before the send, so a day never gets two posts, and a send that may
// have landed stays pending until a later run finds it or a full extra
// day passes. Nothing is ever sent again for a pending row.
const healthPostSchemaV19 = `
CREATE TABLE health_posts(project_id TEXT NOT NULL REFERENCES projects(id),linear_project_id TEXT NOT NULL,day TEXT NOT NULL,health TEXT NOT NULL,was TEXT NOT NULL,act TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','accepted','refused','failed')),sent_at TEXT NOT NULL,update_id TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',settled_at TEXT,PRIMARY KEY(project_id,linear_project_id,day));
`

const (
	// healthRunsPerTick caps health runs in one tick across all projects
	// (HP-M3); the watcher counts them, and one due call never hands out
	// more.
	healthRunsPerTick = 2
	// HealthMaxCalls is what one run may spend: the project read, up to
	// HealthIssuePages issue pages and HealthSampled activity reads. A run
	// is charged this before it starts and refunded what it did not use, so
	// a run is deferred, never partial.
	HealthIssuePages = 4
	HealthSampled    = 20
	HealthMaxCalls   = 1 + HealthIssuePages + HealthSampled
	// healthRetry is when a failed run is tried again.
	healthRetry = time.Hour
	// healthListed bounds the identifiers a signal names.
	healthListed = 10
	// healthHumanQuiet is how long a person's project update keeps
	// ClawBridge from posting over it (SF-H4).
	healthHumanQuiet = 7 * 24 * time.Hour
	// healthPostSettle is how long a pending post may wait for a run to
	// find it before it settles failed: its day and a full extra day.
	healthPostSettle = 48 * time.Hour
	// healthConfirmGap is how old a pending health must be before a forced
	// run may confirm it: the two runs that act are on different days.
	healthConfirmGap = 20 * time.Hour
	// healthPostSkew allows for Linear's clock against the database's when
	// matching a pending post to an update.
	healthPostSkew = 5 * time.Minute
	// HealthPostMarker opens every posted body, so a person can tell
	// ClawBridge's updates apart.
	HealthPostMarker = "— 🤖 ClawBridge project health"
)

// Health values, as Linear names them.
var healthLevels = []string{"onTrack", "atRisk", "offTrack"}

var healthWords = map[string]string{"onTrack": "On track", "atRisk": "At risk", "offTrack": "Off track"}

// HealthPolicy is one Linear project's health policy. Every threshold has
// the design's default; the owner may change them.
type HealthPolicy struct {
	Enabled bool `json:"enabled"`
	// Post asks for Linear project updates. Without it, or when a post is
	// held back or refused, the coordinator session is told instead.
	Post   bool `json:"post"`
	Weekly bool `json:"weekly"`
	// StalledDays is how long a started issue may go without human
	// activity before it counts as stalled.
	StalledDays     int `json:"stalled_days"`
	StalledRisk     int `json:"stalled_risk"`
	StalledOff      int `json:"stalled_off"`
	StalledRiskPct  int `json:"stalled_risk_pct"`
	StalledOffPct   int `json:"stalled_off_pct"`
	BlockedRisk     int `json:"blocked_risk"`
	BlockedOff      int `json:"blocked_off"`
	ScheduleRiskPct int `json:"schedule_risk_pct"`
	ScheduleOffPct  int `json:"schedule_off_pct"`
}

func defaultHealthPolicy() HealthPolicy {
	return HealthPolicy{StalledDays: 5, StalledRisk: 2, StalledOff: 5, StalledRiskPct: 20, StalledOffPct: 40, BlockedRisk: 2, BlockedOff: 5, ScheduleRiskPct: 25, ScheduleOffPct: 50}
}

// ParseHealthPolicy decodes a policy strictly over the defaults: enabled
// is required, every threshold is bounded, and each at-risk bound is below
// its off-track bound.
func ParseHealthPolicy(v any) (HealthPolicy, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 4<<10 {
		return HealthPolicy{}, invalid("health policy must be a small JSON object")
	}
	var probe map[string]any
	if json.Unmarshal(raw, &probe) != nil || probe == nil {
		return HealthPolicy{}, invalid("health policy must be a JSON object")
	}
	if _, ok := probe["enabled"].(bool); !ok {
		return HealthPolicy{}, invalid("health policy requires enabled: true or false")
	}
	p := defaultHealthPolicy()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return HealthPolicy{}, invalid("health policy fields are enabled, post, weekly, stalled_days, stalled_risk, stalled_off, stalled_risk_pct, stalled_off_pct, blocked_risk, blocked_off, schedule_risk_pct, schedule_off_pct")
	}
	for _, b := range []struct {
		name     string
		risk     int
		off      int
		min, max int
	}{{"stalled", p.StalledRisk, p.StalledOff, 1, 1000}, {"stalled_pct", p.StalledRiskPct, p.StalledOffPct, 1, 100}, {"blocked", p.BlockedRisk, p.BlockedOff, 1, 1000}, {"schedule_pct", p.ScheduleRiskPct, p.ScheduleOffPct, 1, 100}} {
		if b.risk < b.min || b.off > b.max || b.risk >= b.off {
			return HealthPolicy{}, invalid(fmt.Sprintf("%s thresholds must be %d–%d with at-risk below off-track", b.name, b.min, b.max))
		}
	}
	if p.StalledDays < 1 || p.StalledDays > 60 {
		return HealthPolicy{}, invalid("stalled_days must be 1–60")
	}
	return p, nil
}

// HealthFacts is what a run read from Linear, typed: counts, issue
// identifiers (never titles or text), and dates.
type HealthFacts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Canceled  int `json:"canceled"`
	Started   int `json:"started"`
	// Stalled are started issues with no human activity in StalledDays;
	// Unverified are recently updated started issues whose activity was not
	// read or not proven human (sampling), which count as active.
	Stalled      []string `json:"stalled"`
	StalledCount int      `json:"stalled_count"`
	Unverified   int      `json:"unverified"`
	Blocked      []string `json:"blocked"`
	BlockedCount int      `json:"blocked_count"`
	// TargetDate and StartDate are Linear's dates (YYYY-MM-DD); StartFrom
	// says whether the start is the project's startDate or its createdAt.
	TargetDate string `json:"target_date"`
	StartDate  string `json:"start_date"`
	StartFrom  string `json:"start_from"`
	// Updates are the project's newest updates, at most
	// HealthUpdatesRead, each with whether the app wrote it. Absent means
	// they were not read, and nothing is posted.
	Updates []HealthUpdate `json:"updates"`
}

// HealthUpdatesRead is how many of a project's newest updates a run reads.
const HealthUpdatesRead = 5

// HealthUpdate is one project update as a run saw it: never its body.
type HealthUpdate struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	ByApp     bool   `json:"by_app"`
}

// created is when Linear created the update; facts are checked to parse.
func (u HealthUpdate) created() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, u.CreatedAt)
	return t
}

func parseHealthFacts(v any) (HealthFacts, error) {
	raw, err := json.Marshal(v)
	bad := invalid("facts must be the typed health facts object")
	if err != nil || len(raw) > 16<<10 {
		return HealthFacts{}, bad
	}
	var f HealthFacts
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&f) != nil || dec.Decode(new(any)) != io.EOF {
		return HealthFacts{}, bad
	}
	for _, n := range []int{f.Total, f.Completed, f.Canceled, f.Started, f.StalledCount, f.Unverified, f.BlockedCount} {
		if n < 0 || n > 100000 {
			return HealthFacts{}, bad
		}
	}
	if f.Completed+f.Canceled+f.Started > f.Total || f.StalledCount > f.Started || f.StalledCount+f.Unverified > f.Started || f.BlockedCount > f.Total-f.Completed-f.Canceled {
		return HealthFacts{}, invalid("health facts counts are inconsistent")
	}
	for _, list := range []struct {
		ids   []string
		count int
	}{{f.Stalled, f.StalledCount}, {f.Blocked, f.BlockedCount}} {
		if len(list.ids) > healthListed || len(list.ids) > list.count {
			return HealthFacts{}, bad
		}
		for _, id := range list.ids {
			if !issueIdentifierPattern.MatchString(id) {
				return HealthFacts{}, invalid("health facts name issues by identifier only")
			}
		}
	}
	for _, d := range []string{f.TargetDate, f.StartDate} {
		if d != "" {
			if _, e := time.Parse(time.DateOnly, d); e != nil {
				return HealthFacts{}, invalid("health facts dates are YYYY-MM-DD")
			}
		}
	}
	if f.StartDate == "" || f.StartFrom != "startDate" && f.StartFrom != "createdAt" {
		return HealthFacts{}, invalid("health facts need a start date from startDate or createdAt")
	}
	if len(f.Updates) > HealthUpdatesRead {
		return HealthFacts{}, bad
	}
	for _, u := range f.Updates {
		if _, e := time.Parse(time.RFC3339Nano, u.CreatedAt); e != nil || !issueUUIDPattern.MatchString(strings.ToLower(u.ID)) {
			return HealthFacts{}, invalid("health facts updates need a UUID and an RFC 3339 time")
		}
	}
	return f, nil
}

// HealthSignal is one rule's result.
type HealthSignal struct {
	Signal string `json:"signal"`
	Health string `json:"health"`
	Detail string `json:"detail"`
}

func levelOf(h string) int {
	for i, l := range healthLevels {
		if l == h {
			return i
		}
	}
	return -1
}

// ComputeHealth applies the rules (design §2) to one run's facts at now:
// the project's health is its worst signal. It returns "" when there is
// nothing to judge (no open or completed issues).
func ComputeHealth(f HealthFacts, p HealthPolicy, now time.Time) (string, []HealthSignal) {
	counted := f.Total - f.Canceled
	if counted <= 0 {
		return "", nil
	}
	signals := []HealthSignal{}
	add := func(name string, level int, detail string) {
		signals = append(signals, HealthSignal{Signal: name, Health: healthLevels[level], Detail: detail})
	}
	completion := float64(f.Completed) / float64(counted)
	if f.TargetDate != "" {
		start, _ := time.Parse(time.DateOnly, f.StartDate)
		// The target date is a whole day: it passes at its end.
		end, _ := time.Parse(time.DateOnly, f.TargetDate)
		end = end.Add(24 * time.Hour)
		level := 0
		detail := ""
		switch {
		case !now.Before(end):
			if completion < 1 {
				level = 2
			}
			detail = fmt.Sprintf("the target date %s has passed with %d%% of issues completed", f.TargetDate, int(completion*100))
		case !end.After(start):
			detail = fmt.Sprintf("the target date %s is not after the start %s; not judged", f.TargetDate, f.StartDate)
		default:
			used := now.Sub(start).Seconds() / end.Sub(start).Seconds()
			if used < 0 {
				used = 0
			}
			gap := used - completion
			switch {
			case gap*100 > float64(p.ScheduleOffPct):
				level = 2
			case gap*100 > float64(p.ScheduleRiskPct):
				level = 1
			}
			detail = fmt.Sprintf("%d%% of the time to %s used, %d%% of issues completed", int(used*100), f.TargetDate, int(completion*100))
		}
		from := "its start date"
		if f.StartFrom == "createdAt" {
			from = "its creation date (no start date)"
		}
		add("schedule", level, detail+fmt.Sprintf(" (counting issues, not estimates; time from %s, %s)", from, f.StartDate))
	}
	if f.Started > 0 {
		level := 0
		switch n, pct := f.StalledCount, f.StalledCount*100; {
		case n >= p.StalledOff || pct >= p.StalledOffPct*f.Started:
			level = 2
		case n >= p.StalledRisk || pct >= p.StalledRiskPct*f.Started:
			level = 1
		}
		detail := fmt.Sprintf("%d of %d started issues have had no human activity for %d days%s", f.StalledCount, f.Started, p.StalledDays, named(f.Stalled, f.StalledCount))
		if f.Unverified > 0 {
			detail += fmt.Sprintf("; %d recently updated were not checked for human activity and count as active", f.Unverified)
		}
		add("stalled", level, detail)
	}
	level := 0
	switch {
	case f.BlockedCount >= p.BlockedOff:
		level = 2
	case f.BlockedCount >= p.BlockedRisk:
		level = 1
	}
	add("blocked", level, fmt.Sprintf("%d open issues are blocked by an open issue%s", f.BlockedCount, named(f.Blocked, f.BlockedCount)))
	worst := 0
	for _, s := range signals {
		if l := levelOf(s.Health); l > worst {
			worst = l
		}
	}
	return healthLevels[worst], signals
}

func named(ids []string, count int) string {
	if len(ids) == 0 {
		return ""
	}
	more := ""
	if count > len(ids) {
		more = fmt.Sprintf(" and %d more", count-len(ids))
	}
	return ": " + strings.Join(ids, ", ") + more
}

// healthSlot is a Linear project's minute of the UTC day, a stable hash
// of its ID, so the projects of one binding run spread over the day.
func healthSlot(linearProject string) time.Duration {
	h := sha256.Sum256([]byte(linearProject))
	return time.Duration(binary.BigEndian.Uint64(h[:8])%(24*60)) * time.Minute
}

// nextHealthAt is the project's first slot after now.
func nextHealthAt(linearProject string, now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(healthSlot(linearProject))
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func healthOperation(op string) bool { return op == "health_set" }

func healthSystemOperation(op string) bool {
	return op == "health_due" || op == "health_force" || op == "health_report" || op == "health_post_settle" || op == "project_flow_settle"
}

// healthSystemOp runs the watcher's health calls, fenced by the watch
// lease like marker writes.
func (s *Store) healthSystemOp(ctx context.Context, tx *transaction, w, p, actor map[string]any, r Request) (any, error) {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	c := commentCall{s: s, tx: tx, wid: w["id"].(string), pid: p["id"].(string), sid: actor["id"].(string), now: at(nowRaw), w: w, a: r.Args}
	switch r.Operation {
	case "health_due":
		return c.healthDue(ctx)
	case "health_force":
		return c.healthForce(ctx)
	case "health_post_settle":
		return c.healthPostSettle(ctx)
	case "project_flow_settle":
		return c.projectFlowSettle(ctx)
	}
	return c.healthReport(ctx)
}

// healthRows is the project's health rows, as `linear health show` and
// health_get present them.
func healthRows(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT linear_project_id,enabled,policy,revision,last_health,pending_health,pending_since,last_acted_at,last_run_at,next_health_at,last_error,last_report,updated_at,updated_by_principal FROM project_health WHERE project_id=? ORDER BY linear_project_id`, pid)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		var policy, report any
		if json.Unmarshal([]byte(str(r, "policy")), &policy) != nil {
			return nil, errors.New("stored health policy is unreadable")
		}
		if raw := str(r, "last_report"); raw != "" && json.Unmarshal([]byte(raw), &report) != nil {
			return nil, errors.New("stored health report is unreadable")
		}
		r["enabled"] = asInt(r["enabled"]) == 1
		r["revision"] = asInt(r["revision"])
		r["policy"], r["last_report"] = policy, report
		posts, err := query(ctx, tx, `SELECT day,health,state,sent_at,update_id,error,settled_at FROM health_posts WHERE project_id=? AND linear_project_id=? ORDER BY day DESC LIMIT 1`, pid, str(r, "linear_project_id"))
		if err != nil {
			return nil, err
		}
		if len(posts) == 1 {
			r["last_post"] = posts[0]
		}
		out = append(out, r)
	}
	return map[string]any{"projects": out, "posting": "a confirmed health is posted to Linear as a project update where the policy's post is true: at most once a day, never over a person's update from the last 7 days, nor when the five updates read are all older than 7 days. Otherwise, and when a post is refused, the coordinator session is told. Keep post off until the deploy probe confirms Linear answers a project's newest five updates"}, nil
}

// healthOp sets one bound Linear project's health policy: the project owner
// only, at the row's revision (0 for a new row). Reading (health_get) is
// open to every member and handled before sessions.
func (s *Store) healthOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid := w["id"].(string), p["id"].(string)
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	lp, err := lowerUUID(r.Args, "linear_project", true)
	if err != nil {
		return nil, err
	}
	if err = healthBound(ctx, tx, pid, lp); err != nil {
		return nil, err
	}
	expected, err := integer(r.Args["expected_revision"])
	if err != nil || r.Args["expected_revision"] == nil {
		return nil, invalid("health_set requires expected_revision")
	}
	policy, err := ParseHealthPolicy(r.Args["health_policy"])
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(policy)
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	enabled := 0
	if policy.Enabled {
		enabled = 1
	}
	rows, err := query(ctx, tx, `SELECT revision,enabled,next_health_at FROM project_health WHERE project_id=? AND linear_project_id=?`, pid, lp)
	if err != nil {
		return nil, err
	}
	current := 0
	if len(rows) == 1 {
		current = asInt(rows[0]["revision"])
	}
	if current != expected {
		return nil, conflict(fmt.Sprintf("health policy changed: current revision is %d", current))
	}
	// Enabling starts at the project's next slot; a policy change while
	// enabled keeps the slot it has.
	next := layout(nextHealthAt(lp, at(nowRaw)))
	if len(rows) == 1 && asInt(rows[0]["enabled"]) == 1 {
		next = str(rows[0], "next_health_at")
	}
	if len(rows) == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO project_health(project_id,linear_project_id,enabled,policy,revision,next_health_at,updated_at,updated_by_principal) VALUES(?,?,?,?,1,?,?,?)`, pid, lp, enabled, string(encoded), next, nowRaw, s.principal)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE project_health SET enabled=?,policy=?,revision=?,next_health_at=?,updated_at=?,updated_by_principal=? WHERE project_id=? AND linear_project_id=? AND revision=?`, enabled, string(encoded), current+1, next, nowRaw, s.principal, pid, lp, current)
	}
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_health", lp, "set", map[string]any{"revision": current + 1, "enabled": policy.Enabled}); err != nil {
		return nil, err
	}
	return healthRows(ctx, tx, pid)
}

// healthBound refuses a Linear project outside the project's binding, and
// any team-board binding: health is per bound Linear project.
func healthBound(ctx context.Context, tx *transaction, pid, lp string) error {
	return linearProjectBound(ctx, tx, pid, lp, "project health")
}

// linearProjectBound checks lp is a Linear project of this project's
// project-set binding; what names the feature in its refusals.
func linearProjectBound(ctx context.Context, tx *transaction, pid, lp, what string) error {
	rows, err := query(ctx, tx, `SELECT mapping,mapping_version FROM taskboard_bindings WHERE project_id=? AND mapping IS NOT NULL`, pid)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return scoped("link the task board before setting "+what, "not_found")
	}
	b, err := readStoredMapping(str(rows[0], "mapping"), asInt(rows[0]["mapping_version"]))
	if err != nil {
		return scoped("this project's task board binding is unreadable", "binding_unreadable")
	}
	if b.Mode != "project_set" {
		return invalid(what + " needs a project-set binding; a team board has no Linear projects")
	}
	if !containsString(b.LinearProjectIDs, lp) {
		return scoped("that Linear project is not in this project's binding", "forbidden")
	}
	return nil
}

// healthDue hands the lease holder at most limit (≤ healthRunsPerTick)
// enabled projects whose slot has come and that are still bound, and only
// as many as the hour's budget can pay in full while leaving the comment
// watch its reserve. Each is charged HealthMaxCalls now.
func (c commentCall) healthDue(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	limit, err := integer(c.a["limit"])
	if err != nil || limit < 0 || limit > healthRunsPerTick {
		return nil, invalid(fmt.Sprintf("limit must be 0–%d", healthRunsPerTick))
	}
	used, budget, err := c.charge(ctx, lease, 0, "watch.health")
	if err != nil {
		return nil, err
	}
	// Without a cap (the default) the tick's limit bounds the daily runs,
	// and flowNudgeRunsPerPass the nudged ones.
	room := limit + flowNudgeRunsPerPass
	if budget > 0 {
		room = (budget - used - watchConnectCalls - 1) / HealthMaxCalls
	}
	if room < limit {
		limit = room
	}
	runs := []any{}
	if room <= 0 {
		return map[string]any{"runs": runs, "deferred": true}, nil
	}
	// A nudge's pass asks only for nudged runs: the daily ones wait for the
	// tick.
	nudgesOnly, _ := c.a["nudges_only"].(bool)
	status, _, err := readFlowPolicy(ctx, c.tx, c.pid)
	if err != nil {
		return nil, err
	}
	flow := flowOn(status, c.s.projectFlowProven)
	rows, err := query(ctx, c.tx, `SELECT linear_project_id,policy FROM project_health WHERE project_id=? AND enabled=1 AND next_health_at<=? ORDER BY next_health_at,linear_project_id`, c.pid, layout(c.now))
	if err != nil {
		return nil, err
	}
	// In a nudge's pass, a Linear project whose daily run is due is left
	// to the tick, and its ask waits for it (R101-M1).
	wait := map[string]bool{}
	if nudgesOnly {
		for _, r := range rows {
			wait[str(r, "linear_project_id")] = true
		}
		if flow {
			due, err := c.flowDue(ctx)
			if err != nil {
				return nil, err
			}
			for _, lp := range due {
				wait[lp] = true
			}
		}
		rows = nil
	}
	// A run that reads for health is health's; a flow-only run and a
	// nudged one are project flow's.
	cost, flowCost := 0, 0
	for _, r := range rows {
		if len(runs) == limit {
			break
		}
		lp := str(r, "linear_project_id")
		if healthBound(ctx, c.tx, c.pid, lp) != nil {
			continue
		}
		var policy HealthPolicy
		if json.Unmarshal([]byte(str(r, "policy")), &policy) != nil {
			continue
		}
		runs = append(runs, map[string]any{"linear_project": lp, "stalled_days": policy.StalledDays, "health": true, "flow": flow})
		cost += HealthMaxCalls
	}
	// A4: with health off, project flow has its own daily run, in the same
	// slot, that reads only what the rules need (§11.2).
	if flow && len(runs) < limit && !nudgesOnly {
		due, err := c.flowDue(ctx)
		if err != nil {
			return nil, err
		}
		for _, lp := range due {
			if len(runs) == limit {
				break
			}
			runs = append(runs, map[string]any{"linear_project": lp, "health": false, "flow": true})
			flowCost += FlowReadCalls
		}
	}
	// Nudged runs come after the daily ones, which read the same and more,
	// with their own allowance (R101-L2).
	if nudgeRoom := min(flowNudgeRunsPerPass, room-len(runs)); flow && nudgeRoom > 0 {
		have := map[string]bool{}
		for _, r := range runs {
			have[str(r.(map[string]any), "linear_project")] = true
		}
		nudged, more, err := c.flowNudgeRuns(ctx, have, wait, nudgeRoom)
		if err != nil {
			return nil, err
		}
		runs, flowCost = append(runs, nudged...), flowCost+more
	}
	if len(runs) > 0 {
		if used, _, err = c.charge(ctx, map[string]any{"budget_used": used, "budget_hour": layout(c.now)[:13], "budget_limit": budget}, cost, "watch.health"); err != nil {
			return nil, err
		}
		if _, _, err = c.charge(ctx, map[string]any{"budget_used": used, "budget_hour": layout(c.now)[:13], "budget_limit": budget}, flowCost, "watch.flow"); err != nil {
			return nil, err
		}
	}
	return map[string]any{"runs": runs}, nil
}

// healthForce hands the lease holder one forced run of a Linear project
// (`operator health-run`), whatever its slot: only where health is enabled
// and the policy posts, only while still bound, and only when the hour's
// budget can pay it in full beside the comment watch's reserve. It is
// charged HealthMaxCalls like a scheduled run, and audited, with whether
// the run may bypass the hysteresis and the day's post (force_new).
func (c commentCall) healthForce(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	lp, err := lowerUUID(c.a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	forceNew, _ := c.a["force_new"].(bool)
	row, err := one(ctx, c.tx, `SELECT policy FROM project_health WHERE project_id=? AND linear_project_id=? AND enabled=1`, c.pid, lp)
	if err != nil {
		return nil, scopedError(err, "project health is not enabled for that Linear project", "not_found")
	}
	if err = healthBound(ctx, c.tx, c.pid, lp); err != nil {
		return nil, err
	}
	var policy HealthPolicy
	if json.Unmarshal([]byte(str(row, "policy")), &policy) != nil {
		return nil, errors.New("stored health policy is unreadable")
	}
	if !policy.Post {
		return nil, scoped("a forced health run needs the project's health policy to post; set post first", "forbidden")
	}
	used, budget, err := c.charge(ctx, lease, 0, "watch.health")
	if err != nil {
		return nil, err
	}
	if budget > 0 && budget-used-watchConnectCalls-1 < HealthMaxCalls {
		return nil, scoped(fmt.Sprintf("the hour's Linear call budget cannot pay a health run now (%d of %d calls used); try again at %s, or raise it with project linear-budget set", used, budget, c.now.UTC().Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339)), "rate_limited")
	}
	if _, _, err = c.charge(ctx, map[string]any{"budget_used": used, "budget_hour": layout(c.now)[:13], "budget_limit": budget}, HealthMaxCalls, "watch.health"); err != nil {
		return nil, err
	}
	if err = c.operatorAudit(ctx, lp, "run_forced", map[string]any{"force_new": forceNew}); err != nil {
		return nil, err
	}
	return map[string]any{"linear_project": lp, "stalled_days": policy.StalledDays}, nil
}

// flowDue is the bound Linear projects without enabled health whose
// project flow run is due: never run, or past their next slot.
func (c commentCall) flowDue(ctx context.Context) ([]string, error) {
	rows, err := query(ctx, c.tx, `SELECT mapping,mapping_version FROM taskboard_bindings WHERE project_id=? AND mapping IS NOT NULL`, c.pid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	b, err := readStoredMapping(str(rows[0], "mapping"), asInt(rows[0]["mapping_version"]))
	if err != nil || b.Mode != "project_set" {
		return nil, nil
	}
	healthy, err := query(ctx, c.tx, `SELECT linear_project_id FROM project_health WHERE project_id=? AND enabled=1`, c.pid)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, r := range healthy {
		skip[str(r, "linear_project_id")] = true
	}
	states, err := query(ctx, c.tx, `SELECT linear_project_id,next_flow_at FROM project_flow_state WHERE project_id=?`, c.pid)
	if err != nil {
		return nil, err
	}
	next := map[string]string{}
	for _, r := range states {
		next[str(r, "linear_project_id")] = str(r, "next_flow_at")
	}
	type due struct{ lp, at string }
	list := []due{}
	for _, lp := range b.LinearProjectIDs {
		if skip[lp] {
			continue
		}
		if at := next[lp]; at == "" || at <= layout(c.now) {
			list = append(list, due{lp, at})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].at != list[j].at {
			return list[i].at < list[j].at
		}
		return list[i].lp < list[j].lp
	})
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.lp)
	}
	return out, nil
}

// healthReport records one run: its calls (the unused part of
// HealthMaxCalls is refunded), and either a failure code or its facts.
// From facts it computes the health, and acts only on a health confirmed by
// two consecutive runs (hysteresis), or weekly when the policy asks: the
// coordinator session is told. The next run is the project's next slot.
//
// A forced run (forced) never moves the project's slot. Without force_new
// it records no observation: it acts only on a health pending since an
// earlier day, as the scheduled run would. With force_new it acts on this
// run alone, posts as "forced", and may take the day from a post that is
// no longer on Linear; both bypasses are audited. Every other post rule
// holds.
func (c commentCall) healthReport(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	lp, err := lowerUUID(c.a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	calls, err := integer(c.a["calls"])
	if err != nil || calls < 0 || calls > HealthMaxCalls {
		return nil, invalid(fmt.Sprintf("calls must be 0–%d", HealthMaxCalls))
	}
	row, err := one(ctx, c.tx, `SELECT * FROM project_health WHERE project_id=? AND linear_project_id=? AND enabled=1`, c.pid, lp)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	healthOn := err == nil
	forced, _ := c.a["forced"].(bool)
	forceNew, _ := c.a["force_new"].(bool)
	if forceNew && !forced {
		return nil, invalid("force_new needs forced")
	}
	if forced && !healthOn {
		return nil, scoped("project health is not enabled for that Linear project", "not_found")
	}
	// A nudged run reads for project flow only, whatever health does, and
	// is never forced: a forced run is health's alone.
	nudge := str(c.a, "nudge")
	if nudge != "" {
		if forced {
			return nil, invalid("a nudged run is never forced")
		}
		if err = c.takeFlowNudge(ctx, lp, nudge); err != nil {
			return nil, err
		}
		healthOn = false
	}
	status, revision, err := readFlowPolicy(ctx, c.tx, c.pid)
	if err != nil {
		return nil, err
	}
	// A forced run is health's alone: project flow keeps its own slot.
	flow := !forced && flowOn(status, c.s.projectFlowProven) && healthBound(ctx, c.tx, c.pid, lp) == nil
	switch {
	case !flow && nudge != "":
		// Flow was turned off after the nudge was handed out.
		_, _, err = c.charge(ctx, lease, calls-flowNudgeCalls(nudge), "watch.flow")
		return map[string]any{"flow": "off"}, err
	case !healthOn && !flow:
		return nil, scoped("project health and project flow are both off for that Linear project", "not_found")
	}
	// The run's difference goes to the kind it was charged as: health's
	// when it read for health, else project flow's.
	charged, kind := FlowReadCalls, "watch.flow"
	switch {
	case nudge != "":
		charged = flowNudgeCalls(nudge)
	case healthOn:
		charged, kind = HealthMaxCalls, "watch.health"
	}
	if _, _, err = c.charge(ctx, lease, calls-charged, kind); err != nil {
		return nil, err
	}
	code := str(c.a, "error")
	if code != "" && (!commentOperationID.MatchString(code) || len(code) > 64) {
		code = "failed"
	}
	out := map[string]any{}
	if flow {
		var facts *FlowFacts
		flowCode := code
		if raw, ok := c.a["flow"]; ok && raw != nil {
			// Facts the bus can't use fail this run's flow alone: health is
			// still recorded and the slot still advances (R96-H2).
			if f, e := parseFlowFacts(raw); e != nil {
				flowCode = "flow_facts_invalid"
			} else {
				if nudge == "r1" {
					// An R1 nudge reads the project only (SF4-M2 a): the
					// issue ClawBridge just started is the one started
					// issue, and no read of every issue is claimed.
					f.Total, f.Started, f.Completed, f.Canceled, f.CompletedSinceCreated, f.Complete = 1, 1, 0, 0, 0, false
				}
				facts = &f
			}
		}
		move, what, e := c.projectFlow(ctx, lp, status, revision, facts, flowCode, nudge != "")
		if e != nil {
			return nil, e
		}
		out["flow"] = what
		if move != nil {
			out["move"] = move
		}
	}
	if !healthOn {
		return out, nil
	}
	nowRaw := layout(c.now)
	if code != "" {
		next := layout(c.now.Add(healthRetry))
		if forced {
			next = str(row, "next_health_at")
		}
		_, err = c.tx.ExecContext(ctx, `UPDATE project_health SET last_run_at=?,last_error=?,next_health_at=? WHERE project_id=? AND linear_project_id=?`, nowRaw, code, next, c.pid, lp)
		out["error"] = code
		return out, err
	}
	facts, err := parseHealthFacts(c.a["facts"])
	if err != nil {
		return nil, err
	}
	var policy HealthPolicy
	if json.Unmarshal([]byte(str(row, "policy")), &policy) != nil {
		return nil, errors.New("stored health policy is unreadable")
	}
	if err = c.settleHealthPosts(ctx, lp, facts); err != nil {
		return nil, err
	}
	health, signals := ComputeHealth(facts, policy, c.now)
	last, pending := str(row, "last_health"), str(row, "pending_health")
	pendingSince, actedAt := row["pending_since"], row["last_acted_at"]
	act := ""
	switch {
	case health == "":
		// Nothing to judge leaves the hysteresis as it is.
	case health == last:
		pending, pendingSince = "", nil
		if policy.Weekly && (actedAt == nil || str(row, "last_acted_at") <= layout(c.now.Add(-7*24*time.Hour))) {
			act = "weekly"
		}
	case health == pending:
		act = "changed"
	default:
		pending, pendingSince = health, nowRaw
	}
	note := ""
	switch {
	case !forced:
	case health == "":
		note = "Nothing to judge: the project has no open or completed issues."
	case forceNew && act == "":
		if err = c.operatorAudit(ctx, lp, "hysteresis_bypassed", map[string]any{"health": health, "was": last}); err != nil {
			return nil, err
		}
		act, note = "forced", "Acted on this forced run alone, without the two daily runs (--force-new)."
	case forceNew:
		// The operator asked for a post: it is the operator's to delete.
		act = "forced"
	case act == "changed" && str(row, "pending_since") > layout(c.now.Add(-healthConfirmGap)):
		act, note = "", "This health was first seen less than a day ago; only a run on a later day confirms it. Nothing was posted."
	case act == "" && health == last:
		note = "Unchanged since the health last acted on, so nothing was posted."
	case act == "":
		note = "Needs a confirming run: this health differs from the last one acted on, and only a later daily run with the same health acts on it (or --force-new). A forced run records no observation. Nothing was posted."
	}
	if forced && !forceNew && act == "" {
		// A plain forced run is not one of the daily observations.
		pending, pendingSince = str(row, "pending_health"), row["pending_since"]
	}
	var post map[string]any
	held := ""
	if act != "" {
		if post, held, err = c.healthPostGo(ctx, lp, health, last, act, policy, facts, signals, forceNew); err != nil {
			return nil, err
		}
		if post == nil {
			if err = c.healthNotice(ctx, lp, health, last, act, signals, held); err != nil {
				return nil, err
			}
		}
		last, pending, pendingSince, actedAt = health, "", nil, nowRaw
	}
	report, _ := json.Marshal(map[string]any{"health": health, "signals": signals, "at": nowRaw, "facts": facts})
	next := layout(nextHealthAt(lp, c.now))
	if forced {
		next = str(row, "next_health_at")
	}
	if _, err = c.tx.ExecContext(ctx, `UPDATE project_health SET last_run_at=?,last_error='',last_report=?,last_health=?,pending_health=?,pending_since=?,last_acted_at=?,next_health_at=? WHERE project_id=? AND linear_project_id=?`, nowRaw, string(report), last, pending, pendingSince, actedAt, next, c.pid, lp); err != nil {
		return nil, err
	}
	out["health"], out["acted"], out["pending_health"], out["next_health_at"] = health, act, pending, next
	if post != nil {
		out["post"] = post
	}
	if forced {
		out["note"], out["held"] = note, held
	}
	return out, nil
}

// healthPostGo decides whether an acted-on health is posted to Linear. It
// returns the post the watcher sends, after inserting its pending row, or
// nil and why it was held back ("" when the policy doesn't post).
//
// With forceNew the day's post is replaced only when it is not on Linear:
// refused, or a forced post the operator deleted. Its ledger row is
// deleted, audited, and the new post takes the day. A pending, accepted
// or failed one never is, so no post on Linear loses its ledger row.
func (c commentCall) healthPostGo(ctx context.Context, lp, health, was, act string, policy HealthPolicy, facts HealthFacts, signals []HealthSignal, forceNew bool) (map[string]any, string, error) {
	if !policy.Post {
		return nil, "", nil
	}
	if facts.Updates == nil {
		return nil, "The project's updates could not be read, so nothing was posted to Linear.", nil
	}
	if u, ok := newestUpdate(facts.Updates); ok && !u.ByApp && c.now.Sub(u.created()) < healthHumanQuiet {
		return nil, "A person posted a project update in the last 7 days, so nothing was posted to Linear.", nil
	} else if ok && len(facts.Updates) == HealthUpdatesRead && c.now.Sub(u.created()) >= healthHumanQuiet {
		// A full read whose newest is old may not hold the newest at all
		// (if Linear answered the oldest first), so a person's recent
		// update could be missing from it (R75-M1).
		return nil, "The project's updates read were all older than 7 days although more may exist, so nothing was posted to Linear.", nil
	}
	open, err := query(ctx, c.tx, `SELECT day FROM health_posts WHERE project_id=? AND linear_project_id=? AND state='pending'`, c.pid, lp)
	if err != nil {
		return nil, "", err
	}
	if len(open) > 0 {
		return nil, "An earlier post to Linear is not confirmed yet, so nothing more was posted.", nil
	}
	day := layout(c.now)[:10]
	if forceNew {
		old, err := query(ctx, c.tx, `SELECT state,act,update_id,error FROM health_posts WHERE project_id=? AND linear_project_id=? AND day=?`, c.pid, lp, day)
		if err != nil {
			return nil, "", err
		}
		if len(old) == 1 {
			gone := str(old[0], "state") == "refused" || str(old[0], "state") == "accepted" && str(old[0], "act") == "forced" && str(old[0], "error") == "deleted"
			if !gone {
				return nil, "Today's post, " + str(old[0], "update_id") + ", may still be on Linear, so nothing more was posted; a forced post is deleted with operator health-run --delete.", nil
			}
			if _, err = c.tx.ExecContext(ctx, `DELETE FROM health_posts WHERE project_id=? AND linear_project_id=? AND day=?`, c.pid, lp, day); err != nil {
				return nil, "", err
			}
			if err = c.operatorAudit(ctx, lp, "post_replaced", map[string]any{"day": day, "state": str(old[0], "state"), "update_id": str(old[0], "update_id")}); err != nil {
				return nil, "", err
			}
		}
	}
	res, err := c.tx.ExecContext(ctx, `INSERT INTO health_posts(project_id,linear_project_id,day,health,was,act,state,sent_at) VALUES(?,?,?,?,?,?,'pending',?) ON CONFLICT DO NOTHING`, c.pid, lp, day, health, was, act, layout(c.now))
	if err != nil {
		return nil, "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, "ClawBridge already posted this project's health today, so nothing more was posted.", nil
	}
	return map[string]any{"day": day, "health": health, "body": healthPostBody(health, was, act, signals)}, "", nil
}

// newestUpdate is the most recently created of updates.
func newestUpdate(updates []HealthUpdate) (HealthUpdate, bool) {
	var newest HealthUpdate
	for _, u := range updates {
		if newest.CreatedAt == "" || u.created().After(newest.created()) {
			newest = u
		}
	}
	return newest, newest.CreatedAt != ""
}

// healthPostBody is a project update's text, from typed fields only: the
// marker, the health and each signal. Issues are named by identifier.
func healthPostBody(health, was, act string, signals []HealthSignal) string {
	var b strings.Builder
	b.WriteString(HealthPostMarker + "\n\n")
	switch {
	case act == "weekly":
		fmt.Fprintf(&b, "%s (weekly summary).", healthWords[health])
	case act == "forced":
		fmt.Fprintf(&b, "%s (a run the server operator forced).", healthWords[health])
	case was == "":
		fmt.Fprintf(&b, "%s (the same on two daily runs).", healthWords[health])
	default:
		fmt.Fprintf(&b, "Now %s, was %s (the same on two daily runs).", healthWords[health], healthWords[was])
	}
	b.WriteString("\n")
	for _, s := range signals {
		fmt.Fprintf(&b, "\n- %s: %s. %s.", s.Signal, healthWords[s.Health], s.Detail)
	}
	b.WriteString("\n\nComputed by the ClawBridge server from Linear facts. Completion counts issues, not estimates.")
	return b.String()
}

// settleHealthPosts settles this Linear project's pending posts from a
// run's updates: an update by the app created after the send is the post.
// One still unfound a full extra day later settles failed, and the
// coordinator session is told. It never sends again.
func (c commentCall) settleHealthPosts(ctx context.Context, lp string, facts HealthFacts) error {
	rows, err := query(ctx, c.tx, `SELECT day,health,was,act,sent_at FROM health_posts WHERE project_id=? AND linear_project_id=? AND state='pending' ORDER BY day`, c.pid, lp)
	if err != nil || len(rows) == 0 {
		return err
	}
	known, err := query(ctx, c.tx, `SELECT update_id FROM health_posts WHERE project_id=? AND linear_project_id=? AND update_id<>''`, c.pid, lp)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, k := range known {
		taken[str(k, "update_id")] = true
	}
	for _, r := range rows {
		// The post is the app's update created nearest its send, of those
		// after it and no other post's.
		sent := at(str(r, "sent_at"))
		found := ""
		var gap time.Duration
		for _, u := range facts.Updates {
			id := strings.ToLower(u.ID)
			if !u.ByApp || u.created().Before(sent.Add(-healthPostSkew)) || taken[id] {
				continue
			}
			if d := u.created().Sub(sent).Abs(); found == "" || d < gap {
				found, gap = id, d
			}
		}
		if found != "" {
			taken[found] = true
		}
		switch {
		case found != "":
			err = c.settleHealthPost(ctx, lp, str(r, "day"), "accepted", found, "found")
		case c.now.Sub(sent) >= healthPostSettle:
			err = c.settleHealthPost(ctx, lp, str(r, "day"), "failed", "", "not_found")
			if err == nil {
				err = c.healthNoticeFromReport(ctx, lp, r, "A post of this health to Linear could not be confirmed: it may or may not be there.")
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c commentCall) settleHealthPost(ctx context.Context, lp, day, state, updateID, code string) error {
	if _, err := c.tx.ExecContext(ctx, `UPDATE health_posts SET state=?,update_id=?,error=?,settled_at=? WHERE project_id=? AND linear_project_id=? AND day=? AND state='pending'`, state, updateID, code, layout(c.now), c.pid, lp, day); err != nil {
		return err
	}
	return c.s.audit(ctx, c.tx, c.wid, c.pid, c.sid, "project_health", lp, "post_"+state, map[string]any{"day": day, "update_id": updateID, "error": code})
}

// healthPostSettle records what the watcher's send did: accepted with the
// update's ID; refused before Linear took it, when the coordinator
// session is told instead; or uncertain, when the row stays pending for a
// later run to find. Only a pending row settles, and the send is charged
// one call.
func (c commentCall) healthPostSettle(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	lp, err := lowerUUID(c.a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	day := str(c.a, "day")
	if _, e := time.Parse(time.DateOnly, day); e != nil {
		return nil, invalid("day must be YYYY-MM-DD")
	}
	row, err := one(ctx, c.tx, `SELECT day,health,was,act,state FROM health_posts WHERE project_id=? AND linear_project_id=? AND day=?`, c.pid, lp, day)
	if err != nil {
		return nil, scopedError(err, "no health post for that Linear project and day", "not_found")
	}
	if str(row, "state") != "pending" {
		return map[string]any{"state": str(row, "state")}, nil
	}
	if _, _, err = c.charge(ctx, lease, 1, "watch.health"); err != nil {
		return nil, err
	}
	code := str(c.a, "error")
	if code != "" && (!commentOperationID.MatchString(code) || len(code) > 64) {
		code = "failed"
	}
	switch outcome := str(c.a, "outcome"); outcome {
	case "accepted":
		id, e := lowerUUID(c.a, "update_id", true)
		if e != nil {
			return nil, e
		}
		err = c.settleHealthPost(ctx, lp, day, "accepted", id, "")
	case "refused":
		if code == "" {
			code = "refused"
		}
		if err = c.settleHealthPost(ctx, lp, day, "refused", "", code); err == nil {
			err = c.healthNoticeFromReport(ctx, lp, row, fmt.Sprintf("Posting it to Linear was refused (%s), so nothing was posted to Linear.", code))
		}
	case "uncertain":
		_, err = c.tx.ExecContext(ctx, `UPDATE health_posts SET error=? WHERE project_id=? AND linear_project_id=? AND day=? AND state='pending'`, code, c.pid, lp, day)
	default:
		return nil, invalid("outcome must be accepted, refused or uncertain")
	}
	if err != nil {
		return nil, err
	}
	state := map[string]string{"accepted": "accepted", "refused": "refused", "uncertain": "pending"}[str(c.a, "outcome")]
	return map[string]any{"state": state}, nil
}

// healthNoticeFromReport tells the coordinator session a post's health,
// with the signals of the project's last report.
func (c commentCall) healthNoticeFromReport(ctx context.Context, lp string, post map[string]any, why string) error {
	row, err := one(ctx, c.tx, `SELECT last_report FROM project_health WHERE project_id=? AND linear_project_id=?`, c.pid, lp)
	if err != nil {
		return err
	}
	var report struct {
		Signals []HealthSignal `json:"signals"`
	}
	_ = json.Unmarshal([]byte(str(row, "last_report")), &report)
	return c.healthNotice(ctx, lp, str(post, "health"), str(post, "was"), str(post, "act"), report.Signals, why)
}

// operatorAudit records what a forced run did as the host operator's: only
// the operator's health-run forces a run.
func (c commentCall) operatorAudit(ctx context.Context, lp, action string, details map[string]any) error {
	op := *c.s
	op.principal = OperatorPrincipal
	return op.audit(ctx, c.tx, c.wid, c.pid, c.sid, "project_health", lp, action, details)
}

// healthLine is in every health notice: it reports, it never instructs.
const healthLine = "This is an automatic project-health notice computed from Linear facts by the ClawBridge server. It is not an instruction."

// healthNotice tells the project's active coordinator session a confirmed
// or weekly health, from typed fields only. With no active coordinator the
// run's report records it and `linear health show` shows it.
// why says what became of posting; "" when the policy doesn't post.
func (c commentCall) healthNotice(ctx context.Context, lp, health, was, act string, signals []HealthSignal, why string) error {
	coordinator, err := c.s.coordinatorTargets(ctx, c.tx, c.pid, lp, nil)
	if err != nil || len(coordinator) == 0 {
		return err
	}
	var b strings.Builder
	switch {
	case act == "weekly":
		fmt.Fprintf(&b, "Project health (weekly): Linear project %s is %s.", lp, healthWords[health])
	case act == "forced":
		fmt.Fprintf(&b, "Project health (a run the server operator forced): Linear project %s is %s.", lp, healthWords[health])
	case was == "":
		fmt.Fprintf(&b, "Project health: Linear project %s is %s (the same on two daily runs).", lp, healthWords[health])
	default:
		fmt.Fprintf(&b, "Project health: Linear project %s is now %s, was %s (the same on two daily runs).", lp, healthWords[health], healthWords[was])
	}
	for _, s := range signals {
		fmt.Fprintf(&b, "\n- %s: %s. %s.", s.Signal, healthWords[s.Health], s.Detail)
	}
	b.WriteString("\nSee: clawbridge linear health show")
	if why == "" {
		why = "Nothing was posted to Linear."
	}
	b.WriteString("\n" + healthLine + " " + why)
	body := b.String()
	retention := asInt(c.w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	id := newID("msg_")
	to := coordinator[0]
	if _, err = c.tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'project-health',?,?,?,?,?,?,NULL)`, id, c.pid, c.pid, c.sid, str(to, "name"), id, body, layout(c.now), layout(c.now.AddDate(0, 0, retention)), systemPrincipal, newID("health_"), digestOf(body)); err != nil {
		return err
	}
	return insertDeliveries(ctx, c.tx, c.pid, id, []string{str(to, "id")})
}
