package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/notify"
)

var (
	noticeShape = regexp.MustCompile(`^\d+ ClawBridge messages? (is|are) waiting; call inbox$`)
	digits      = regexp.MustCompile(`^\d+$`)
)

// checkPush asserts line is exactly the fixed notification: the notice and
// three integer meta values, nothing else.
func checkPush(t *testing.T, line []byte, count, seq int64) {
	t.Helper()
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Content string            `json:"content"`
			Meta    map[string]string `json:"meta"`
		} `json:"params"`
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		t.Fatal(err, string(line))
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(line, &top)
	if len(top) != 3 || msg.JSONRPC != "2.0" || msg.Method != "notifications/claude/channel" {
		t.Fatal(string(line))
	}
	if !noticeShape.MatchString(msg.Params.Content) || msg.Params.Content != notify.NoticeText(count) {
		t.Fatal(msg.Params.Content)
	}
	if len(msg.Params.Meta) != 3 || msg.Params.Meta["kind"] != "wake" || msg.Params.Meta["count"] != strconv.FormatInt(count, 10) || msg.Params.Meta["seq"] != strconv.FormatInt(seq, 10) {
		t.Fatal(msg.Params.Meta)
	}
	for k, v := range msg.Params.Meta {
		if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(k) || (k != "kind" && !digits.MatchString(v)) {
			t.Fatal(k, v)
		}
	}
}

// The push is built from two integers and fixed words only.
func TestNotificationIsFixed(t *testing.T) {
	for _, c := range [][2]int64{{1, 1}, {2, 7}, {10, 10}, {999, 123456}, {notify.MaxSequence, notify.MaxSequence}} {
		line := Notification(c[0], c[1])
		if bytes.ContainsRune(line, '\n') {
			t.Fatal("a newline inside one JSON-RPC line")
		}
		checkPush(t, line, c[0], c[1])
	}
	if got := string(Notification(1, 3)); got != `{"jsonrpc":"2.0","method":"notifications/claude/channel","params":{"content":"1 ClawBridge message is waiting; call inbox","meta":{"kind":"wake","count":"1","seq":"3"}}}` {
		t.Fatal(got)
	}
}

// fakeExec answers notification_status queue:true with the harness's
// counts, and text a hostile server might add.
type harness struct {
	t      *testing.T
	dir    string
	t0     time.Time
	mu     sync.Mutex
	status map[string]any
	fail   error
	// routeFail makes opening the route fail.
	routeFail error
	rec       ctxwatch.QueueRecord
	recOK     bool
	pushes    [][]byte
	w         *watcher
	// cur is the watcher's clock: the tick's time, plus sendDelay at a send.
	cur       time.Time
	sendDelay time.Duration
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, dir: t.TempDir(), t0: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), recOK: true}
	h.rec = ctxwatch.QueueRecord{Version: 1, Session: "agent", Workspace: "w", Project: "p", Instance: "inst-1", DB: "/db", MCPPID: 5, AgentPID: 99}
	h.set("accept", 0, 0)
	c := Config{
		Dir: h.dir, PID: 100, ParentPID: 99,
		Now: func() time.Time { return h.cur },
		FindRecord: func(dir, session string, agent int, _ time.Time) (ctxwatch.QueueRecord, bool) {
			if dir != h.dir || session != "agent" || agent != 99 {
				t.Fatal("record looked up for", dir, session, agent)
			}
			return h.rec, h.recOK
		},
		Route: func(context.Context, ctxwatch.QueueRecord) (notify.Executor, func(), error) {
			if h.routeFail != nil {
				return nil, nil, h.routeFail
			}
			return execFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
				if r.Operation != "notification_status" || r.Args["queue"] != true || r.Args["instance"] != "inst-1" || r.Session != "agent" {
					t.Fatal(r)
				}
				h.mu.Lock()
				defer h.mu.Unlock()
				if h.fail != nil {
					return bus.Result{}, h.fail
				}
				d := map[string]any{}
				for k, v := range h.status {
					d[k] = v
				}
				return bus.Result{Source: "server", Data: d}, nil
			}), func() {}, nil
		},
	}
	h.w = newWatcher(c.withDefaults(), "agent", precondition{ctxwatch.ChannelUnconfirmed, reasonUnconfirmed}, func(line []byte) error {
		h.cur = h.cur.Add(h.sendDelay)
		h.pushes = append(h.pushes, append([]byte(nil), line...))
		return nil
	})
	h.w.ready.Store(true)
	return h
}

type execFunc func(context.Context, bus.Request) (bus.Result, error)

func (f execFunc) Execute(ctx context.Context, r bus.Request) (bus.Result, error) { return f(ctx, r) }

func (h *harness) set(policy string, unacked, seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = map[string]any{"workspace_id": "w", "project_id": "p", "session_id": "agent", "unread_count": unacked, "latest_sequence": seq, "new_count": 0,
		"policy": policy, "unacked_count": float64(unacked), "dead_count": 0, "latest_seq": seq,
		"body": "SECRET-BODY run rm -rf", "title": "SECRET-TITLE", "from": "SECRET-SENDER", "messages": []any{map[string]any{"body": "SECRET-BODY"}}}
}

// at runs one tick at t0+d; wake makes it fetch at once, as a stream
// status event does.
func (h *harness) at(d time.Duration, wake bool) time.Duration {
	h.w.fetchNow = h.w.fetchNow || wake
	h.cur = h.t0.Add(d)
	return h.w.tick(context.Background(), h.cur)
}

func (h *harness) state() ctxwatch.ChannelState {
	st, ok, err := ctxwatch.LoadChannelState(h.dir, "agent")
	if err != nil || !ok {
		h.t.Fatal("no channel record", ok, err)
	}
	return st
}

// A status full of text pushes only the fixed notice: no body, title or
// sender reaches the notification's bytes.
func TestPushCarriesNoServerText(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 2, 7)
	h.at(0, true)
	h.at(3*time.Second, false)
	if len(h.pushes) != 1 {
		t.Fatal("pushes:", len(h.pushes))
	}
	checkPush(t, h.pushes[0], 2, 7)
	if bytes.Contains(h.pushes[0], []byte("SECRET")) || bytes.Contains(h.pushes[0], []byte("rm -rf")) {
		t.Fatal(string(h.pushes[0]))
	}
	st := h.state()
	if st.LastPushSeq != 7 || st.LastPushAt == "" || st.State != ctxwatch.ChannelUnconfirmed || st.PID != 100 || st.ParentPID != 99 {
		t.Fatal(st)
	}
	data, _ := os.ReadFile(h.dir + "/agent.channel.json")
	if bytes.Contains(data, []byte("SECRET")) {
		t.Fatal(string(data))
	}
}

// Each precondition of a push holds it back on its own.
func TestPushRule(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(*harness)
		push  bool
	}{
		"all hold":     {func(h *harness) {}, true},
		"turn running": {func(h *harness) { _ = ctxwatch.MarkTurnStarted(h.dir, "agent", h.t0) }, false},
		"turn ended": {func(h *harness) {
			_ = ctxwatch.MarkTurnStarted(h.dir, "agent", h.t0)
			_ = ctxwatch.MarkTurnEnded(h.dir, "agent", h.t0)
		}, true},
		"compaction": {func(h *harness) {
			_ = ctxwatch.SaveCompactionHold(h.dir, "agent", ctxwatch.CompactionHold{Instance: "inst-1"})
		}, false},
		"policy hold":     {func(h *harness) { h.set("hold", 2, 7) }, false},
		"policy refuse":   {func(h *harness) { h.set("refuse", 2, 7) }, false},
		"nothing unacked": {func(h *harness) { h.set("accept", 0, 7) }, false},
		"stop mark covers": {func(h *harness) {
			_ = ctxwatch.SaveStopMark(h.dir, "agent", ctxwatch.StopMark{Instance: "inst-1", LatestSeq: 7})
		}, false},
		"stop mark behind": {func(h *harness) {
			_ = ctxwatch.SaveStopMark(h.dir, "agent", ctxwatch.StopMark{Instance: "inst-1", LatestSeq: 6})
		}, true},
		"stop mark of another instance": {func(h *harness) {
			_ = ctxwatch.SaveStopMark(h.dir, "agent", ctxwatch.StopMark{Instance: "inst-0", LatestSeq: 9})
		}, true},
		"not initialized": {func(h *harness) { h.w.ready.Store(false) }, false},
		"no MCP server":   {func(h *harness) { h.recOK = false }, false},
		"status fails":    {func(h *harness) { h.fail = errors.New("unavailable") }, false},
		"older server":    {func(h *harness) { h.mu.Lock(); delete(h.status, "unacked_count"); h.mu.Unlock() }, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.set("accept", 2, 7)
			c.setup(h)
			h.at(0, true)
			h.at(3*time.Second, false)
			h.at(10*time.Second, false)
			if got := len(h.pushes) == 1; got != c.push || len(h.pushes) > 1 {
				t.Fatal("pushes:", len(h.pushes))
			}
		})
	}
}

// Without a record the state says so, and is checked again in 30 s.
func TestNoMCPServerIsInactive(t *testing.T) {
	h := newHarness(t)
	h.recOK = false
	if d := h.at(0, false); d != 30*time.Second {
		t.Fatal(d)
	}
	if st := h.state(); st.State != ctxwatch.ChannelInactive || st.Reason != "no ClawBridge MCP server for this session" {
		t.Fatal(st)
	}
}

// A seq already pushed is never pushed again; a newer one is.
func TestPushOncePerSeq(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 2, 7)
	h.at(0, true)
	h.at(3*time.Second, false)
	h.set("accept", 1, 7)
	h.at(2*time.Minute, true)
	h.at(2*time.Minute+5*time.Second, false)
	if len(h.pushes) != 1 {
		t.Fatal("pushed seq 7 again:", len(h.pushes))
	}
	h.set("accept", 2, 8)
	h.at(3*time.Minute, true)
	h.at(3*time.Minute+3*time.Second, false)
	if len(h.pushes) != 2 {
		t.Fatal("seq 8 not pushed:", len(h.pushes))
	}
	checkPush(t, h.pushes[1], 2, 8)
}

// Three changes within the debounce make one push, of the last counts.
func TestPushCoalescesABurst(t *testing.T) {
	h := newHarness(t)
	for i := int64(1); i <= 3; i++ {
		h.set("accept", i, 10+i)
		h.at(time.Duration(i-1)*time.Second, true)
	}
	h.at(3*time.Second, false)
	h.at(4*time.Second, false)
	if len(h.pushes) != 0 {
		t.Fatal("pushed inside the debounce")
	}
	if d := h.at(5*time.Second, false); len(h.pushes) != 1 || d <= 0 {
		t.Fatal("pushes:", len(h.pushes), d)
	}
	checkPush(t, h.pushes[0], 3, 13)
}

// The debounce re-checks before sending: a count gone to 0 pushes nothing.
func TestPushRechecksAfterTheDebounce(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 2, 7)
	if d := h.at(0, true); d != 3*time.Second {
		t.Fatal("the debounce isn't waited for:", d)
	}
	h.set("accept", 0, 7)
	h.at(3*time.Second, false)
	h.at(6*time.Second, false)
	h.at(time.Minute, false)
	if len(h.pushes) != 0 {
		t.Fatal("pushed counts gone stale")
	}
}

// At most one push a minute; the next one comes once the minute is up.
func TestPushRateLimit(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 1, 7)
	h.at(0, true)
	h.at(3*time.Second, false)
	h.set("accept", 2, 8)
	h.at(10*time.Second, true)
	if d := h.at(20*time.Second, false); len(h.pushes) != 1 || d > 43*time.Second {
		t.Fatal("pushes:", len(h.pushes), d)
	}
	h.at(62*time.Second, false)
	if len(h.pushes) != 1 {
		t.Fatal("pushed within a minute")
	}
	h.at(63*time.Second, false)
	if len(h.pushes) != 2 {
		t.Fatal("no push after the minute:", len(h.pushes))
	}
	checkPush(t, h.pushes[1], 2, 8)
}

// Counts read while a turn ran are read again once it has ended: the
// agent may have handled them.
func TestPushAfterTurnUsesFreshCounts(t *testing.T) {
	h := newHarness(t)
	_ = ctxwatch.MarkTurnStarted(h.dir, "agent", h.t0)
	h.set("accept", 2, 7)
	h.at(0, true)
	h.at(3*time.Second, false)
	h.set("accept", 0, 7)
	_ = ctxwatch.MarkTurnEnded(h.dir, "agent", h.t0.Add(20*time.Second))
	h.at(25*time.Second, false)
	h.at(29*time.Second, false)
	if len(h.pushes) != 0 {
		t.Fatal("pushed counts read during the turn")
	}
}

// channelTurn is the agent taking the push of seq as a turn at t0+d, as
// the UserPromptSubmit hook marks it, and ending that turn a second later.
func (h *harness) channelTurn(seq int64, d time.Duration) {
	h.t.Helper()
	if err := ctxwatch.MarkChannelTurn(h.dir, "agent", seq, h.t0.Add(d)); err != nil {
		h.t.Fatal(err)
	}
	if err := ctxwatch.MarkTurnEnded(h.dir, "agent", h.t0.Add(d+time.Second)); err != nil {
		h.t.Fatal(err)
	}
}

// A channel turn of the pushed seq confirms the push; none within 120 s
// demotes the channel to fallback; a later confirmed push promotes it
// again.
func TestConfirmationAndFallback(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 1, 1)
	h.at(0, true)
	h.at(3*time.Second, false)
	if len(h.pushes) != 1 || h.state().State != ctxwatch.ChannelUnconfirmed {
		t.Fatal(len(h.pushes), h.state())
	}
	h.channelTurn(1, 4*time.Second)
	h.at(10*time.Second, false)
	st := h.state()
	if st.State != ctxwatch.ChannelActive || st.ConfirmedAt == "" || st.Reason != "" {
		t.Fatal(st)
	}
	// The next push goes unanswered.
	h.set("accept", 1, 2)
	h.at(2*time.Minute, true)
	h.at(2*time.Minute+3*time.Second, false)
	if len(h.pushes) != 2 {
		t.Fatal("pushes:", len(h.pushes))
	}
	h.at(2*time.Minute+3*time.Second+119*time.Second, false)
	if h.state().State != ctxwatch.ChannelActive {
		t.Fatal("demoted before 120 s", h.state())
	}
	if d := h.at(2*time.Minute+3*time.Second+120*time.Second, false); h.state().State != ctxwatch.ChannelFallback || d <= 0 {
		t.Fatal(h.state())
	}
	if h.state().Reason != "no channel turn within 120 s of a push" {
		t.Fatal(h.state().Reason)
	}
	// A later push that is answered promotes it again.
	h.set("accept", 1, 3)
	h.at(10*time.Minute, true)
	h.at(10*time.Minute+3*time.Second, false)
	if len(h.pushes) != 3 {
		t.Fatal("no push from fallback:", len(h.pushes))
	}
	h.channelTurn(3, 10*time.Minute+30*time.Second)
	h.at(10*time.Minute+31*time.Second, false)
	if h.state().State != ctxwatch.ChannelActive {
		t.Fatal(h.state())
	}
}

// An active channel that can't read the counts can't push, and the
// notifier stands down for it: after a minute without counts it leaves
// active, so the notifier types again. Counts read again bring it back to
// unconfirmed, and only a confirmed push makes it active again.
func TestUnreadableCountsLeaveActive(t *testing.T) {
	for _, name := range []string{"status fails", "route fails"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.set("accept", 1, 1)
			h.at(0, true)
			h.at(3*time.Second, false)
			h.channelTurn(1, 4*time.Second)
			h.at(10*time.Second, false)
			if st := h.state(); st.State != ctxwatch.ChannelActive {
				t.Fatal(st)
			}
			last := h.w.fetchedAt
			if name == "status fails" {
				h.fail = errors.New("unavailable")
			} else {
				// The route is opened again, as after a changed record,
				// and now fails.
				h.routeFail = errors.New("unreachable")
				h.w.drop()
			}
			at := 10 * time.Second
			for h.state().State == ctxwatch.ChannelActive {
				if at > 5*time.Minute {
					t.Fatal("still active", h.state())
				}
				at += h.at(at, false)
			}
			if gone := h.cur.Sub(last); gone <= unreadableAfter || gone > unreadableAfter+maxBackoff/2+time.Second {
				t.Fatal("left active after", gone)
			}
			st := h.state()
			if st.State != ctxwatch.ChannelFallback || st.Reason != ctxwatch.ChannelReasonUnreadable || st.ConfirmedAt != "" || ctxwatch.ChannelSkipsTyping(st, h.cur) {
				t.Fatal(st)
			}
			// Counts again: unconfirmed until a later push is confirmed.
			h.fail, h.routeFail = nil, nil
			h.at(at+time.Second, true)
			if st := h.state(); st.State != ctxwatch.ChannelUnconfirmed || st.Reason != reasonUnconfirmed || st.ConfirmedAt != "" {
				t.Fatal(st)
			}
			h.set("accept", 1, 2)
			base := at + 2*time.Minute
			h.at(base, true)
			h.at(base+3*time.Second, false)
			if len(h.pushes) != 2 {
				t.Fatal("pushes:", len(h.pushes))
			}
			h.channelTurn(2, base+4*time.Second)
			h.at(base+10*time.Second, false)
			if st := h.state(); st.State != ctxwatch.ChannelActive || st.ConfirmedAt == "" {
				t.Fatal(st)
			}
		})
	}
}

// Only a channel turn of the pushed seq, begun after the push and within
// 120 s of it, confirms: an ordinary turn (a typed hint woke the agent,
// which then called tools), another seq's turn, or one too early or too
// late leaves the push unconfirmed, and then fallback.
func TestOnlyTheChannelTurnConfirms(t *testing.T) {
	for name, answer := range map[string]func(h *harness){
		"typed hint turn": func(h *harness) {
			_ = ctxwatch.MarkTurnStarted(h.dir, "agent", h.t0.Add(5*time.Second))
			_ = ctxwatch.MarkTurnEnded(h.dir, "agent", h.t0.Add(20*time.Second))
		},
		"another seq":  func(h *harness) { h.channelTurn(6, 5*time.Second) },
		"before push":  func(h *harness) { h.channelTurn(7, 2*time.Second) },
		"too late":     func(h *harness) { h.channelTurn(7, 3*time.Second+121*time.Second) },
		"no turn file": func(h *harness) {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.set("accept", 2, 7)
			h.at(0, true)
			h.at(3*time.Second, false)
			if len(h.pushes) != 1 {
				t.Fatal("pushes:", len(h.pushes))
			}
			answer(h)
			h.at(30*time.Second, false)
			h.at(3*time.Second+125*time.Second, false)
			if st := h.state(); st.State != ctxwatch.ChannelFallback || st.ConfirmedAt != "" {
				t.Fatal(st)
			}
		})
	}
}

// A first push nobody answers demotes too.
func TestFirstPushUnansweredFallsBack(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 1, 1)
	h.at(0, true)
	h.at(3*time.Second, false)
	h.at(time.Minute, false)
	if h.state().State != ctxwatch.ChannelUnconfirmed {
		t.Fatal(h.state())
	}
	h.at(3*time.Second+120*time.Second, false)
	if h.state().State != ctxwatch.ChannelFallback {
		t.Fatal(h.state())
	}
}

// The push is stamped when it is written, not when its tick began: the
// confirmation window and the rate limit start then.
func TestPushStampedAtSend(t *testing.T) {
	h := newHarness(t)
	h.sendDelay = 20 * time.Second
	h.set("accept", 2, 7)
	h.at(0, true)
	h.at(3*time.Second, false)
	if st := h.state(); st.LastPushAt != h.t0.Add(23*time.Second).Format(time.RFC3339Nano) {
		t.Fatal(st.LastPushAt)
	}
	// A channel turn before the send isn't this push's.
	h.channelTurn(7, 10*time.Second)
	h.at(30*time.Second, false)
	if st := h.state(); st.State == ctxwatch.ChannelActive {
		t.Fatal("confirmed by a turn before the send")
	}
}

// A restarted server takes the last push from its session's record, so it
// doesn't push the same seq again, nor sooner than the rate limit; it
// doesn't wait for a confirmation of the earlier process's push.
func TestRestartKeepsTheLastPush(t *testing.T) {
	h := newHarness(t)
	h.set("accept", 2, 7)
	h.at(0, true)
	h.at(3*time.Second, false)
	if len(h.pushes) != 1 {
		t.Fatal(len(h.pushes))
	}
	again := newHarness(t)
	again.dir = h.dir
	again.w.c.Dir = h.dir
	again.w.seed()
	again.set("accept", 2, 7)
	again.at(10*time.Second, true)
	again.at(15*time.Second, false)
	again.at(5*time.Minute, true)
	again.at(5*time.Minute+5*time.Second, false)
	if len(again.pushes) != 0 {
		t.Fatal("the restarted server pushed seq 7 again")
	}
	if st := again.state(); st.State != ctxwatch.ChannelUnconfirmed || st.LastPushSeq != 7 {
		t.Fatal(st)
	}
	// Within the minute of the earlier push, a newer seq waits.
	third := newHarness(t)
	third.dir = h.dir
	third.w.c.Dir = h.dir
	third.w.seed()
	third.set("accept", 3, 8)
	third.at(10*time.Second, true)
	third.at(15*time.Second, false)
	if len(third.pushes) != 0 {
		t.Fatal("pushed within a minute of the earlier process's push")
	}
	third.at(63*time.Second, false)
	if len(third.pushes) != 1 {
		t.Fatal("no push after the minute")
	}
	checkPush(t, third.pushes[0], 3, 8)
}

// The record is rewritten every 30 s even when nothing changed.
func TestStateRewrittenEvery30s(t *testing.T) {
	h := newHarness(t)
	h.at(0, true)
	first := h.state().UpdatedAt
	h.at(10*time.Second, false)
	if h.state().UpdatedAt != first {
		t.Fatal("rewritten without a change within 30 s")
	}
	h.at(30*time.Second, false)
	if h.state().UpdatedAt == first {
		t.Fatal("not rewritten after 30 s")
	}
}

// Errors back off, doubling to a minute, and a success resets it.
func TestStatusErrorsBackOff(t *testing.T) {
	h := newHarness(t)
	h.fail = errors.New("unavailable")
	var backoffs []time.Duration
	at := time.Duration(0)
	for range 20 {
		d := h.at(at, false)
		if d > 30*time.Second || d <= 0 {
			t.Fatal("waits", d)
		}
		if n := len(backoffs); n == 0 || backoffs[n-1] != h.w.backoff {
			backoffs = append(backoffs, h.w.backoff)
		}
		at += d
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute}
	if len(backoffs) != len(want) {
		t.Fatal(backoffs)
	}
	for i := range want {
		if backoffs[i] != want[i] {
			t.Fatal(backoffs)
		}
	}
	h.fail = nil
	h.at(at+time.Hour, true)
	if h.w.backoff != 0 {
		t.Fatal("a success kept the backoff")
	}
}

// What can be seen of Claude Code decides the state before any push.
func TestPreconditions(t *testing.T) {
	ours := "/usr/local/bin/claude --resume x --channels plugin:clawbridge@clawbridge --model opus"
	for name, c := range map[string]struct {
		env    map[string]string
		goos   string
		args   string
		argErr error
		state  string
		reason string
	}{
		"ours":             {nil, "darwin", ours, nil, ctxwatch.ChannelUnconfirmed, reasonUnconfirmed},
		"in a list":        {nil, "linux", "claude --channels plugin:other@x plugin:clawbridge@clawbridge", nil, ctxwatch.ChannelUnconfirmed, reasonUnconfirmed},
		"comma list":       {nil, "linux", "claude --channels=plugin:other@x,plugin:clawbridge@clawbridge", nil, ctxwatch.ChannelUnconfirmed, reasonUnconfirmed},
		"development flag": {nil, "linux", "claude --dangerously-load-development-channels plugin:clawbridge@clawbridge", nil, ctxwatch.ChannelUnconfirmed, reasonUnconfirmed},
		"no channels":      {nil, "darwin", "claude --resume x", nil, ctxwatch.ChannelInactive, reasonNotLaunched},
		"after the list":   {nil, "darwin", "claude --channels plugin:other@x --model plugin:clawbridge@clawbridge", nil, ctxwatch.ChannelInactive, reasonNotLaunched},
		"another server":   {nil, "darwin", "claude --channels server:clawbridge", nil, ctxwatch.ChannelInactive, reasonNotLaunched},
		"bedrock":          {map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, "darwin", ours, nil, ctxwatch.ChannelInactive, "third-party provider"},
		"vertex":           {map[string]string{"CLAUDE_CODE_USE_VERTEX": "true"}, "darwin", ours, nil, ctxwatch.ChannelInactive, "third-party provider"},
		"foundry":          {map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1"}, "darwin", ours, nil, ctxwatch.ChannelInactive, "third-party provider"},
		"provider off":     {map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0"}, "darwin", ours, nil, ctxwatch.ChannelUnconfirmed, reasonUnconfirmed},
		"custom endpoint":  {map[string]string{"ANTHROPIC_BASE_URL": "https://llm.example.test"}, "darwin", ours, nil, ctxwatch.ChannelUnconfirmed, reasonCustomEndpoint},
		"windows":          {nil, "windows", "", errors.New("unused"), ctxwatch.ChannelUnconfirmed, reasonNoArgv},
		"argv unreadable":  {nil, "linux", "", errors.New("no ps"), ctxwatch.ChannelUnconfirmed, reasonNoArgv},
	} {
		cfg := Config{GOOS: c.goos, ParentPID: 99, Getenv: func(k string) string { return c.env[k] }, ParentArgs: func(pid int) (string, error) {
			if pid != 99 {
				t.Fatal(pid)
			}
			return c.args, c.argErr
		}}
		if got := preconditions(cfg); got.state != c.state || got.reason != c.reason {
			t.Fatal(name, got)
		}
	}
}

// A state that can't work never pushes, and says why.
func TestInactivePreconditionNeverPushes(t *testing.T) {
	h := newHarness(t)
	h.w.pre = precondition{ctxwatch.ChannelInactive, "third-party provider"}
	h.set("accept", 2, 7)
	h.at(0, true)
	h.at(5*time.Second, false)
	if len(h.pushes) != 0 || h.state().State != ctxwatch.ChannelInactive || h.state().Reason != "third-party provider" {
		t.Fatal(len(h.pushes), h.state())
	}
}

// The session comes from the environment; empty or a literal ${...} is
// unset.
func TestEnvValue(t *testing.T) {
	for v, want := range map[string]string{"agent": "agent", "": "", "  ": "", "${CLAWBRIDGE_SESSION}": "", "${CLAWBRIDGE_SESSION:-}": "", " agent ": "agent"} {
		if got := envValue(func(string) string { return v }, "CLAWBRIDGE_SESSION"); got != want {
			t.Fatalf("%q: %q", v, got)
		}
	}
}

// rpc runs Serve over lines and returns what it wrote.
func rpc(t *testing.T, c Config, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, c); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("not a JSON-RPC line: %q", line)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// The handshake: the channel capability, no tools, ping, and an error for
// anything else; notifications get no answer.
func TestHandshake(t *testing.T) {
	dir := t.TempDir()
	c := Config{Dir: dir, Getenv: func(string) string { return "" }}
	msgs := rpc(t, c,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x"}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`,
		`not json`,
		``,
	)
	if len(msgs) != 5 {
		t.Fatal(msgs)
	}
	init := msgs[0]["result"].(map[string]any)
	if msgs[0]["id"] != float64(1) || init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(map[string]any)["name"] != "clawbridge-wake" {
		t.Fatal(init)
	}
	caps := init["capabilities"].(map[string]any)
	exp := caps["experimental"].(map[string]any)
	if len(caps) != 1 || len(exp) != 1 || len(exp["claude/channel"].(map[string]any)) != 0 {
		t.Fatal(caps)
	}
	if init["instructions"] != Instructions {
		t.Fatal(init["instructions"])
	}
	if msgs[1]["id"] != "two" || len(msgs[1]["result"].(map[string]any)["tools"].([]any)) != 0 {
		t.Fatal(msgs[1])
	}
	if msgs[2]["id"] != float64(3) || len(msgs[2]["result"].(map[string]any)) != 0 {
		t.Fatal(msgs[2])
	}
	if msgs[3]["id"] != float64(4) || msgs[3]["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatal(msgs[3])
	}
	if msgs[4]["id"] != nil || msgs[4]["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatal(msgs[4])
	}
	// A protocol version that isn't a plain token isn't echoed.
	msgs = rpc(t, c, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"x\"<y>"}}`)
	if v := msgs[0]["result"].(map[string]any)["protocolVersion"]; v != defaultProtocol {
		t.Fatal(v)
	}
}

// Without a session the server answers the handshake and nothing more: no
// record looked up, no state written, nothing pushed.
func TestNoSessionDoesNothing(t *testing.T) {
	for _, session := range []string{"", "${CLAWBRIDGE_SESSION}", "../escape"} {
		dir := t.TempDir()
		c := Config{Dir: dir, Getenv: func(k string) string {
			if k == "CLAWBRIDGE_SESSION" {
				return session
			}
			return ""
		}, FindRecord: func(string, string, int, time.Time) (ctxwatch.QueueRecord, bool) {
			t.Fatal("a record was looked up without a session")
			return ctxwatch.QueueRecord{}, false
		}}
		msgs := rpc(t, c, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if len(msgs) != 1 {
			t.Fatal(session, msgs)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatal(session, "wrote state:", entries)
		}
	}
}

// With a session, Serve runs the watcher and leaves its record when stdin
// closes, marked as ended.
func TestServeWithSessionWritesStateAndEnds(t *testing.T) {
	dir := t.TempDir()
	c := Config{Dir: dir, PID: 100, ParentPID: 99, GOOS: "linux",
		Getenv: func(k string) string {
			if k == "CLAWBRIDGE_SESSION" {
				return "agent"
			}
			return ""
		},
		ParentArgs: func(int) (string, error) { return "claude --channels plugin:clawbridge@clawbridge", nil },
		FindRecord: func(string, string, int, time.Time) (ctxwatch.QueueRecord, bool) {
			return ctxwatch.QueueRecord{}, false
		},
	}
	// An earlier server's last push is kept.
	if err := ctxwatch.SaveChannelState(dir, "agent", ctxwatch.ChannelState{PID: 1, State: ctxwatch.ChannelActive, LastPushSeq: 9, LastPushAt: "2026-10-30T10:00:00Z", UpdatedAt: "2026-10-30T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	rpc(t, c, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	st, ok, err := ctxwatch.LoadChannelState(dir, "agent")
	if err != nil || !ok || st.State != ctxwatch.ChannelInactive || st.Reason != reasonEnded || st.PID != 100 || st.LastPushSeq != 9 {
		t.Fatal(st, ok, err)
	}
}

// A cancel (SIGTERM) ends the server while stdin stays open.
func TestServeEndsOnCancel(t *testing.T) {
	dir := t.TempDir()
	c := Config{Dir: dir, PID: 100, ParentPID: 99, GOOS: "windows",
		Getenv: func(k string) string {
			if k == "CLAWBRIDGE_SESSION" {
				return "agent"
			}
			return ""
		},
		FindRecord: func(string, string, int, time.Time) (ctxwatch.QueueRecord, bool) {
			return ctxwatch.QueueRecord{}, false
		},
	}
	inR, inW := io.Pipe()
	defer inW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- Serve(ctx, inR, io.Discard, c) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve waited for stdin after a cancel")
	}
	if st, _, _ := ctxwatch.LoadChannelState(dir, "agent"); st.Reason != reasonEnded {
		t.Fatal(st)
	}
}

// End to end through Serve on the real clock: the MCP server's record is
// found as a hook finds it (this test process stands in for the MCP server,
// a child of the same agent), nothing is pushed before the client has
// initialized, and once it has, one fixed notice follows the debounce.
func TestServePushesAfterInitialized(t *testing.T) {
	dir := t.TempDir()
	agent := os.Getppid()
	rec := ctxwatch.QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "inst-1", DB: "/db", MCPPID: os.Getpid(), AgentPID: agent, Stream: "polling", Leases: []ctxwatch.QueueLease{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := ctxwatch.SaveQueueRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	c := Config{Dir: dir, ParentPID: agent, GOOS: "linux",
		Getenv: func(k string) string {
			if k == "CLAWBRIDGE_SESSION" {
				return "agent"
			}
			return ""
		},
		ParentArgs: func(int) (string, error) { return "claude --channels " + PluginChannel, nil },
		Route: func(context.Context, ctxwatch.QueueRecord) (notify.Executor, func(), error) {
			return execFunc(func(context.Context, bus.Request) (bus.Result, error) {
				return bus.Result{Source: "local-authority", Data: map[string]any{"policy": "accept", "unacked_count": int64(2), "latest_seq": int64(5), "body": "SECRET"}}, nil
			}), func() {}, nil
		},
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ended := make(chan error, 1)
	go func() { ended <- Serve(context.Background(), inR, outW, c); outW.Close() }()
	lines := make(chan string, 8)
	go func() {
		s := bufio.NewScanner(outR)
		for s.Scan() {
			lines <- s.Text()
		}
		close(lines)
	}()
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	if line := <-lines; !strings.Contains(line, `"id":1`) {
		t.Fatal(line)
	}
	// Past the debounce, still nothing before initialized.
	select {
	case line := <-lines:
		t.Fatal("pushed before initialized:", line)
	case <-time.After(3500 * time.Millisecond):
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	select {
	case line := <-lines:
		checkPush(t, []byte(line), 2, 5)
	case <-time.After(10 * time.Second):
		t.Fatal("no push")
	}
	// The record follows the push.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		st, _, _ := ctxwatch.LoadChannelState(dir, "agent")
		if st.State == ctxwatch.ChannelUnconfirmed && st.LastPushSeq == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(st)
		}
	}
	inW.Close()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't end with stdin")
	}
	for line := range lines {
		t.Fatal("more output:", line)
	}
}
