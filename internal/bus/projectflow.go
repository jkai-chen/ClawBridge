package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// Schema 21: project status follows the work (A4, live/STATUS-FLOW-DESIGN.md
// §11). project_flow_moves is the ledger of the moves ClawBridge sent: R4's
// last move, the never-twice guard (a pending row blocks another send) and
// the audit. project_flow_state holds what the runs saw: the last status
// type (SF4-M1), the done notice's episode, and a hold. The same DDL
// serves SQLite and PostgreSQL.
const projectFlowSchemaV21 = `
CREATE TABLE project_flow_moves(project_id TEXT NOT NULL REFERENCES projects(id),linear_project_id TEXT NOT NULL,seq INTEGER NOT NULL,rule TEXT NOT NULL,from_type TEXT NOT NULL,to_type TEXT NOT NULL,to_status_id TEXT NOT NULL,policy_revision INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','accepted','refused','failed')),sent_at TEXT NOT NULL,settled_at TEXT,error TEXT NOT NULL DEFAULT '',PRIMARY KEY(project_id,linear_project_id,seq));
CREATE TABLE project_flow_state(project_id TEXT NOT NULL REFERENCES projects(id),linear_project_id TEXT NOT NULL,last_seen_type TEXT NOT NULL DEFAULT '',last_seen_at TEXT,done_key TEXT NOT NULL DEFAULT '',held_since TEXT,held_type TEXT NOT NULL DEFAULT '',resumed_at TEXT,refused_day TEXT NOT NULL DEFAULT '',next_flow_at TEXT,last_error TEXT NOT NULL DEFAULT '',PRIMARY KEY(project_id,linear_project_id));
`

// ProjectFlowUnproven refuses turning project flow on before the operator
// has run the deploy probe (§11.11 L5).
const ProjectFlowUnproven = "project flow is not yet proven on this server (serve --project-flow-proven, after the deploy probe)"

const (
	// FlowReadCalls is what a run for project flow alone may spend: the
	// project read with the status list, and the issue pages.
	FlowReadCalls = 1 + HealthIssuePages
	// flowMoveCalls is a move's save_project and its read-back.
	flowMoveCalls = 2
	// flowSettleAfter is how long a pending move waits for a read to show
	// it before it settles failed; it is never sent again blindly.
	flowSettleAfter = time.Hour
	// flowListedStatuses bounds the project status list a run reports.
	flowListedStatuses = 50
)

// SetProjectFlowProven records serve --project-flow-proven.
func (s *Store) SetProjectFlowProven(proven bool) { s.projectFlowProven = proven }

// projectRank orders the project status types a move goes through
// (backlog < planned < started < completed); paused and canceled have
// none, and ClawBridge never moves to or from them (R3).
var projectRank = map[string]int{"backlog": 0, "planned": 1, "started": 2, "completed": 3}

var projectStatusTypes = map[string]bool{"backlog": true, "planned": true, "started": true, "paused": true, "completed": true, "canceled": true}

// FlowStatus is one project status as Linear lists it. Its name is never
// read: a workspace's status names are free text the rules don't need.
type FlowStatus struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
	Archived bool    `json:"archived"`
}

// FlowFacts is what a run read for project flow, typed: the project's
// status and creation time, the status list, and issue counts. Complete is
// false when an issue page failed or the page cap stopped the read (L1).
type FlowFacts struct {
	Status    FlowStatus   `json:"status"`
	CreatedAt string       `json:"created_at"`
	Statuses  []FlowStatus `json:"statuses"`
	Total     int          `json:"total"`
	Completed int          `json:"completed"`
	Canceled  int          `json:"canceled"`
	Started   int          `json:"started"`
	// CompletedSinceCreated counts completed issues whose completedAt is
	// after the project's createdAt (L2).
	CompletedSinceCreated int  `json:"completed_since_created"`
	Complete              bool `json:"complete"`
}

func parseFlowFacts(v any) (FlowFacts, error) {
	raw, err := json.Marshal(v)
	bad := invalid("flow must be the typed project flow facts object")
	if err != nil || len(raw) > 32<<10 {
		return FlowFacts{}, bad
	}
	var f FlowFacts
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&f) != nil || dec.Decode(new(any)) != io.EOF {
		return FlowFacts{}, bad
	}
	for _, n := range []int{f.Total, f.Completed, f.Canceled, f.Started, f.CompletedSinceCreated} {
		if n < 0 || n > 100000 {
			return FlowFacts{}, bad
		}
	}
	if f.Completed+f.Canceled+f.Started > f.Total || f.CompletedSinceCreated > f.Completed {
		return FlowFacts{}, invalid("project flow facts counts are inconsistent")
	}
	if _, e := time.Parse(time.RFC3339Nano, f.CreatedAt); e != nil {
		return FlowFacts{}, invalid("project flow facts need the project's createdAt")
	}
	if len(f.Statuses) > flowListedStatuses {
		return FlowFacts{}, bad
	}
	if !lowerUUIDPattern.MatchString(f.Status.ID) || !projectStatusTypes[f.Status.Type] {
		return FlowFacts{}, invalid("the project's status is a canonical UUID with a project status type")
	}
	// A listed status of a type the rules don't know can never be a target:
	// it is left out, not a reason to refuse the run.
	listed := f.Statuses[:0]
	for _, st := range f.Statuses {
		if !lowerUUIDPattern.MatchString(st.ID) {
			return FlowFacts{}, invalid("project statuses are canonical UUIDs")
		}
		if projectStatusTypes[st.Type] {
			listed = append(listed, st)
		}
	}
	f.Statuses = listed
	return f, nil
}

// flowOn reports whether project flow runs for a bound Linear project:
// under an enabled status policy whose project_flow is on, on a server
// whose operator has proven it (L5, checked at every run, not only when
// it is turned on).
func flowOn(policy StatusPolicy, proven bool) bool {
	return policy.Enabled && policy.ProjectFlow && proven
}

// flowTarget is the status a move to typ goes to: the policy's override
// when it is an unarchived status of that type, else the lowest-position
// unarchived status of that type. ok is false when there is none.
func flowTarget(f FlowFacts, policy StatusPolicy, typ string) (FlowStatus, bool) {
	want := ""
	if t := policy.ProjectTargets; t != nil {
		want = map[string]string{"started": t.Started, "completed": t.Completed}[typ]
	}
	var best *FlowStatus
	for i := range f.Statuses {
		st := &f.Statuses[i]
		if st.Archived || st.Type != typ {
			continue
		}
		if want != "" {
			if st.ID == want {
				return *st, true
			}
			continue
		}
		if best == nil || st.Position < best.Position || st.Position == best.Position && st.ID < best.ID {
			best = st
		}
	}
	if best == nil {
		return FlowStatus{}, false
	}
	return *best, true
}

// below reports whether type a ranks below type b; an unranked type is
// below nothing and nothing is below it.
func below(a, b string) bool {
	ra, okA := projectRank[a]
	rb, okB := projectRank[b]
	return okA && okB && ra < rb
}

// flowState is a Linear project's project_flow_state row, created empty.
func (c commentCall) flowState(ctx context.Context, lp string) (map[string]any, error) {
	if _, err := c.tx.ExecContext(ctx, `INSERT INTO project_flow_state(project_id,linear_project_id) VALUES(?,?) ON CONFLICT DO NOTHING`, c.pid, lp); err != nil {
		return nil, err
	}
	return one(ctx, c.tx, `SELECT * FROM project_flow_state WHERE project_id=? AND linear_project_id=?`, c.pid, lp)
}

// projectFlow applies the rules (§11.1, §11.11) to one run's facts. It
// settles a pending move the read shows, holds a project a person moved
// back, and returns at most one move for the watcher to send, after
// inserting its pending row. what says what the run decided, for show
// and the tick. A run whose read failed (f nil) is tried again in an hour.
// A nudged run leaves the daily slot and its error alone (R101-M1).
func (c commentCall) projectFlow(ctx context.Context, lp string, policy StatusPolicy, revision int64, f *FlowFacts, code string, nudged bool) (move map[string]any, what string, err error) {
	state, err := c.flowState(ctx, lp)
	if err != nil {
		return nil, "", err
	}
	now := layout(c.now)
	if f == nil {
		if code == "" {
			code = "failed"
		}
		if nudged {
			return nil, "error", nil
		}
		_, err = c.tx.ExecContext(ctx, `UPDATE project_flow_state SET last_error=?,next_flow_at=? WHERE project_id=? AND linear_project_id=?`, code, layout(c.now.Add(healthRetry)), c.pid, lp)
		return nil, "error", err
	}
	cur := f.Status.Type
	// An uncertain move is settled by this read: never sent twice blindly.
	pending, err := query(ctx, c.tx, `SELECT seq,to_type,to_status_id,sent_at FROM project_flow_moves WHERE project_id=? AND linear_project_id=? AND state='pending' ORDER BY seq`, c.pid, lp)
	if err != nil {
		return nil, "", err
	}
	open := false
	// lost is the type of an uncertain move no read ever showed: Linear may
	// have applied it and a person moved the project back since, which
	// can't be told apart from a send that never landed (R96-M1).
	lost := ""
	for _, m := range pending {
		switch {
		case str(m, "to_status_id") == f.Status.ID:
			err = c.settleFlowMove(ctx, lp, asInt(m["seq"]), "accepted", "found")
		case c.now.Sub(at(str(m, "sent_at"))) >= flowSettleAfter:
			err = c.settleFlowMove(ctx, lp, asInt(m["seq"]), "failed", "not_found")
			if below(lost, str(m, "to_type")) || lost == "" {
				lost = str(m, "to_type")
			}
		default:
			open = true
		}
		if err != nil {
			return nil, "", err
		}
	}
	// R4 and SF4-M1: a type below the last one seen, or below ClawBridge's
	// last move since any resume, means a person moved it back; so may one
	// below a lost move.
	lastMoved := ""
	moves, err := query(ctx, c.tx, `SELECT to_type FROM project_flow_moves WHERE project_id=? AND linear_project_id=? AND state='accepted' AND settled_at>COALESCE(?,'') ORDER BY seq DESC LIMIT 1`, c.pid, lp, state["resumed_at"])
	if err != nil {
		return nil, "", err
	}
	if len(moves) == 1 {
		lastMoved = str(moves[0], "to_type")
	}
	held := state["held_since"] != nil
	seen := str(state, "last_seen_type")
	if !held && (below(cur, seen) || below(cur, lastMoved) || below(cur, lost)) {
		was := seen
		if below(was, lastMoved) || was == "" {
			was = lastMoved
		}
		if below(was, lost) || was == "" {
			was = lost
		}
		if _, err = c.tx.ExecContext(ctx, `UPDATE project_flow_state SET held_since=?,held_type=? WHERE project_id=? AND linear_project_id=?`, now, cur, c.pid, lp); err != nil {
			return nil, "", err
		}
		if err = c.s.audit(ctx, c.tx, c.wid, c.pid, c.sid, "project_flow", lp, "held", map[string]any{"type": cur, "was": was}); err != nil {
			return nil, "", err
		}
		body := fmt.Sprintf("Linear project %s was moved back by a person, to %s (ClawBridge had seen or moved it to %s). ClawBridge now holds its status and won't move it again until the project owner or a lead runs: clawbridge linear project-flow resume --linear-project %s", lp, cur, was, lp)
		if was == lost && !below(cur, seen) && !below(cur, lastMoved) {
			body = fmt.Sprintf("Linear project %s is %s, but ClawBridge's move of it to %s was never confirmed: Linear may have applied it and a person moved it back. ClawBridge now holds its status and won't move it again until the project owner or a lead runs: clawbridge linear project-flow resume --linear-project %s", lp, cur, lost, lp)
		}
		if err = c.flowNotice(ctx, lp, "project-held", body); err != nil {
			return nil, "", err
		}
		held = true
	}
	// Only a ranked type is remembered: paused or canceled in between must
	// not hide a later drop (R96-M2).
	if _, ranked := projectRank[cur]; ranked {
		seen = cur
	}
	if nudged {
		_, err = c.tx.ExecContext(ctx, `UPDATE project_flow_state SET last_seen_type=?,last_seen_at=? WHERE project_id=? AND linear_project_id=?`, seen, now, c.pid, lp)
	} else {
		_, err = c.tx.ExecContext(ctx, `UPDATE project_flow_state SET last_seen_type=?,last_seen_at=?,last_error='',next_flow_at=? WHERE project_id=? AND linear_project_id=?`, seen, now, layout(nextHealthAt(lp, c.now)), c.pid, lp)
	}
	if err != nil {
		return nil, "", err
	}
	switch {
	case held:
		return nil, "held", nil
	case open:
		return nil, "pending", nil
	}
	if f.Completed+f.Canceled < f.Total || f.Complete && f.Total == 0 {
		// An open issue, or none on a full read: not a done episode. A
		// partial read proves no end (R96-L1).
		if err = c.flowDoneKey(ctx, lp, ""); err != nil {
			return nil, "", err
		}
	}
	if _, ok := projectRank[cur]; !ok || cur == "completed" {
		// R3: paused, completed and canceled are left alone.
		return nil, "left", nil
	}
	allClosed := f.Complete && f.Total > 0 && f.Completed+f.Canceled == f.Total
	if allClosed {
		key := fmt.Sprintf("%s|%d", f.Status.ID, f.Total)
		noticed := str(state, "done_key") == key
		switch {
		case f.Completed == 0:
			// L3: an all-canceled project is not done.
			if !noticed {
				body := fmt.Sprintf("All %d issues of Linear project %s are canceled. Its status is still %s; ClawBridge doesn't move a project whose issues were all canceled.", f.Total, lp, cur)
				if err = c.flowNotice(ctx, lp, "project-done", body); err != nil {
					return nil, "", err
				}
				err = c.flowDoneKey(ctx, lp, key)
			}
			return nil, "all_canceled", err
		case policy.AgentCloses(lp):
			return c.flowMove(ctx, lp, policy, revision, f, "R2", "completed")
		case !noticed:
			body := fmt.Sprintf("All %d issues of Linear project %s are closed (%d completed, %d canceled). Its status is still %s. Who closes it is people (the project's close rule); move it in Linear when you're ready.", f.Total, lp, f.Completed, f.Canceled, cur)
			if err = c.flowNotice(ctx, lp, "project-done", body); err != nil {
				return nil, "", err
			}
			if err = c.flowDoneKey(ctx, lp, key); err != nil {
				return nil, "", err
			}
			what = "done_noticed"
		default:
			what = "done_noticed"
		}
	}
	// R1: started work starts the project; L2: only work done since the
	// project was created counts as started.
	if (cur == "backlog" || cur == "planned") && (f.Started > 0 || f.CompletedSinceCreated > 0) {
		return c.flowMove(ctx, lp, policy, revision, f, "R1", "started")
	}
	if what == "" {
		what = "none"
	}
	return nil, what, nil
}

func (c commentCall) flowDoneKey(ctx context.Context, lp, key string) error {
	_, err := c.tx.ExecContext(ctx, `UPDATE project_flow_state SET done_key=? WHERE project_id=? AND linear_project_id=?`, key, c.pid, lp)
	return err
}

// flowMove inserts a pending move to the target of typ and returns it for
// the watcher; with no such status it moves nothing and says so.
func (c commentCall) flowMove(ctx context.Context, lp string, policy StatusPolicy, revision int64, f *FlowFacts, rule, typ string) (map[string]any, string, error) {
	target, ok := flowTarget(*f, policy, typ)
	if !ok {
		_, err := c.tx.ExecContext(ctx, `UPDATE project_flow_state SET last_error='target_unknown' WHERE project_id=? AND linear_project_id=?`, c.pid, lp)
		return nil, "target_unknown", err
	}
	row, err := one(ctx, c.tx, `SELECT COALESCE(MAX(seq),0) AS seq FROM project_flow_moves WHERE project_id=? AND linear_project_id=?`, c.pid, lp)
	if err != nil {
		return nil, "", err
	}
	seq := asInt(row["seq"]) + 1
	if _, err = c.tx.ExecContext(ctx, `INSERT INTO project_flow_moves(project_id,linear_project_id,seq,rule,from_type,to_type,to_status_id,policy_revision,state,sent_at) VALUES(?,?,?,?,?,?,?,?,'pending',?)`, c.pid, lp, seq, rule, f.Status.Type, typ, target.ID, revision, layout(c.now)); err != nil {
		return nil, "", err
	}
	if err = c.s.audit(ctx, c.tx, c.wid, c.pid, c.sid, "project_flow", lp, "move_sent", map[string]any{"seq": seq, "rule": rule, "from": f.Status.Type, "to": typ, "to_status_id": target.ID, "policy_revision": revision}); err != nil {
		return nil, "", err
	}
	return map[string]any{"linear_project": lp, "seq": seq, "rule": rule, "to_type": typ, "to_status_id": target.ID}, "move", nil
}

// flowWhy is the rule a move followed, in words (LESSONS #8).
func flowWhy(rule string, revision int) string {
	if rule == "manual" {
		return "set by hand (linear project status set; history names who)"
	}
	if rule == "R2" {
		return fmt.Sprintf("all issues closed; the project's close rule is agents (status policy rev %d)", revision)
	}
	return fmt.Sprintf("an issue started (status policy rev %d)", revision)
}

func (c commentCall) settleFlowMove(ctx context.Context, lp string, seq int, state, code string) error {
	if _, err := c.tx.ExecContext(ctx, `UPDATE project_flow_moves SET state=?,error=?,settled_at=? WHERE project_id=? AND linear_project_id=? AND seq=? AND state='pending'`, state, code, layout(c.now), c.pid, lp, seq); err != nil {
		return err
	}
	if state == "accepted" {
		row, err := one(ctx, c.tx, `SELECT to_type FROM project_flow_moves WHERE project_id=? AND linear_project_id=? AND seq=?`, c.pid, lp, seq)
		if err != nil {
			return err
		}
		if _, err = c.tx.ExecContext(ctx, `UPDATE project_flow_state SET last_seen_type=?,last_seen_at=? WHERE project_id=? AND linear_project_id=?`, str(row, "to_type"), layout(c.now), c.pid, lp); err != nil {
			return err
		}
	}
	return c.s.audit(ctx, c.tx, c.wid, c.pid, c.sid, "project_flow", lp, "move_"+state, map[string]any{"seq": seq, "error": code})
}

// flowRefusals are a refused move's reasons, as show and the notice say
// them.
var flowRefusals = map[string]string{
	"app_scope":              "the ClawBridge app lacks Linear's write scope (app_scope)",
	"capability_unavailable": "save_project isn't in the app's tool list (capability_unavailable)",
}

// projectFlowSettle records what the watcher's send did: accepted when the
// read-back showed the status; refused before Linear took it, when the
// coordinator is told once a day; or uncertain, when the row stays pending
// for the next read. Only a pending row settles; the send and read-back are
// charged.
func (c commentCall) projectFlowSettle(ctx context.Context) (any, error) {
	lease, err := c.fence(ctx)
	if err != nil {
		return nil, err
	}
	lp, err := lowerUUID(c.a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	seq, err := integer(c.a["seq"])
	if err != nil || seq < 1 {
		return nil, invalid("seq must be a move's sequence number")
	}
	calls, err := integer(c.a["calls"])
	if err != nil || calls < 0 || calls > flowMoveCalls {
		return nil, invalid(fmt.Sprintf("calls must be 0–%d", flowMoveCalls))
	}
	row, err := one(ctx, c.tx, `SELECT to_type,state FROM project_flow_moves WHERE project_id=? AND linear_project_id=? AND seq=?`, c.pid, lp, seq)
	if err != nil {
		return nil, scopedError(err, "no project flow move with that sequence", "not_found")
	}
	if str(row, "state") != "pending" {
		return map[string]any{"state": str(row, "state")}, nil
	}
	if _, _, err = c.charge(ctx, lease, calls, "watch.flow"); err != nil {
		return nil, err
	}
	code := str(c.a, "error")
	if code != "" && (!commentOperationID.MatchString(code) || len(code) > 64) {
		code = "failed"
	}
	outcome := str(c.a, "outcome")
	switch outcome {
	case "accepted":
		err = c.settleFlowMove(ctx, lp, seq, "accepted", "")
	case "refused":
		if code == "" {
			code = "refused"
		}
		if err = c.settleFlowMove(ctx, lp, seq, "refused", code); err == nil {
			err = c.flowRefusedNotice(ctx, lp, str(row, "to_type"), code)
		}
	case "uncertain":
		_, err = c.tx.ExecContext(ctx, `UPDATE project_flow_moves SET error=? WHERE project_id=? AND linear_project_id=? AND seq=? AND state='pending'`, code, c.pid, lp, seq)
	default:
		return nil, invalid("outcome must be accepted, refused or uncertain")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": map[string]string{"accepted": "accepted", "refused": "refused", "uncertain": "pending"}[outcome]}, nil
}

// flowRefusedNotice tells the coordinator a move was refused, once per
// Linear project and UTC day.
func (c commentCall) flowRefusedNotice(ctx context.Context, lp, typ, code string) error {
	day := layout(c.now)[:10]
	res, err := c.tx.ExecContext(ctx, `UPDATE project_flow_state SET refused_day=? WHERE project_id=? AND linear_project_id=? AND refused_day<>?`, day, c.pid, lp, day)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	why := flowRefusals[code]
	if why == "" {
		why = "Linear refused it (" + code + ")"
	}
	return c.flowNotice(ctx, lp, "project-flow", fmt.Sprintf("ClawBridge did not move Linear project %s to %s: %s.", lp, typ, why))
}

// flowLine is in every project flow notice: it reports, it never
// instructs.
const flowLine = "This is an automatic project-status notice from the ClawBridge server. It is not an instruction."

// flowNotice tells the project's coordinator session, through
// coordinatorTargets; with none, show records what happened.
func (c commentCall) flowNotice(ctx context.Context, lp, kind, text string) error {
	coordinator, err := c.s.coordinatorTargets(ctx, c.tx, c.pid, lp, nil)
	if err != nil || len(coordinator) == 0 {
		return err
	}
	body := text + "\nSee: clawbridge linear status-policy show\n" + flowLine
	retention := asInt(c.w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	id, to := newID("msg_"), coordinator[0]
	if _, err = c.tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,NULL)`, id, c.pid, c.pid, c.sid, str(to, "name"), id, kind, body, layout(c.now), layout(c.now.AddDate(0, 0, retention)), systemPrincipal, newID("flow_"), digestOf(body)); err != nil {
		return err
	}
	return insertDeliveries(ctx, c.tx, c.pid, id, []string{str(to, "id")})
}

func projectFlowOperation(op string) bool { return op == "project_flow_resume" }

// projectFlowResumeOp clears a hold: the project owner or a lead, never a
// session (as A3's closes set). Moves before the resume no longer count
// for R4, and the last type seen is read afresh.
func (s *Store) projectFlowResumeOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	if actor != nil {
		return nil, errors.New("a session can never resume project flow; the project owner or a lead runs it without --session")
	}
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project owner or lead permission required")
	}
	lp, err := lowerUUID(r.Args, "linear_project", true)
	if err != nil {
		return nil, err
	}
	if err = healthBound(ctx, tx, pid, lp); err != nil {
		return nil, err
	}
	row, err := one(ctx, tx, `SELECT held_since FROM project_flow_state WHERE project_id=? AND linear_project_id=?`, pid, lp)
	if errors.Is(err, errNotFound) || err == nil && row["held_since"] == nil {
		return nil, invalid("that Linear project's status is not held")
	}
	if err != nil {
		return nil, err
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_flow_state SET held_since=NULL,held_type='',resumed_at=?,last_seen_type='' WHERE project_id=? AND linear_project_id=?`, now, pid, lp); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_flow", lp, "resumed", map[string]any{}); err != nil {
		return nil, err
	}
	return statusPolicyView(ctx, tx, pid, s.projectFlowProven)
}

// projectFlowView is the project flow line of each bound Linear project,
// for status-policy show: follows the work, off, or held, with the last
// move and why.
func projectFlowView(ctx context.Context, tx *transaction, pid string, policy StatusPolicy, proven bool) ([]any, error) {
	rows, err := query(ctx, tx, `SELECT mapping,mapping_version FROM taskboard_bindings WHERE project_id=? AND mapping IS NOT NULL`, pid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	b, err := readStoredMapping(str(rows[0], "mapping"), asInt(rows[0]["mapping_version"]))
	if err != nil || b.Mode != "project_set" {
		return nil, nil
	}
	projects := append([]string{}, b.LinearProjectIDs...)
	sort.Strings(projects)
	out := []any{}
	for _, lp := range projects {
		line := map[string]any{"linear_project": lp, "project_flow": "off"}
		switch {
		case flowOn(policy, proven):
			line["project_flow"] = "follows the work"
		case flowOn(policy, true):
			line["project_flow"] = "off (not proven on this server)"
		}
		state, err := one(ctx, tx, `SELECT held_since,last_seen_type,last_error FROM project_flow_state WHERE project_id=? AND linear_project_id=?`, pid, lp)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, err
		}
		if err == nil {
			if state["held_since"] != nil {
				line["project_flow"] = "held (moved back by a person since " + str(state, "held_since")[:10] + ")"
			}
			if t := str(state, "last_seen_type"); t != "" {
				line["status_type"] = t
			}
			if e := str(state, "last_error"); e != "" {
				line["last_error"] = e
			}
		}
		moves, err := query(ctx, tx, `SELECT seq,rule,to_type,state,error,sent_at,policy_revision FROM project_flow_moves WHERE project_id=? AND linear_project_id=? ORDER BY seq DESC LIMIT 1`, pid, lp)
		if err != nil {
			return nil, err
		}
		if len(moves) == 1 {
			m := moves[0]
			last := map[string]any{"to_type": str(m, "to_type"), "state": str(m, "state"), "at": str(m, "sent_at")}
			switch str(m, "state") {
			case "accepted":
				last["why"] = "moved to " + str(m, "to_type") + " by ClawBridge: " + flowWhy(str(m, "rule"), asInt(m["policy_revision"]))
			case "refused":
				reason := flowRefusals[str(m, "error")]
				if reason == "" {
					reason = "Linear refused it (" + str(m, "error") + ")"
				}
				last["why"] = "not moved: " + reason
			}
			line["last_move"] = last
		}
		out = append(out, line)
	}
	return out, nil
}
