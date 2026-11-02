package notify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
)

// fakeProcs is the gate's clock and process table.
type fakeProcs struct {
	now    time.Time
	alive  map[int]bool
	parent map[int]int
}

const (
	fakeAgent = 4242
	fakeMCP   = 4243
)

// liveAgent gives c's session a fresh queue record whose agent and MCP
// server are alive under fake processes, and returns the fakes to break.
func liveAgent(t *testing.T, c *Config) *fakeProcs {
	t.Helper()
	p := &fakeProcs{now: time.Now().UTC(), alive: map[int]bool{fakeAgent: true, fakeMCP: true}, parent: map[int]int{fakeMCP: fakeAgent}}
	c.Live = LiveProbe{Now: func() time.Time { return p.now },
		Alive:  func(pid int) bool { return p.alive[pid] },
		Parent: func(pid int) (int, bool) { v, ok := p.parent[pid]; return v, ok }}
	p.record(t, c, p.now)
	return p
}

func (p *fakeProcs) record(t *testing.T, c *Config, at time.Time) {
	t.Helper()
	if err := ctxwatch.SaveQueueRecord(c.ContextDir, ctxwatch.QueueRecord{Session: c.Binding.Session, Workspace: "w", Project: "p", Instance: "i", DB: "db",
		MCPPID: fakeMCP, AgentPID: fakeAgent, Stream: "stream", Leases: []ctxwatch.QueueLease{}, UpdatedAt: at.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
}

// FR68: each way the agent can be gone holds the batch: nothing typed, the
// checkpoint where it was, one hint_held per batch however often it is
// polled; and when the agent is back the batch is typed once.
func TestCommandAdapterHoldsUnlessTheAgentIsRunning(t *testing.T) {
	for name, break_ := range map[string]func(*testing.T, *Config, *fakeProcs){
		"record missing": func(t *testing.T, c *Config, p *fakeProcs) {
			path, _ := ctxwatch.BindingPath(c.ContextDir, c.Binding.Session)
			if err := os.Remove(strings.TrimSuffix(path, ".json") + ".queue.json"); err != nil {
				t.Fatal(err)
			}
		},
		"record stale": func(t *testing.T, c *Config, p *fakeProcs) { p.now = p.now.Add(AgentRecordFresh + time.Second) },
		"agent dead":   func(t *testing.T, c *Config, p *fakeProcs) { p.alive[fakeAgent] = false },
		"mcp dead":     func(t *testing.T, c *Config, p *fakeProcs) { p.alive[fakeMCP] = false },
		// The agent's PID was reissued to another process: the MCP server
		// is no longer its child.
		"pid reused": func(t *testing.T, c *Config, p *fakeProcs) { p.parent[fakeMCP] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			hints := 0
			c := commandConfig(t, &hints)
			c.Once = false
			p := liveAgent(t, &c)
			break_(t, &c, p)
			var diag bytes.Buffer
			c.Diagnostics = &diag
			waits := 0
			c.Wait = func(context.Context, time.Duration) error {
				waits++
				switch waits {
				case 4:
					if hints != 0 || load(t, c.StatePath).After != 0 || strings.Count(diag.String(), "hint_held") != 1 {
						t.Fatal("not held once", hints, diag.String())
					}
					*p = *fakeLive(c, p)
					p.record(t, &c, p.now)
				case 6:
					return context.Canceled
				}
				return nil
			}
			if err := Run(context.Background(), c); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if hints != 1 || load(t, c.StatePath).After != 9 {
				t.Fatal("not typed once after the agent returned", hints, load(t, c.StatePath).After)
			}
			out := diag.String()
			if strings.Count(out, "hint_held") != 1 || !strings.Contains(out, `"reason":"agent_not_running"`) || strings.Contains(out, "waiting") || strings.Contains(out, "SECRET-INSTANCE") {
				t.Fatal(out)
			}
		})
	}
}

// fakeLive repairs every break: all alive, MCP under the agent, now fresh.
func fakeLive(c Config, p *fakeProcs) *fakeProcs {
	p.now = time.Now().UTC()
	p.alive[fakeAgent], p.alive[fakeMCP] = true, true
	p.parent[fakeMCP] = fakeAgent
	return p
}

// A live agent is typed to, and a record just inside the bound counts.
func TestCommandAdapterTypesToALiveAgent(t *testing.T) {
	hints := 0
	c := commandConfig(t, &hints)
	p := liveAgent(t, &c)
	p.now = p.now.Add(AgentRecordFresh - time.Second)
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 1 || load(t, c.StatePath).After != 9 {
		t.Fatal(hints)
	}
}

// With no context directory there are no records, and today's behaviour
// stands: the notice is typed.
func TestCommandAdapterWithoutContextDirIsUngated(t *testing.T) {
	hints := 0
	c := commandConfig(t, &hints)
	c.ContextDir = ""
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if hints != 1 || load(t, c.StatePath).After != 9 {
		t.Fatal(hints)
	}
}

// The JSON adapter types nothing and is not gated, record or no record.
func TestJSONAdapterIsNotGated(t *testing.T) {
	hints := 0
	c := commandConfig(t, &hints)
	c.Binding.Adapter, c.Binding.Command = "json", nil
	c.CommandRunner = nil
	var out bytes.Buffer
	c.Output = &out
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "inbox_changed") || load(t, c.StatePath).After != 9 {
		t.Fatal(out.String())
	}
}

// --only-if runs after the gate, once per type: exit 0 types; a refusal or a
// timeout holds the batch, saying only_if_refused once.
func TestOnlyIf(t *testing.T) {
	for name, tc := range map[string]struct {
		run   func(ctx context.Context) error
		typed bool
	}{
		"exit 0":  {func(context.Context) error { return nil }, true},
		"exit 1":  {func(context.Context) error { return errors.New("exit status 1") }, false},
		"timeout": {func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, false},
	} {
		t.Run(name, func(t *testing.T) {
			hints := 0
			c := commandConfig(t, &hints)
			liveAgent(t, &c)
			c.OnlyIf = []string{testProgram(t), "--arg"}
			c.OnlyIfTimeout = 20 * time.Millisecond
			var got []string
			c.OnlyIfRunner = func(ctx context.Context, argv, env []string, stdin []byte) error {
				got = argv
				if stdin != nil {
					t.Fatal("only-if got stdin")
				}
				return tc.run(ctx)
			}
			var diag bytes.Buffer
			c.Diagnostics = &diag
			if err := Run(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0] != testProgram(t) {
				t.Fatal(got)
			}
			if tc.typed != (hints == 1) || (load(t, c.StatePath).After == 9) != tc.typed {
				t.Fatal(hints, load(t, c.StatePath).After)
			}
			if !tc.typed && (!strings.Contains(diag.String(), `"reason":"only_if_refused"`) || strings.Count(diag.String(), "hint_held") != 1) {
				t.Fatal(diag.String())
			}
		})
	}
}

// The built-in gate comes first: --only-if never runs for a dead agent.
func TestOnlyIfDoesNotRunWhenTheGateHolds(t *testing.T) {
	hints, ran := 0, 0
	c := commandConfig(t, &hints)
	p := liveAgent(t, &c)
	p.alive[fakeAgent] = false
	c.OnlyIf = []string{testProgram(t)}
	c.OnlyIfRunner = func(context.Context, []string, []string, []byte) error { ran++; return nil }
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if ran != 0 || hints != 0 {
		t.Fatal(ran, hints)
	}
}

// A program that is not an absolute path, or not an executable, is refused
// at startup with nothing run; so is --only-if on another adapter.
func TestOnlyIfRefusedAtStartup(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"relative": func(c *Config) { c.OnlyIf = []string{"check.sh"} },
		"missing":  func(c *Config) { c.OnlyIf = []string{filepath.Join(t.TempDir(), "nope")} },
		"json adapt": func(c *Config) {
			c.Binding.Adapter, c.Binding.Command = "json", nil
			c.OnlyIf = []string{testProgram(t)}
		},
	} {
		hints := 0
		c := commandConfig(t, &hints)
		mutate(&c)
		err := Run(context.Background(), c)
		if err == nil || !IsPermanent(err) || hints != 0 {
			t.Fatal(name, err)
		}
	}
}

// The real runner: exec, no shell, exit status decides, the timeout kills.
func TestRunOnlyIfProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX tools")
	}
	ctx := context.Background()
	if err := runOnlyIf(ctx, []string{"/bin/sh", "-c", "exit 0"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := runOnlyIf(ctx, []string{"/bin/sh", "-c", "exit 1"}, nil, nil); err == nil {
		t.Fatal("exit 1 accepted")
	}
	// No shell: a metacharacter argument is literal text.
	if err := runOnlyIf(ctx, []string{"/bin/sh", "-c", `[ "$1" = '$(false);x' ]`, "sh", "$(false);x"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := runOnlyIf(bounded, []string{"/bin/sleep", "10"}, nil, nil); err == nil || time.Since(start) > 5*time.Second {
		t.Fatal("timeout not enforced", err)
	}
}

// testProgram is an absolute executable path that exists everywhere.
func testProgram(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Where the parent check is unsupported it is skipped: a live record and live
// PIDs are typed to, a dead agent PID is still held.
func TestCommandAdapterWithoutAParentCheck(t *testing.T) {
	for name, dead := range map[string]bool{"live": false, "agent dead": true} {
		hints := 0
		c := commandConfig(t, &hints)
		p := liveAgent(t, &c)
		c.Live.ParentUnsupported = true
		c.Live.Parent = func(int) (int, bool) { t.Fatal("parent consulted"); return 0, false }
		p.alive[fakeAgent] = !dead
		if err := Run(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if (hints == 1) != !dead || (load(t, c.StatePath).After == 9) != !dead {
			t.Fatal(name, hints)
		}
	}
}
