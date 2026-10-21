package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

type executorFunc func(context.Context, bus.Request) (bus.Result, error)

func (f executorFunc) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	return f(ctx, r)
}
func result(unread, latest, fresh int64) bus.Result {
	return bus.Result{Source: "local-authority", Data: NotificationStatus{Status: Status{"ws-id", "prj-id", "ses-id", unread, latest, fresh}, Policy: "accept"}}
}
func config(t *testing.T) Config {
	t.Helper()
	return Config{Binding: Binding{Authority: "local:test", Principal: "local:user", Workspace: "workspace-name", Project: "project-name", Session: "session-name", Instance: "SECRET-INSTANCE", Adapter: "json"}, StatePath: filepath.Join(t.TempDir(), "notify.json"), Interval: time.Second, Once: true}
}
func load(t *testing.T, path string) checkpoint {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var s checkpoint
	if json.Unmarshal(b, &s) != nil {
		t.Fatal("invalid checkpoint")
	}
	return s
}
func fakeCodex(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake codex")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nexit 99\n"), 0700); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestNotificationJSONCoalescesPinsAndNeverRegressesAcrossRestart(t *testing.T) {
	c := config(t)
	var output bytes.Buffer
	c.Output = &output
	c.Once = false
	steps := [][3]int64{{2, 42, 2}, {2, 42, 0}, {0, 0, 0}, {1, 50, 1}}
	calls := 0
	c.Executor = executorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation != "notification_status" || r.Args["instance"] != "SECRET-INSTANCE" {
			t.Fatal(r)
		}
		after := int64(0)
		if calls > 0 {
			after = 42
			if r.Workspace != "ws-id" || r.Project != "prj-id" || r.Session != "ses-id" {
				t.Fatal("not canonically pinned", r)
			}
		}
		if r.Args["after"] != after {
			t.Fatal("cursor regressed", r)
		}
		s := steps[calls]
		calls++
		return result(s[0], s[1], s[2]), nil
	})
	c.Wait = func(context.Context, time.Duration) error {
		if calls == 4 {
			return context.Canceled
		}
		return nil
	}
	if e := Run(context.Background(), c); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || load(t, c.StatePath).After != 50 {
		t.Fatal(output.String())
	}
	if strings.Contains(output.String(), "SECRET-INSTANCE") {
		t.Fatal("instance leaked")
	}
	c.Once = true
	c.Executor = executorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if r.Args["after"] != int64(50) {
			t.Fatal(r)
		}
		return result(1, 50, 0), nil
	})
	before := output.Len()
	if e := Run(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if output.Len() != before {
		t.Fatal("restart repeated unchanged mail")
	}
	if _, e := os.Stat(c.StatePath + ".lock"); !os.IsNotExist(e) {
		t.Fatal("lock leaked", e)
	}
}
func TestNotificationCodexRetriesWithSameFixedHintAndOnlyAdvancesAfterAcceptance(t *testing.T) {
	c := config(t)
	c.Binding.Adapter = "codex"
	c.Binding.Thread = "01a15f5e-6ded-7be2-9912-14eb4c0ec476"
	c.CodexCommand = fakeCodex(t)
	c.Once = false
	var attempts [][]string
	polls := 0
	c.Executor = executorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		polls++
		if r.Args["after"] != int64(0) {
			t.Fatal("advanced on failure")
		}
		return result(2, 42, 2), nil
	})
	c.Runner = RunnerFunc(func(_ context.Context, path string, args []string) ([]byte, error) {
		if path != c.CodexCommand {
			t.Fatal(path)
		}
		if reflect.DeepEqual(args, []string{"queue", "--help"}) {
			return []byte("queue --thread --message"), nil
		}
		attempts = append(attempts, append([]string(nil), args...))
		if load(t, c.StatePath).After != 0 {
			t.Fatal("advanced before host")
		}
		if len(attempts) == 1 {
			return nil, errors.New("ambiguous host failure")
		}
		return []byte("raw unrelated secret output"), nil
	})
	c.Wait = func(_ context.Context, d time.Duration) error {
		if len(attempts) == 2 {
			return context.Canceled
		}
		if d != time.Second {
			t.Fatal(d)
		}
		return nil
	}
	var diag bytes.Buffer
	c.Diagnostics = &diag
	if e := Run(context.Background(), c); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if polls != 2 || len(attempts) != 2 || !reflect.DeepEqual(attempts[0], attempts[1]) || load(t, c.StatePath).After != 42 {
		t.Fatal(attempts)
	}
	a := attempts[0]
	if len(a) != 5 || a[0] != "queue" || a[1] != "--thread" || a[2] != c.Binding.Thread || a[3] != "--message" || strings.Contains(a[4], "SECRET-INSTANCE") || !strings.Contains(a[4], "not human approval") {
		t.Fatal(a)
	}
	if strings.Contains(diag.String(), "raw unrelated") {
		t.Fatal("forwarded raw host output")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestNotificationOutputFailureAndUnsafeStateNeverAdvance(t *testing.T) {
	c := config(t)
	calls := 0
	c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) { calls++; return result(1, 5, 1), nil })
	c.Output = failingWriter{}
	if e := Run(context.Background(), c); e == nil || load(t, c.StatePath).After != 0 {
		t.Fatal(e)
	}
	c.Binding.Session = "different"
	before := calls
	if e := Run(context.Background(), c); e == nil || calls != before {
		t.Fatal("mismatch reached authority", e)
	}
	c.Binding.Session = "session-name"
	if e := os.WriteFile(c.StatePath+".lock", []byte("crashed"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := Run(context.Background(), c); e == nil || calls != before {
		t.Fatal("stole lock", e)
	}
	if e := os.Remove(c.StatePath + ".lock"); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(c.StatePath); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("missing", c.StatePath); e != nil {
		t.Fatal(e)
	}
	if e := Run(context.Background(), c); e == nil || calls != before {
		t.Fatal("symlink reached authority", e)
	}
}
func TestNotificationCheckpointFailureStopsAfterAcceptedHint(t *testing.T) {
	c := config(t)
	c.Once = false
	c.Binding.Adapter = "codex"
	c.Binding.Thread = "01a15f5e-6ded-7be2-9912-14eb4c0ec476"
	c.CodexCommand = fakeCodex(t)
	c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) { return result(1, 5, 1), nil })
	attempts := 0
	c.Runner = RunnerFunc(func(_ context.Context, _ string, args []string) ([]byte, error) {
		if args[1] == "--help" {
			return []byte("queue --thread --message"), nil
		}
		attempts++
		if e := os.Remove(c.StatePath); e != nil {
			t.Fatal(e)
		}
		if e := os.Mkdir(c.StatePath, 0700); e != nil {
			t.Fatal(e)
		}
		return nil, nil
	})
	c.Wait = func(context.Context, time.Duration) error { t.Fatal("retried after uncertain checkpoint"); return nil }
	if e := Run(context.Background(), c); e == nil || !IsPermanent(e) || attempts != 1 {
		t.Fatal(e, attempts)
	}
}
func TestNotificationRejectsMirrorMalformedScopeAndInvalidOptions(t *testing.T) {
	for _, r := range []bus.Result{{Source: "offline-mirror", Data: Status{}}, result(1, 0, 1), result(1, 2, 2), {Source: "server", Data: map[string]any{"workspace_id": "ws-id"}}} {
		if _, e := ParseStatus(r); e == nil || !IsPermanent(e) {
			t.Fatal(r, e)
		}
	}
	c := config(t)
	c.Binding.Adapter = "codex"
	c.Binding.Thread = "alias"
	c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) {
		t.Fatal("invalid options polled")
		return result(0, 0, 0), nil
	})
	if Run(context.Background(), c) == nil {
		t.Fatal("thread alias accepted")
	}
}

func TestNotificationCodexHostStorageAndExecutableBoundAcrossRestart(t *testing.T) {
	for _, change := range []string{"home", "executable"} {
		t.Run(change, func(t *testing.T) {
			c := config(t)
			c.Binding.Adapter = "codex"
			c.Binding.Thread = "01234567-89ab-cdef-0123-456789abcdef"
			c.CodexCommand = fakeCodex(t)
			t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "not-created"))
			calls, hosts := 0, 0
			c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) { calls++; return result(0, 0, 0), nil })
			c.Runner = RunnerFunc(func(context.Context, string, []string) ([]byte, error) {
				hosts++
				return []byte("queue --thread --message"), nil
			})
			if err := Run(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if change == "home" {
				t.Setenv("CODEX_HOME", t.TempDir())
			} else {
				c.CodexCommand = fakeCodex(t)
			}
			if err := Run(context.Background(), c); err == nil {
				t.Fatal("host binding changed silently")
			}
			if calls != 1 || hosts != 1 {
				t.Fatal("binding mismatch invoked authority/host", calls, hosts)
			}
			after, _ := os.ReadFile(c.StatePath)
			if !bytes.Equal(before, after) {
				t.Fatal("binding mismatch overwrote state")
			}
		})
	}
}

func TestNotificationCorruptCheckpointRequiredFieldsNeverReplay(t *testing.T) {
	for _, key := range []string{"after", "version", "binding", "scope"} {
		for _, damage := range []string{"missing", "null"} {
			t.Run(key+"/"+damage, func(t *testing.T) {
				c := config(t)
				calls := 0
				c.Executor = executorFunc(func(context.Context, bus.Request) (bus.Result, error) { calls++; return result(1, 7, 1), nil })
				var output bytes.Buffer
				c.Output = &output
				if err := Run(context.Background(), c); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(c.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(original, &fields); err != nil {
					t.Fatal(err)
				}
				if damage == "missing" {
					delete(fields, key)
				} else {
					fields[key] = json.RawMessage("null")
				}
				damaged, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.StatePath, damaged, 0600); err != nil {
					t.Fatal(err)
				}
				before := output.Len()
				if err := Run(context.Background(), c); err == nil {
					t.Fatal("corrupt checkpoint accepted")
				}
				after, err := os.ReadFile(c.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || output.Len() != before || !bytes.Equal(damaged, after) {
					t.Fatal("corrupt state reached authority/sink or changed bytes")
				}
				if _, err := os.Stat(c.StatePath + ".lock"); !os.IsNotExist(err) {
					t.Fatal("lock leaked", err)
				}
			})
		}
	}
}
