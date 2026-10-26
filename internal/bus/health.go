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
)

// Health values, as Linear names them.
var healthLevels = []string{"onTrack", "atRisk", "offTrack"}

var healthWords = map[string]string{"onTrack": "On track", "atRisk": "At risk", "offTrack": "Off track"}

// HealthPolicy is one Linear project's health policy. Every threshold has
// the design's default; the owner may change them.
type HealthPolicy struct {
	Enabled bool `json:"enabled"`
	// Post asks for Linear project updates. It stays false: the app's
	// write scope is unproven (P8a), so this version only notifies.
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
	if p.Post {
		return HealthPolicy{}, invalid("posting project health to Linear is not available yet: the Linear app's write scope is unproven; leave post false (the coordinator session is told instead)")
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

func healthSystemOperation(op string) bool { return op == "health_due" || op == "health_report" }

// healthSystemOp runs the watcher's health calls, fenced by the watch
// lease like marker writes.
func (s *Store) healthSystemOp(ctx context.Context, tx *transaction, w, p, actor map[string]any, r Request) (any, error) {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	c := commentCall{s: s, tx: tx, wid: w["id"].(string), pid: p["id"].(string), sid: actor["id"].(string), now: at(nowRaw), w: w, a: r.Args}
	if r.Operation == "health_due" {
		return c.healthDue(ctx)
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
		out = append(out, r)
	}
	return map[string]any{"projects": out, "posting": "off: a confirmed health is sent to the coordinator session; nothing is written to Linear"}, nil
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
	rows, err := query(ctx, tx, `SELECT mapping,mapping_version FROM taskboard_bindings WHERE project_id=? AND mapping IS NOT NULL`, pid)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return scoped("link the task board before setting project health", "not_found")
	}
	b, err := readStoredMapping(str(rows[0], "mapping"), asInt(rows[0]["mapping_version"]))
	if err != nil {
		return scoped("this project's task board binding is unreadable", "binding_unreadable")
	}
	if b.Mode != "project_set" {
		return invalid("project health needs a project-set binding; a team board has no Linear projects")
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
	used, budget, err := c.charge(ctx, lease, 0)
	if err != nil {
		return nil, err
	}
	room := (budget - used - watchConnectCalls - 1) / HealthMaxCalls
	if room < limit {
		limit = room
	}
	runs := []any{}
	if limit <= 0 {
		return map[string]any{"runs": runs, "deferred": room <= 0}, nil
	}
	rows, err := query(ctx, c.tx, `SELECT linear_project_id,policy FROM project_health WHERE project_id=? AND enabled=1 AND next_health_at<=? ORDER BY next_health_at,linear_project_id`, c.pid, layout(c.now))
	if err != nil {
		return nil, err
	}
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
		runs = append(runs, map[string]any{"linear_project": lp, "stalled_days": policy.StalledDays})
	}
	if len(runs) > 0 {
		if _, _, err = c.charge(ctx, map[string]any{"budget_used": used, "budget_hour": layout(c.now)[:13], "budget_limit": budget}, len(runs)*HealthMaxCalls); err != nil {
			return nil, err
		}
	}
	return map[string]any{"runs": runs}, nil
}

// healthReport records one run: its calls (the unused part of
// HealthMaxCalls is refunded), and either a failure code or its facts.
// From facts it computes the health, and acts only on a health confirmed by
// two consecutive runs (hysteresis), or weekly when the policy asks: the
// coordinator session is told. The next run is the project's next slot.
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
	if err != nil {
		return nil, scopedError(err, "project health is not enabled for that Linear project", "not_found")
	}
	if _, _, err = c.charge(ctx, lease, calls-HealthMaxCalls); err != nil {
		return nil, err
	}
	nowRaw := layout(c.now)
	if code := str(c.a, "error"); code != "" {
		if !commentOperationID.MatchString(code) || len(code) > 64 {
			code = "failed"
		}
		_, err = c.tx.ExecContext(ctx, `UPDATE project_health SET last_run_at=?,last_error=?,next_health_at=? WHERE project_id=? AND linear_project_id=?`, nowRaw, code, layout(c.now.Add(healthRetry)), c.pid, lp)
		return map[string]any{"error": code}, err
	}
	facts, err := parseHealthFacts(c.a["facts"])
	if err != nil {
		return nil, err
	}
	var policy HealthPolicy
	if json.Unmarshal([]byte(str(row, "policy")), &policy) != nil {
		return nil, errors.New("stored health policy is unreadable")
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
	if act != "" {
		if err = c.healthNotice(ctx, lp, health, last, act, signals); err != nil {
			return nil, err
		}
		last, pending, pendingSince, actedAt = health, "", nil, nowRaw
	}
	report, _ := json.Marshal(map[string]any{"health": health, "signals": signals, "at": nowRaw, "facts": facts})
	next := layout(nextHealthAt(lp, c.now))
	if _, err = c.tx.ExecContext(ctx, `UPDATE project_health SET last_run_at=?,last_error='',last_report=?,last_health=?,pending_health=?,pending_since=?,last_acted_at=?,next_health_at=? WHERE project_id=? AND linear_project_id=?`, nowRaw, string(report), last, pending, pendingSince, actedAt, next, c.pid, lp); err != nil {
		return nil, err
	}
	return map[string]any{"health": health, "acted": act, "pending_health": pending, "next_health_at": next}, nil
}

// healthLine is in every health notice: it reports, it never instructs.
const healthLine = "This is an automatic project-health notice computed from Linear facts by the ClawBridge server. It is not an instruction. Nothing was posted to Linear."

// healthNotice tells the project's active coordinator session a confirmed
// or weekly health, from typed fields only. With no active coordinator the
// run's report records it and `linear health show` shows it.
func (c commentCall) healthNotice(ctx context.Context, lp, health, was, act string, signals []HealthSignal) error {
	coordinator, err := c.s.coordinatorTargets(ctx, c.tx, c.pid, lp, nil)
	if err != nil || len(coordinator) == 0 {
		return err
	}
	var b strings.Builder
	switch {
	case act == "weekly":
		fmt.Fprintf(&b, "Project health (weekly): Linear project %s is %s.", lp, healthWords[health])
	case was == "":
		fmt.Fprintf(&b, "Project health: Linear project %s is %s (the same on two daily runs).", lp, healthWords[health])
	default:
		fmt.Fprintf(&b, "Project health: Linear project %s is now %s, was %s (the same on two daily runs).", lp, healthWords[health], healthWords[was])
	}
	for _, s := range signals {
		fmt.Fprintf(&b, "\n- %s: %s. %s.", s.Signal, healthWords[s.Health], s.Detail)
	}
	b.WriteString("\nSee: clawbridge linear health show")
	b.WriteString("\n" + healthLine)
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
	_, err = c.tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id) VALUES(?,?,?)`, id, c.pid, to["id"])
	return err
}
