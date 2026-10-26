package bus

import (
	"context"
	"errors"
	"time"
)

// Project flow nudges (live/STATUS-FLOW-DESIGN.md §11.2, §11.11 SF4-M2):
// when ClawBridge itself moves an issue, the project it belongs to gets an
// early flow check instead of waiting for its daily slot. A claim that
// landed in a started-type state asks an R1 check, which reads the project
// only: the issue just started is the evidence. A release that landed in a
// completed-type state asks an R2 check, which reads every issue. Nudged
// reads are capped per workspace and per Linear project; past the cap the
// daily run catches up.
const projectFlowSchemaV22 = `
CREATE TABLE project_flow_nudges(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),linear_project_id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('r1','r2')),asked_at TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('asked','granted','done','dropped')),granted_at TEXT);
CREATE INDEX project_flow_nudges_granted ON project_flow_nudges(state,granted_at);
`

const (
	// flowNudgesPerHour caps a workspace's nudged reads in any hour.
	flowNudgesPerHour = 20
	// flowNudgeEvery is the least time between two nudged reads of one
	// Linear project; asks in between wait for it.
	flowNudgeEvery = 10 * time.Minute
	// flowNudgeExpiry is how long an ask waits before the daily run is left
	// to catch up.
	flowNudgeExpiry = time.Hour
)

// askFlowNudge records the early check a settled state move asks for, if
// any: a claim that landed in a started-type state (r1), or a release that
// landed in a completed-type state (r2), of an issue in a bound Linear
// project whose flow is on. A canceled release, or one that landed in a
// started state, asks nothing.
func (m markerCall) askFlowNudge(ctx context.Context, landed string) error {
	policy, _, err := readFlowPolicy(ctx, m.tx, m.pid)
	if err != nil || !flowOn(policy, m.s.projectFlowProven) {
		return err
	}
	typ := ""
	for _, t := range policy.Teams {
		if v := t.Workflow[landed]; v != "" {
			typ = v
			break
		}
	}
	kind := map[string]string{"claim|started": "r1", "close|completed": "r2"}[m.phase+"|"+typ]
	if kind == "" {
		return nil
	}
	row, err := one(ctx, m.tx, `SELECT issue_project_id FROM session_assignments WHERE id=? AND project_id=?`, m.assignment, m.pid)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	lp := str(row, "issue_project_id")
	if lp == "" || healthBound(ctx, m.tx, m.pid, lp) != nil {
		return nil
	}
	// One ask waits per Linear project: a later r2 widens a waiting r1.
	if _, err = m.tx.ExecContext(ctx, `DELETE FROM project_flow_nudges WHERE project_id=? AND asked_at<?`, m.pid, layout(m.now.Add(-2*flowNudgeExpiry))); err != nil {
		return err
	}
	waiting, err := query(ctx, m.tx, `SELECT id,kind FROM project_flow_nudges WHERE project_id=? AND linear_project_id=? AND state='asked'`, m.pid, lp)
	if err != nil {
		return err
	}
	if len(waiting) > 0 {
		if kind == "r2" && str(waiting[0], "kind") == "r1" {
			_, err = m.tx.ExecContext(ctx, `UPDATE project_flow_nudges SET kind='r2' WHERE id=?`, str(waiting[0], "id"))
		}
		return err
	}
	_, err = m.tx.ExecContext(ctx, `INSERT INTO project_flow_nudges(id,project_id,linear_project_id,kind,asked_at,state) VALUES(?,?,?,?,?,'asked')`, newID("fn_"), m.pid, lp, kind, layout(m.now))
	return err
}

// flowNudgeCalls is what a nudged run may spend: the project read for r1,
// and the issue pages too for r2.
func flowNudgeCalls(kind string) int {
	if kind == "r1" {
		return 1
	}
	return FlowReadCalls
}

// flowNudgeRuns grants waiting asks as flow-only runs, up to room of them.
// An ask is dropped when a run in hand already reads its Linear project,
// when it waited past flowNudgeExpiry, when its project left the binding,
// or when the workspace has had flowNudgesPerHour nudged reads in the last
// hour. One whose Linear project was read by a nudge in the last
// flowNudgeEvery waits.
func (c commentCall) flowNudgeRuns(ctx context.Context, have map[string]bool, room int) ([]any, int, error) {
	asks, err := query(ctx, c.tx, `SELECT id,linear_project_id,kind,asked_at FROM project_flow_nudges WHERE project_id=? AND state='asked' ORDER BY asked_at,id`, c.pid)
	if err != nil {
		return nil, 0, err
	}
	now := layout(c.now)
	runs, cost := []any{}, 0
	for _, ask := range asks {
		id, lp, kind := str(ask, "id"), str(ask, "linear_project_id"), str(ask, "kind")
		drop := have[lp] || c.now.Sub(at(str(ask, "asked_at"))) >= flowNudgeExpiry || healthBound(ctx, c.tx, c.pid, lp) != nil
		if !drop {
			if len(runs) >= room {
				continue
			}
			recent, err := one(ctx, c.tx, `SELECT COUNT(*) AS n FROM project_flow_nudges WHERE project_id=? AND linear_project_id=? AND state IN ('granted','done') AND granted_at>?`, c.pid, lp, layout(c.now.Add(-flowNudgeEvery)))
			if err != nil {
				return nil, 0, err
			}
			if asInt(recent["n"]) > 0 {
				continue
			}
			spent, err := one(ctx, c.tx, `SELECT COUNT(*) AS n FROM project_flow_nudges n JOIN projects p ON p.id=n.project_id WHERE p.workspace_id=? AND n.state IN ('granted','done') AND n.granted_at>?`, c.wid, layout(c.now.Add(-time.Hour)))
			if err != nil {
				return nil, 0, err
			}
			drop = asInt(spent["n"]) >= flowNudgesPerHour
		}
		if drop {
			if _, err = c.tx.ExecContext(ctx, `UPDATE project_flow_nudges SET state='dropped' WHERE id=?`, id); err != nil {
				return nil, 0, err
			}
			continue
		}
		if _, err = c.tx.ExecContext(ctx, `UPDATE project_flow_nudges SET state='granted',granted_at=? WHERE id=?`, now, id); err != nil {
			return nil, 0, err
		}
		have[lp] = true
		runs = append(runs, map[string]any{"linear_project": lp, "health": false, "flow": true, "nudge": kind})
		cost += flowNudgeCalls(kind)
	}
	return runs, cost, nil
}

// takeFlowNudge marks the granted nudge a report answers done; a report
// naming no granted nudge of that kind is refused.
func (c commentCall) takeFlowNudge(ctx context.Context, lp, kind string) error {
	if kind != "r1" && kind != "r2" {
		return invalid("nudge must be r1 or r2")
	}
	row, err := one(ctx, c.tx, `SELECT id FROM project_flow_nudges WHERE project_id=? AND linear_project_id=? AND kind=? AND state='granted' ORDER BY granted_at DESC LIMIT 1`, c.pid, lp, kind)
	if errors.Is(err, errNotFound) {
		return scoped("this nudge was not handed out; ask health_due first", "conflict")
	}
	if err != nil {
		return err
	}
	_, err = c.tx.ExecContext(ctx, `UPDATE project_flow_nudges SET state='done' WHERE id=?`, str(row, "id"))
	return err
}
