package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
)

// fakeClock is the watcher's clock and Wait: a wait moves the clock by its
// duration and runs the test's hook, so nothing really sleeps (FR59).
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	hook  func(*fakeClock, time.Duration) error
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Wait(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.waits = append(f.waits, d)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(f, d)
	}
	return nil
}

func (f *fakeClock) holds() (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.waits {
		if d == holdOff {
			n++
		}
	}
	return n
}

func holdConfig(t *testing.T, hints *int) (Config, *fakeClock) {
	t.Helper()
	c := commandConfig(t, hints)
	fc := &fakeClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	c.Now, c.Wait = fc.Now, fc.Wait
	return c, fc
}

// firstState writes an unconfirmed ("no push confirmed yet"), live record at
// the fake clock's time, changed by mutate.
func firstState(t *testing.T, c Config, mutate func(*ctxwatch.ChannelState)) {
	t.Helper()
	st := ctxwatch.ChannelState{PID: os.Getpid(), State: ctxwatch.ChannelUnconfirmed, Reason: ctxwatch.ChannelReasonUnconfirmed,
		UpdatedAt: c.Now().UTC().Format(time.RFC3339)}
	if mutate != nil {
		mutate(&st)
	}
	if err := ctxwatch.SaveChannelState(c.ContextDir, c.Binding.Session, st); err != nil {
		t.Fatal(err)
	}
}

func pushed(c Config, seq int64) func(*ctxwatch.ChannelState) {
	return func(s *ctxwatch.ChannelState) {
		s.LastPushSeq, s.LastPushAt = seq, c.Now().UTC().Format(time.RFC3339Nano)
	}
}

// An unconfirmed channel and an idle agent: the notifier waits 10 s, and a
// push the channel made meanwhile means no typed line and no checkpoint.
func TestNotifyHoldsOffForAnUnconfirmedChannelThatPushes(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	firstState(t, c, nil)
	fc.hook = func(f *fakeClock, d time.Duration) error {
		firstState(t, c, pushed(c, 9))
		return nil
	}
	var diag bytes.Buffer
	c.Diagnostics = &diag
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 0 || load(t, c.StatePath).After != 0 || fc.holds() != 1 || len(fc.waits) != 1 {
		t.Fatal(hints, load(t, c.StatePath).After, fc.waits)
	}
	if !strings.Contains(diag.String(), `"reason":"channel_push"`) || strings.Contains(diag.String(), "hint_accepted") {
		t.Fatal(diag.String())
	}
}

// markTurn writes a channel turn for seq started at at, ended at end (zero:
// still running); at is also its ChannelAt unless badAt.
func markTurn(t *testing.T, c Config, seq int64, at, end time.Time, badAt bool) {
	t.Helper()
	rec := ctxwatch.TurnRecord{Version: 1, StartedAt: at.UTC().Format(time.RFC3339Nano), ChannelSeq: seq, ChannelAt: at.UTC().Format(time.RFC3339Nano)}
	if badAt {
		rec.ChannelAt = "not a time"
	}
	if !end.IsZero() {
		rec.EndedAt = end.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.ContextDir, c.Binding.Session+".turn.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// (3) A channel turn for the batch that is still running, within 120 s:
// the batch is skipped and the deadline is returned.
func TestNotifyHoldOffSeesARunningChannelTurn(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	firstState(t, c, nil)
	fc.hook = func(f *fakeClock, d time.Duration) error {
		markTurn(t, c, 9, c.Now(), time.Time{}, false)
		return nil
	}
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 0 || load(t, c.StatePath).After != 0 || fc.holds() != 1 {
		t.Fatal(hints, fc.waits)
	}
	started := fc.Now()
	ok, deadline := channelPushed(c, 9)
	if !ok || !deadline.Equal(started.Add(ctxwatch.ChannelConfirmWithin)) {
		t.Fatal(ok, deadline)
	}
}

// (1) The channel turn ended and the batch is still unacked: typed once, at
// the next evaluation, even though the record's own push is under 120 s old.
// (2) A channel turn still running at 121 s: typed. An unparseable
// ChannelAt is no channel turn: typed after the hold-off.
func TestNotifyTypesWhenAChannelTurnEndedOrRunsPast120s(t *testing.T) {
	for name, tc := range map[string]struct {
		end   bool
		bad   bool
		after time.Duration
	}{
		"ended, unacked":        {end: true, after: 20 * time.Second},
		"running at 121 s":      {after: 121 * time.Second},
		"unparseable ChannelAt": {bad: true, after: 0},
	} {
		hints := 0
		c, fc := holdConfig(t, &hints)
		c.Once, c.Interval = false, 300*time.Second
		firstState(t, c, nil)
		var turnAt time.Time
		fc.hook = func(f *fakeClock, d time.Duration) error {
			if hints > 0 {
				return context.Canceled
			}
			if d == holdOff {
				turnAt = c.Now()
				firstState(t, c, pushed(c, 9))
				var end time.Time
				if tc.end {
					end = turnAt.Add(time.Second)
				}
				markTurn(t, c, 9, turnAt, end, tc.bad)
			}
			return nil
		}
		typedAt := time.Time{}
		runner := c.CommandRunner
		c.CommandRunner = func(ctx context.Context, a, b []string, d []byte) error {
			typedAt = c.Now()
			return runner(ctx, a, b, d)
		}
		if err := Run(context.Background(), c); !errors.Is(err, context.Canceled) {
			t.Fatal(name, err)
		}
		if hints != 1 || load(t, c.StatePath).After != 9 {
			t.Fatal(name, hints, fc.waits)
		}
		if got := typedAt.Sub(turnAt); (name == "running at 121 s" && (got < 120*time.Second || got > 125*time.Second)) || (name == "ended, unacked" && got > 125*time.Second) {
			t.Fatal(name, got, fc.waits)
		}
	}
}

// A push of an older batch doesn't cover this one.
func TestNotifyHoldOffIgnoresAnOlderPush(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	firstState(t, c, nil)
	fc.hook = func(f *fakeClock, d time.Duration) error {
		firstState(t, c, pushed(c, 8))
		return nil
	}
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 1 || load(t, c.StatePath).After != 9 {
		t.Fatal(hints)
	}
}

// No push in the 10 s: the batch is typed, after the wait.
func TestNotifyTypesAfterTheHoldOffWithoutAPush(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	firstState(t, c, nil)
	fc.hook = func(f *fakeClock, d time.Duration) error {
		if hints != 0 {
			t.Error("typed before the wait ended")
		}
		return nil
	}
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 1 || load(t, c.StatePath).After != 9 || len(fc.waits) != 1 || fc.waits[0] != 10*time.Second {
		t.Fatal(hints, fc.waits)
	}
}

// A push that is never confirmed: the batch stays skipped for 120 s from the
// push, then is typed. The loop wakes at that deadline, not at its safety
// re-poll (here a 300 s interval).
func TestNotifyTypesAPushedBatchOnceItIsUnconfirmedFor120s(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	c.Once, c.Interval = false, 300*time.Second
	firstState(t, c, nil)
	var pushAt time.Time
	var typedAt time.Time
	fc.hook = func(f *fakeClock, d time.Duration) error {
		if d == holdOff {
			firstState(t, c, pushed(c, 9))
			pushAt = c.Now()
			return nil
		}
		if hints > 0 {
			return context.Canceled
		}
		if c.Now().Sub(pushAt) > 125*time.Second {
			t.Error("not typed by the deadline")
			return context.Canceled
		}
		return nil
	}
	runner := c.CommandRunner
	c.CommandRunner = func(ctx context.Context, a, b []string, d []byte) error {
		typedAt = c.Now()
		return runner(ctx, a, b, d)
	}
	var diag bytes.Buffer
	c.Diagnostics = &diag
	if err := Run(context.Background(), c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if hints != 1 || load(t, c.StatePath).After != 9 || typedAt.Sub(pushAt) < ctxwatch.ChannelConfirmWithin || typedAt.Sub(pushAt) > 125*time.Second || strings.Count(diag.String(), "hint_skipped") != 1 {
		t.Fatal(hints, typedAt.Sub(pushAt), fc.waits, diag.String())
	}
}

// The cases that type at once, with no wait.
func TestNotifyDoesNotHoldOffWhenTheChannelCannotConfirm(t *testing.T) {
	for name, mutate := range map[string]func(*ctxwatch.ChannelState){
		"fallback": func(s *ctxwatch.ChannelState) { s.State = ctxwatch.ChannelFallback },
		"inactive": func(s *ctxwatch.ChannelState) { s.State = ctxwatch.ChannelInactive },
		"stale": func(s *ctxwatch.ChannelState) {
			s.UpdatedAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC).Add(-91 * time.Second).Format(time.RFC3339)
		},
		"dead pid":        func(s *ctxwatch.ChannelState) { s.PID = deadPID(t) },
		"no argv":         func(s *ctxwatch.ChannelState) { s.Reason = ctxwatch.ChannelReasonNoArgv },
		"custom endpoint": func(s *ctxwatch.ChannelState) { s.Reason = ctxwatch.ChannelReasonCustomEndpoint },
		"busy agent":      nil,
		"no record":       nil,
	} {
		hints := 0
		c, fc := holdConfig(t, &hints)
		switch name {
		case "busy agent":
			firstState(t, c, nil)
			if err := ctxwatch.MarkTurnStarted(c.ContextDir, c.Binding.Session, c.Now()); err != nil {
				t.Fatal(err)
			}
		case "no record":
		default:
			firstState(t, c, mutate)
		}
		if err := Run(context.Background(), c); err != nil {
			t.Fatal(name, err)
		}
		if hints != 1 || load(t, c.StatePath).After != 9 || len(fc.waits) != 0 {
			t.Fatal(name, hints, fc.waits)
		}
	}
}

// A batch is held off once: when its typing fails and is retried, the retry
// types without waiting 10 s again.
func TestNotifyHoldsTheSameBatchOnlyOnce(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	c.Once = false
	firstState(t, c, nil)
	tries := 0
	runner := c.CommandRunner
	c.CommandRunner = func(ctx context.Context, a, b []string, d []byte) error {
		if tries++; tries == 1 {
			return errors.New("host busy")
		}
		return runner(ctx, a, b, d)
	}
	fc.hook = func(f *fakeClock, d time.Duration) error {
		if hints > 0 {
			return context.Canceled
		}
		return nil
	}
	if err := Run(context.Background(), c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if tries != 2 || hints != 1 || fc.holds() != 1 {
		t.Fatal(tries, hints, fc.waits)
	}
}

// The JSON adapter wakes nobody: it never waits.
func TestNotifyJSONNeverHoldsOff(t *testing.T) {
	hints := 0
	c, fc := holdConfig(t, &hints)
	c.Binding.Adapter, c.Binding.Command = "json", nil
	var out bytes.Buffer
	c.Output = &out
	firstState(t, c, nil)
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "inbox_changed") || len(fc.waits) != 0 || load(t, c.StatePath).After != 9 {
		t.Fatal(out.String(), fc.waits)
	}
}
