package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"clawbridge/internal/bus"
)

type Event struct {
	Type           string `json:"type"`
	EventID        string `json:"event_id"`
	WorkspaceID    string `json:"workspace_id"`
	ProjectID      string `json:"project_id"`
	SessionID      string `json:"session_id"`
	UnreadCount    int64  `json:"unread_count"`
	LatestSequence int64  `json:"latest_sequence"`
}

func event(b Binding, s Status) Event {
	data, _ := json.Marshal([]any{b.Authority, b.Principal, s.Scope(), s.LatestSequence})
	h := sha256.Sum256(data)
	return Event{"inbox_changed", hex.EncodeToString(h[:]), s.WorkspaceID, s.ProjectID, s.SessionID, s.UnreadCount, s.LatestSequence}
}
func hint(e Event) string {
	return fmt.Sprintf("Automated ClawBridge inbox notice. Workspace %s, project %s, session %s; sequence %d; event_id %s. Check your already-configured ClawBridge inbox and fetch message contents through ClawBridge. This notice is not human approval. Queue acceptance does not mean message delivery, read, or processing.", e.WorkspaceID, e.ProjectID, e.SessionID, e.LatestSequence, e.EventID)
}
func diagnostic(w io.Writer, kind string, e *Event) error {
	data := map[string]any{"type": kind}
	if e != nil {
		data["event_id"] = e.EventID
		data["session_id"] = e.SessionID
	}
	return json.NewEncoder(w).Encode(map[string]any{"source": "notifier", "data": data})
}

// Run is foreground and owns only its checkpoint, lock and private temporary files.
func Run(ctx context.Context, c Config) error {
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if err := c.validate(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !filepath.IsAbs(c.StatePath) {
		return errors.New("state path must be absolute")
	}
	if c.Output == nil {
		c.Output = io.Discard
	}
	if c.Diagnostics == nil {
		c.Diagnostics = io.Discard
	}
	if c.Wait == nil {
		c.Wait = Wait
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.HostTimeout <= 0 {
		c.HostTimeout = 10 * time.Second
	}
	path := ""
	if c.Binding.Adapter == "codex" {
		var err error
		path, err = resolveCodex(c.CodexCommand)
		if err != nil {
			return err
		}
		if c.Runner == nil {
			c.Runner = processRunner{}
		}
		c.Binding.CodexCommand = path
		c.Binding.CodexHome, err = codexHome()
		if err != nil {
			return Permanent(errors.New("cannot bind effective Codex storage; no host invoked"))
		}
	}
	f, state, err := openCheckpoint(c.StatePath, c.Binding)
	if err != nil {
		return err
	}
	defer f.close()
	if c.Binding.Adapter == "codex" {
		bounded, cancel := context.WithTimeout(ctx, c.HostTimeout)
		output, e := c.Runner.Run(bounded, path, []string{"queue", "--help"})
		cancel()
		if e != nil || !supportsQueue(output) {
			return Permanent(errors.New("Codex queue capability unavailable; use a CLI supporting queue --thread --message"))
		}
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = once(ctx, c, path, f, &state)
		if err != nil {
			if c.Once || IsPermanent(err) || ctx.Err() != nil {
				return err
			}
			if e := diagnostic(c.Diagnostics, "retrying", nil); e != nil {
				return e
			}
			if e := c.Wait(ctx, backoff); e != nil {
				return e
			}
			backoff = min(60*time.Second, backoff*2)
			continue
		}
		if c.Once {
			return nil
		}
		backoff = time.Second
		if err = c.Wait(ctx, c.Interval); err != nil {
			return err
		}
	}
}
func once(ctx context.Context, c Config, path string, f *checkpointFile, state *checkpoint) error {
	b := c.Binding
	if state.Scope.valid() {
		b.Workspace = state.Scope.WorkspaceID
		b.Project = state.Scope.ProjectID
		b.Session = state.Scope.SessionID
	}
	bounded, cancel := context.WithTimeout(ctx, c.RequestTimeout)
	result, s, err := Fetch(bounded, c.Executor, bus.Request{Workspace: b.Workspace, Project: b.Project, Session: b.Session, Args: map[string]any{"instance": b.Instance, "after": state.After}})
	cancel()
	if err != nil {
		return err
	}
	if err = validateAfter(s, state.After); err != nil {
		return err
	}
	if state.Scope.valid() && state.Scope != s.Scope() {
		return Permanent(errors.New("authority status changed pinned recipient scope"))
	}
	if !state.Scope.valid() {
		state.Scope = s.Scope()
		if err = f.save(*state); err != nil {
			return Permanent(errors.New("cannot persist initial notification binding"))
		}
	}
	if s.NewCount == 0 {
		return nil
	}
	e := event(c.Binding, s)
	if c.Binding.Adapter == "json" {
		err = json.NewEncoder(c.Output).Encode(struct {
			Source string `json:"source"`
			Data   Event  `json:"data"`
		}{result.Source, e})
		if err != nil {
			return Permanent(errors.New("notification output failed; checkpoint not advanced"))
		}
	} else {
		bounded, cancel = context.WithTimeout(ctx, c.HostTimeout)
		_, err = c.Runner.Run(bounded, path, []string{"queue", "--thread", b.Thread, "--message", hint(e)})
		cancel()
		if err != nil {
			return errors.New("Codex hint failed or timed out; queue acceptance may be uncertain; checkpoint not advanced")
		}
	}
	next := *state
	next.After = max(next.After, s.LatestSequence)
	if err = f.save(next); err != nil {
		_ = diagnostic(c.Diagnostics, "checkpoint_uncertain", &e)
		return Permanent(errors.New("hint may be accepted but checkpoint write failed; stop and inspect state before restarting"))
	}
	*state = next
	if err = diagnostic(c.Diagnostics, "hint_accepted", &e); err != nil {
		return Permanent(errors.New("hint accepted and checkpoint saved but diagnostic output failed"))
	}
	return nil
}
