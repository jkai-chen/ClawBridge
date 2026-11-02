package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
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
	if c.Binding.Adapter == "command" {
		argv, err := resolveCommand(c.Binding.Command)
		if err != nil {
			return err
		}
		c.Binding.Command = argv
		if c.CommandRunner == nil {
			c.CommandRunner = runCommand
		}
	}
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
	// Codex's queue support is checked before its first hint. Only a help
	// text that ran and lacks queue --thread --message is permanent; a
	// probe that timed out or failed to run (a slow first start, a scanned
	// new binary) is retried with the loop's backoff, said once, and no hint
	// is sent until it passes.
	probed := c.Binding.Adapter != "codex"
	probeFailed := false
	probe := func() error {
		bounded, cancel := context.WithTimeout(ctx, c.HostTimeout)
		output, e := c.Runner.Run(bounded, path, []string{"queue", "--help"})
		cancel()
		switch {
		case e == nil && supportsQueue(output):
			probed = true
			return nil
		case e == nil && len(bytes.TrimSpace(output)) > 0:
			// It ran and said something else: no queue support (or help over
			// 16 KiB, which supportsQueue doesn't read).
			return Permanent(errors.New("Codex queue capability unavailable (its queue --help lacks queue --thread --message, or is over 16 KiB); use a CLI supporting queue --thread --message"))
		}
		// No usable output: an error, a timeout or an empty answer.
		if !probeFailed {
			probeFailed = true
			if err := diagnostic(c.Diagnostics, "codex_probe_retrying", nil); err != nil {
				return Permanent(err)
			}
		}
		return errors.New("Codex queue capability check timed out or failed to run; retrying")
	}
	if !c.Once {
		// The watcher holds the session's process: it keeps its lease.
		// Both loops end before Run returns: the reporter writes under
		// ContextDir, which the caller may remove once Run is done.
		beat, stop := context.WithCancel(ctx)
		var loops sync.WaitGroup
		defer loops.Wait()
		defer stop()
		b := c.Binding
		session := func() (bus.Request, bool) {
			return bus.Request{Workspace: b.Workspace, Project: b.Project, Session: b.Session, Args: map[string]any{"instance": b.Instance}}, true
		}
		loops.Add(1)
		go func() {
			defer loops.Done()
			KeepAlive(beat, c.Executor, HeartbeatInterval, session)
		}()
		if c.ContextDir != "" {
			loops.Add(1)
			go func() {
				defer loops.Done()
				ReportContext(beat, c.Executor, c.ContextDir, ContextInterval, session)
			}()
		}
	}
	// The server's observe stream wakes the loop when the session's queue
	// changes; without one (local mode, an older server) it polls.
	var wake *waker
	if s, ok := c.Executor.(Streamer); ok && !c.Once {
		streamCtx, stop := context.WithCancel(ctx)
		defer stop()
		wake = startWaker(streamCtx, s, c.Binding)
	}
	backoff := time.Second
	// skipped is the newest batch left to the channel, said once.
	var skipped int64
	var hold holdState
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = nil
		if !probed {
			err = probe()
		}
		if err == nil {
			err = once(ctx, c, path, f, &state, &skipped, &hold)
		}
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
		// A batch left to a push that is not yet confirmed is typed once
		// the push is 120 s old: wake at that deadline, not at the next
		// safety poll.
		var limit time.Duration
		if !hold.recheck.IsZero() {
			limit = max(hold.recheck.Sub(c.now()), time.Second)
		}
		if err = wake.wait(ctx, c, limit); err != nil {
			return err
		}
	}
}

// channelHasIt says whether the session's Claude Code channel wakes the
// agent instead of a typed hint (FR26): its server's record is fresh, the
// server alive, and a push of its was confirmed. The record is the one of
// the binding's session, as CLAWBRIDGE_SESSION names it.
func channelHasIt(c Config) bool {
	if c.ContextDir == "" {
		return false
	}
	st, ok, err := ctxwatch.LoadChannelState(c.ContextDir, c.Binding.Session)
	return err == nil && ok && ctxwatch.ChannelSkipsTyping(st, c.now())
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// holdOff is how long the notifier waits, with the agent idle and its
// channel unconfirmed, for the channel's first push before it types (FR59).
const holdOff = 10 * time.Second

// holdState is what the loop remembers about the channel hold-off.
type holdState struct {
	// seq is the newest batch already held off: it is held at most once.
	seq int64
	// said is the newest batch whose skip was reported.
	said int64
	// recheck is when a skipped push goes unconfirmed (120 s after it);
	// zero when nothing is skipped.
	recheck time.Time
}

// channelPushed says whether the channel has already pushed the batch up to
// seq, and until when that holds. Two signs count, each for 120 s only, so a
// push that leads to no ack never holds a batch back forever: the agent's
// turn record shows a channel turn for seq that is still running (when one
// covers the batch it decides alone), or, with no such turn yet, the
// channel's record names a push of seq or later. The deadline is when the
// sign runs out; the loop wakes for it.
func channelPushed(c Config, seq int64) (bool, time.Time) {
	if c.ContextDir == "" {
		return false, time.Time{}
	}
	now := c.now()
	turn, tok := ctxwatch.LoadTurn(c.ContextDir, c.Binding.Session)
	if tok && turn.ChannelSeq > 0 && turn.ChannelSeq >= seq {
		// A channel turn covers the batch: it alone decides. The record's
		// push was answered by that turn, so its own window must not keep
		// the batch skipped after the turn has ended.
		if at, err := time.Parse(time.RFC3339Nano, turn.ChannelAt); err == nil {
			deadline := at.Add(ctxwatch.ChannelConfirmWithin)
			if now.Before(deadline) && !at.After(now.Add(time.Minute)) && !ctxwatch.Idle(turn, tok, now) {
				return true, deadline
			}
			return false, time.Time{}
		}
	}
	st, ok, err := ctxwatch.LoadChannelState(c.ContextDir, c.Binding.Session)
	if err != nil || !ok || st.LastPushSeq < seq {
		return false, time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, st.LastPushAt)
	if err != nil {
		return false, time.Time{}
	}
	if deadline := at.Add(ctxwatch.ChannelConfirmWithin); now.Before(deadline) && !at.After(now.Add(time.Minute)) {
		return true, deadline
	}
	return false, time.Time{}
}

// heldOff decides, just before a non-json adapter would type the batch of
// seq, whether the channel has it (FR59). A channel that is unconfirmed
// ("no push confirmed yet") and an idle agent: the notifier's typed line
// would start the turn first and the channel, which pushes only to an idle
// agent, would never get to confirm. So it waits holdOff, once per batch,
// and skips the batch if the channel pushed meanwhile; otherwise it types.
// A batch skipped for a push is typed when that push is 120 s old and the
// channel has not become active.
func heldOff(ctx context.Context, c Config, seq int64, h *holdState) (bool, error) {
	h.recheck = time.Time{}
	if h.seq != seq && c.ContextDir != "" {
		st, ok, err := ctxwatch.LoadChannelState(c.ContextDir, c.Binding.Session)
		turn, tok := ctxwatch.LoadTurn(c.ContextDir, c.Binding.Session)
		if err == nil && ok && ctxwatch.ChannelMayHoldOff(st, c.now()) && ctxwatch.Idle(turn, tok, c.now()) {
			h.seq = seq
			if pushed, _ := channelPushed(c, seq); !pushed {
				if err := c.Wait(ctx, holdOff); err != nil {
					return false, err
				}
			}
		}
	}
	if h.seq != seq {
		return false, nil
	}
	pushed, deadline := channelPushed(c, seq)
	h.recheck = deadline
	return pushed, nil
}

func once(ctx context.Context, c Config, path string, f *checkpointFile, state *checkpoint, skipped *int64, hold *holdState) error {
	hold.recheck = time.Time{}
	b := c.Binding
	if state.Scope.valid() {
		b.Workspace = state.Scope.WorkspaceID
		b.Project = state.Scope.ProjectID
		b.Session = state.Scope.SessionID
	}
	bounded, cancel := context.WithTimeout(ctx, c.RequestTimeout)
	result, notification, err := FetchNotification(bounded, c.Executor, bus.Request{Workspace: b.Workspace, Project: b.Project, Session: b.Session, Args: map[string]any{"instance": b.Instance, "after": state.After}})
	cancel()
	if err != nil {
		return err
	}
	s := notification.Status
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
	if notification.Policy != "accept" {
		return nil
	}
	if s.NewCount == 0 {
		return nil
	}
	e := event(c.Binding, s)
	// A confirmed channel wakes the agent: no typed hint as well. The
	// checkpoint stays where it is, so if the channel falls back the same
	// batch is still hinted. A dropped push shows only when the channel
	// server demotes itself, 120 s after it; with its 60 s between pushes
	// and, on an observe stream that has nothing more to say, this loop's
	// 60 s safety re-poll, a batch may wait up to about 4 minutes longer
	// than with typing alone.
	if c.Binding.Adapter != "json" && channelHasIt(c) {
		if *skipped == s.LatestSequence {
			return nil
		}
		*skipped = s.LatestSequence
		if err = json.NewEncoder(c.Diagnostics).Encode(map[string]any{"source": "notifier", "data": map[string]any{"type": "hint_skipped", "reason": "channel", "event_id": e.EventID, "session_id": e.SessionID}}); err != nil {
			return Permanent(errors.New("diagnostic output failed"))
		}
		return nil
	}
	if c.Binding.Adapter != "json" {
		skip, err := heldOff(ctx, c, s.LatestSequence, hold)
		if err != nil {
			return err
		}
		if skip {
			if hold.said == s.LatestSequence {
				return nil
			}
			hold.said = s.LatestSequence
			if err = json.NewEncoder(c.Diagnostics).Encode(map[string]any{"source": "notifier", "data": map[string]any{"type": "hint_skipped", "reason": "channel_push", "event_id": e.EventID, "session_id": e.SessionID}}); err != nil {
				return Permanent(errors.New("diagnostic output failed"))
			}
			return nil
		}
	}
	switch c.Binding.Adapter {
	case "command":
		if err = commandHint(ctx, c, e); err != nil {
			return err
		}
	case "json":
		err = json.NewEncoder(c.Output).Encode(struct {
			Source string `json:"source"`
			Data   Event  `json:"data"`
		}{result.Source, e})
		if err != nil {
			return Permanent(errors.New("notification output failed; checkpoint not advanced"))
		}
	default:
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
