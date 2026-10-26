package agentd

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"
)

// Server is the part of the ClawBridge server agentd uses, with its launcher
// token (§11.8): which owner refs need an agent, a one-time agent credential
// for one of them, and a report for doctor. It can say which refs run, never
// how: the spec is agentSpec.
type Server interface {
	// Wanted returns the owner refs that need an agent, and the server-wide
	// cap (dispatcher_model_max_agents).
	Wanted(ctx context.Context) (refs []string, maxAgents int, err error)
	// Credential mints a one-time agent credential (cba_…) for ref.
	Credential(ctx context.Context, ref string) (string, error)
	// Report sends a fixed code for ref, such as agent_crashloop.
	Report(ctx context.Context, ref, code string) error
}

const (
	reconcileEvery   = 30 * time.Second
	restartsPerHour  = 3
	codeCrashloop    = "agent_crashloop"
	maxAgentsCeiling = 16
)

// Reconciler starts and stops agent containers to match the server's wanted
// set, under the cap.
type Reconciler struct {
	cfg    Config
	server Server
	docker *docker
	log    *slog.Logger
	now    func() time.Time
	// restarts holds, per ref, when an exited container was replaced.
	restarts map[string][]time.Time
	reported map[string]bool
}

// NewReconciler returns a Reconciler for cfg.
func NewReconciler(cfg Config, server Server, log *slog.Logger) *Reconciler {
	return &Reconciler{cfg: cfg, server: server, docker: newDocker(cfg.Docker), log: log, now: time.Now,
		restarts: map[string][]time.Time{}, reported: map[string]bool{}}
}

// Run reconciles at once, then every 30 seconds until ctx ends. A failed
// round is logged and the next one tries again.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	for {
		if err := r.Step(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("reconcile failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Step is one reconciliation.
func (r *Reconciler) Step(ctx context.Context) error {
	refs, maxAgents, err := r.server.Wanted(ctx)
	if err != nil {
		return errors.New("the server's wanted set could not be read")
	}
	if maxAgents < 1 || maxAgents > maxAgentsCeiling {
		return errors.New("the server's agent cap is out of range")
	}
	wanted := map[string]bool{}
	for _, ref := range refs {
		if !validOwnerRef(ref) {
			// The value is the server's, not echoed.
			r.log.Warn("refused a wanted owner ref that is not p_<8-32 lowercase letters or digits>")
			continue
		}
		wanted[ref] = true
	}
	containers, err := r.docker.list(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, c := range containers {
		switch {
		case !wanted[c.ref]:
			r.stopAgent(ctx, c.ref)
		case c.running:
			live[c.ref] = true
		default:
			// It exited: remove it, and count the restart that replaces it.
			if r.act("remove exited", c.ref, r.docker.remove(ctx, c.ref)) {
				r.restarts[c.ref] = append(r.restarts[c.ref], r.now())
			}
		}
	}
	// A lowered cap stops the extra agents, so Docker never runs more than it.
	running := slices.Sorted(maps.Keys(live))
	for _, ref := range running[min(maxAgents, len(running)):] {
		r.log.Warn("over the agent cap", "ref", ref, "max_agents", maxAgents)
		r.stopAgent(ctx, ref)
		delete(live, ref)
	}
	missing := make([]string, 0, len(wanted))
	for ref := range wanted {
		if !live[ref] {
			missing = append(missing, ref)
		}
	}
	slices.Sort(missing)
	for _, ref := range missing {
		if len(live) >= maxAgents {
			r.log.Warn("agent cap reached", "max_agents", maxAgents)
			break
		}
		if r.crashlooping(ref) {
			if !r.reported[ref] && r.server.Report(ctx, ref, codeCrashloop) == nil {
				r.reported[ref] = true
			}
			r.log.Warn("not restarting", "ref", ref, "reason", codeCrashloop)
			continue
		}
		delete(r.reported, ref)
		if r.startAgent(ctx, ref) {
			live[ref] = true
		}
	}
	return nil
}

// crashlooping reports whether ref used its restarts in the last hour.
func (r *Reconciler) crashlooping(ref string) bool {
	cutoff := r.now().Add(-time.Hour)
	recent := slices.DeleteFunc(r.restarts[ref], func(t time.Time) bool { return t.Before(cutoff) })
	r.restarts[ref] = recent
	return len(recent) > restartsPerHour
}

// startAgent creates the owner's network and container, attaches to its
// stdin, starts it and hands over a fresh credential. On any failure it
// removes the container, so the next round starts clean.
func (r *Reconciler) startAgent(ctx context.Context, ref string) bool {
	if !r.act("create network", ref, r.docker.createNetwork(ctx, ref)) ||
		!r.act("create", ref, r.docker.create(ctx, agentSpec(r.cfg.Image, r.cfg.ServerURL, ref), ref)) {
		return false
	}
	send, err := r.docker.attach(ctx, ref)
	ok := r.act("attach", ref, err) && r.act("start", ref, r.docker.start(ctx, ref))
	if ok {
		credential, err := r.server.Credential(ctx, ref)
		if err != nil {
			err = errors.New("the server did not mint an agent credential")
		} else {
			err = send(credential)
		}
		ok = r.act("hand over credential", ref, err)
	} else if send != nil {
		_ = send("")
	}
	if !ok {
		r.act("remove failed start", ref, r.docker.remove(ctx, ref))
	}
	return ok
}

func (r *Reconciler) stopAgent(ctx context.Context, ref string) {
	if r.act("stop", ref, r.docker.stop(ctx, ref)) && r.act("remove", ref, r.docker.remove(ctx, ref)) {
		r.act("remove network", ref, r.docker.removeNetwork(ctx, ref))
	}
}

// act logs one action's outcome: the ref, the action and, on failure, the
// Docker call and status. It never logs a credential: none reaches an error.
func (r *Reconciler) act(action, ref string, err error) bool {
	if err != nil {
		r.log.Warn("action failed", "ref", ref, "action", action, "error", err.Error())
		return false
	}
	r.log.Info("action", "ref", ref, "action", action)
	return true
}
