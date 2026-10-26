package watcher

import (
	"context"
	"encoding/json"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/dispatch"
)

// The dispatcher's rules engine (live/DISPATCHER-DESIGN.md r4 §3–§5): last
// in the tick, after document notices. It takes the pending claims the bus
// hands out, reads an issue as the app only for a claim that could be
// confirmed (at most dispatchReadsPerTick, charged to the hour's budget,
// and only while the comment watch keeps its reserve), runs the rules and
// the guardrails, and records each decision, which the bus re-checks and
// carries out as the dispatcher session.

const (
	dispatchReadsPerTick = 5
	// dispatchReserve is what the hour's budget keeps for the comment watch.
	dispatchReserve = 3
)

// DispatchRun is what one tick's dispatcher did.
type DispatchRun struct {
	Cases   int            `json:"cases"`
	Reads   int            `json:"reads"`
	Actions map[string]int `json:"actions,omitempty"`
}

type dispatchCase struct {
	SubjectID       string   `json:"subject_id"`
	SubjectRevision string   `json:"subject_revision"`
	IssueUUID       string   `json:"issue_uuid"`
	IssueProjectID  string   `json:"issue_project_id"`
	IssueLabels     []string `json:"issue_labels"`
	SessionID       string   `json:"session_id"`
	Principal       string   `json:"principal"`
	Status          string   `json:"status"`
	Presence        string   `json:"presence"`
	AbsentSeconds   int      `json:"absent_seconds"`
	Holders         int      `json:"holders"`
	Waits           int      `json:"waits"`
	AgeSeconds      int      `json:"age_seconds"`
}

func (w *Watcher) dispatcher(ctx context.Context, pid string, fence map[string]any) (*DispatchRun, error) {
	r, err := w.Store.ExecuteDispatcher(ctx, pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(r.Data)
	var due struct {
		Enabled        bool                   `json:"enabled"`
		PolicyRevision int64                  `json:"policy_revision"`
		Domains        []bus.DispatcherDomain `json:"domains"`
		Cases          []dispatchCase         `json:"cases"`
	}
	if err = json.Unmarshal(raw, &due); err != nil {
		return nil, err
	}
	if !due.Enabled || len(due.Cases) == 0 {
		return nil, nil
	}
	run := &DispatchRun{Cases: len(due.Cases), Actions: map[string]int{}}
	now := w.now()
	for _, dc := range due.Cases {
		c := dispatch.Case{
			Session:        dispatch.Session{ID: dc.SessionID, Principal: dc.Principal, Status: dc.Status, Presence: dc.Presence, AbsentFor: time.Duration(dc.AbsentSeconds) * time.Second},
			Issue:          dispatch.Issue{ID: dc.IssueUUID, ProjectID: dc.IssueProjectID, Labels: dc.IssueLabels},
			Domains:        due.Domains,
			Holders:        dc.Holders,
			PolicyRevision: due.PolicyRevision, FactsRevision: due.PolicyRevision,
			Now: now,
		}
		source, readFailed := "client", false
		if dispatch.NeedsRead(c) {
			readFailed = true
			if run.Reads < dispatchReadsPerTick && w.dispatchCharge(ctx, pid, fence) {
				run.Reads++
				if facts, e := w.IssueFacts(ctx, dc.IssueUUID); e == nil {
					c.Issue, source, readFailed = facts, "app", false
				}
			}
		}
		proposal, action, reason := dispatch.Run(c, readFailed, dc.Waits, time.Duration(dc.AgeSeconds)*time.Second)
		guardrail := "permitted"
		if action != proposal.Action {
			guardrail = "overridden"
		}
		out, err := w.Store.ExecuteDispatcher(ctx, pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_decision_record", Args: map[string]any{
			"subject_id": dc.SubjectID, "subject_revision": dc.SubjectRevision, "policy_revision": due.PolicyRevision,
			"engine": "rules", "proposal": string(proposal.Action), "reason_code": reason, "guardrail": guardrail,
			"action": string(action), "domain": proposal.Domain, "facts_source": source,
		}})
		if err != nil {
			return run, err
		}
		if m, ok := out.Data.(map[string]any); ok {
			if taken, _ := m["action"].(string); taken != "" {
				run.Actions[taken]++
			}
		}
	}
	return run, nil
}

// dispatchCharge pays one authoritative read from the hour's budget, only
// while the comment watch keeps its reserve.
func (w *Watcher) dispatchCharge(ctx context.Context, pid string, fence map[string]any) bool {
	args := map[string]any{"calls": 0}
	for k, v := range fence {
		args[k] = v
	}
	r, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "watch_charge", Args: args})
	if err != nil {
		return false
	}
	m, _ := r.Data.(map[string]any)
	used, _ := m["budget_used"].(int)
	limit, _ := m["budget_limit"].(int)
	if limit-used <= dispatchReserve {
		return false
	}
	args["calls"] = 1
	_, err = w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "watch_charge", Args: args})
	return err == nil
}
