package ctxwatch

import "encoding/json"

// codexLine is the part of a Codex rollout line the reader looks at.
type codexLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   *struct {
		Type string `json:"type"`
		Info *struct {
			LastTokenUsage *struct {
				InputTokens *int64 `json:"input_tokens"`
			} `json:"last_token_usage"`
			ModelContextWindow int64 `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

// codexParser reads a Codex rollout: the context in use is the last
// token_count event's last_token_usage.input_tokens (cached tokens
// included), with the window it states; a compaction is a compacted line.
type codexParser struct {
	known     bool
	counts    int
	usages    int
	tokens    int64
	window    int64
	usageAt   string
	compacts  int
	compactAt string
}

func (p *codexParser) line(raw []byte) {
	var l codexLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "session_meta", "event_msg", "response_item", "turn_context", "compacted":
		p.known = true
	}
	switch {
	case l.Type == "compacted":
		p.compacts++
		p.compactAt = stamp(l.Timestamp)
		// The usage before a compaction no longer says anything about
		// the context: unknown until the next model call.
		p.tokens, p.window, p.usageAt = 0, 0, ""
	case l.Type == "event_msg" && l.Payload != nil && l.Payload.Type == "token_count":
		info := l.Payload.Info
		// A token_count before the first call has no info yet: not a sign
		// of a changed format.
		if info == nil {
			return
		}
		p.counts++
		if info.LastTokenUsage == nil || info.LastTokenUsage.InputTokens == nil {
			return
		}
		p.usages++
		p.tokens = *info.LastTokenUsage.InputTokens
		p.window = info.ModelContextWindow
		p.usageAt = stamp(l.Timestamp)
	}
}

func (p *codexParser) finish(r *Reading) {
	switch {
	case !p.known:
		r.Status, r.Reason = StatusUnsupported, "no line in Codex's rollout format"
	case p.counts > 0 && p.usages == 0:
		r.Status, r.Reason = StatusUnsupported, "token counts carry no usage in a known form"
	default:
		r.Status = StatusOK
		r.Tokens, r.Window, r.UsageAt = p.tokens, max(p.window, 0), p.usageAt
		r.Compactions, r.LastCompactionAt = p.compacts, p.compactAt
	}
}
