package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/secretenv"
)

// The command adapter types into a terminal, and a terminal may hold a bare
// shell (the agent exited) or a half-typed relaunch line. So before it types
// it checks that the session's agent is up (agentLive) and, if the person
// gave one, runs their precondition (--only-if). A batch that fails either
// is HELD: nothing is typed and the checkpoint stays, so the same batch is
// tried again on the next poll or stream wake and typed once the agent is
// live. The json and codex adapters type nothing and are not gated.

// AgentRecordFresh is how old the session's queue record may be for its
// agent to count as running: twice the MCP server's refresh (it rewrites the
// record every minute, mcpserver recordEvery), so one missed write is
// tolerated and a second is not. It is tighter than ctxwatch.QueueRecordFresh
// (3 minutes), which the hooks use, because typing into a dead pane is worse
// than waiting a minute.
const AgentRecordFresh = 2 * time.Minute

// OnlyIfTimeout is how long the --only-if command may run.
const OnlyIfTimeout = 5 * time.Second

// Hold reasons, written to the diagnostic.
const (
	holdAgentNotRunning = "agent_not_running"
	holdOnlyIfRefused   = "only_if_refused"
)

// LiveProbe is how the gate looks at the clock and at processes; nil
// fields use the real ones (tests replace them).
type LiveProbe struct {
	Now    func() time.Time
	Alive  func(pid int) bool
	Parent func(pid int) (int, bool)
	// ParentUnsupported skips the parent check, where the platform cannot
	// answer it (ctxwatch.ParentCheckSupported); the notifier's job is
	// delivery, so there it checks freshness and liveness only.
	ParentUnsupported bool
}

func (p LiveProbe) withDefaults() LiveProbe {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Alive == nil {
		p.Alive = ctxwatch.ProcessAlive
	}
	if p.Parent == nil {
		p.Parent = ctxwatch.ParentPID
		p.ParentUnsupported = !ctxwatch.ParentCheckSupported
	}
	return p
}

// gateState remembers the newest batch a hold was said for.
type gateState struct{ said int64 }

// agentLive says whether the session's agent is running: its queue record
// is fresh, the agent and its MCP server are alive, and the MCP server is
// still the agent's child. The queue record stores no process start time, so
// that parent link is the PID-reuse guard (the one the hooks use,
// ctxwatch.HookQueueRecord): if the agent exited and its PID was reissued,
// the orphaned MCP server (or a reused MCP PID) no longer has it as parent.
// Where the platform cannot read a parent PID, that check is skipped and a
// reused PID goes undetected. With no ContextDir there are no records, and the gate does not apply.
func agentLive(c Config) bool {
	if c.ContextDir == "" {
		return true
	}
	p := c.Live.withDefaults()
	r, ok, err := ctxwatch.LoadQueueRecord(c.ContextDir, c.Binding.Session)
	if err != nil || !ok || r.AgentPID <= 0 || r.MCPPID <= 0 {
		return false
	}
	at, err := time.Parse(time.RFC3339, r.UpdatedAt)
	now := p.Now()
	if err != nil || now.Sub(at) > AgentRecordFresh || at.Sub(now) > time.Minute {
		return false
	}
	if !p.Alive(r.AgentPID) || !p.Alive(r.MCPPID) {
		return false
	}
	if p.ParentUnsupported {
		return true
	}
	parent, ok := p.Parent(r.MCPPID)
	return ok && parent == r.AgentPID
}

// holdReason is why the command adapter must not type now, or "" to type.
// The built-in gate runs first, then --only-if.
func holdReason(ctx context.Context, c Config) string {
	if !agentLive(c) {
		return holdAgentNotRunning
	}
	if len(c.OnlyIf) > 0 {
		timeout := c.OnlyIfTimeout
		if timeout <= 0 {
			timeout = OnlyIfTimeout
		}
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		env := []string{"CLAWBRIDGE_SESSION=" + c.Binding.Session}
		if c.OnlyIfRunner(bounded, c.OnlyIf, env, nil) != nil {
			return holdOnlyIfRefused
		}
	}
	return ""
}

// hold says once per batch (its newest sequence) that it is held. The
// diagnostic carries the event's IDs and the reason, never message content.
func (g *gateState) hold(c Config, e Event, reason string) error {
	if g.said == e.LatestSequence {
		return nil
	}
	g.said = e.LatestSequence
	if err := json.NewEncoder(c.Diagnostics).Encode(map[string]any{"source": "notifier", "data": map[string]any{"type": "hint_held", "reason": reason, "event_id": e.EventID, "session_id": e.SessionID}}); err != nil {
		return Permanent(errors.New("diagnostic output failed"))
	}
	return nil
}

// runOnlyIf runs the precondition: argv, no shell, no stdin, output
// discarded, ClawBridge's secrets scrubbed from its environment. Exit 0 is
// nil; anything else (exit code, timeout, exec error) is an error.
func runOnlyIf(ctx context.Context, argv, env []string, _ []byte) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(secretenv.Scrub(os.Environ()), env...)
	cmd.WaitDelay = 250 * time.Millisecond
	// nil Stdin/Stdout/Stderr are the null device.
	return cmd.Run()
}

// prepareOnlyIf checks the --only-if argv once, at startup: an absolute
// path to an executable, at most 64 arguments.
func prepareOnlyIf(c *Config) error {
	if len(c.OnlyIf) == 0 {
		return nil
	}
	if c.Binding.Adapter != "command" {
		return Permanent(errors.New("only the command adapter takes --only-if"))
	}
	if len(c.OnlyIf) > 64 || !filepath.IsAbs(c.OnlyIf[0]) {
		return Permanent(errors.New("--only-if needs an absolute program path, at most 64 arguments"))
	}
	for _, a := range c.OnlyIf {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return Permanent(errors.New("--only-if arguments must be text of at most 4096 bytes"))
		}
	}
	if _, err := exec.LookPath(c.OnlyIf[0]); err != nil {
		return Permanent(errors.New("the --only-if program is not an executable"))
	}
	if c.OnlyIfRunner == nil {
		c.OnlyIfRunner = runOnlyIf
	}
	return nil
}
