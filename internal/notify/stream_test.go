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

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// streamingExecutor answers notification_status from steps and follows a
// fake stream: each value sent on events is a status event.
type streamingExecutor struct {
	mu       sync.Mutex
	steps    [][3]int64
	calls    int
	events   chan struct{}
	streamed chan remote.StreamScope
	end      error
}

func (s *streamingExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Operation != "notification_status" {
		return bus.Result{Source: "local-authority", Data: map[string]any{}}, nil
	}
	step := s.steps[min(s.calls, len(s.steps)-1)]
	s.calls++
	return result(step[0], step[1], step[2]), nil
}

func (s *streamingExecutor) Stream(ctx context.Context, scope remote.StreamScope, opts remote.StreamOptions, handle func(remote.StreamEvent) error) error {
	s.streamed <- scope
	if s.end != nil {
		return s.end
	}
	opts.OnConnect()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.events:
			if err := handle(remote.StreamEvent{Name: "status", Data: json.RawMessage(`{}`)}); err != nil {
				return err
			}
		}
	}
}

// lines is a goroutine-safe writer.
type lines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With a stream, a status event checks at once instead of after the poll
// interval: here five minutes, which the test never waits.
func TestNotifyWakesOnTheStream(t *testing.T) {
	c := config(t)
	c.Once, c.Interval = false, 300*time.Second
	var output lines
	c.Output = &output
	// The first check at start, the second on the stream's connect, the
	// third only on its status event.
	s := &streamingExecutor{steps: [][3]int64{{0, 0, 0}, {0, 0, 0}, {1, 7, 1}}, events: make(chan struct{}), streamed: make(chan remote.StreamScope, 1)}
	c.Executor = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	scope := <-s.streamed
	if scope.Mode != "observe" || scope.Instance != "SECRET-INSTANCE" || scope.Session != "session-name" {
		t.Fatal(scope)
	}
	eventually(t, "no check on connect", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.calls == 2 })
	time.Sleep(100 * time.Millisecond)
	if s.mu.Lock(); s.calls != 2 || output.String() != "" {
		t.Fatal(s.calls, output.String())
	}
	s.mu.Unlock()
	s.events <- struct{}{}
	eventually(t, "no hint after the stream's status event", func() bool { return strings.Contains(output.String(), `"latest_sequence":7`) })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// An older server has no stream: notify says so once on its diagnostics
// and polls at its interval as before.
func TestNotifyPollsWithoutAStream(t *testing.T) {
	c := config(t)
	c.Once, c.Interval = false, time.Second
	var output, diagnostics lines
	c.Output, c.Diagnostics = &output, &diagnostics
	s := &streamingExecutor{steps: [][3]int64{{0, 0, 0}, {1, 7, 1}}, streamed: make(chan remote.StreamScope, 1), end: remote.ErrStreamUnsupported}
	c.Executor = s
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	eventually(t, "no hint from polling", func() bool { return strings.Contains(output.String(), `"latest_sequence":7`) })
	if strings.Count(diagnostics.String(), "stream_unavailable") != 1 {
		t.Fatal(diagnostics.String())
	}
	cancel()
	<-done
}

// M6: the command adapter runs argv, never a shell, and is given only the
// fixed notice and the event's IDs, whatever the waiting messages hold.
func TestNotifyCommandAdapterGivesFixedTextOnly(t *testing.T) {
	c := config(t)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c.Binding.Adapter, c.Binding.Command = "command", []string{program, "send", "--text", "{text}", "literal $(whoami) {text}"}
	var argv, env []string
	var stdin []byte
	c.CommandRunner = func(_ context.Context, a, e []string, in []byte) error {
		argv, env, stdin = a, e, in
		return nil
	}
	c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) { return result(2, 9, 2), nil })
	if err = Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	text := "2 ClawBridge messages are waiting; call inbox"
	if strings.Join(argv, "|") != program+"|send|--text|"+text+"|literal $(whoami) "+text {
		t.Fatal(argv)
	}
	if strings.Join(env, "|") != "CLAWBRIDGE_NOTICE_TEXT="+text+"|CLAWBRIDGE_NOTICE_COUNT=2" {
		t.Fatal(env)
	}
	var e map[string]any
	if json.Unmarshal(stdin, &e) != nil || e["type"] != "inbox_changed" || e["session_id"] != "ses-id" || e["latest_sequence"] != float64(9) || len(e) != 7 {
		t.Fatal(string(stdin))
	}
	if noticeText(1) != "1 ClawBridge message is waiting; call inbox" {
		t.Fatal(noticeText(1))
	}
	// A failed command doesn't advance the checkpoint.
	c.StatePath = filepath.Join(t.TempDir(), "again.json")
	c.CommandRunner = func(context.Context, []string, []string, []byte) error { return errors.New("no terminal") }
	if err = Run(context.Background(), c); err == nil {
		t.Fatal("a failed command was taken as delivered")
	}
}

// The real runner hands the notice over as one argument, so shell
// characters in it stay text.
func TestNotifyCommandRunnerNoShell(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	argv, err := resolveCommand([]string{"/bin/sh", "-c", `printf %s "$1" > "$2"`, "sh", "a ' \" $(x) `y` ;b", out})
	if err != nil {
		t.Fatal(err)
	}
	if err = runCommand(context.Background(), argv, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "a ' \" $(x) `y` ;b" {
		t.Fatalf("%q", b)
	}
	for _, bad := range [][]string{nil, {""}, {"no-such-program-for-notify"}, {"/bin/sh", "a\x00b"}} {
		if _, err = resolveCommand(bad); err == nil {
			t.Fatal(bad)
		}
	}
}
