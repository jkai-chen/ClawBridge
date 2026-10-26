package doctor

import (
	"fmt"

	"clawbridge/internal/ctxwatch"
)

// ContextState is what the client read about this session's context: its
// binding record and, when bound, its transcript (CONTEXT-WATCH §4.6).
type ContextState struct {
	Session string
	Bound   bool
	// BindingPath is the record's path; Transcript the bound file's.
	BindingPath, Transcript string
	Reading                 ctxwatch.Reading
	// Levels are the cached effective levels, when a report has cached any.
	Levels *ctxwatch.Levels
	// Err is a failure to read the record or the transcript.
	Err error
}

// checkContext reports the session's context: counts and the transcript's
// path, never content. It warns, and never fails.
func checkContext(s ContextState, add func(Check)) {
	c := Check{ID: "context", Title: "Context watching", Status: Pass, Path: s.BindingPath}
	switch {
	case s.Err != nil:
		c.Status, c.Message = Warn, s.Err.Error()
		c.Action = "Check the transcript the session is bound to, and bind it again if it moved."
	case !s.Bound:
		c.Message = fmt.Sprintf("Session %s is not bound to a transcript; its context isn't watched.", s.Session)
	case s.Reading.Status != ctxwatch.StatusOK:
		c.Status, c.Path = Warn, s.Transcript
		c.Message = fmt.Sprintf("The bound %s transcript is unsupported: %s.", s.Reading.Agent, s.Reading.Reason)
		c.Action = "Update ClawBridge; the agent's transcript format may have changed."
	default:
		c.Path = s.Transcript
		c.Message = fmt.Sprintf("%d tokens in use", s.Reading.Tokens)
		if s.Reading.UsageAt == "" {
			// Before the first model call, or since a compaction: no level.
			c.Message = "Context unknown until the next model call"
		} else if s.Reading.Window > 0 {
			c.Message += fmt.Sprintf(" of a %d-token window", s.Reading.Window)
		}
		least := ""
		if s.Reading.Partial {
			least = "at least "
		}
		c.Message += fmt.Sprintf(", %s%d compaction(s).", least, s.Reading.Compactions)
		if s.Levels != nil && s.Reading.UsageAt != "" {
			switch s.Levels.Level(s.Reading.Tokens) {
			case "handover":
				c.Status = Warn
				c.Message += fmt.Sprintf(" Past the handover level (%d).", s.Levels.HandoverTokens)
				c.Action = "Write the handover note now, then compact at a clean break."
			case "warn":
				c.Status = Warn
				c.Message += fmt.Sprintf(" Past the warn level (%d).", s.Levels.WarnTokens)
				c.Action = "Plan a handover before the handover level."
			}
		}
	}
	add(c)
}
