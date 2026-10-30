// Package channel is `clawbridge channel serve` (FR26): the MCP server a
// ClawBridge Claude Code plugin starts, which wakes an idle agent through a
// Claude Code channel when its ClawBridge messages wait. It has no tools:
// it pushes a fixed notice, and the agent reads its mail through the
// session's own ClawBridge MCP server, where it is marked untrusted.
package channel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
)

// PluginChannel is how Claude Code names our channel in --channels.
const PluginChannel = "plugin:clawbridge@clawbridge"

const (
	// pollEvery is how often the counts are asked for, the record looked
	// for and the state rewritten.
	pollEvery = 30 * time.Second
	// maxBackoff caps the wait after failed asks.
	maxBackoff = 60 * time.Second
	// debounce coalesces a burst of changes into one push.
	debounce = 3 * time.Second
	// fresh is how old counts may be when they are pushed.
	fresh = 5 * time.Second
	// pushEvery is the least time between two pushes.
	pushEvery = 60 * time.Second
	// confirmWithin is how long a push waits for the agent's tool call.
	confirmWithin = 120 * time.Second
	// turnRecheck is how often a running turn is looked at again.
	turnRecheck = 5 * time.Second
	// askTimeout bounds one ask of the counts.
	askTimeout = 15 * time.Second
	// unreadableAfter is how long an active channel goes without reading
	// the counts before it leaves active: it can't push what it can't read.
	unreadableAfter = 2 * pollEvery
)

// The reasons the state record gives (ctxwatch names them for doctor).
const (
	reasonUnconfirmed    = ctxwatch.ChannelReasonUnconfirmed
	reasonCustomEndpoint = ctxwatch.ChannelReasonCustomEndpoint
	reasonNoArgv         = ctxwatch.ChannelReasonNoArgv
	reasonNotLaunched    = ctxwatch.ChannelReasonNotLaunched
	reasonProvider       = ctxwatch.ChannelReasonProvider
	reasonNoServer       = ctxwatch.ChannelReasonNoServer
	reasonFallback       = ctxwatch.ChannelReasonFallback
	reasonEnded          = ctxwatch.ChannelReasonEnded
	reasonUnreadable     = ctxwatch.ChannelReasonUnreadable
)

// Route opens the route a queue record names: its local database or the
// configured hosted server.
type Route func(context.Context, ctxwatch.QueueRecord) (notify.Executor, func(), error)

// Config is the channel server's world; zero fields take the real one.
type Config struct {
	// Getenv reads the scope and the provider settings.
	Getenv func(string) string
	// Dir is the context directory of the records.
	Dir string
	// PID is this process, ParentPID the agent that started it.
	PID, ParentPID int
	// ParentArgs reads a process's command line.
	ParentArgs func(pid int) (string, error)
	GOOS       string
	Route      Route
	FindRecord func(dir, session string, agentPID int, now time.Time) (ctxwatch.QueueRecord, bool)
	Now        func() time.Time
	After      func(time.Duration) <-chan time.Time
	// Version is the serverInfo version.
	Version string
}

func (c Config) withDefaults() Config {
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	if c.PID == 0 {
		c.PID = os.Getpid()
	}
	if c.ParentPID == 0 {
		c.ParentPID = os.Getppid()
	}
	if c.ParentArgs == nil {
		c.ParentArgs = psArgs
	}
	if c.GOOS == "" {
		c.GOOS = runtime.GOOS
	}
	if c.FindRecord == nil {
		c.FindRecord = ctxwatch.HookQueueRecord
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.After == nil {
		c.After = time.After
	}
	return c
}

// psArgs reads pid's command line with ps.
func psArgs(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// -ww: never truncated to a terminal's width.
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}

// envValue is a scope variable: empty, or a ${...} the plugin's env left
// unexpanded, is unset.
func envValue(getenv func(string) string, name string) string {
	v := strings.TrimSpace(getenv(name))
	if strings.HasPrefix(v, "${") {
		return ""
	}
	return v
}

// Notification is the one message the server ever pushes: the notify
// adapter's fixed notice and three meta values, all from two integers it
// computed. No text from a server, message, sender or title can reach it.
func Notification(unacked, latestSeq int64) []byte {
	type meta struct {
		Kind  string `json:"kind"`
		Count string `json:"count"`
		Seq   string `json:"seq"`
	}
	type params struct {
		Content string `json:"content"`
		Meta    meta   `json:"meta"`
	}
	line, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  params `json:"params"`
	}{"2.0", "notifications/claude/channel", params{notify.NoticeText(unacked), meta{"wake", strconv.FormatInt(unacked, 10), strconv.FormatInt(latestSeq, 10)}}})
	return line
}

// precondition is what can be seen of Claude Code before any push: whether
// the channel can work here at all, and the reason given until a push is
// confirmed.
type precondition struct{ state, reason string }

// preconditions reads them: a third-party provider drops channel events;
// the agent must name our plugin in --channels (or the development flag
// the e2e test uses); a custom endpoint may or may not get them.
func preconditions(c Config) precondition {
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if v := strings.ToLower(envValue(c.Getenv, k)); v != "" && v != "0" && v != "false" {
			return precondition{ctxwatch.ChannelInactive, reasonProvider}
		}
	}
	if c.GOOS == "windows" {
		return precondition{ctxwatch.ChannelUnconfirmed, reasonNoArgv}
	}
	args, err := c.ParentArgs(c.ParentPID)
	if err != nil || args == "" {
		return precondition{ctxwatch.ChannelUnconfirmed, reasonNoArgv}
	}
	if !namesChannel(args) {
		return precondition{ctxwatch.ChannelInactive, reasonNotLaunched}
	}
	if envValue(c.Getenv, "ANTHROPIC_BASE_URL") != "" {
		return precondition{ctxwatch.ChannelUnconfirmed, reasonCustomEndpoint}
	}
	return precondition{ctxwatch.ChannelUnconfirmed, reasonUnconfirmed}
}

// namesChannel says whether a command line names our plugin among the
// values of --channels (or --dangerously-load-development-channels):
// space- or comma-separated, up to the next flag.
func namesChannel(args string) bool {
	fields := strings.Fields(args)
	for i, f := range fields {
		for _, flag := range []string{"--channels", "--dangerously-load-development-channels"} {
			var values []string
			switch {
			case f == flag:
				for _, v := range fields[i+1:] {
					if strings.HasPrefix(v, "-") {
						break
					}
					values = append(values, v)
				}
			case strings.HasPrefix(f, flag+"="):
				values = []string{strings.TrimPrefix(f, flag+"=")}
			}
			for _, v := range values {
				for _, name := range strings.Split(v, ",") {
					if name == PluginChannel {
						return true
					}
				}
			}
		}
	}
	return false
}

// watcher follows one session's queue and pushes the wakes. Only its own
// goroutine touches its fields, but ready, which the handshake sets.
type watcher struct {
	c       Config
	session string
	pre     precondition
	send    func([]byte) error
	// ready is set once the client has initialized: no push before.
	ready atomic.Bool
	wake  chan struct{}

	rec        ctxwatch.QueueRecord
	haveRec    bool
	exec       notify.Executor
	done       func()
	stopStream context.CancelFunc

	counts    notify.QueueStatus
	have      bool
	changedAt time.Time
	fetchedAt time.Time
	nextFetch time.Time
	fetchNow  bool
	backoff   time.Duration

	pushedSeq   int64
	pushedAt    time.Time
	answered    bool
	confirmedAt time.Time
	// mode is "" until a push is confirmed, then active, or fallback after
	// one went unanswered.
	mode string
	// unreadable is set when an active channel read no counts for
	// unreadableAfter, until a read succeeds: then a push is confirmed
	// again before the channel is active.
	unreadable bool

	written   ctxwatch.ChannelState
	writtenAt time.Time
}

func newWatcher(c Config, session string, pre precondition, send func([]byte) error) *watcher {
	return &watcher{c: c, session: session, pre: pre, send: send, wake: make(chan struct{}, 1)}
}

func (w *watcher) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// seed takes the last push from the session's record, left by an earlier
// channel server: a restart doesn't push the same seq again, nor within the
// minute. That push's confirmation isn't waited for: the earlier process
// was watching it.
func (w *watcher) seed() {
	st, ok, err := ctxwatch.LoadChannelState(w.c.Dir, w.session)
	if err != nil || !ok || st.LastPushSeq <= 0 {
		return
	}
	w.pushedSeq, w.answered = st.LastPushSeq, true
	if at, err := time.Parse(time.RFC3339Nano, st.LastPushAt); err == nil {
		w.pushedAt = at
	}
}

// run ticks until ctx ends, then leaves the record marked as stopped.
func (w *watcher) run(ctx context.Context) {
	defer func() {
		w.drop()
		w.write(w.c.Now(), ctxwatch.ChannelInactive, reasonEnded, true)
	}()
	w.seed()
	for {
		d := w.tick(ctx, w.c.Now())
		select {
		case <-ctx.Done():
			return
		case <-w.c.After(d):
		case <-w.wake:
			w.fetchNow = true
		}
	}
}

// tick does what is due at now and says how long until the next tick.
func (w *watcher) tick(ctx context.Context, now time.Time) time.Duration {
	if w.pre.state == ctxwatch.ChannelInactive {
		w.write(now, ctxwatch.ChannelInactive, w.pre.reason, false)
		return pollEvery
	}
	rec, ok := w.c.FindRecord(w.c.Dir, w.session, w.c.ParentPID, now)
	if !ok {
		w.drop()
		w.write(now, ctxwatch.ChannelInactive, reasonNoServer, false)
		return pollEvery
	}
	if !w.haveRec || rec.Instance != w.rec.Instance || rec.Server != w.rec.Server || rec.DB != w.rec.DB || rec.Workspace != w.rec.Workspace || rec.Project != w.rec.Project {
		w.drop()
		w.fetchNow = true
	}
	w.rec, w.haveRec = rec, true
	w.confirm(now)
	if w.fetchNow || !now.Before(w.nextFetch) {
		w.fetch(ctx, now)
	}
	wait, refetch := w.decide(now)
	if refetch {
		w.fetch(ctx, now)
		wait, refetch = w.decide(now)
	}
	if w.mode == ctxwatch.ChannelActive && now.Sub(w.fetchedAt) > unreadableAfter {
		// Neither path would wake the agent: the notifier stands down for
		// an active channel. Leaving active makes it type again.
		w.mode, w.confirmedAt, w.unreadable = "", time.Time{}, true
	}
	if !refetch && wait == 0 {
		if w.send(Notification(w.counts.Unacked, w.counts.LatestSeq)) == nil {
			// Stamped when written: the tick may have spent a while asking.
			w.pushedSeq, w.pushedAt, w.answered = w.counts.LatestSeq, w.c.Now(), false
		}
		wait = pollEvery
	}
	state, reason := w.state()
	w.write(now, state, reason, false)
	if d := w.nextFetch.Sub(now); d < wait {
		wait = d
	}
	if !w.pushedAt.IsZero() && !w.answered {
		if d := w.pushedAt.Add(confirmWithin).Sub(now); d > 0 && d < wait {
			wait = d
		}
	}
	return max(min(wait, pollEvery), 100*time.Millisecond)
}

// drop closes the route and its stream; the next tick opens them again.
func (w *watcher) drop() {
	if w.stopStream != nil {
		w.stopStream()
		w.stopStream = nil
	}
	if w.done != nil {
		w.done()
		w.done = nil
	}
	w.exec, w.haveRec = nil, false
}

// fetch asks for the counts. The route opens on first use; a hosted one
// also streams the session's status events, each of which wakes a fetch.
func (w *watcher) fetch(ctx context.Context, now time.Time) {
	w.fetchNow = false
	if w.exec == nil {
		e, done, err := w.c.Route(ctx, w.rec)
		if err != nil {
			w.fail(now)
			return
		}
		w.exec, w.done = e, done
		if s, ok := e.(notify.Streamer); ok {
			streamCtx, stop := context.WithCancel(ctx)
			w.stopStream = stop
			scope := remote.StreamScope{Workspace: w.rec.Workspace, Project: w.rec.Project, Session: w.rec.Session, Instance: w.rec.Instance, Mode: "observe"}
			go func() {
				_ = s.Stream(streamCtx, scope, remote.StreamOptions{}, func(e remote.StreamEvent) error {
					if e.Name == "status" {
						w.poke()
					}
					return nil
				})
			}()
		}
	}
	bounded, cancel := context.WithTimeout(ctx, askTimeout)
	s, err := notify.FetchQueueStatus(bounded, w.exec, bus.Request{Workspace: w.rec.Workspace, Project: w.rec.Project, Session: w.rec.Session, Args: map[string]any{"instance": w.rec.Instance}})
	cancel()
	if err != nil {
		w.fail(now)
		return
	}
	w.backoff, w.unreadable = 0, false
	if !w.have || s != w.counts {
		w.counts, w.have, w.changedAt = s, true, now
	}
	w.fetchedAt, w.nextFetch = now, now.Add(pollEvery)
}

// fail backs off: a second, doubling to maxBackoff.
func (w *watcher) fail(now time.Time) {
	w.backoff = min(maxBackoff, max(time.Second, 2*w.backoff))
	w.nextFetch = now.Add(w.backoff)
}

// decide says whether to push now (0, false), to ask again first (_, true),
// or how long to wait. A push needs: the client initialized; counts with
// mail unacked under policy accept; a newer seq than the last push and
// than the Stop hook's mark; no compaction hold; the agent idle; a minute
// since the last push; and counts read again at least the debounce after
// their last change, and lately.
func (w *watcher) decide(now time.Time) (time.Duration, bool) {
	if !w.ready.Load() || !w.have {
		return pollEvery, false
	}
	c := w.counts
	if c.Policy != "accept" || c.Unacked <= 0 || c.LatestSeq <= w.pushedSeq {
		return pollEvery, false
	}
	if _, held := ctxwatch.LoadCompactionHold(w.c.Dir, w.session); held {
		return pollEvery, false
	}
	if m, ok := ctxwatch.LoadStopMark(w.c.Dir, w.session); ok && m.Instance == w.rec.Instance && c.LatestSeq <= m.LatestSeq {
		return pollEvery, false
	}
	if turn, ok := ctxwatch.LoadTurn(w.c.Dir, w.session); !ctxwatch.Idle(turn, ok, now) {
		return turnRecheck, false
	}
	if !w.pushedAt.IsZero() {
		if d := w.pushedAt.Add(pushEvery).Sub(now); d > 0 {
			return d, false
		}
	}
	if settle := w.changedAt.Add(debounce); now.Before(settle) {
		w.nextFetch = settle
		return settle.Sub(now), false
	}
	if w.fetchedAt.Sub(w.changedAt) < debounce || now.Sub(w.fetchedAt) > fresh {
		return turnRecheck, true
	}
	return 0, false
}

// confirm watches for the turn the last push started: the turn record's
// channel seq is the pushed one, begun within confirmWithin after the push.
// Only our channel's tag sets it (the UserPromptSubmit hook reads it with
// ctxwatch.ChannelTurnSeq), so a turn a typed hint started, or any tool
// call, never confirms a channel that delivers nothing. One makes the
// channel active; none, fallback.
func (w *watcher) confirm(now time.Time) {
	if w.pushedAt.IsZero() || w.answered {
		return
	}
	turn, ok := ctxwatch.LoadTurn(w.c.Dir, w.session)
	if at, err := time.Parse(time.RFC3339Nano, turn.ChannelAt); ok && err == nil && turn.ChannelSeq == w.pushedSeq && !at.Before(w.pushedAt) && !at.After(w.pushedAt.Add(confirmWithin)) {
		w.answered, w.mode, w.confirmedAt = true, ctxwatch.ChannelActive, now
		return
	}
	if !now.Before(w.pushedAt.Add(confirmWithin)) {
		w.mode = ctxwatch.ChannelFallback
	}
}

func (w *watcher) state() (string, string) {
	if w.unreadable {
		return ctxwatch.ChannelFallback, reasonUnreadable
	}
	switch w.mode {
	case ctxwatch.ChannelActive:
		return ctxwatch.ChannelActive, ""
	case ctxwatch.ChannelFallback:
		return ctxwatch.ChannelFallback, reasonFallback
	}
	return ctxwatch.ChannelUnconfirmed, w.pre.reason
}

// write saves the record on a change, every pollEvery, or when forced.
func (w *watcher) write(now time.Time, state, reason string, force bool) {
	st := ctxwatch.ChannelState{Version: 1, PID: w.c.PID, ParentPID: w.c.ParentPID, State: state, Reason: reason, LastPushSeq: w.pushedSeq}
	if !w.pushedAt.IsZero() {
		st.LastPushAt = w.pushedAt.UTC().Format(time.RFC3339Nano)
	}
	if !w.confirmedAt.IsZero() {
		st.ConfirmedAt = w.confirmedAt.UTC().Format(time.RFC3339)
	}
	if !force && st == w.written && !w.writtenAt.IsZero() && now.Sub(w.writtenAt) < pollEvery {
		return
	}
	st.UpdatedAt = now.UTC().Format(time.RFC3339)
	if ctxwatch.SaveChannelState(w.c.Dir, w.session, st) == nil {
		st.UpdatedAt = ""
		w.written, w.writtenAt = st, now
	}
}
