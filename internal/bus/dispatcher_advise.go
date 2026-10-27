package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The model as an advisor, and the opt-in tools (DISPATCHER-MODEL-API d7).
// In rules mode with advise on, the rules still decide everything: an
// escalation goes out at once, and the tick then opens one advise case for
// it. The agent ends it with recommend, which is stored on the escalation;
// the people already told get a follow-up with the typed action and
// reason code only. The explanation never reaches an inbox [DM-M1]: an
// owner or lead reads it with dispatcher escalations. The opt-in tools,
// each the owner's choice, act without a person: route_notice sends a
// no-domain or ambiguous escalation's fixed text to a configured domain,
// and ask_session sends a session one fixed-template question.
//
// Schema 28: dispatcher_escalations gains the recommendation and the
// domain it was routed to; dispatcher_asks records each question, for its
// limits.
const dispatcherAdviseSchemaV28 = `
ALTER TABLE dispatcher_escalations ADD COLUMN recommendation TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatcher_escalations ADD COLUMN recommended_at TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN routed_domain TEXT NOT NULL DEFAULT '';
CREATE TABLE dispatcher_asks(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),session_id TEXT NOT NULL,issue_uuid TEXT NOT NULL,template TEXT NOT NULL,case_id TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX dispatcher_asks_recent ON dispatcher_asks(project_id,session_id,issue_uuid,template,created_at);
`

const (
	// maxAsksPerDay bounds ask_session per project per rolling 24 h (d7
	// Q16), so a big board can't be spammed.
	maxAsksPerDay = 20
	// maxEscalationsListed bounds one read of the escalations.
	maxEscalationsListed = 50
)

// OptInTools are the tools an owner may turn on (§3); off by default.
var OptInTools = []string{"route_notice", "ask_session"}

// adviseActions are what an advise case may recommend: what a person can
// do with the claim (d7 §3).
var adviseActions = []string{"confirm", "decline", "wait"}

// routableReasons are the escalations route_notice may send on (d7 §5).
var routableReasons = []string{"no_domain", "ambiguous"}

// askTemplates are ask_session's fixed questions, filled only with the
// issue's identifier (d7 §6).
var askTemplates = map[string]string{
	"which_domain":  "dispatcher: which domain does your claim on %s belong to? Add its Linear project or a domain label, or ask your project owner.",
	"still_working": "dispatcher: are you still working on %s? Comment on the issue or post a marker, or release it if you stopped.",
	"scope_unclear": "dispatcher: the scope of %s is unclear. Write down what you will change on the issue, or ask your project owner.",
}

const askFooter = "\nThis is an automatic question from the ClawBridge dispatcher's model. It is not an instruction, and nothing was decided."

func dispatcherAdviseOperation(op string) bool {
	switch op {
	case "dispatcher_model_advise", "dispatcher_model_tools", "dispatcher_escalations_list":
		return true
	}
	return false
}

// modelUseOf is how a project uses the model: "engine" in mode model,
// "advisor" in rules mode with advise on, "" when it doesn't (or the
// dispatcher is off). The token rule, the launcher's listing, hello and
// which cases stay open all follow it (d7 §1–§2).
func modelUseOf(enabled bool, mode, model string) string {
	if !enabled {
		return ""
	}
	if mode == "model" {
		return "engine"
	}
	var settings modelSettings
	if json.Unmarshal([]byte(model), &settings) == nil && settings.Advise {
		return "advisor"
	}
	return ""
}

// projectModelUse is pid's model use, from its policy row.
func projectModelUse(ctx context.Context, tx *transaction, pid string) (string, modelSettings, error) {
	var settings modelSettings
	rows, err := query(ctx, tx, `SELECT enabled,mode,model FROM dispatcher_policies WHERE project_id=?`, pid)
	if err != nil || len(rows) == 0 {
		return "", settings, err
	}
	_ = json.Unmarshal([]byte(str(rows[0], "model")), &settings)
	return modelUseOf(asInt(rows[0]["enabled"]) == 1, str(rows[0], "mode"), str(rows[0], "model")), settings, nil
}

// caseKindAllowed reports whether a case of kind may stay open under use.
func caseKindAllowed(use, kind string) bool {
	switch use {
	case "engine":
		return kind == "decide" || kind == "chat"
	case "advisor":
		return kind == "advise"
	}
	return false
}

// closeCasesOutside closes pid's open cases the project's use no longer
// allows (R106-L7): all of them when the model is off, the advise cases in
// model mode, the decide and chat cases in rules mode.
func closeCasesOutside(ctx context.Context, tx *transaction, pid, use, nowRaw string) error {
	where := ""
	switch use {
	case "engine":
		where = " AND kind='advise'"
	case "advisor":
		where = " AND kind<>'advise'"
	}
	_, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET closed_at=?,outcome='model_off' WHERE project_id=? AND closed_at IS NULL`+where, nowRaw, pid)
	return err
}

// enabledOptInTools are the opt-in tools the settings turn on, known ones
// only, in a fixed order.
func enabledOptInTools(settings modelSettings) []string {
	out := []string{}
	for _, t := range OptInTools {
		if slices.Contains(settings.OptInTools, t) {
			out = append(out, t)
		}
	}
	return out
}

// modelOptIns are the opt-ins the settings have on: advice, then the
// tools.
func modelOptIns(settings modelSettings) []string {
	out := []string{}
	if settings.Advise {
		out = append(out, "advise")
	}
	return append(out, enabledOptInTools(settings)...)
}

// clearModelOptIns turns advice and the opt-in tools off, as disable and
// every enable do, so each comes back only through its own preview
// (R132-M1). The rest of the model settings stay.
func clearModelOptIns(ctx context.Context, tx *transaction, pid string) error {
	settings, raw, err := readModelSettings(ctx, tx, pid)
	if err != nil || len(modelOptIns(settings)) == 0 {
		return err
	}
	delete(raw, "advise")
	delete(raw, "opt_in_tools")
	encoded, _ := json.Marshal(raw)
	_, err = tx.ExecContext(ctx, `UPDATE dispatcher_policies SET model=? WHERE project_id=?`, string(encoded), pid)
	return err
}

// errModelProjectsCap refuses turning the model on in one project more
// than an owner's agent serves.
var errModelProjectsCap = invalid(fmt.Sprintf("you have the model on in %d projects, the most one dispatcher agent serves: turn it off in one of them first (dispatcher disable, or dispatcher model advise off)", maxModelProjects))

// modelCapPreview is what a preview adds when turning the model on for pid
// would take owner past maxModelProjects, or nil. Enable and advise on
// refuse it at --confirm.
func modelCapPreview(ctx context.Context, tx *transaction, owner, pid string) (map[string]any, error) {
	all, err := allModelProjects(ctx, tx, owner)
	if err != nil || slices.Contains(all, pid) || len(all) < maxModelProjects {
		return nil, err
	}
	return map[string]any{"model_projects": len(all) + 1, "served": maxModelProjects, "served_by": "project id, first ones"}, nil
}

// dispatcherAdviseOp runs the owner's advise and tools switches and the
// escalations read. They are a person's CLI commands, never a session's.
func (s *Store) dispatcherAdviseOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, r Request) (any, error) {
	pid := str(p, "id")
	if r.Operation == "dispatcher_escalations_list" {
		if projectRole != "owner" && projectRole != "lead" {
			return nil, errors.New("project owner or lead permission required")
		}
		return escalationsList(ctx, tx, pid, r.Args["all"] == true, asInt(r.Args["limit"]))
	}
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	a := r.Args
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid(r.Operation + " requires expected_revision")
	}
	_, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if meta["revision"].(int64) == 0 {
		return nil, invalid("the dispatcher has no policy yet: set it up first")
	}
	if meta["revision"].(int64) != int64(expected) {
		return nil, conflict(fmt.Sprintf("the dispatcher policy changed: current revision is %d", meta["revision"]))
	}
	settings, raw, err := readModelSettings(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	preview := map[string]any{}
	details := map[string]any{}
	switch r.Operation {
	case "dispatcher_model_advise":
		on, ok := a["on"].(bool)
		if !ok {
			return nil, invalid("dispatcher_model_advise requires on")
		}
		if on {
			if meta["enabled"] != true {
				return nil, invalid("turn the dispatcher on first (dispatcher enable)")
			}
			if meta["mode"] == "model" {
				return nil, invalid("in model mode the model already decides; advice is for rules mode")
			}
			keys, err := query(ctx, tx, `SELECT provider_id,model,caps FROM dispatcher_model_keys WHERE owner_principal=? AND state='active'`, s.principal)
			if err != nil {
				return nil, err
			}
			if len(keys) != 1 {
				return nil, invalid("advice needs your model setup to have passed its test: run clawbridge dispatcher model setup first")
			}
			var caps any
			_ = json.Unmarshal([]byte(str(keys[0], "caps")), &caps)
			// The tools already set come back on with advice (R132-M1).
			preview = map[string]any{"use": "advisor: the model recommends on escalations, a person decides",
				"provider": str(keys[0], "provider_id"), "model": str(keys[0], "model"), "caps": caps,
				"issue_text_to_provider": true, "agent_required": true, "opt_in_tools": enabledOptInTools(settings)}
			over, err := modelCapPreview(ctx, tx, s.principal, pid)
			if err != nil {
				return nil, err
			}
			if over != nil {
				preview["model_projects_over_cap"], preview["blocked"] = over, "model_projects_over_cap"
				if a["confirm"] == true {
					return nil, errModelProjectsCap
				}
			}
		}
		preview["advise"] = on
		raw["advise"], details["advise"] = on, on
	case "dispatcher_model_tools":
		list, ok := a["tools"].([]any)
		if names, isStrings := a["tools"].([]string); isStrings {
			list, ok = []any{}, true
			for _, n := range names {
				list = append(list, n)
			}
		}
		if !ok {
			return nil, invalid("dispatcher_model_tools requires tools, a list (empty turns them all off)")
		}
		chosen := []string{}
		for _, v := range list {
			t, _ := v.(string)
			if !slices.Contains(OptInTools, t) {
				return nil, invalid("the opt-in tools are route_notice and ask_session")
			}
			if !slices.Contains(chosen, t) {
				chosen = append(chosen, t)
			}
		}
		chosen = enabledOptInTools(modelSettings{OptInTools: chosen})
		preview = map[string]any{"opt_in_tools": chosen,
			"acts_without_a_person": "route_notice sends an escalation's fixed text to a domain you configured; ask_session sends a session one fixed-template question"}
		raw["opt_in_tools"], details["opt_in_tools"] = chosen, chosen
	}
	if a["confirm"] != true {
		preview["preview"] = true
		return preview, nil
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(raw)
	revision := int64(expected) + 1
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_policies SET model=?,revision=?,updated_at=?,updated_by_principal=?,updated_by_session=NULL WHERE project_id=? AND revision=?`, string(encoded), revision, nowRaw, s.principal, pid, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, conflict("the dispatcher policy changed concurrently; read it again")
	}
	use, _, err := projectModelUse(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if err = closeCasesOutside(ctx, tx, pid, use, nowRaw); err != nil {
		return nil, err
	}
	details["revision"] = revision
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "dispatcher_policy", pid, r.Operation, details); err != nil {
		return nil, err
	}
	preview["revision"] = revision
	return preview, nil
}

// escalationsList is pid's escalations, newest first, only those whose
// claim is still pending unless all: what an owner or lead reads with dispatcher escalations,
// including the model's recommendation and its note. The CLI strips the
// note of terminal controls (D6-L1).
func escalationsList(ctx context.Context, tx *transaction, pid string, all bool, limit int) (any, error) {
	if limit < 1 || limit > maxEscalationsListed {
		limit = maxEscalationsListed
	}
	// Nothing resolves an escalation row: a person decides the claim. So
	// the default is the escalations whose claim is still pending at its
	// revision (R132-L2).
	where := " AND a.state='proposed' AND a.revision=e.subject_revision"
	if all {
		where = ""
	}
	rows, err := query(ctx, tx, `SELECT e.*,a.issue_identifier,a.state AS proposal_state,a.revision AS proposal_revision,s.name AS session_name FROM dispatcher_escalations e LEFT JOIN session_assignments a ON a.project_id=e.project_id AND a.id=e.subject_id LEFT JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id WHERE e.project_id=?`+where+` ORDER BY e.created_at DESC,e.id LIMIT ?`, pid, limit)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		var recipients []string
		_ = json.Unmarshal([]byte(str(r, "recipients")), &recipients)
		pending := str(r, "proposal_state") == "proposed" && str(r, "proposal_revision") == str(r, "subject_revision")
		item := map[string]any{"id": str(r, "id"), "session": str(r, "session_name"), "issue_identifier": str(r, "issue_identifier"),
			"revision": str(r, "subject_revision"), "domain": str(r, "domain"), "reason_code": str(r, "reason_code"),
			"recipients": recipients, "delivered": asInt(r["delivered"]) == 1, "claim_pending": pending,
			"routed_domain": str(r, "routed_domain"), "created_at": at(str(r, "created_at")).Format(time.RFC3339)}
		if raw := str(r, "recommendation"); raw != "" {
			var rec map[string]any
			_ = json.Unmarshal([]byte(raw), &rec)
			item["recommendation"] = map[string]any{"action": rec["action"], "reason_code": rec["reason_code"],
				"models_note_not_checked": rec["explanation"], "case_id": rec["case_id"]}
		}
		out = append(out, item)
	}
	return map[string]any{"escalations": out}, nil
}

// openEscalation is the open escalation of a claim's revision, or nil.
func openEscalation(ctx context.Context, tx *transaction, pid, subject, revision string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT * FROM dispatcher_escalations WHERE project_id=? AND subject_id=? AND subject_revision=? AND state='open' ORDER BY created_at DESC,id LIMIT 1`, pid, subject, revision)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// adviseCaseFacts is what an advise case adds to a decide case's: its
// permitted actions, its reason codes (the escalation's first) and the
// escalation itself. nil means the claim's revision has no open
// escalation, so there is nothing to advise on.
func adviseCaseFacts(ctx context.Context, tx *transaction, pid, subject, revision string) ([]string, []string, map[string]any, error) {
	e, err := openEscalation(ctx, tx, pid, subject, revision)
	if err != nil || e == nil {
		return nil, nil, nil, err
	}
	reasons := []string{str(e, "reason_code")}
	for _, r := range DecideReasonCodes {
		if !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	return adviseActions, reasons, map[string]any{"id": str(e, "id"), "reason_code": str(e, "reason_code"), "domain": str(e, "domain")}, nil
}

// pendingAt reports whether a claim is still pending at revision.
func pendingAt(ctx context.Context, tx *transaction, pid, subject, revision string) (bool, error) {
	rows, err := query(ctx, tx, `SELECT 1 FROM session_assignments WHERE project_id=? AND id=? AND state='proposed' AND revision=?`, pid, subject, revision)
	return len(rows) == 1, err
}

// deliverDispatcher sends one fixed-text dispatcher message to each
// recipient, from the named system session, and returns their names.
func (s *Store) deliverDispatcher(ctx context.Context, tx *transaction, w map[string]any, pid, sender string, to []map[string]any, body, nowRaw string) ([]string, error) {
	from, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, sender)
	if err != nil {
		return nil, err
	}
	retention := asInt(w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	names := []string{}
	for _, r := range to {
		id := newID("msg_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'dispatcher',?,?,?,?,?,?,NULL)`,
			id, pid, pid, from["id"], str(r, "name"), id, body, nowRaw, layout(at(nowRaw).AddDate(0, 0, retention)), dispatcherPrincipal, newID("dsp_"), digestOf(body)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id) VALUES(?,?,?)`, id, pid, r["id"]); err != nil {
			return nil, err
		}
		names = append(names, str(r, "name"))
	}
	return names, nil
}

// recommendOutcome stores an advise case's recommendation on its
// escalation and, while the claim is still pending at its revision, tells
// the people the escalation reached: the typed action and reason code
// only, never the explanation [DM-M1]. It returns the tool's result.
func (s *Store) recommendOutcome(ctx context.Context, tx *transaction, w map[string]any, c, a map[string]any, nowRaw string) (map[string]any, string, error) {
	var permitted, reasons []string
	_ = json.Unmarshal([]byte(str(c, "permitted")), &permitted)
	_ = json.Unmarshal([]byte(str(c, "reason_codes")), &reasons)
	if !slices.Contains(permitted, str(a, "action")) || !slices.Contains(reasons, str(a, "reason_code")) {
		return nil, "invalid_argument", nil
	}
	pid := str(c, "project_id")
	var facts map[string]any
	_ = json.Unmarshal([]byte(str(c, "facts")), &facts)
	esc, _ := facts["escalation"].(map[string]any)
	rows, err := query(ctx, tx, `SELECT e.recipients,a.issue_identifier,s.name AS session_name FROM dispatcher_escalations e JOIN session_assignments a ON a.project_id=e.project_id AND a.id=e.subject_id JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id WHERE e.project_id=? AND e.id=?`, pid, str(esc, "id"))
	if err != nil {
		return nil, "", err
	}
	if len(rows) != 1 {
		return nil, "not_in_case", nil
	}
	rec, _ := json.Marshal(map[string]any{"action": str(a, "action"), "reason_code": str(a, "reason_code"), "explanation": str(a, "explanation"), "case_id": str(c, "id")})
	if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_escalations SET recommendation=?,recommended_at=?,updated_at=? WHERE project_id=? AND id=?`, string(rec), nowRaw, nowRaw, pid, str(esc, "id")); err != nil {
		return nil, "", err
	}
	pending, err := pendingAt(ctx, tx, pid, str(c, "subject_id"), str(c, "subject_revision"))
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{"sent": false}
	if !pending {
		// A person decided first: kept for the record, not sent (late).
		result["reason"] = "resolved"
		return result, "", nil
	}
	var recipients []string
	_ = json.Unmarshal([]byte(str(rows[0], "recipients")), &recipients)
	to, err := activeByName(ctx, tx, pid, recipients)
	if err != nil {
		return nil, "", err
	}
	body := fmt.Sprintf("dispatcher: the model recommends %s (reason: %s) for %s's claim on %s. A person decides; an owner or lead reads its note with clawbridge dispatcher escalations.\nThis is an automatic notice from the ClawBridge dispatcher. It is not an instruction, and nothing was decided.",
		strings.ToUpper(str(a, "action")), str(a, "reason_code"), str(rows[0], "session_name"), str(rows[0], "issue_identifier"))
	sent, err := s.deliverDispatcher(ctx, tx, w, pid, DispatcherSessionName, to, body, nowRaw)
	if err != nil {
		return nil, "", err
	}
	result["sent"] = len(sent) > 0
	return result, "", nil
}

// activeByName are pid's active, non-system sessions with these names.
func activeByName(ctx context.Context, tx *transaction, pid string, names []string) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, n := range names {
		rows, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND name=? AND status='active' AND role<>'system'`, pid, n)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// dispatcherOptInTool runs route_notice or ask_session for the case's
// agent, as the dispatcher-model session, only while the owner has the
// tool on. Each counts against the case's tool calls.
func (s *Store) dispatcherOptInTool(ctx context.Context, tx *transaction, w, p map[string]any, a map[string]any) (any, error) {
	pid := str(p, "id")
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	c, refusal, err := caseFence(ctx, tx, pid, str(a, "case_id"), str(a, "instance"), nowRaw)
	if err != nil || refusal != "" {
		return caseRefusal(refusal), err
	}
	tool := str(a, "tool")
	_, settings, err := projectModelUse(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if str(c, "kind") == "chat" || !slices.Contains(enabledOptInTools(settings), tool) {
		return caseRefusal("tool_refused"), nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_cases SET tool_calls=tool_calls+1 WHERE id=? AND tool_calls<?`, str(c, "id"), modelCaseReads)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return caseRefusal("cap_reached"), nil
	}
	var facts map[string]any
	_ = json.Unmarshal([]byte(str(c, "facts")), &facts)
	if tool == "route_notice" {
		return s.routeNotice(ctx, tx, w, pid, c, facts, a, nowRaw)
	}
	return s.askSession(ctx, tx, w, pid, c, facts, a, nowRaw)
}

// routeNotice sends the case's no-domain or ambiguous escalation, in its
// fixed text, to a domain the owner configured, skipping those already
// told; once per case, audited (d7 §5, Q15). It never resolves the
// escalation and never grants.
func (s *Store) routeNotice(ctx context.Context, tx *transaction, w map[string]any, pid string, c, facts, a map[string]any, nowRaw string) (any, error) {
	esc, _ := facts["escalation"].(map[string]any)
	if esc == nil || str(a, "notice_id") != str(esc, "id") {
		return caseRefusal("not_in_case"), nil
	}
	rows, err := query(ctx, tx, `SELECT * FROM dispatcher_escalations WHERE project_id=? AND id=? AND state='open'`, pid, str(esc, "id"))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || !slices.Contains(routableReasons, str(rows[0], "reason_code")) || str(rows[0], "routed_domain") != "" {
		return caseRefusal("invalid_argument"), nil
	}
	e := rows[0]
	domains, _, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	var dom *DispatcherDomain
	for i := range domains.Domains {
		if domains.Domains[i].ID == str(a, "domain") {
			dom = &domains.Domains[i]
		}
	}
	// A no-domain or ambiguous escalation has no domain of its own.
	if dom == nil {
		return caseRefusal("invalid_argument"), nil
	}
	live, err := query(ctx, tx, `SELECT a.*,s.name AS session_name FROM session_assignments a JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id WHERE a.project_id=? AND a.id=? AND a.state='proposed' AND a.revision=?`, pid, str(e, "subject_id"), str(e, "subject_revision"))
	if err != nil {
		return nil, err
	}
	if len(live) != 1 {
		return caseRefusal("invalid_argument"), nil
	}
	chain, err := s.dispatcherChain(ctx, tx, w, pid, dom, str(live[0], "session_id"))
	if err != nil {
		return nil, err
	}
	var told []string
	_ = json.Unmarshal([]byte(str(e, "recipients")), &told)
	to := []map[string]any{}
	for _, r := range chain {
		if !slices.Contains(told, str(r, "name")) {
			to = append(to, r)
		}
	}
	sent, err := s.deliverDispatcher(ctx, tx, w, pid, DispatcherSessionName, to, escalationBody(live[0], str(e, "reason_code"), dom.ID), nowRaw)
	if err != nil {
		return nil, err
	}
	recipients, _ := json.Marshal(append(told, sent...))
	delivered := asInt(e["delivered"])
	if len(sent) > 0 {
		delivered = 1
	}
	if _, err = tx.ExecContext(ctx, `UPDATE dispatcher_escalations SET routed_domain=?,recipients=?,delivered=?,updated_at=? WHERE id=?`, dom.ID, string(recipients), delivered, nowRaw, str(e, "id")); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "dispatcher_route", str(e, "id"), "route_notice", map[string]any{"domain": dom.ID, "case_id": str(c, "id"), "recipients": sent}); err != nil {
		return nil, err
	}
	return caseResult(map[string]any{"routed": true, "delivered": len(sent)}), nil
}

// askSession sends a session named in the case one fixed-template
// question about the case's issue, from the dispatcher-model session: at
// most one per case, one per (session, issue, template) a day and
// maxAsksPerDay per project a day, and only while the case's claim is
// pending; otherwise it answers sent false, not an error (d7 §6, Q16).
func (s *Store) askSession(ctx context.Context, tx *transaction, w map[string]any, pid string, c, facts, a map[string]any, nowRaw string) (any, error) {
	template := str(a, "template")
	text, ok := askTemplates[template]
	if !ok {
		return caseRefusal("invalid_argument"), nil
	}
	issue, _ := facts["issue"].(map[string]any)
	if issue == nil || str(issue, "id") != str(a, "issue_id") {
		return caseRefusal("not_in_case"), nil
	}
	named := false
	if session, _ := facts["session"].(map[string]any); str(session, "id") == str(a, "session_id") {
		named = true
	}
	holders, _ := facts["holders"].([]any)
	for _, h := range holders {
		if m, _ := h.(map[string]any); str(m, "session_id") == str(a, "session_id") {
			named = true
		}
	}
	target, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND id=? AND status='active' AND role<>'system'`, pid, str(a, "session_id"))
	if err != nil {
		return nil, err
	}
	if !named || len(target) != 1 {
		return caseRefusal("not_in_case"), nil
	}
	// Once a person has decided the claim, nothing more is asked (R132-M2).
	pending, err := pendingAt(ctx, tx, pid, str(c, "subject_id"), str(c, "subject_revision"))
	if err != nil {
		return nil, err
	}
	if !pending {
		return caseResult(map[string]any{"sent": false, "reason": "resolved"}), nil
	}
	day := layout(at(nowRaw).Add(-24 * time.Hour))
	var inCase, repeat, project int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN case_id=? THEN 1 END),COUNT(CASE WHEN session_id=? AND issue_uuid=? AND template=? AND created_at>=? THEN 1 END),COUNT(CASE WHEN created_at>=? THEN 1 END) FROM dispatcher_asks WHERE project_id=?`,
		str(c, "id"), str(a, "session_id"), str(a, "issue_id"), template, day, day, pid).Scan(&inCase, &repeat, &project); err != nil {
		return nil, err
	}
	switch {
	case inCase > 0:
		return caseResult(map[string]any{"sent": false, "reason": "one_per_case"}), nil
	case repeat > 0:
		return caseResult(map[string]any{"sent": false, "reason": "recent"}), nil
	case project >= maxAsksPerDay:
		return caseResult(map[string]any{"sent": false, "reason": "project_cap"}), nil
	}
	body := fmt.Sprintf(text, str(issue, "identifier")) + askFooter
	if _, err = s.deliverDispatcher(ctx, tx, w, pid, DispatcherModelSessionName, target, body, nowRaw); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_asks(id,project_id,session_id,issue_uuid,template,case_id,created_at) VALUES(?,?,?,?,?,?,?)`, newID("ask_"), pid, str(a, "session_id"), str(a, "issue_id"), template, str(c, "id"), nowRaw); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "dispatcher_ask", str(a, "session_id"), template, map[string]any{"case_id": str(c, "id"), "issue_uuid": str(a, "issue_id")}); err != nil {
		return nil, err
	}
	return caseResult(map[string]any{"sent": true}), nil
}
