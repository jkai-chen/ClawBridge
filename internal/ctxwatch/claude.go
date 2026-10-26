package ctxwatch

import "encoding/json"

// claudeLine is the part of a Claude Code transcript line the reader looks
// at. Everything else in the line, the message content included, is
// skipped by the decoder and never kept.
type claudeLine struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	Message     *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              *int64 `json:"input_tokens"`
			CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// claudeParser reads Claude Code's JSONL: the context in use is the last
// main-thread assistant line's input, cache-read and cache-creation tokens;
// a compaction is a compact_boundary system line. The window is not in the
// file.
type claudeParser struct {
	known      bool
	assistants int
	usages     int
	tokens     int64
	usageAt    string
	compacts   int
	compactAt  string
}

func (p *claudeParser) line(raw []byte) {
	var l claudeLine
	if json.Unmarshal(raw, &l) != nil || l.Type == "" {
		return
	}
	switch l.Type {
	case "user", "assistant", "system", "summary":
		p.known = p.known || l.SessionID != "" || l.Type == "summary"
	}
	switch {
	case l.Type == "system" && l.Subtype == "compact_boundary":
		p.compacts++
		p.compactAt = stamp(l.Timestamp)
		// The usage before a compaction no longer says anything about
		// the context: unknown until the next model call.
		p.tokens, p.usageAt = 0, ""
	case l.Type == "assistant" && !l.IsSidechain && l.Message != nil:
		// A subagent's sidechain has its own context, and a synthetic
		// message (an API error shown in the transcript) has no real usage.
		if l.Message.Model == "<synthetic>" {
			return
		}
		p.assistants++
		u := l.Message.Usage
		if u == nil || u.InputTokens == nil {
			return
		}
		p.usages++
		p.tokens = *u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		p.usageAt = stamp(l.Timestamp)
	}
}

func (p *claudeParser) finish(r *Reading) {
	switch {
	case !p.known:
		r.Status, r.Reason = StatusUnsupported, "no line in Claude Code's transcript format"
	case p.assistants > 0 && p.usages == 0:
		r.Status, r.Reason = StatusUnsupported, "assistant lines carry no usage in a known form"
	default:
		r.Status = StatusOK
		r.Tokens, r.UsageAt = p.tokens, p.usageAt
		r.Compactions, r.LastCompactionAt = p.compacts, p.compactAt
	}
}
