package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// An escalation's structured payload (v0.4, R4): what kind of question it
// is, the question in a fixed template, the options a person may answer
// with, the option the engine would pick (advisory: nothing applies it),
// and when it times out. A person (the project owner or a lead, never a
// session) answers it with dispatcher_escalation_answer, which records the
// answer and resolves the row: it never confirms or releases the claim.

// escalationKinds maps every reason an escalation carries today to its
// kind. A reason not in it is other.
//   - approval: an engine would confirm, and a guardrail asks a person
//     first;
//   - decision: the engine couldn't decide (the loop budget, uncertain, an
//     abstain's reasons, the rules finding no answer);
//   - escalation: a trust or policy refusal.
var escalationKinds = map[string]string{
	"trusted":                           "approval",
	"guardrail:facts_not_authoritative": "approval",
	"guardrail:session_not_active":      "approval",
	"guardrail:policy_changed":          "approval",

	"loop_budget":        "decision",
	"uncertain":          "decision",
	"insufficient_facts": "decision",
	"refused":            "decision",
	"facts_unavailable":  "decision",
	"session_stale":      "decision",
	"session_gone":       "decision",
	"no_domain":          "decision",
	"label_only":         "decision",
	"ambiguous":          "decision",

	"untrusted":                        "escalation",
	"guardrail:untrusted":              "escalation",
	"overlap":                          "escalation",
	"guardrail:overlap":                "escalation",
	"issue_closed":                     "escalation",
	"guardrail:issue_closed":           "escalation",
	"guardrail:not_one_project_domain": "escalation",
	"guardrail:session_not_gone":       "escalation",
	"guardrail:unknown_action":         "other",
}

// escalationKindOf is reason's kind; an unknown reason is other.
func escalationKindOf(reason string) string {
	if k, ok := escalationKinds[reason]; ok {
		return k
	}
	return "other"
}

// escalationQuestions are the fixed questions, one per kind, filled only
// with the session's name and the issue's identifier (or UUID).
var escalationQuestions = map[string]string{
	"approval":   "Should %s's claim of %s be confirmed?",
	"decision":   "The dispatcher could not decide %s's claim of %s: confirm it, release it, or leave it for now?",
	"escalation": "The dispatcher's policy stopped %s's claim of %s: confirm it anyway, release it, or leave it for now?",
	"other":      "What should happen to %s's claim of %s?",
}

// EscalationOption is one answer a person may give.
type EscalationOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

const (
	maxEscalationOptions = 10
	maxOptionLabel       = 80
	// defaultEscalationTimeout is an escalation's timeout without a
	// domain's stale_after_hours.
	defaultEscalationTimeout = 24 * time.Hour
)

var optionIDPattern = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

var (
	optionConfirm = EscalationOption{ID: "confirm", Label: "Confirm the claim"}
	optionRelease = EscalationOption{ID: "release", Label: "Release the claim"}
	optionWait    = EscalationOption{ID: "wait", Label: "Leave it for now"}
)

// escalationOptionsFor are a claim escalation's options by kind: confirm
// and release, and wait where the engine had no answer or was refused.
func escalationOptionsFor(kind string) []EscalationOption {
	if kind == "decision" || kind == "escalation" {
		return []EscalationOption{optionConfirm, optionRelease, optionWait}
	}
	return []EscalationOption{optionConfirm, optionRelease}
}

// optionForAction is the option an engine action stands for: confirm,
// decline (release) or wait; anything else stands for none.
func optionForAction(action string) string {
	switch action {
	case "confirm":
		return "confirm"
	case "decline":
		return "release"
	case "wait":
		return "wait"
	}
	return ""
}

// escalationPayload is the payload a new escalation carries.
type escalationPayload struct {
	Kind      string
	Question  string
	Options   []EscalationOption
	Suggested string
	TimeoutAt string
}

// validate refuses a payload the code built wrongly: too many options,
// a bad or duplicate id, a bad label, or a suggestion not among them.
func (p escalationPayload) validate() error {
	if len(p.Options) > maxEscalationOptions {
		return fmt.Errorf("an escalation has at most %d options, not %d", maxEscalationOptions, len(p.Options))
	}
	seen := map[string]bool{}
	for _, o := range p.Options {
		if !optionIDPattern.MatchString(o.ID) {
			return fmt.Errorf("escalation option id %q is not ^[a-z][a-z_]{0,31}$", o.ID)
		}
		if seen[o.ID] {
			return fmt.Errorf("escalation option id %q is repeated", o.ID)
		}
		seen[o.ID] = true
		if o.Label == "" || len(o.Label) > maxOptionLabel {
			return fmt.Errorf("escalation option %q needs a label of 1 to %d characters", o.ID, maxOptionLabel)
		}
	}
	if p.Suggested != "" && !seen[p.Suggested] {
		return fmt.Errorf("the suggested option %q is not among the options", p.Suggested)
	}
	return nil
}

// plainName keeps a session name or issue identifier to the characters a
// valid one has: both are checked when stored, so this only guards the
// question against a row written some other way.
func plainName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			b.WriteRune(r)
		}
		if b.Len() >= 128 {
			break
		}
	}
	return b.String()
}

// buildEscalationPayload is the payload for an escalation of proposal (the
// pending claim's row with its session's name) for reason. suggested is
// the engine's own action (confirm, decline or wait), or empty. dom may be
// nil.
func buildEscalationPayload(proposal map[string]any, reason, suggested string, dom *DispatcherDomain, nowRaw string) (escalationPayload, error) {
	kind := escalationKindOf(reason)
	issue := plainName(str(proposal, "issue_identifier"))
	if issue == "" {
		issue = plainName(str(proposal, "issue_uuid"))
	}
	timeout := defaultEscalationTimeout
	if dom != nil && dom.StaleAfterHours > 0 {
		timeout = time.Duration(dom.StaleAfterHours) * time.Hour
	}
	p := escalationPayload{
		Kind:      kind,
		Question:  fmt.Sprintf(escalationQuestions[kind], plainName(str(proposal, "session_name")), issue),
		Options:   escalationOptionsFor(kind),
		TimeoutAt: layout(at(nowRaw).Add(timeout)),
	}
	if id := optionForAction(suggested); id != "" && slices.ContainsFunc(p.Options, func(o EscalationOption) bool { return o.ID == id }) {
		p.Suggested = id
	}
	return p, p.validate()
}

// escalationOptions decodes a row's options; nil for an old row.
func escalationOptions(row map[string]any) []EscalationOption {
	raw := str(row, "options")
	if raw == "" {
		return nil
	}
	var out []EscalationOption
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

func optionIDs(options []EscalationOption) []string {
	ids := make([]string, len(options))
	for i, o := range options {
		ids[i] = o.ID
	}
	return ids
}

// errAnswerFromSession refuses an answer through a session: it is a
// person's, as who closes issues is.
var errAnswerFromSession = scoped("a session can never answer an escalation; the project owner or a lead runs it without --session", "forbidden")

// escalationAnswer records a person's answer to one escalation: one of
// its options, by the project owner or a lead, once. The same answer again
// returns the stored one; a different one is a conflict. It resolves the
// row, audits it and records a decision of kind escalation, and never acts
// on the claim: the person still confirms or releases it.
func (s *Store) escalationAnswer(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, r Request) (any, error) {
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project owner or lead permission required")
	}
	pid := str(p, "id")
	id, option := str(r.Args, "escalation_id"), str(r.Args, "option")
	if id == "" || option == "" {
		return nil, invalid("dispatcher_escalation_answer needs escalation_id and option")
	}
	rows, err := query(ctx, tx, `SELECT e.*,a.session_id,a.state AS proposal_state,a.revision AS proposal_revision,s.name AS session_name FROM dispatcher_escalations e LEFT JOIN session_assignments a ON a.project_id=e.project_id AND a.id=e.subject_id LEFT JOIN sessions s ON s.id=a.session_id AND s.project_id=a.project_id WHERE e.project_id=? AND e.id=?`, pid, id)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, scoped("no escalation "+id+" in this project", "not_found")
	}
	e := rows[0]
	options := escalationOptions(e)
	if len(options) == 0 {
		return nil, invalid("this escalation has no options; act with assignment confirm or release")
	}
	if !slices.Contains(optionIDs(options), option) {
		return nil, invalid(fmt.Sprintf("option %q is not one of this escalation's: %s", option, strings.Join(optionIDs(options), ", ")))
	}
	if answered := str(e, "answer"); answered != "" {
		if answered != option {
			return nil, conflict(fmt.Sprintf("this escalation was already answered %s", answered))
		}
		return answerResult(e, answered, str(e, "answered_by"), str(e, "answered_at")), nil
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE dispatcher_escalations SET answer=?,answered_by=?,answered_at=?,state='resolved',updated_at=? WHERE project_id=? AND id=? AND answer IS NULL`, option, s.principal, nowRaw, nowRaw, pid, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, conflict("this escalation was answered concurrently; read it again")
	}
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "dispatcher_escalation", id, "dispatcher_escalation_answer", map[string]any{"option": option, "subject_id": str(e, "subject_id"), "revision": str(e, "subject_revision")}); err != nil {
		return nil, err
	}
	if err = controlDecision(ctx, tx, pid, "escalation", str(e, "subject_type"), str(e, "subject_id"), str(e, "subject_revision"), str(e, "domain"), "answered", "answer:"+option, "none", nowRaw); err != nil {
		return nil, err
	}
	return answerResult(e, option, s.principal, nowRaw), nil
}

// answerResult is an answer's result: what was recorded, and, for confirm
// or release while the claim is still pending at the escalation's
// revision, the command that acts on it.
func answerResult(e map[string]any, option, by, when string) map[string]any {
	out := map[string]any{"escalation_id": str(e, "id"), "answer": option, "answered_by": by, "answered_at": at(when).Format(time.RFC3339), "state": "resolved"}
	note := "recorded; nothing was confirmed or released"
	switch {
	case str(e, "proposal_state") != "proposed" || str(e, "proposal_revision") != str(e, "subject_revision"):
		note = "recorded; the claim has moved on since this escalation"
	case str(e, "session_name") != "" && (option == "confirm" || option == "release"):
		note = fmt.Sprintf("recorded; to act on it, run clawbridge assignment %s %s --revision %s", option, str(e, "session_name"), str(e, "subject_revision"))
	}
	out["note"] = note
	return out
}
