package ctxwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Binding is a session's local record of its transcript (§4.1), at
// <state home>/context/<session>.json. It is private to the user and never
// sent anywhere. The binding commands write it (SaveBinding).
type Binding struct {
	Version    int    `json:"version"`
	Agent      string `json:"agent"`
	Transcript string `json:"transcript"`
	BoundAt    string `json:"bound_at"`
	// Levels and HandoverHint are cached from the server's answer to a
	// context report (§4.4, CW-L5), so nothing local makes a network call.
	Levels       *Levels `json:"levels,omitempty"`
	HandoverHint string  `json:"handover_hint,omitempty"`
	// CompactingAt and CompactTrigger are the last compaction the agent
	// announced (Claude Code's PreCompact hook), for the reporter (§4.4).
	// ClaudeSessionID and AgentPID are the Claude Code session and process
	// a hook bound, so a nested agent in the same project can't take the
	// binding over while that process lives.
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	AgentPID        int    `json:"agent_pid,omitempty"`
	CompactingAt    string `json:"compacting_at,omitempty"`
	CompactTrigger  string `json:"compact_trigger,omitempty"`
	// ReportedCompactions is the server's compaction count for the session,
	// and SeenCompactionAt the last compaction counted into it. The reader
	// sees only a transcript's tail, so its own count can fall; this one
	// only moves on a compaction newer than the last seen.
	ReportedCompactions int64  `json:"reported_compactions,omitempty"`
	SeenCompactionAt    string `json:"seen_compaction_at,omitempty"`
}

// Levels are a session's effective thresholds, in tokens.
type Levels struct {
	WarnTokens     int64 `json:"warn_tokens"`
	HandoverTokens int64 `json:"handover_tokens"`
}

// Level names where tokens stand against levels: ok, warn or handover.
func (l Levels) Level(tokens int64) string {
	switch {
	case l.HandoverTokens > 0 && tokens >= l.HandoverTokens:
		return "handover"
	case l.WarnTokens > 0 && tokens >= l.WarnTokens:
		return "warn"
	}
	return "ok"
}

// BindingPath is where session's binding record lives under dir.
func BindingPath(dir, session string) (string, error) {
	if session == "" || session != filepath.Base(session) || strings.ContainsAny(session, `/\:`) || session == "." || session == ".." {
		return "", fmt.Errorf("%q is not a session name", session)
	}
	return filepath.Join(dir, session+".json"), nil
}

// LoadBinding reads session's binding record from dir. A missing record is
// not an error: the session is simply not bound.
func LoadBinding(dir, session string) (Binding, bool, error) {
	path, err := BindingPath(dir, session)
	if err != nil {
		return Binding{}, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return Binding{}, false, fmt.Errorf("the binding record %s is not a regular file", path)
	}
	if err := privateToUser(info); err != nil {
		return Binding{}, false, fmt.Errorf("the binding record %s %s", path, err)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return Binding{}, false, fmt.Errorf("the binding record %s is not readable", path)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return Binding{}, false, fmt.Errorf("the binding record %s is not readable", path)
	}
	var b Binding
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil || b.Version != 1 || (b.Agent != ClaudeCode && b.Agent != Codex) || !filepath.IsAbs(b.Transcript) {
		return Binding{}, false, fmt.Errorf("the binding record %s is not valid; bind the session again", path)
	}
	return b, true, nil
}
