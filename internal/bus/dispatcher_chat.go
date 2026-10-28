package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Chat with the dispatcher's model (DISPATCHER-MODEL-API §5, d6): a
// project owner, or a lead the owner allowed, asks from the CLI with no
// session. The ask opens a chat case the project's agent takes like any
// other; it reads through the read tools and list_decisions, and ends
// with reply or abstain. The reply is kept on the case and shown only to
// the asker's own CLI: it never reaches an inbox or a session [DM-M1].
//
// Schema 25: dispatcher_cases gains the asker (the raw principal, never
// sent) and the message as the agent sees it (the asker's opaque ref and
// the text). A partial unique index keeps one open chat per asker.
const dispatcherChatSchemaV25 = `
ALTER TABLE dispatcher_cases ADD COLUMN asked_by TEXT;
ALTER TABLE dispatcher_cases ADD COLUMN message TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX dispatcher_cases_chat_open ON dispatcher_cases(project_id,asked_by) WHERE kind='chat' AND closed_at IS NULL;
`

const (
	// maxChatQuestion bounds a question, in bytes.
	maxChatQuestion = 2048
	// maxChatReply bounds a reply, in characters (the agent schema's).
	maxChatReply = 2000
	// A chat's deadline: the owner's deadline_seconds, else this.
	defaultChatDeadline = 120
	// chatShare is the part of a key-holder's runs a chat may use: a
	// quarter of each budget, at least one, so decides keep the rest
	// (D6-M1).
	chatShare = 4
	// maxDecisionsListed bounds one read of the decision log.
	maxDecisionsListed = 50
)

func dispatcherChatOperation(op string) bool {
	switch op {
	case "dispatcher_chat_ask", "dispatcher_chat_show", "dispatcher_chat_leads", "dispatcher_decisions_list":
		return true
	}
	return false
}

// modelSettings are the owner's model settings in the policy: the case
// deadline, whether leads may chat, advice and the opt-in tools.
type modelSettings struct {
	Deadline  int  `json:"deadline_seconds"`
	ChatLeads bool `json:"chat_leads"`
	// Advise turns the model on as the advisor in rules mode, and
	// OptInTools are the owner's opt-in tools (d7).
	Advise     bool     `json:"advise"`
	OptInTools []string `json:"opt_in_tools"`
}

func readModelSettings(ctx context.Context, tx *transaction, pid string) (modelSettings, map[string]any, error) {
	var out modelSettings
	raw := map[string]any{}
	rows, err := query(ctx, tx, `SELECT model FROM dispatcher_policies WHERE project_id=?`, pid)
	if err != nil || len(rows) == 0 {
		return out, raw, err
	}
	_ = json.Unmarshal([]byte(str(rows[0], "model")), &raw)
	_ = json.Unmarshal([]byte(str(rows[0], "model")), &out)
	return out, raw, nil
}

// chatOverCaps reports whether a key-holder's chats have used their share
// of its runs this hour or day.
func chatOverCaps(ctx context.Context, tx *transaction, holder, nowRaw string) (bool, error) {
	_, caps, err := holderKey(ctx, tx, holder)
	if err != nil {
		return true, err
	}
	now := at(nowRaw)
	var hourRuns, dayRuns int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN served_at>=? THEN 1 END),COUNT(*) FROM dispatcher_cases WHERE lease_key=? AND kind='chat' AND served_at>=?`, layout(now.Add(-time.Hour)), holder, layout(now.Add(-24*time.Hour))).Scan(&hourRuns, &dayRuns); err != nil {
		return true, err
	}
	hour, day := chatShareOf(caps)
	return hourRuns >= hour || dayRuns >= day, nil
}

// chatShareOf is a key's chat runs per hour and per day: a quarter of its
// runs, rounded down, so caps under 4 leave chat none [#121 M1].
func chatShareOf(caps ModelCaps) (hour, day int) {
	return caps.RunsPerHour / chatShare, caps.RunsPerDay / chatShare
}

// chatAvailability is whether pid's agents can take a chat now:
// "available", "no_agent", "no_share" when no live key-holder's caps leave
// chat any runs, or "caps" when every live key-holder is past its runs or
// its chat share.
func chatAvailability(ctx context.Context, tx *transaction, pid, nowRaw string) (string, error) {
	holders, err := liveHolders(ctx, tx, pid, nowRaw)
	if err != nil || len(holders) == 0 {
		return "no_agent", err
	}
	shared := false
	for _, h := range holders {
		if _, caps, e := holderKey(ctx, tx, h); e == nil {
			if hour, day := chatShareOf(caps); hour > 0 && day > 0 {
				shared = true
			}
		}
	}
	if !shared {
		return "no_share", nil
	}
	for _, h := range holders {
		over, err := overCaps(ctx, tx, h, nowRaw)
		if err == nil && !over {
			over, err = chatOverCaps(ctx, tx, h, nowRaw)
		}
		if errors.Is(err, ErrModelUnauthorized) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !over {
			return "available", nil
		}
	}
	return "caps", nil
}

// dispatcherChatOp runs the chat commands and the decision log read. They
// are a person's CLI commands, never a session's.
func (s *Store) dispatcherChatOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, r Request) (any, error) {
	pid := str(p, "id")
	settings, raw, err := readModelSettings(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	switch r.Operation {
	case "dispatcher_chat_leads":
		if projectRole != "owner" {
			return nil, errors.New("project owner permission required")
		}
		return s.chatLeads(ctx, tx, w, p, raw, r.Args)
	case "dispatcher_chat_show":
		// The asker's own chats only: another person's reads as not found.
		return chatShow(ctx, tx, pid, s.principal, str(r.Args, "case_id"))
	}
	switch {
	case projectRole == "owner", projectRole == "lead" && (settings.ChatLeads || r.Operation == "dispatcher_decisions_list"):
	case projectRole == "lead":
		return nil, scoped("leads may chat with the dispatcher only when the project owner allows it (dispatcher chat leads on)", "forbidden")
	default:
		return nil, errors.New("project owner or lead permission required")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if r.Operation == "dispatcher_decisions_list" {
		hours, limit := asInt(r.Args["since_hours"]), asInt(r.Args["limit"])
		if hours < 1 || hours > 168 || limit < 1 || limit > maxDecisionsListed {
			return nil, invalid("since_hours is 1 to 168 and limit 1 to 50")
		}
		return decisionLog(ctx, tx, pid, nowRaw, hours, limit)
	}
	return s.chatAsk(ctx, tx, w, p, settings, nowRaw, r.Args)
}

// chatAsk opens a chat case for the caller's question.
func (s *Store) chatAsk(ctx context.Context, tx *transaction, w, p map[string]any, settings modelSettings, nowRaw string, a map[string]any) (any, error) {
	pid := str(p, "id")
	text, _ := a["text"].(string)
	if strings.TrimSpace(text) == "" || len(text) > maxChatQuestion || !utf8.ValidString(text) {
		return nil, invalid("the question is 1 to 2048 bytes of text")
	}
	_, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if meta["enabled"] != true || meta["mode"] != "model" {
		return nil, invalid("chat needs the dispatcher on with the model engine (dispatcher enable --engine model)")
	}
	if _, err = expireCases(ctx, tx, pid, nowRaw); err != nil {
		return nil, err
	}
	open, err := query(ctx, tx, `SELECT id FROM dispatcher_cases WHERE project_id=? AND kind='chat' AND asked_by=? AND closed_at IS NULL`, pid, s.principal)
	if err != nil {
		return nil, err
	}
	if len(open) > 0 {
		return nil, scoped("you have a chat open: "+str(open[0], "id")+"; wait for it, or read it with dispatcher chat show", "conflict")
	}
	switch availability, err := chatAvailability(ctx, tx, pid, nowRaw); {
	case err != nil:
		return nil, err
	case availability == "no_agent":
		return nil, scoped("no model agent is running for this project; try again once it is", "unavailable")
	case availability == "no_share":
		return nil, scoped("the model's caps leave no runs for chat: chat gets a quarter of the runs, so the owner's setup needs at least 4 runs an hour and 4 a day", "rate_limited")
	case availability == "caps":
		return nil, scoped("the model's run budget for chat is used up for now; try again later", "rate_limited")
	}
	ref, err := principalRef(ctx, tx, pid, s.principal, nowRaw)
	if err != nil {
		return nil, err
	}
	deadline := settings.Deadline
	if deadline < minCaseDeadline || deadline > maxCaseDeadline {
		deadline = defaultChatDeadline
	}
	message, _ := json.Marshal(map[string]any{"from": map[string]any{"session": "", "principal": ref}, "text": text})
	id, due := newID("case_"), layout(at(nowRaw).Add(time.Duration(deadline)*time.Second))
	if _, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,policy_revision,deadline,created_at,asked_by,message) VALUES(?,?,'chat','message',?,'',?,?,?,?,?)`,
		id, pid, newID("msg_"), meta["revision"], due, nowRaw, s.principal, string(message)); err != nil {
		return nil, err
	}
	if err = caseNotify(ctx, tx, pid); err != nil {
		return nil, err
	}
	// The length only: the question is the asker's own text.
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "dispatcher_chat", id, "dispatcher_chat_ask", map[string]any{"length": len(text)}); err != nil {
		return nil, err
	}
	return map[string]any{"case_id": id, "deadline": at(due).Format(time.RFC3339)}, nil
}

// chatShow is one of principal's chats, or its latest: the question, its
// state and the reply. A chat past its deadline reads as expired before
// any request closes it.
func chatShow(ctx context.Context, tx *transaction, pid, principal, caseID string) (any, error) {
	q, args := `SELECT * FROM dispatcher_cases WHERE project_id=? AND kind='chat' AND asked_by=?`, []any{pid, principal}
	if caseID != "" {
		q, args = q+` AND id=?`, append(args, caseID)
	}
	rows, err := query(ctx, tx, q+` ORDER BY created_at DESC,id DESC LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, scoped("no such chat", "not_found")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	c := rows[0]
	var message struct {
		Text string `json:"text"`
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(str(c, "message")), &message)
	_ = json.Unmarshal([]byte(str(c, "outcome_body")), &body)
	out := map[string]any{"case_id": str(c, "id"), "asked_at": at(str(c, "created_at")).Format(time.RFC3339), "deadline": at(str(c, "deadline")).Format(time.RFC3339), "question": message.Text}
	// An open chat in a project whose dispatcher is now off, or in rules
	// mode, will never be answered: it reads as the model turned off
	// (#121 L2).
	modelOn := true
	if c["closed_at"] == nil {
		policy, e := query(ctx, tx, `SELECT 1 FROM dispatcher_policies WHERE project_id=? AND enabled<>0 AND mode='model'`, pid)
		if e != nil {
			return nil, e
		}
		modelOn = len(policy) == 1
	}
	switch outcome := str(c, "outcome"); {
	case !modelOn:
		out["state"], out["reason_code"] = "closed", "model_off"
	case c["closed_at"] == nil && str(c, "deadline") > nowRaw:
		out["state"] = "waiting"
	case c["closed_at"] == nil || outcome == "expired":
		out["state"] = "expired"
	case outcome == "reply":
		out["state"], out["reply"] = "replied", str(body, "text")
	case outcome == "abstain":
		out["state"], out["reason_code"] = "abstained", str(body, "reason_code")
	default:
		// model_off, no_agent or caps: closed before an answer.
		out["state"], out["reason_code"] = "closed", outcome
	}
	return out, nil
}

// chatLeads sets whether the project's leads may chat (D6-M1(c)), by
// compare-and-swap on the policy revision.
func (s *Store) chatLeads(ctx context.Context, tx *transaction, w, p map[string]any, settings map[string]any, a map[string]any) (any, error) {
	pid := str(p, "id")
	allow, ok := a["allow"].(bool)
	expected, err := integer(a["expected_revision"])
	if !ok || err != nil || a["expected_revision"] == nil {
		return nil, invalid("dispatcher_chat_leads requires allow and expected_revision")
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
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	settings["chat_leads"] = allow
	raw, _ := json.Marshal(settings)
	revision := int64(expected) + 1
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_policies SET model=?,revision=?,updated_at=?,updated_by_principal=?,updated_by_session=NULL WHERE project_id=? AND revision=?`, string(raw), revision, nowRaw, s.principal, pid, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, conflict("the dispatcher policy changed concurrently; read it again")
	}
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "dispatcher_policy", pid, "dispatcher_chat_leads", map[string]any{"revision": revision, "chat_leads": allow}); err != nil {
		return nil, err
	}
	return map[string]any{"chat_leads": allow, "revision": revision}, nil
}

// decisionLog is pid's decision log, newest first: what an owner or lead
// reads with dispatcher decisions, and the model with list_decisions in a
// chat (D6-L4). A model decision names its case.
func decisionLog(ctx context.Context, tx *transaction, pid, nowRaw string, hours, limit int) ([]any, error) {
	rows, err := query(ctx, tx, `SELECT d.subject_type,d.subject_id,d.engine,d.action,d.reason_code,d.explanation,d.guardrail,d.created_at,(SELECT c.id FROM dispatcher_cases c WHERE c.project_id=d.project_id AND c.kind='decide' AND c.subject_id=d.subject_id AND c.subject_revision=d.subject_revision) AS case_id FROM dispatcher_decisions d WHERE d.project_id=? AND d.created_at>=? ORDER BY d.created_at DESC,d.id DESC LIMIT ?`,
		pid, layout(at(nowRaw).Add(-time.Duration(hours)*time.Hour)), limit)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, d := range rows {
		var caseID any
		if str(d, "engine") == "model" && str(d, "case_id") != "" {
			caseID = str(d, "case_id")
		}
		out = append(out, map[string]any{
			"case_id": caseID, "subject": map[string]any{"type": str(d, "subject_type"), "id": str(d, "subject_id")},
			"engine": str(d, "engine"), "action": str(d, "action"), "reason_code": str(d, "reason_code"),
			"explanation": str(d, "explanation"), "guardrail": str(d, "guardrail"), "created_at": at(str(d, "created_at")).Format(time.RFC3339),
		})
	}
	return out, nil
}
