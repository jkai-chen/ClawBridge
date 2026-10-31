package watcher

import (
	"context"
	"encoding/json"
	"strings"
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
//
// In mode model (DISPATCHER-MODEL-API d5) a claim with an agent available
// gets a decide case instead, opened with the facts read here and the
// guardrails' permitted set; the agent decides it through the
// dispatcher-model API. A claim whose case is still open is left to it
// (D5-M2). The rules decide at once when no agent is available, and on the
// tick after a claim's case closed without a final decision (abstained,
// expired, or a model wait), since a subject revision gets one model case
// (D5-M1); each such record says model_fallback and why.
//
// A claim whose issue has used the model's loop budget (its judgements in
// 24 hours) gets no case: the rules decide it with model_fallback:loop_budget,
// and the bus records the skip and tells a person once per issue.
//
// In rules mode with the model as the advisor (d7), the rules decide as
// always; an escalation they record goes out at once, and with an agent
// available the claim then gets an advise case, with the same facts.

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
	// Opened and Waiting count the model cases opened this tick and the
	// claims left to an open one; Advised the advise cases opened.
	Opened  int `json:"model_opened,omitempty"`
	Waiting int `json:"model_waiting,omitempty"`
	Advised int `json:"model_advised,omitempty"`
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
	ModelCase       string   `json:"model_case"`
	OverBudget      bool     `json:"over_budget"`
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
		Mode           string                 `json:"mode"`
		Model          string                 `json:"model"`
		ModelUse       string                 `json:"model_use"`
	}
	if err = json.Unmarshal(raw, &due); err != nil {
		return nil, err
	}
	if !due.Enabled || len(due.Cases) == 0 {
		return nil, nil
	}
	run := &DispatchRun{Cases: len(due.Cases), Actions: map[string]int{}}
	now := w.now()
	advise := due.ModelUse == "advisor" && due.Model == "available"
	for _, dc := range due.Cases {
		c := dispatch.Case{
			Session:        dispatch.Session{ID: dc.SessionID, Principal: dc.Principal, Status: dc.Status, Presence: dc.Presence, AbsentFor: time.Duration(dc.AbsentSeconds) * time.Second},
			Issue:          dispatch.Issue{ID: dc.IssueUUID, ProjectID: dc.IssueProjectID, Labels: dc.IssueLabels},
			Domains:        due.Domains,
			Holders:        dc.Holders,
			PolicyRevision: due.PolicyRevision, FactsRevision: due.PolicyRevision,
			Now: now,
		}
		fallback := ""
		if due.Mode == "model" {
			switch {
			case dc.ModelCase == "open" || dc.ModelCase == "leased":
				run.Waiting++
				continue
			case dc.ModelCase != "":
				fallback = strings.TrimPrefix(dc.ModelCase, "closed:")
			case dc.OverBudget:
				fallback = "loop_budget"
			case due.Model != "available":
				fallback = due.Model
			}
		}
		source, readFailed := "client", false
		var text *IssueText
		if dispatch.NeedsRead(c) {
			readFailed = true
			if run.Reads < dispatchReadsPerTick && w.dispatchCharge(ctx, pid, fence) {
				run.Reads++
				if due.Mode == "model" && fallback == "" || advise {
					if facts, t, e := w.IssueFactsText(ctx, dc.IssueUUID); e == nil {
						c.Issue, source, readFailed, text = facts, "app", false, &t
					}
				} else if facts, e := w.IssueFacts(ctx, dc.IssueUUID); e == nil {
					c.Issue, source, readFailed = facts, "app", false
				}
			}
		}
		age := time.Duration(dc.AgeSeconds) * time.Second
		if due.Mode == "model" && fallback == "" {
			// Without the authoritative read the model couldn't confirm:
			// the rules wait for it, and the model gets the claim later.
			if !readFailed {
				opened, why, err := w.openModelCase(ctx, pid, "decide", due.PolicyRevision, dc, c, source, text, dispatch.Rules(c, false, dc.Waits, age).Domain)
				if err != nil {
					return run, err
				}
				if opened {
					run.Opened++
				}
				// The bus's own count found the budget used since the due
				// read: the rules decide now.
				if why != "over_budget" {
					continue
				}
				fallback = "loop_budget"
			} else {
				fallback = "facts_unavailable"
			}
		}
		proposal, action, reason := dispatch.Run(c, readFailed, dc.Waits, age)
		guardrail := "permitted"
		if action != proposal.Action {
			guardrail = "overridden"
		}
		out, err := w.Store.ExecuteDispatcher(ctx, pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_decision_record", Args: map[string]any{
			"subject_id": dc.SubjectID, "subject_revision": dc.SubjectRevision, "policy_revision": due.PolicyRevision,
			"engine": "rules", "proposal": string(proposal.Action), "reason_code": reason, "guardrail": guardrail,
			"action": string(action), "domain": proposal.Domain, "facts_source": source, "app_project_id": appProject(c, source),
			"explanation": modelFallback(fallback), "over_budget": fallback == "loop_budget",
		}})
		if err != nil {
			return run, err
		}
		taken := ""
		if m, ok := out.Data.(map[string]any); ok {
			if taken, _ = m["action"].(string); taken != "" {
				run.Actions[taken]++
			}
		}
		// Advice is asked for in the tick that escalates, and only then: an
		// escalation it misses gets none (Q14). The decision is already
		// recorded, so a failure is logged and the tick goes on (R132-L3).
		if advise && taken == string(dispatch.Escalate) {
			opened, _, err := w.openModelCase(ctx, pid, "advise", due.PolicyRevision, dc, c, source, text, proposal.Domain)
			if err != nil {
				logf("dispatcher: project %s: no advise case for %s: %v", pid, dc.SubjectID, err)
			}
			if opened {
				run.Advised++
			}
		}
	}
	return run, nil
}

// modelFallback is a rules record's explanation in model mode: why the
// model didn't decide it.
func modelFallback(why string) string {
	if why == "" {
		return ""
	}
	return "model_fallback:" + why
}

// openModelCase opens the claim's decide or advise case: the issue's facts (state
// only from the app's read), the domains it matches and how, the
// guardrails' permitted set, and the issue's text only when this tick read
// it (Q8). Not opened, it says why.
func (w *Watcher) openModelCase(ctx context.Context, pid, kind string, policyRevision int64, dc dispatchCase, c dispatch.Case, source string, text *IssueText, domain string) (bool, string, error) {
	state := "unknown"
	if c.Issue.Authoritative {
		state = c.Issue.StateType
	}
	var project any
	if c.Issue.ProjectID != "" {
		project = c.Issue.ProjectID
	}
	labels := c.Issue.Labels
	if labels == nil {
		labels = []string{}
	}
	matches := []any{}
	for _, d := range dispatch.ProjectDomains(c.Issue, c.Domains) {
		// The model has no other way to see trust: the domain's list stays hidden
		// from it, so the server says whether the claiming session is on it.
		matches = append(matches, map[string]any{"domain": d.ID, "by": "project", "trusted": dispatch.Trusted(c.Session, d)})
	}
	for _, d := range dispatch.LabelDomains(c.Issue, c.Domains) {
		// A label match never grants trust, so it is never trusted.
		matches = append(matches, map[string]any{"domain": d.ID, "by": "label", "trusted": false})
	}
	permitted := []any{}
	for _, a := range dispatch.Permitted(c) {
		switch a {
		case dispatch.Confirm, dispatch.Decline, dispatch.Wait, dispatch.Escalate:
			permitted = append(permitted, string(a))
		}
	}
	args := map[string]any{
		"kind": kind, "subject_id": dc.SubjectID, "subject_revision": dc.SubjectRevision, "policy_revision": policyRevision,
		"issue":          map[string]any{"id": dc.IssueUUID, "state_type": state, "project_id": project, "labels": labels, "authoritative": c.Issue.Authoritative},
		"domain_matches": matches, "permitted": permitted, "facts_source": source, "app_project_id": appProject(c, source), "domain": domain,
	}
	if text != nil {
		args["untrusted"] = map[string]any{"title": text.Title, "description_excerpt": text.Description}
	}
	out, err := w.Store.ExecuteDispatcher(ctx, pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_case_open", Args: args})
	if err != nil {
		return false, "", err
	}
	m, _ := out.Data.(map[string]any)
	why, _ := m["reason"].(string)
	return m["opened"] == true, why, nil
}

// appProject is the issue's project as the app read it, or "".
func appProject(c dispatch.Case, source string) string {
	if source != "app" {
		return ""
	}
	return c.Issue.ProjectID
}

// dispatchCharge pays one authoritative read from the hour's budget, only
// while the comment watch keeps its reserve.
func (w *Watcher) dispatchCharge(ctx context.Context, pid string, fence map[string]any) bool {
	args := map[string]any{"calls": 0, "kind": "watch.dispatcher"}
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
	if limit > 0 && limit-used <= dispatchReserve {
		return false
	}
	args["calls"] = 1
	_, err = w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "watch_charge", Args: args})
	return err == nil
}
