package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Reading and setting a bound Linear project's status (live/
// PROJECT-STATUS-PLAN.md). The server's AppWriter reads and writes Linear
// as the app; these operations are its steps around those calls:
// project_status_begin authorizes and charges the Linear calls,
// project_status_claim records a set before it is sent, and
// project_status_settle records what became of it. A client never calls
// them itself (WithAppStep).
const (
	// ProjectStatusIssuePages bounds the issue pages a read takes; past
	// them its counts are partial.
	ProjectStatusIssuePages = 5
	// ProjectStatusReadCalls is a read's cost, charged before it is made:
	// the project with the status list, and the issue pages.
	ProjectStatusReadCalls = 1 + ProjectStatusIssuePages
	// ProjectStatusSetCalls is what a set adds: the write and its read-back.
	ProjectStatusSetCalls = 2
	// projectStatusInProgress is how long a claimed set without an outcome
	// is taken to be still sending: the server's write deadline plus a
	// margin. An older one is uncertain.
	projectStatusInProgress = 75 * time.Second
)

// ProjectStatusTypes are the project status types a set may name.
var ProjectStatusTypes = map[string]bool{"backlog": true, "planned": true, "started": true, "completed": true, "canceled": true}

type appStepKey struct{}

// WithAppStep marks ctx as the server's own AppWriter step. A request
// reaches the bus with its HTTP context, which a client cannot mark.
func WithAppStep(ctx context.Context) context.Context {
	return context.WithValue(ctx, appStepKey{}, true)
}

func appStep(ctx context.Context) bool {
	on, _ := ctx.Value(appStepKey{}).(bool)
	return on
}

// projectStatusErrorCode is a settled set's failure code: never Linear's
// text.
var projectStatusErrorCode = regexp.MustCompile(`^[a-z_]{0,40}$`)

func projectStatusOperation(op string) bool {
	return op == "project_status_begin" || op == "project_status_claim" || op == "project_status_settle"
}

// projectStatusRecord is a set's record in the operation ledger, kept per
// caller and Linear project under its operation_id (M4).
type projectStatusRecord struct {
	Version  int    `json:"v"`
	Digest   string `json:"digest"`
	State    string `json:"state"`
	Seq      int    `json:"seq"`
	Held     string `json:"held,omitempty"`
	FromType string `json:"from_type"`
	ToType   string `json:"to_type"`
	ToStatus string `json:"to_status_id"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

func (s *Store) projectStatusOp(ctx context.Context, tx *transaction, wid, pid string, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	if !appStep(ctx) {
		return nil, scoped("project status is read and set through linear project status, which the server runs as its ClawBridge app", "forbidden")
	}
	a := r.Args
	lp, err := lowerUUID(a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	settle := r.Operation == "project_status_settle"
	set := settle || r.Operation == "project_status_claim" || a["set"] == true
	// A settle records what a claimed set became whatever changed since:
	// the record's key holds its caller and Linear project.
	if !settle {
		if actor != nil && actor["status"] != "active" {
			return nil, invalid("session is retired or moved; it cannot perform this operation")
		}
		if set {
			if err = s.projectStatusSetter(ctx, tx, pid, p, projectRole, actor, r.Operation, a); err != nil {
				return nil, err
			}
		}
		// M1: the Linear project is in this project's binding, read in the
		// same transaction as the charge or the claim.
		if err = linearProjectBound(ctx, tx, pid, lp, "project status"); err != nil {
			return nil, err
		}
	}
	policy, _, err := readFlowPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	on := flowOn(policy, s.projectFlowProven)
	out := map[string]any{"linear_project": lp, "agent_closes": policy.AgentCloses(lp)}
	if out["flow"], err = flowStateOf(ctx, tx, pid, lp, on); err != nil {
		return nil, err
	}
	if !set {
		return out, s.chargeProjectStatus(ctx, tx, pid, ProjectStatusReadCalls)
	}
	toType := str(a, "to_type")
	if !ProjectStatusTypes[toType] {
		return nil, invalid("to_type must be backlog, planned, started, completed or canceled")
	}
	// Who closes the project's issues closes the project: where people do,
	// no session sets it completed or canceled, the coordinator's included.
	if !settle && (toType == "completed" || toType == "canceled") && actor != nil && !policy.AgentCloses(lp) {
		return nil, scoped(fmt.Sprintf("people close Linear project %s (its close rule in the status policy is human): only the project owner or a lead sets it %s, in their own terminal with no session", lp, toType), "forbidden")
	}
	ns, id, hash, err := s.projectStatusKey(pid, actor, lp, a)
	if err != nil {
		return nil, err
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if settle {
		return s.settleProjectStatus(ctx, tx, wid, pid, lp, ns, id, hash, now, a, on)
	}
	recorded, err := projectStatusRecorded(ctx, tx, ns, id, hash, at(now))
	if err != nil {
		return nil, err
	}
	if recorded != nil {
		// A replay costs no Linear call: the recorded outcome answers.
		out["recorded"] = recorded
		return out, nil
	}
	if r.Operation == "project_status_begin" {
		// A preview reads; a confirmed set also writes and reads back.
		calls := ProjectStatusReadCalls
		if a["confirm"] == true {
			calls += ProjectStatusSetCalls
		}
		return out, s.chargeProjectStatus(ctx, tx, pid, calls)
	}
	return s.claimProjectStatus(ctx, tx, wid, pid, lp, ns, id, hash, now, a, out)
}

// projectStatusSetter: the project's designated coordinator session,
// unbounded, or the project owner or a lead without a session. While the
// dispatcher is on, a domain's coordinator session sets too, for a project
// whose teams are all write teams of its own lane: the teams are the
// server's own Linear read, which only the claim carries, so begin lets a
// domain coordinator through and the claim, before any Linear write,
// enforces the bound. Any other session is refused, an owner's or a
// lead's included.
func (s *Store) projectStatusSetter(ctx context.Context, tx *transaction, pid string, p map[string]any, projectRole string, actor map[string]any, op string, a map[string]any) error {
	if actor == nil {
		if projectRole != "owner" && projectRole != "lead" {
			return scoped("setting a Linear project's status needs the project owner or a lead", "forbidden")
		}
		return nil
	}
	// The designated coordinator seat, whoever holds it (the dispatcher's
	// system session included), sets unbounded, as before.
	if str(p, "coordinator_session") != "" && p["coordinator_session"] == actor["id"] {
		return nil
	}
	ok, err := isLinearCoordinator(ctx, tx, pid, actor)
	if err != nil {
		return err
	}
	if !ok {
		return scoped("only the project's designated coordinator session or, while the dispatcher is on, a domain's coordinator session (for a project whose teams are all in its lane) sets a Linear project's status; other sessions read it", "forbidden")
	}
	// Linear may leave a private team out of a project's team list with
	// no sign, so the bound check below can't be trusted until the operator
	// has probed that the app sees such a team.
	if !s.projectStatusDomainProven {
		return scoped("project_status_domain_unproven: a domain coordinator's status set waits for the operator to prove that the app sees a project's private teams (serve --project-status-domain-proven); the project owner or a lead sets the status", "forbidden")
	}
	if op != "project_status_claim" {
		return nil
	}
	b, _, err := storedBinding(ctx, tx, pid)
	if err != nil {
		return err
	}
	if len(b.Lanes) == 0 {
		return scoped("outside_lane: this binding has no lanes, so a coordinator sets no project's status; the project owner or a lead does", "forbidden")
	}
	lane, writes, _, _, err := s.sessionLane(ctx, tx, pid, actor)
	if err != nil {
		return err
	}
	if _, exists := b.Lane(lane); !exists {
		return scoped("outside_lane: the coordinator session has no lane, so it sets no project's status; the project owner pins it with linear lanes pin", "forbidden")
	}
	teams, err := uuidList(a, "project_teams")
	if err != nil {
		return err
	}
	if more, _ := a["project_teams_more"].(bool); more || len(teams) == 0 {
		return scoped(fmt.Sprintf("outside_lane: the project's teams weren't read in full, so a coordinator of lane %s can't tell they are all its own; the project owner or a lead sets its status", lane), "forbidden")
	}
	inLane := map[string]bool{}
	for _, t := range writes {
		inLane[strings.ToLower(t)] = true
	}
	for _, t := range teams {
		if !inLane[strings.ToLower(t)] {
			return scoped(fmt.Sprintf("outside_lane: the project carries team %s, which isn't a write team of lane %s; the project owner or a lead sets the status of a cross-lane project", t, lane), "forbidden")
		}
	}
	return nil
}

// projectStatusKey is a set's ledger key: the caller and the Linear
// project name the namespace, and the request's own fields its digest, so
// one operation_id is one request of one caller.
func (s *Store) projectStatusKey(pid string, actor map[string]any, lp string, a map[string]any) (ns, id, hash string, err error) {
	id = str(a, "operation_id")
	if !ledgerOperationID.MatchString(id) {
		return "", "", "", invalid("operation_id requires 1–128 ASCII identifier characters")
	}
	caller := "person:" + s.principal
	if actor != nil {
		caller = "session:" + str(actor, "id")
	}
	fields := []string{lp, str(a, "to_type")}
	for _, k := range []string{"status_id", "expected_status_id"} {
		v, e := lowerUUID(a, k, false)
		if e != nil {
			return "", "", "", e
		}
		fields = append(fields, v)
	}
	return digestOf("project_status", pid, caller, lp), id, digestOf(fields...), nil
}

// readProjectStatusRecord reads a set's ledger record, if any. The same
// operation_id with another request is refused.
func readProjectStatusRecord(ctx context.Context, tx *transaction, ns, id, hash string) (*projectStatusRecord, string, error) {
	rows, err := query(ctx, tx, `SELECT record FROM linear_write_ledger WHERE namespace=? AND operation_id=?`, ns, id)
	if err != nil || len(rows) == 0 {
		return nil, "", err
	}
	raw := str(rows[0], "record")
	var rec projectStatusRecord
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.Version != 1 || rec.At == "" {
		return nil, "", errors.New("corrupt operation ledger")
	}
	if rec.Digest != hash {
		return nil, "", invalid("operation_id already used with a different project status request; use a new operation_id")
	}
	return &rec, raw, nil
}

// projectStatusRecorded is what a recorded set answers its operation_id:
// nil when there is none. One still being sent is in progress; a set whose
// outcome was never recorded is uncertain, and it is never sent again.
func projectStatusRecorded(ctx context.Context, tx *transaction, ns, id, hash string, now time.Time) (map[string]any, error) {
	rec, _, err := readProjectStatusRecord(ctx, tx, ns, id, hash)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.State == "pending" {
		if now.Sub(at(rec.At)) < projectStatusInProgress {
			return nil, scoped("this status set is being sent to Linear by another request; retry the same operation_id shortly", "in_progress")
		}
		rec.State, rec.Error = "uncertain", "unsettled"
	}
	out := map[string]any{"state": rec.State, "from_type": rec.FromType, "to_type": rec.ToType, "to_status_id": rec.ToStatus}
	if rec.Error != "" {
		out["error"] = rec.Error
	}
	return out, nil
}

// claimProjectStatus records a set before it is sent, in one transaction
// with the binding check: the ledger row, a pending project flow move
// (flow's never-twice guard, so neither sends while the other is in
// flight, M3) and, when flow is on, the hold that keeps flow from moving
// the project afterwards.
func (s *Store) claimProjectStatus(ctx context.Context, tx *transaction, wid, pid, lp, ns, id, hash, now string, a map[string]any, out map[string]any) (any, error) {
	from, to := str(a, "from_type"), str(a, "to_type")
	if !projectStatusTypes[from] {
		return nil, invalid("from_type must be a project status type")
	}
	toStatus, err := lowerUUID(a, "to_status_id", true)
	if err != nil {
		return nil, err
	}
	pending, err := query(ctx, tx, `SELECT seq,sent_at FROM project_flow_moves WHERE project_id=? AND linear_project_id=? AND state='pending' ORDER BY seq`, pid, lp)
	if err != nil {
		return nil, err
	}
	for _, m := range pending {
		if at(now).Sub(at(str(m, "sent_at"))) < flowSettleAfter {
			return nil, scoped("a status move of this Linear project is in flight; read its status again shortly", "conflict")
		}
		// No read settled it within the hour (flow may be off since): it
		// stays unconfirmed, and it is never sent again.
		if _, err = tx.ExecContext(ctx, `UPDATE project_flow_moves SET state='failed',error='not_found',settled_at=? WHERE project_id=? AND linear_project_id=? AND seq=? AND state='pending'`, now, pid, lp, asInt(m["seq"])); err != nil {
			return nil, err
		}
	}
	_, revision, err := readFlowPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	row, err := one(ctx, tx, `SELECT COALESCE(MAX(seq),0) AS seq FROM project_flow_moves WHERE project_id=? AND linear_project_id=?`, pid, lp)
	if err != nil {
		return nil, err
	}
	seq := asInt(row["seq"]) + 1
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_flow_moves(project_id,linear_project_id,seq,rule,from_type,to_type,to_status_id,policy_revision,state,sent_at) VALUES(?,?,?,'manual',?,?,?,?,'pending',?)`, pid, lp, seq, from, to, toStatus, revision, now); err != nil {
		return nil, err
	}
	rec := projectStatusRecord{Version: 1, Digest: hash, State: "pending", Seq: seq, FromType: from, ToType: to, ToStatus: toStatus, At: now}
	if out["flow"] == "on" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_flow_state(project_id,linear_project_id) VALUES(?,?) ON CONFLICT DO NOTHING`, pid, lp); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE project_flow_state SET held_since=?,held_type=? WHERE project_id=? AND linear_project_id=? AND held_since IS NULL`, now, to, pid, lp); err != nil {
			return nil, err
		}
		rec.Held, out["flow"] = now, "held"
	}
	raw, _ := json.Marshal(rec)
	if _, err = tx.ExecContext(ctx, `INSERT INTO linear_write_ledger(namespace,operation_id,record,updated_at) VALUES(?,?,?,?)`, ns, id, string(raw), now); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_status", lp, "set_sent", map[string]any{"from": from, "to": to, "to_status_id": toStatus, "operation_id": id, "seq": seq}); err != nil {
		return nil, err
	}
	out["seq"] = seq
	return out, nil
}

// projectStatusOutcomes are what a sent set became, and the flow move
// state each settles: an uncertain one is never sent again, so its move
// is failed rather than left for a read.
var projectStatusOutcomes = map[string]string{"accepted": "accepted", "refused": "refused", "uncertain": "failed"}

// settleProjectStatus records what became of a claimed set: the ledger
// record, its flow move, and one history row naming the caller. A refused
// set (nothing reached Linear) lifts the hold its claim put on.
func (s *Store) settleProjectStatus(ctx context.Context, tx *transaction, wid, pid, lp, ns, id, hash, now string, a map[string]any, on bool) (any, error) {
	outcome, code := str(a, "outcome"), str(a, "error")
	state, ok := projectStatusOutcomes[outcome]
	if !ok {
		return nil, invalid("outcome must be accepted, refused or uncertain")
	}
	if !projectStatusErrorCode.MatchString(code) || (outcome == "accepted") != (code == "") {
		return nil, invalid("error must be a short code, given for a refused or uncertain set only")
	}
	rec, _, err := readProjectStatusRecord(ctx, tx, ns, id, hash)
	if err != nil {
		return nil, err
	}
	seq, err := integer(a["seq"])
	if rec == nil || rec.State != "pending" || err != nil || seq != rec.Seq {
		return nil, scoped("no status set awaits settling under that operation_id", "conflict")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_flow_moves SET state=?,error=?,settled_at=? WHERE project_id=? AND linear_project_id=? AND seq=? AND state='pending'`, state, code, now, pid, lp, seq); err != nil {
		return nil, err
	}
	if _, ranked := projectRank[rec.ToType]; ranked && outcome == "accepted" {
		if _, err = tx.ExecContext(ctx, `UPDATE project_flow_state SET last_seen_type=?,last_seen_at=? WHERE project_id=? AND linear_project_id=?`, rec.ToType, now, pid, lp); err != nil {
			return nil, err
		}
	}
	if outcome == "refused" && rec.Held != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE project_flow_state SET held_since=NULL,held_type='' WHERE project_id=? AND linear_project_id=? AND held_since=?`, pid, lp, rec.Held); err != nil {
			return nil, err
		}
	}
	rec.State, rec.Error, rec.At = outcome, code, now
	next, _ := json.Marshal(rec)
	// The record was read in this transaction, under the workspace lock.
	if _, err = tx.ExecContext(ctx, `UPDATE linear_write_ledger SET record=?,updated_at=? WHERE namespace=? AND operation_id=?`, string(next), now, ns, id); err != nil {
		return nil, err
	}
	details := map[string]any{"from": rec.FromType, "to": rec.ToType, "to_status_id": rec.ToStatus, "outcome": outcome, "operation_id": id, "seq": seq}
	if code != "" {
		details["error"] = code
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_status", lp, "set", details); err != nil {
		return nil, err
	}
	flow, err := flowStateOf(ctx, tx, pid, lp, on)
	if err != nil {
		return nil, err
	}
	return map[string]any{"linear_project": lp, "state": outcome, "flow": flow}, nil
}

// flowStateOf says whether project flow moves lp: off, on, or held.
func flowStateOf(ctx context.Context, tx *transaction, pid, lp string, on bool) (string, error) {
	if !on {
		return "off", nil
	}
	rows, err := query(ctx, tx, `SELECT held_since FROM project_flow_state WHERE project_id=? AND linear_project_id=?`, pid, lp)
	if err != nil {
		return "", err
	}
	if len(rows) == 1 && rows[0]["held_since"] != nil {
		return "held", nil
	}
	return "on", nil
}

// chargeProjectStatus charges calls to the project's hourly watch budget
// before they are made (L4: a read refused afterwards stays charged). The
// limit is the project's own budget, else the last watcher's, else the
// default. A project no watcher ever ran for gets an expired lease row to
// carry the hour's count, which the first watcher then takes over.
func (s *Store) chargeProjectStatus(ctx context.Context, tx *transaction, pid string, calls int) error {
	b, err := s.projectBudget(ctx, tx, pid)
	if err != nil {
		return err
	}
	if b.spent(calls) {
		return scoped(fmt.Sprintf("this project's Linear budget for the hour is spent (%d of %d calls used; this needs %d); try again at %s", b.used, b.limit, calls, b.resetAt.Format(time.RFC3339)), "rate_limited")
	}
	return b.charge(ctx, tx, pid, calls, "project_status")
}

// projectBudget is a project's hourly Linear budget at the database's now:
// the calls used this hour and the limit, the project's own budget, else
// the last watcher's, else the default.
type projectBudget struct {
	hour        string
	used, limit int
	// people and agents are the hour's calls charged for personal-profile
	// writes (FR8): a person's own, and an agent session's. The rest of
	// used is the app's: the server's jobs and its app writes.
	people, agents int
	// source is where the limit comes from: "project" (its owner set it),
	// "watcher" (the last watcher's) or "default" (the server's).
	source  string
	resetAt time.Time
	row     bool
}

func (s *Store) projectBudget(ctx context.Context, tx *transaction, pid string) (projectBudget, error) {
	return s.readProjectBudget(ctx, tx, pid, forUpdate(tx))
}

// readProjectBudget reads the budget, locking its row when lock says so;
// a read-only caller (the overview) passes "".
func (s *Store) readProjectBudget(ctx context.Context, tx *transaction, pid, lock string) (projectBudget, error) {
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return projectBudget{}, err
	}
	b := projectBudget{hour: now[:13], limit: watchDefaultBudget, source: "default", resetAt: at(now).Truncate(time.Hour).Add(time.Hour)}
	rows, err := query(ctx, tx, `SELECT budget_hour,budget_used,budget_limit,budget_people,budget_agents FROM watch_lease WHERE project_id=?`+lock, pid)
	if err != nil {
		return b, err
	}
	if len(rows) == 1 {
		b.row = true
		if n := asInt(rows[0]["budget_limit"]); n > 0 {
			b.limit, b.source = n, "watcher"
		}
		if str(rows[0], "budget_hour") == b.hour {
			b.used = asInt(rows[0]["budget_used"])
			b.people, b.agents = asInt(rows[0]["budget_people"]), asInt(rows[0]["budget_agents"])
		}
	}
	if n, ok, e := watchBudgetOverride(ctx, tx, pid); e != nil {
		return b, e
	} else if ok {
		b.limit, b.source = n, "project"
	}
	return b, nil
}

// LinearBudget is a project's hourly Linear budget as status shows it.
type LinearBudget struct {
	Used     int       `json:"used"`
	Limit    int       `json:"limit"`
	Source   string    `json:"source"`
	ResetsAt time.Time `json:"window_resets_at"`
	// Capped is false when no cap is set, the default (the owner's rule
	// for v0.3.9: Linear's own rate limit, through the pacer and the
	// queues, is the brake); Limit is then 0.
	Capped bool `json:"capped"`
	// By splits the hour's calls between the app, people and agents
	// (FR8): counts only, never who.
	By LinearBudgetSplit `json:"by"`
	// Kinds are the hour's calls by the job that made them, the most
	// first; LastHour is the previous hour's. Names and counts only.
	Kinds    []LinearBudgetKind `json:"kinds,omitempty"`
	LastHour *LinearBudgetHour  `json:"last_hour,omitempty"`
}

// LinearBudgetSplit is who spent the hour's Linear calls: the app (the
// server's jobs and app writes), people (their own writes, without a
// session) and agents (sessions writing through a personal profile).
type LinearBudgetSplit struct {
	App    int `json:"app"`
	People int `json:"people"`
	Agents int `json:"agents"`
}

func (b projectBudget) view() LinearBudget {
	return LinearBudget{Used: b.used, Limit: max(b.limit, 0), Source: b.source, ResetsAt: b.resetAt.UTC(), Capped: b.capped(),
		By: LinearBudgetSplit{App: max(b.used-b.people-b.agents, 0), People: b.people, Agents: b.agents}}
}

// LinearBudgetOf reads a project's hourly Linear budget.
func (s *Store) LinearBudgetOf(ctx context.Context, pid string) (LinearBudget, error) {
	var out LinearBudget
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		b, err := s.projectBudget(ctx, tx, pid)
		if err != nil {
			return err
		}
		out, err = budgetView(ctx, tx, pid, b)
		return err
	})
	return out, err
}

// charge adds calls to the hour. A project no watcher ever ran for gets an
// expired lease row to carry the hour's count, which the first watcher
// then takes over.
// capped reports a cap set: by the project's owner, the watcher's
// --budget, or the server's CLAWBRIDGE_LINEAR_BUDGET. None is the default.
func (b projectBudget) capped() bool { return b.limit > 0 }

// spent reports a cap the hour has reached; never without a cap.
func (b projectBudget) spent(calls int) bool { return b.capped() && b.used+calls > b.limit }

func (b projectBudget) charge(ctx context.Context, tx *transaction, pid string, calls int, kind string) error {
	return b.chargeBy(ctx, tx, pid, calls, "", kind)
}

// chargeBy charges calls to the hour for the app (by ""), "people" or
// "agents", and to kind's row. A refund (negative calls) never takes a
// count below zero.
func (b projectBudget) chargeBy(ctx context.Context, tx *transaction, pid string, calls int, by, kind string) error {
	if err := recordCallKind(ctx, tx, pid, b.hour, kind, calls); err != nil {
		return err
	}
	people, agents := 0, 0
	switch by {
	case "people":
		people = calls
	case "agents":
		agents = calls
	}
	if !b.row {
		calls, people, agents = max(calls, 0), max(people, 0), max(agents, 0)
		_, err := tx.ExecContext(ctx, `INSERT INTO watch_lease(project_id,session_id,lease_token,generation,expires_at,budget_hour,budget_used,budget_limit,budget_people,budget_agents) VALUES(?,'','',0,'',?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET budget_used=CASE WHEN watch_lease.budget_hour=excluded.budget_hour THEN watch_lease.budget_used+excluded.budget_used ELSE excluded.budget_used END,budget_people=CASE WHEN watch_lease.budget_hour=excluded.budget_hour THEN watch_lease.budget_people+excluded.budget_people ELSE excluded.budget_people END,budget_agents=CASE WHEN watch_lease.budget_hour=excluded.budget_hour THEN watch_lease.budget_agents+excluded.budget_agents ELSE excluded.budget_agents END,budget_hour=excluded.budget_hour`, pid, b.hour, calls, watchDefaultBudget, people, agents)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE watch_lease SET budget_hour=?,budget_used=?,budget_people=?,budget_agents=? WHERE project_id=?`, b.hour, max(b.used+calls, 0), max(b.people+people, 0), max(b.agents+agents, 0), pid)
	return err
}

func forUpdate(tx *transaction) string {
	if tx.postgres {
		return ` FOR UPDATE`
	}
	return ""
}

// LinearBudgetRoom says whether a project's hourly Linear budget has room
// for one more write now, and when the hour rolls (§5). Agent writes count
// against it like every other call made for the project.
func (s *Store) LinearBudgetRoom(ctx context.Context, pid string) (ok bool, used, limit int, resetAt time.Time, err error) {
	err = s.ledgerTx(ctx, func(tx *transaction) error {
		b, err := s.projectBudget(ctx, tx, pid)
		ok, used, limit, resetAt = !b.spent(1), b.used, max(b.limit, 0), b.resetAt
		return err
	})
	return ok, used, limit, resetAt, err
}

// ChargeLinearBudget charges calls a write made to its project's hour, as
// kind (app_write.<operation>). It never refuses: the write was already
// admitted, and its calls were made.
func (s *Store) ChargeLinearBudget(ctx context.Context, pid string, calls int, kind string) error {
	if calls <= 0 {
		return nil
	}
	return s.ledgerTx(ctx, func(tx *transaction) error {
		return s.chargeLinearBudget(ctx, tx, pid, calls, kind)
	})
}

// ChargeLinearBudgetBy charges calls the server made for a person (by
// "people") or a session (by "agents") to its project's hour, as kind: an
// app read made under the caller, such as an issue's history
// (app_read.history). Like ChargeLinearBudget it never refuses for the
// hour; any other by is refused.
func (s *Store) ChargeLinearBudgetBy(ctx context.Context, pid string, calls int, by, kind string) error {
	if by != "people" && by != "agents" {
		return fmt.Errorf("linear budget: charge by %q: want people or agents", by)
	}
	if calls <= 0 {
		return nil
	}
	return s.ledgerTx(ctx, func(tx *transaction) error {
		return s.chargeLinearBudgetBy(ctx, tx, pid, calls, by, kind)
	})
}

func (s *Store) chargeLinearBudgetBy(ctx context.Context, tx *transaction, pid string, calls int, by, kind string) error {
	b, err := s.projectBudget(ctx, tx, pid)
	if err != nil {
		return err
	}
	return b.chargeBy(ctx, tx, pid, calls, by, kind)
}

func (s *Store) chargeLinearBudget(ctx context.Context, tx *transaction, pid string, calls int, kind string) error {
	b, err := s.projectBudget(ctx, tx, pid)
	if err != nil {
		return err
	}
	return b.charge(ctx, tx, pid, calls, kind)
}
