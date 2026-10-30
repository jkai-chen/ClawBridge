package notify

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
)

// ContextInterval is how often a process holding a session tries to report
// its context (CONTEXT-WATCH §4.4). Tests shorten it.
var ContextInterval = time.Minute

const (
	// contextRefresh is the longest a session goes without a report while
	// nothing changes, so list_sessions and the TUI stay fresh; it is also
	// how long the reporter waits after the project answers that context
	// watching is off.
	contextRefresh = 10 * time.Minute
)

// ReportContext reports the context of the session request names, read
// from its bound transcript under dir, until ctx ends. Of the processes
// holding one session (the MCP server, notify, a second MCP server), only
// the one holding <dir>/<session>.lock reads and reports (CW-L4); the lock
// passes to another at its next tick when its holder exits. A report goes
// when the level changes, when a compaction appears, and otherwise every
// contextRefresh. The answer's levels and hint are cached in the binding
// record for the hooks, which make no network call (CW-L5).
func ReportContext(ctx context.Context, e Executor, dir string, interval time.Duration, request func() (bus.Request, bool)) {
	r := &contextReporter{e: e, dir: dir, now: time.Now}
	defer r.release()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		req, ok := request()
		if !ok || ctx.Err() != nil {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, interval/2)
		r.tick(bounded, req)
		cancel()
	}
}

type contextReporter struct {
	e        Executor
	dir      string
	now      func() time.Time
	lock     *os.File
	session  string
	lastAt   time.Time
	level    string
	count    int64
	offUntil time.Time
}

func (r *contextReporter) release() {
	if r.lock != nil {
		r.lock.Close()
		r.lock = nil
	}
}

var storedCount = regexp.MustCompile(`invalid_compactions: the session's compaction count is (\d+)`)

// tick makes one report, if this process holds the session's lock and one
// is due. Every failure waits for the next tick.
func (r *contextReporter) tick(ctx context.Context, req bus.Request) {
	if req.Session != r.session {
		// A process re-bound to another session reports that one.
		r.release()
		*r = contextReporter{e: r.e, dir: r.dir, now: r.now, session: req.Session}
	}
	path, err := ctxwatch.BindingPath(r.dir, req.Session)
	if err != nil {
		return
	}
	if r.lock == nil {
		if ctxwatch.MakeContextDir(r.dir) != nil {
			return
		}
		f, held, err := ctxwatch.TryLock(strings.TrimSuffix(path, ".json") + ".lock")
		if err != nil || !held {
			return
		}
		r.lock = f
	}
	now := r.now()
	if now.Before(r.offUntil) {
		return
	}
	b, bound, err := ctxwatch.LoadBinding(r.dir, req.Session)
	if err != nil || !bound {
		return
	}
	reading, err := ctxwatch.ReadBound(b)
	if err != nil || reading.Status != ctxwatch.StatusOK || reading.NotWritten {
		return
	}
	count, seen := b.ReportedCompactions, b.SeenCompactionAt
	trigger := ""
	if last, ok := parseTime(reading.LastCompactionAt); ok {
		if prev, ok := parseTime(seen); !ok || last.After(prev) {
			count, seen = count+1, reading.LastCompactionAt
			trigger = compactionTrigger(b, last)
		}
	}
	level := "ok"
	if b.Levels != nil {
		level = b.Levels.Level(reading.Tokens)
	}
	if !r.lastAt.IsZero() && now.Sub(r.lastAt) < contextRefresh && level == r.level && count == r.count {
		return
	}
	args := map[string]any{"instance": req.Args["instance"], "tokens": reading.Tokens, "level": level, "compactions": count,
		"observed_at": now.UTC().Format(time.RFC3339)}
	if reading.Window > 0 {
		args["window"] = reading.Window
	}
	if trigger != "" {
		args["trigger"] = trigger
	}
	result, err := r.e.Execute(ctx, bus.Request{Operation: "report_context", Workspace: req.Workspace, Project: req.Project, Session: req.Session, Args: args})
	if err != nil {
		// Out of step with the server's count (a binding record made
		// again, say): take the server's, and report at the next tick.
		if m := storedCount.FindStringSubmatch(err.Error()); m != nil {
			if n, e := strconv.ParseInt(m[1], 10, 64); e == nil {
				b.ReportedCompactions = n
				_ = ctxwatch.SaveBinding(r.dir, req.Session, b)
			}
		}
		return
	}
	answer, _ := result.Data.(map[string]any)
	if answer["enabled"] != true {
		r.offUntil = now.Add(contextRefresh)
		return
	}
	r.lastAt, r.count = now, count
	r.level, _ = answer["level"].(string)
	if n, ok := asInt64(answer["compactions"]); ok {
		r.count = n
	}
	// Cache what the hooks need, on the record as it is now: a hook may
	// have bound the session again meanwhile.
	fresh, bound, err := ctxwatch.LoadBinding(r.dir, req.Session)
	if err != nil || !bound {
		return
	}
	fresh.ReportedCompactions = r.count
	if fresh.Transcript == b.Transcript {
		fresh.SeenCompactionAt = seen
	}
	fresh.Levels = nil
	if l, ok := answer["levels"].(map[string]any); ok {
		warn, ok1 := asInt64(l["warn_tokens"])
		handover, ok2 := asInt64(l["handover_tokens"])
		if ok1 && ok2 {
			fresh.Levels = &ctxwatch.Levels{WarnTokens: warn, HandoverTokens: handover}
		}
	}
	fresh.HandoverHint, _ = answer["handover_hint"].(string)
	_ = ctxwatch.SaveBinding(r.dir, req.Session, fresh)
}

// asInt64 reads a JSON number that came over HTTP (float64) or in process
// (an integer).
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), n == float64(int64(n))
	}
	return 0, false
}

// compactionTriggerWindow is how long before a compaction the PreCompact
// hook's note of it may be: the summary takes a while to write.
const compactionTriggerWindow = 15 * time.Minute

// compactionTrigger is the trigger (manual or auto) the PreCompact hook
// noted for the compaction at at, or "" when the note is of another one.
func compactionTrigger(b ctxwatch.Binding, at time.Time) string {
	noted, ok := parseTime(b.CompactingAt)
	if !ok || noted.After(at.Add(time.Minute)) || at.Sub(noted) > compactionTriggerWindow {
		return ""
	}
	return b.CompactTrigger
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}
