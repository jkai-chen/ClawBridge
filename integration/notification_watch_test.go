package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
)

type notificationExecutorFunc func(context.Context, bus.Request) (bus.Result, error)

func (f notificationExecutorFunc) Execute(c context.Context, r bus.Request) (bus.Result, error) {
	return f(c, r)
}
func notificationResult(unread, latest, fresh int64) bus.Result {
	return bus.Result{Source: "local-authority", Data: notify.Status{WorkspaceID: "ws_canonical", ProjectID: "prj_canonical", SessionID: "ses_canonical", UnreadCount: unread, LatestSequence: latest, NewCount: fresh}}
}
func notificationConfig(t *testing.T) notify.Config {
	t.Helper()
	return notify.Config{Binding: notify.Binding{Authority: "synthetic-local-authority-route", Principal: "local-test-principal", Workspace: "workspace-alias", Project: "project-alias", Session: "recipient-alias", Instance: "private-instance", Adapter: "json"}, StatePath: filepath.Join(t.TempDir(), "private state", "checkpoint.json"), Interval: time.Second, Once: true, Output: io.Discard, Diagnostics: io.Discard}
}
func notificationCursor(t *testing.T, r bus.Request) int64 {
	t.Helper()
	if r.Operation != "inbox_status" || len(r.Args) != 2 || r.Args["instance"] != "private-instance" {
		t.Fatalf("poll used wrong operation/actor/args: %+v", r)
	}
	switch n := r.Args["after"].(type) {
	case int64:
		return n
	case int:
		return int64(n)
	default:
		t.Fatalf("unexpected after type %T", n)
		return -1
	}
}
func notificationNoTemps(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".lock") || strings.HasPrefix(e.Name(), ".clawbridge-notify-") {
			t.Fatalf("owned lock/temp left: %s", e.Name())
		}
	}
}
func TestNotificationJSONCheckpointAndCanonicalPinning(t *testing.T) {
	c := notificationConfig(t)
	var out, diag bytes.Buffer
	c.Output = &out
	c.Diagnostics = &diag
	phase := 0
	calls := 0
	c.Executor = notificationExecutorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
		calls++
		after := notificationCursor(t, r)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("poll missing bounded deadline")
		}
		if calls == 1 {
			if r.Workspace != "workspace-alias" || r.Project != "project-alias" || r.Session != "recipient-alias" || after != 0 {
				t.Error("initial selectors changed")
			}
		} else if r.Workspace != "ws_canonical" || r.Project != "prj_canonical" || r.Session != "ses_canonical" || after != 9 {
			t.Errorf("restart lost canonical binding/monotonic cursor: %+v", r)
		}
		switch phase {
		case 0:
			return notificationResult(3, 9, 3), nil
		case 1:
			return notificationResult(3, 9, 0), nil
		case 2:
			return notificationResult(0, 0, 0), nil
		default:
			return notificationResult(1, 12, 1), nil
		}
	})
	for phase = 0; phase < 4; phase++ {
		out.Reset()
		if err := notify.Run(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if phase == 1 || phase == 2 {
			if out.Len() != 0 {
				t.Fatal("unchanged/read-expired mail generated repeat reminder")
			}
		} else {
			var r bus.Result
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			data := object(t, jsonData(t, r.Data))
			if r.Source != "local-authority" || data["type"] != "inbox_changed" || data["event_id"] == "" || data["session_id"] != "ses_canonical" {
				t.Fatalf("invalid JSON event: %v", r)
			}
			if strings.Contains(out.String(), "private-instance") {
				t.Fatal("event leaked instance")
			}
		}
		notificationNoTemps(t, c.StatePath)
	}
	if calls != 4 {
		t.Fatalf("--once polled %d times across four runs", calls)
	}
}

type notificationFailWriter struct{ bytes.Buffer }

func (w *notificationFailWriter) Write(p []byte) (int, error) {
	w.Buffer.Write(p)
	return 0, errors.New("synthetic stdout failure")
}
func TestNotificationOutputFailureRetainsEventAndCursor(t *testing.T) {
	c := notificationConfig(t)
	bad := new(notificationFailWriter)
	c.Output = bad
	c.Executor = notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if notificationCursor(t, r) != 0 {
			t.Error("failed sink advanced cursor")
		}
		return notificationResult(2, 7, 2), nil
	})
	if err := notify.Run(context.Background(), c); err == nil {
		t.Fatal("output failure reported success")
	}
	var out bytes.Buffer
	c.Output = &out
	if err := notify.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bad.Bytes(), out.Bytes()) {
		t.Fatal("retry changed event identity/content")
	}
	notificationNoTemps(t, c.StatePath)
}
func TestNotificationBindingAndUnsafeStateRefusals(t *testing.T) {
	for _, field := range []string{"authority", "principal", "workspace", "project", "session", "instance", "adapter"} {
		t.Run(field, func(t *testing.T) {
			c := notificationConfig(t)
			calls := 0
			c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
				calls++
				return notificationResult(0, 0, 0), nil
			})
			if err := notify.Run(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "authority":
				c.Binding.Authority = "different"
			case "principal":
				c.Binding.Principal = "different"
			case "workspace":
				c.Binding.Workspace = "different"
			case "project":
				c.Binding.Project = "different"
			case "session":
				c.Binding.Session = "different"
			case "instance":
				c.Binding.Instance = "different"
			case "adapter":
				c.Binding.Adapter = "invalid"
			}
			if err := notify.Run(context.Background(), c); err == nil {
				t.Fatal("binding mismatch accepted")
			}
			if calls != 1 {
				t.Fatal("mismatched state reached authority")
			}
			after, _ := os.ReadFile(c.StatePath)
			if !bytes.Equal(original, after) {
				t.Fatal("mismatched state overwritten")
			}
			notificationNoTemps(t, c.StatePath)
		})
	}
	for _, kind := range []string{"corrupt", "oversized", "unknown-version", "symlink", "stale-lock"} {
		t.Run(kind, func(t *testing.T) {
			c := notificationConfig(t)
			if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(t.TempDir(), "unrelated")
			if err := os.WriteFile(sentinel, []byte("preserve unrelated data"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "corrupt":
				os.WriteFile(c.StatePath, []byte("not JSON"), 0600)
			case "oversized":
				os.WriteFile(c.StatePath, bytes.Repeat([]byte("x"), 32769), 0600)
			case "unknown-version":
				os.WriteFile(c.StatePath, []byte(`{"version":999}`), 0600)
			case "symlink":
				if err := os.Symlink(sentinel, c.StatePath); err != nil {
					t.Skip("symlink unavailable")
				}
			case "stale-lock":
				os.WriteFile(c.StatePath+".lock", []byte("other process lock"), 0600)
			}
			c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
				t.Error("unsafe state reached authority")
				return notificationResult(0, 0, 0), nil
			})
			if err := notify.Run(context.Background(), c); err == nil {
				t.Fatal("unsafe state accepted")
			}
			content, _ := os.ReadFile(sentinel)
			if string(content) != "preserve unrelated data" {
				t.Fatal("unrelated file changed")
			}
			if kind == "stale-lock" {
				content, _ := os.ReadFile(c.StatePath + ".lock")
				if string(content) != "other process lock" {
					t.Fatal("foreign lock stolen/removed")
				}
			} else {
				notificationNoTemps(t, c.StatePath)
			}
		})
	}
}
func TestNotificationBackoffAndCancellation(t *testing.T) {
	c := notificationConfig(t)
	c.Once = false
	calls := 0
	var waits []time.Duration
	c.Executor = notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		calls++
		notificationCursor(t, r)
		if calls <= 8 {
			return bus.Result{}, errors.New("temporary outage")
		}
		return notificationResult(0, 0, 0), nil
	})
	c.Wait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 9 {
			return context.Canceled
		}
		return nil
	}
	if err := notify.Run(context.Background(), c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second, time.Second}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("retry/recovery waits=%v", waits)
	}
	notificationNoTemps(t, c.StatePath)
	for _, status := range []int{400, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := notificationConfig(t)
			c.Once = false
			c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
				return bus.Result{}, &remote.HTTPError{Status: status, Code: "forbidden"}
			})
			c.Wait = func(context.Context, time.Duration) error {
				t.Error("definitive denial retried")
				return context.Canceled
			}
			if err := notify.Run(context.Background(), c); err == nil {
				t.Fatal("denial accepted")
			}
			notificationNoTemps(t, c.StatePath)
		})
	}
}
func TestNotificationConcurrentStateLock(t *testing.T) {
	c := notificationConfig(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	c.Executor = notificationExecutorFunc(func(ctx context.Context, _ bus.Request) (bus.Result, error) {
		close(entered)
		select {
		case <-release:
			return notificationResult(0, 0, 0), nil
		case <-ctx.Done():
			return bus.Result{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- notify.Run(ctx, c) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("watcher did not poll")
	}
	second := c
	second.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
		t.Error("duplicate writer reached authority")
		return notificationResult(0, 0, 0), nil
	})
	if err := notify.Run(ctx, second); err == nil {
		t.Error("concurrent writer accepted")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	notificationNoTemps(t, c.StatePath)
}

func notificationCodexConfig(t *testing.T) notify.Config {
	t.Helper()
	c := notificationConfig(t)
	c.Binding.Adapter = "codex"
	c.Binding.Thread = "01234567-89ab-cdef-0123-456789abcdef"
	c.CodexCommand = filepath.Join(t.TempDir(), "fake codex")
	if err := os.WriteFile(c.CodexCommand, []byte("#!/bin/sh\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// The injected runner is the only execution path. Never resolve the real CLI.
	return c
}
func TestNotificationCodexArgvAndAmbiguousRetry(t *testing.T) {
	c := notificationCodexConfig(t)
	var diag bytes.Buffer
	c.Diagnostics = &diag
	after := int64(0)
	c.Executor = notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if got := notificationCursor(t, r); got != after {
			t.Fatalf("cursor=%d want=%d", got, after)
		}
		fresh := int64(2)
		if after == 7 {
			fresh = 0
		}
		return notificationResult(2, 7, fresh), nil
	})
	var submitted [][]string
	attempt := 0
	c.Runner = notify.RunnerFunc(func(ctx context.Context, path string, args []string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("host attempt lacks bounded deadline")
		}
		if path != c.CodexCommand {
			t.Fatal("executable changed")
		}
		if reflect.DeepEqual(args, []string{"queue", "--help"}) {
			return []byte("queue --thread --message"), nil
		}
		if len(args) != 5 || args[0] != "queue" || args[1] != "--thread" || args[2] != c.Binding.Thread || args[3] != "--message" {
			t.Fatalf("unsafe queue argv: %q", args)
		}
		submitted = append(submitted, append([]string(nil), args...))
		attempt++
		if !strings.Contains(args[4], "not human approval") || !strings.Contains(args[4], "already-configured") || !strings.Contains(args[4], "ses_canonical") {
			t.Fatal("hint lacks fixed scope/approval/inbox guidance")
		}
		for _, bad := range []string{"private-instance", "recipient-alias", c.StatePath, "peer body", "--approval-policy"} {
			if strings.Contains(args[4], bad) {
				t.Fatal("hint contains forbidden context")
			}
		}
		if attempt == 1 {
			return []byte("synthetic-private-host-output"), errors.New("synthetic ambiguous timeout")
		}
		return []byte("synthetic-private-host-output"), nil
	})
	if err := notify.Run(context.Background(), c); err == nil {
		t.Fatal("failed host accepted")
	}
	if strings.Contains(diag.String(), "hint_accepted") {
		t.Fatal("failed host declared acceptance")
	}
	if err := notify.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	after = 7
	if err := notify.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(submitted) != 2 || !reflect.DeepEqual(submitted[0], submitted[1]) {
		t.Fatal("ambiguous retry event changed or unchanged unread repeatedly queued")
	}
	if strings.Contains(diag.String(), "synthetic-private-host-output") || !strings.Contains(diag.String(), "hint_accepted") {
		t.Fatal("host diagnostics are unsafe or acceptance absent")
	}
	notificationNoTemps(t, c.StatePath)
}
func TestNotificationCodexCheckpointHostBinding(t *testing.T) {
	for _, field := range []string{"executable", "CODEX_HOME"} {
		t.Run(field, func(t *testing.T) {
			c := notificationCodexConfig(t)
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			calls, hosts := 0, 0
			c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
				calls++
				return notificationResult(0, 0, 0), nil
			})
			c.Runner = notify.RunnerFunc(func(context.Context, string, []string) ([]byte, error) {
				hosts++
				return []byte("queue --thread --message"), nil
			})
			if err := notify.Run(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if field == "executable" {
				next := filepath.Join(t.TempDir(), "second fake codex")
				if err := os.WriteFile(next, []byte("#!/bin/sh\nexit 92\n"), 0700); err != nil {
					t.Fatal(err)
				}
				c.CodexCommand = next
			} else {
				t.Setenv("CODEX_HOME", t.TempDir())
			}
			if err := notify.Run(context.Background(), c); err == nil {
				t.Error("host-binding change reused checkpoint")
			}
			if calls != 1 || hosts != 1 {
				t.Errorf("mismatched host binding reached authority/host: calls=%d hosts=%d", calls, hosts)
			}
			current, _ := os.ReadFile(c.StatePath)
			if !bytes.Equal(original, current) {
				t.Error("binding mismatch modified checkpoint")
			}
		})
	}
}
func TestNotificationAcceptedHintCheckpointFailureStops(t *testing.T) {
	c := notificationConfig(t)
	c.Once = false
	c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) { return notificationResult(1, 8, 1), nil })
	var output bytes.Buffer
	c.Output = notificationWriterFunc(func(p []byte) (int, error) {
		n, _ := output.Write(p)
		if err := os.Remove(c.StatePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(c.StatePath, 0700); err != nil {
			t.Fatal(err)
		}
		return n, nil
	})
	c.Wait = func(context.Context, time.Duration) error {
		t.Error("checkpoint failure continued poll loop")
		return context.Canceled
	}
	err := notify.Run(context.Background(), c)
	if err == nil || !notify.IsPermanent(err) || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("accepted/checkpoint failure not terminal uncertain outcome: %v", err)
	}
	if bytes.Count(output.Bytes(), []byte("\n")) != 1 {
		t.Fatal("accepted output repeated")
	}
	notificationNoTemps(t, c.StatePath)
}

type notificationWriterFunc func([]byte) (int, error)

func (f notificationWriterFunc) Write(p []byte) (int, error) { return f(p) }
func TestNotificationStatusRejectsMalformedMetadata(t *testing.T) {
	for _, bad := range []string{
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses","unread_count":null,"latest_sequence":0,"new_count":0}`,
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses","unread_count":0,"latest_sequence":null,"new_count":0}`,
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses","unread_count":0,"latest_sequence":0,"new_count":null}`,
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses","unread_count":1.5,"latest_sequence":4,"new_count":1}`,
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses","unread_count":0,"latest_sequence":4,"new_count":0}`,
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses","unread_count":1,"latest_sequence":4,"new_count":2}`,
		`{"workspace_id":"ws","project_id":"prj","session_id":"ses; untrusted","unread_count":1,"latest_sequence":4,"new_count":1}`,
	} {
		if _, err := notify.ParseStatus(bus.Result{Source: "server", Data: json.RawMessage(bad)}); err == nil {
			t.Errorf("malformed status accepted: %s", bad)
		}
	}
	r := notificationResult(1, 1, 1)
	r.Source = "offline-mirror"
	if _, err := notify.ParseStatus(r); err == nil {
		t.Fatal("mirror status accepted")
	}
}

func TestNotificationPollAndHostCancellation(t *testing.T) {
	for _, where := range []string{"poll", "host"} {
		t.Run(where, func(t *testing.T) {
			c := notificationConfig(t)
			entered := make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if where == "poll" {
				c.Executor = notificationExecutorFunc(func(ctx context.Context, _ bus.Request) (bus.Result, error) {
					close(entered)
					<-ctx.Done()
					return bus.Result{}, ctx.Err()
				})
			} else {
				c = notificationCodexConfig(t)
				c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) { return notificationResult(1, 1, 1), nil })
				c.Runner = notify.RunnerFunc(func(ctx context.Context, _ string, args []string) ([]byte, error) {
					if len(args) == 2 {
						return []byte("queue --thread --message"), nil
					}
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				})
			}
			done := make(chan error, 1)
			go func() { done <- notify.Run(ctx, c) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("target operation never entered")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled operation reported acceptance")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not stop promptly")
			}
			notificationNoTemps(t, c.StatePath)
		})
	}
}
func TestNotificationCodexFakeSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable fixture; native Windows host adapter separately pending")
	}
	c := notificationCodexConfig(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	log := filepath.Join(t.TempDir(), "exact argv log")
	t.Setenv("CLAWBRIDGE_NOTIFICATION_FIXTURE_LOG", log)
	script := `#!/bin/sh
if [ "$#" = 2 ] && [ "$1" = queue ] && [ "$2" = --help ]; then
 printf '%s\n' 'queue --thread --message'
 exit 0
fi
printf '%s\000' "$@" > "$CLAWBRIDGE_NOTIFICATION_FIXTURE_LOG"
printf '%s\n' 'synthetic-host-output-not-for-user'
`
	if err := os.WriteFile(c.CodexCommand, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) { return notificationResult(1, 3, 1), nil })
	var out, diag bytes.Buffer
	c.Output = &out
	c.Diagnostics = &diag
	if err := notify.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(args) != 5 || !reflect.DeepEqual(args[:4], []string{"queue", "--thread", c.Binding.Thread, "--message"}) {
		t.Fatalf("default runner argv=%q", args)
	}
	if !strings.Contains(args[4], "ses_canonical") || strings.Contains(args[4], "private-instance") {
		t.Fatal("fake child received unsafe hint")
	}
	if out.Len() != 0 || strings.Contains(diag.String(), "synthetic-host-output-not-for-user") || !strings.Contains(diag.String(), "hint_accepted") {
		t.Fatal("default runner output/diagnostic contract violated")
	}
	notificationNoTemps(t, c.StatePath)
}

func TestNotificationCorruptCheckpointCursorRefused(t *testing.T) {
	for _, kind := range []string{"missing", "null", "fractional", "negative"} {
		t.Run(kind, func(t *testing.T) {
			c := notificationConfig(t)
			calls := 0
			c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
				calls++
				return notificationResult(1, 7, 1), nil
			})
			if err := notify.Run(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]json.RawMessage
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				delete(state, "after")
			case "null":
				state["after"] = json.RawMessage("null")
			case "fractional":
				state["after"] = json.RawMessage("1.5")
			case "negative":
				state["after"] = json.RawMessage("-1")
			}
			damaged, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.StatePath, damaged, 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			c.Output = &out
			if err := notify.Run(context.Background(), c); err == nil {
				t.Error("corrupt cursor accepted as a fresh zero checkpoint")
			}
			if calls != 1 || out.Len() != 0 {
				t.Error("corrupt checkpoint reached authority or repeated an accepted hint")
			}
			after, err := os.ReadFile(c.StatePath)
			if err != nil || !bytes.Equal(after, damaged) {
				t.Error("corrupt state was overwritten")
			}
			notificationNoTemps(t, c.StatePath)
		})
	}
}

func TestNotificationCheckpointMandatoryFields(t *testing.T) {
	for _, key := range []string{"version", "binding", "scope", "after"} {
		for _, kind := range []string{"missing", "null"} {
			t.Run(key+"/"+kind, func(t *testing.T) {
				c := notificationConfig(t)
				calls := 0
				c.Executor = notificationExecutorFunc(func(context.Context, bus.Request) (bus.Result, error) {
					calls++
					return notificationResult(0, 0, 0), nil
				})
				if err := notify.Run(context.Background(), c); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(c.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				var state map[string]json.RawMessage
				if err := json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				if kind == "missing" {
					delete(state, key)
				} else {
					state[key] = json.RawMessage("null")
				}
				damaged, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.StatePath, damaged, 0600); err != nil {
					t.Fatal(err)
				}
				if err := notify.Run(context.Background(), c); err == nil {
					t.Error("checkpoint without mandatory field accepted")
				}
				if calls != 1 {
					t.Error("damaged checkpoint reached authority")
				}
				after, err := os.ReadFile(c.StatePath)
				if err != nil || !bytes.Equal(after, damaged) {
					t.Error("damaged checkpoint overwritten")
				}
				notificationNoTemps(t, c.StatePath)
			})
		}
	}
}
