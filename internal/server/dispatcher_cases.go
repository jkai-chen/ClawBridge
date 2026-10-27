package server

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"clawbridge/internal/bus"
)

// The dispatcher model's case API (DISPATCHER-MODEL-API §2–§4, d5): a
// project token (cbm_) opens hello, cases/next and its case's tools. Reads
// and outcomes run in the bus as the project's dispatcher-model session.
const (
	ModelHelloPath     = "/v1/dispatcher-model/hello"
	ModelCasesNextPath = "/v1/dispatcher-model/cases/next"
	modelCasesPrefix   = "/v1/dispatcher-model/cases/"
	// casePollEvery is how often cases/next checks for a case while it
	// waits; casePollLimit bounds the wait, so the server's write timeout
	// and the proxy's outlast it (D5-L2).
	casePollEvery = time.Second
	casePollLimit = 30 * time.Second
)

// caseRefusals are the case API's refusals and their statuses.
var caseRefusals = map[string]int{
	"case_not_found": 404, "case_closed": 409, "case_leased": 409, "deadline_passed": 409,
	"tool_refused": 400, "invalid_argument": 400, "not_in_case": 400, "cap_reached": 429,
}

// caseError is a refusal in the case API's own codes.
type caseError struct{ code, tool string }

func (e caseError) Error() string { return e.code }

// modelActions are v1's actions (the schema's ACTIONS): a decide names
// one; the case's permitted set narrows it.
var modelActions = []string{"confirm", "escalate", "wait", "decline", "route", "release_pending", "release"}

var (
	caseIDPattern = regexp.MustCompile(`^case_[0-9a-f]{32}$`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	codePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// casePath is a case tool path's case ID and tool name.
func casePath(path string) (caseID, tool string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, modelCasesPrefix), "/")
	if !strings.HasPrefix(path, modelCasesPrefix) || len(parts) != 3 || parts[1] != "tools" || parts[0] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// casePolls allows one cases/next poll in flight per token.
type casePolls struct {
	mu    sync.Mutex
	held  map[string]bool
	limit int
}

func (p *casePolls) take(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[key] || len(p.held) >= p.limit {
		return false
	}
	p.held[key] = true
	return true
}

func (p *casePolls) give(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, key)
}

// serveCasesNext long-polls for a case: a check now, then one a second
// until a case comes or wait_seconds pass. It stops at once when the
// client goes (D5-L2). A case that expired on the way nudges the rules'
// fallback.
func serveCasesNext(ctx context.Context, store ModelStore, token string, body map[string]json.RawMessage, maxAgents int, polls *casePolls, nudge func(string)) (any, error) {
	f, err := fields(body, "wait_seconds")
	if err != nil {
		return nil, err
	}
	var wait int
	if json.Unmarshal(f["wait_seconds"], &wait) != nil || wait < 0 || wait > int(casePollLimit/time.Second) {
		return nil, errModelArgument
	}
	key := tokenKey(token)
	if !polls.take(key) {
		return nil, errPollBusy
	}
	defer polls.give(key)
	until := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		next, err := store.ModelNextCase(ctx, token, maxAgents)
		if errors.Is(err, bus.ErrCaseLeased) {
			return nil, caseError{code: "case_leased"}
		}
		if err != nil {
			return nil, err
		}
		if next.Expired && nudge != nil {
			nudge(next.ProjectID)
		}
		if next.Case != nil {
			return map[string]any{"case": next.Case}, nil
		}
		if !time.Now().Before(until) {
			return map[string]any{"case": nil}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(casePollEvery):
		}
	}
}

// errPollBusy is a second cases/next while one is in flight for a token.
var errPollBusy = errors.New("server_busy")

// serveCaseTool runs one tool call: its closed body checked here, its case
// and lease checked in the bus.
func serveCaseTool(ctx context.Context, store ModelStore, token, caseID, tool string, body map[string]json.RawMessage, nudge func(string)) (any, error) {
	if !caseIDPattern.MatchString(caseID) {
		return nil, caseError{code: "case_not_found"}
	}
	args, err := toolArgs(tool, body)
	if err != nil {
		return nil, err
	}
	out, err := store.ModelTool(ctx, token, caseID, tool, args)
	if err != nil {
		return nil, err
	}
	if out.Fallback && nudge != nil {
		nudge(out.ProjectID)
	}
	if out.Error != "" {
		return nil, caseError{code: out.Error, tool: tool}
	}
	return map[string]any{"ok": true, "result": out.Result}, nil
}

// toolArgs reads a tool's body as exactly its schema (§3): requests are
// closed. A tool this release doesn't serve is refused by name.
func toolArgs(tool string, body map[string]json.RawMessage) (map[string]any, error) {
	refused := caseError{code: "tool_refused", tool: tool}
	switch tool {
	case "read_issue_facts":
		f, err := fields(body, "issue_id")
		if err != nil {
			return nil, err
		}
		id, err := stringField(f["issue_id"])
		if err != nil || !uuidPattern.MatchString(id) {
			return nil, errModelArgument
		}
		return map[string]any{"issue_id": strings.ToLower(id)}, nil
	case "list_domain":
		f, err := fields(body, "domain")
		if err != nil {
			return nil, err
		}
		domain, err := stringField(f["domain"])
		if err != nil || domain == "" || len(domain) > 128 {
			return nil, errModelArgument
		}
		return map[string]any{"domain": domain}, nil
	case "list_sessions":
		f, err := fields(body, "active_only")
		if err != nil {
			return nil, err
		}
		var active bool
		if json.Unmarshal(f["active_only"], &active) != nil {
			return nil, errModelArgument
		}
		return map[string]any{"active_only": active}, nil
	case "session_activity":
		f, err := fields(body, "session_id", "since_hours")
		if err != nil {
			return nil, err
		}
		session, err := stringField(f["session_id"])
		var hours int
		if err != nil || session == "" || len(session) > 128 || json.Unmarshal(f["since_hours"], &hours) != nil || hours < 1 || hours > 168 {
			return nil, errModelArgument
		}
		return map[string]any{"session_id": session, "since_hours": hours}, nil
	case "decide":
		f, err := fields(body, "action", "reason_code", "explanation", "usage")
		if err != nil {
			return nil, err
		}
		action, err1 := stringField(f["action"])
		reason, err2 := stringField(f["reason_code"])
		explanation, err3 := stringField(f["explanation"])
		usage, err4 := usageField(f["usage"])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || !slices.Contains(modelActions, action) || !codePattern.MatchString(reason) || utf8.RuneCountInString(explanation) > 280 {
			return nil, errModelArgument
		}
		return map[string]any{"action": action, "reason_code": reason, "explanation": explanation, "usage": usage}, nil
	case "abstain":
		f, err := fields(body, "reason_code", "usage")
		if err != nil {
			return nil, err
		}
		reason, err1 := stringField(f["reason_code"])
		usage, err2 := usageField(f["usage"])
		if err1 != nil || err2 != nil || !slices.Contains(bus.AbstainReasonCodes, reason) {
			return nil, errModelArgument
		}
		return map[string]any{"reason_code": reason, "usage": usage}, nil
	}
	// recommend, reply, list_decisions and the opt-in tools come with
	// advise and chat (PRs B and C).
	return nil, refused
}

// usageField reads an outcome's usage report [A10], closed.
func usageField(raw json.RawMessage) (map[string]any, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return nil, errModelArgument
	}
	f, err := fields(body, "input_tokens", "output_tokens", "tool_calls", "duration_ms", "provider", "model")
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, n := range []string{"input_tokens", "output_tokens", "tool_calls", "duration_ms"} {
		var v int64
		// Bounded so input plus output fits the 32-bit column (R106-L3).
		if json.Unmarshal(f[n], &v) != nil || v < 0 || v > 1_000_000_000 {
			return nil, errModelArgument
		}
		out[n] = v
	}
	for _, n := range []string{"provider", "model"} {
		v, err := stringField(f[n])
		if err != nil || v == "" || len(v) > 128 {
			return nil, errModelArgument
		}
		out[n] = v
	}
	return out, nil
}
