package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Server is the part of the ClawBridge server agentd uses, with its launcher
// token (§11.8): which owner refs need an agent, a one-time agent credential
// for one of them, and a report for doctor. It can say which refs run, never
// how: the specs are compiled in.
type Server interface {
	// Wanted returns the owners that need an agent, and the server-wide cap
	// (dispatcher_model_max_agents).
	Wanted(ctx context.Context) (wanted []Wanted, maxAgents int, err error)
	// Credential mints a one-time agent credential (cba_…) for ref.
	Credential(ctx context.Context, ref string) (string, error)
	// Report sends a fixed code for ref, such as agent_crashloop.
	Report(ctx context.Context, ref, code string) error
}

// Wanted is one owner that needs an agent: its ref, the host:port of its
// provider, which its agent may reach besides the server, and its config
// revision (the pending_test one when there is one, else the active one).
type Wanted struct {
	Ref      string
	Provider string
	Revision int64
}

const (
	reconcileEvery = 30 * time.Second
	// subnetQuarantine is far longer than the egress proxy's policy reload
	// (2 s), which closes the old owner's tunnels.
	subnetQuarantine = time.Minute
	restartsPerHour  = 3
	codeCrashloop    = "agent_crashloop"
	maxAgentsCeiling = 16
)

// Reconciler starts and stops agent containers to match the server's wanted
// set, under the cap, with the egress proxy on each owner's network and its
// policy written before an agent starts.
type Reconciler struct {
	cfg    Config
	server Server
	docker *docker
	log    *slog.Logger
	now    func() time.Time
	// serverTarget is the server's host:port in every owner's allow-list.
	serverTarget string
	// restarts holds, per ref, when an exited container was replaced.
	restarts map[string][]time.Time
	reported map[string]bool
	// policy is the egress policy last written.
	policy []byte
	// freed holds when each owner subnet was given up. It isn't handed to
	// another owner for subnetQuarantine, so no tunnel or policy line of the
	// old owner can still match the new owner's agent. It is kept in
	// memory only: a subnet freed in the minute before agentd restarts can
	// be handed out again at once, guarded then only by the egress proxy's
	// policy reload.
	freed map[netip.Prefix]time.Time
}

// NewReconciler returns a Reconciler for cfg, which LoadConfig validated.
func NewReconciler(cfg Config, server Server, log *slog.Logger) *Reconciler {
	target, _ := serverTarget(cfg.ServerURL)
	return &Reconciler{cfg: cfg, server: server, docker: newDocker(cfg.Docker), log: log, now: time.Now,
		serverTarget: target, restarts: map[string][]time.Time{}, reported: map[string]bool{}, freed: map[netip.Prefix]time.Time{}}
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
	wants, maxAgents, err := r.server.Wanted(ctx)
	if err != nil {
		return errors.New("the server's wanted set could not be read")
	}
	if maxAgents < 1 || maxAgents > maxAgentsCeiling {
		return errors.New("the server's agent cap is out of range")
	}
	// The server's values are checked, and never echoed.
	provider := map[string]string{}
	revision := map[string]int64{}
	for _, w := range wants {
		if !validOwnerRef(w.Ref) {
			r.log.Warn("refused a wanted owner ref that is not p_<8-32 lowercase letters or digits>")
			continue
		}
		if w.Revision < 1 {
			r.log.Warn("refused an owner without a config revision", "ref", w.Ref)
			continue
		}
		target, err := normalTarget(w.Provider)
		if ip, ipErr := netip.ParseAddr(hostOf(target)); err == nil && ipErr == nil && !publicAddr(ip) {
			err = errors.New("not public")
		}
		if err != nil {
			r.log.Warn("refused an owner whose provider is not a host:port", "ref", w.Ref)
			continue
		}
		provider[w.Ref] = target
		revision[w.Ref] = w.Revision
	}
	// No agent runs without the egress proxy.
	if err := r.ensureEgress(ctx); err != nil {
		return err
	}
	subnets, err := r.docker.listNetworks(ctx)
	if err != nil {
		return err
	}
	containers, err := r.docker.list(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, c := range containers {
		switch {
		case provider[c.ref] == "":
			r.stopAgent(ctx, c.ref, subnets)
		case c.revision != revision[c.ref]:
			// A rotated key or a new model runs in a fresh instance (§11.6),
			// which fetches the new revision; not a crash. Its network stays.
			r.log.Info("config revision changed", "ref", c.ref, "from", c.revision, "to", revision[c.ref])
			name := containerName(c.ref)
			if r.act("stop for new revision", c.ref, r.docker.stop(ctx, name)) {
				r.act("remove for new revision", c.ref, r.docker.remove(ctx, name))
			}
		case c.running:
			live[c.ref] = true
		default:
			// It exited: remove it, and count the restart that replaces it.
			if r.act("remove exited", c.ref, r.docker.remove(ctx, containerName(c.ref))) {
				r.restarts[c.ref] = append(r.restarts[c.ref], r.now())
			}
		}
	}
	// A lowered cap stops the extra agents, so Docker never runs more than it.
	running := slices.Sorted(maps.Keys(live))
	for _, ref := range running[min(maxAgents, len(running)):] {
		r.log.Warn("over the agent cap", "ref", ref, "max_agents", maxAgents)
		r.stopAgent(ctx, ref, subnets)
		delete(live, ref)
	}
	// Keep the egress proxy on every live owner's network: a recreated
	// egress container has lost them.
	for ref := range live {
		if !r.ensureNetwork(ctx, ref, subnets) {
			r.log.Warn("the owner's agent has no egress this round", "ref", ref)
		}
	}
	var starting []string
	for _, ref := range slices.Sorted(maps.Keys(provider)) {
		if live[ref] {
			continue
		}
		if len(live)+len(starting) >= maxAgents {
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
		if r.ensureNetwork(ctx, ref, subnets) {
			starting = append(starting, ref)
		}
	}
	// The policy allows a new agent's egress before the agent starts.
	if err := r.writePolicy(append(slices.Collect(maps.Keys(live)), starting...), subnets, provider); err != nil {
		return err
	}
	for _, ref := range starting {
		r.startAgent(ctx, ref, revision[ref])
	}
	return nil
}

// ensureEgress creates the egress network and keeps the egress proxy
// running, from its fixed spec. Its policy file exists before it starts.
func (r *Reconciler) ensureEgress(ctx context.Context) error {
	if err := r.docker.createNetwork(ctx, egressNetSpec()); err != nil {
		return err
	}
	exists, running, err := r.docker.running(ctx, egressName)
	if err != nil || running {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.cfg.PolicyDir, policyFile)); errors.Is(err, os.ErrNotExist) {
		if err := r.writeFile([]byte(`{"networks":[]}` + "\n")); err != nil {
			return err
		}
	}
	if exists && !r.act("remove exited", egressName, r.docker.remove(ctx, egressName)) {
		return errors.New("the egress proxy could not be replaced")
	}
	if !r.act("create", egressName, r.docker.create(ctx, egressName, egressSpec(r.cfg.EgressImage, r.cfg.PolicyDir))) ||
		!r.act("start", egressName, r.docker.start(ctx, egressName)) {
		return errors.New("the egress proxy could not be started")
	}
	return nil
}

// ensureNetwork gives ref its network, on a free subnet of the pool when it
// has none yet, with the egress proxy connected.
func (r *Reconciler) ensureNetwork(ctx context.Context, ref string, subnets map[string]netip.Prefix) bool {
	if _, ok := subnets[ref]; !ok {
		for p, at := range r.freed {
			if r.now().Sub(at) >= subnetQuarantine {
				delete(r.freed, p)
			}
		}
		subnet, ok := freeSubnet(subnets, r.freed)
		if !ok {
			r.log.Warn("no free subnet", "ref", ref, "pool", subnetPool.String())
			return false
		}
		if !r.act("create network", ref, r.docker.createNetwork(ctx, agentNetwork(ref, subnet))) {
			return false
		}
		subnets[ref] = subnet
	}
	connected, err := r.docker.egressConnected(ctx, ref)
	if !r.act("inspect network", ref, err) {
		return false
	}
	return connected || r.act("connect egress", ref, r.docker.connectEgress(ctx, ref))
}

// freeSubnet returns the first /28 of the pool that no owner uses and that
// is not quarantined.
func freeSubnet(subnets map[string]netip.Prefix, quarantined map[netip.Prefix]time.Time) (netip.Prefix, bool) {
	used := map[netip.Prefix]bool{}
	for _, s := range subnets {
		used[s] = true
	}
	for s := range quarantined {
		used[s] = true
	}
	base := subnetPool.Addr().As4()
	first := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	size := uint32(1) << (32 - subnetBits)
	for i := range uint32(1) << (subnetBits - subnetPool.Bits()) {
		n := first + i*size
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), subnetBits)
		if !used[p] {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// writePolicy writes the egress policy for refs, when it changed: each
// owner's subnet may reach the server and that owner's provider.
func (r *Reconciler) writePolicy(refs []string, subnets map[string]netip.Prefix, provider map[string]string) error {
	slices.Sort(refs)
	p := EgressPolicy{Networks: []EgressNetwork{}}
	for _, ref := range refs {
		if _, ok := subnets[ref]; !ok {
			// Its network is missing this round: it gets no egress.
			continue
		}
		allow := []string{r.serverTarget}
		if provider[ref] != r.serverTarget {
			allow = append(allow, provider[ref])
		}
		p.Networks = append(p.Networks, EgressNetwork{OwnerRef: ref, Source: subnets[ref].String(), Allow: allow})
	}
	// The proxy would refuse what doesn't compile; so does agentd.
	if _, err := compileEgress(p); err != nil {
		return errors.New("the egress policy is not valid: " + err.Error())
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if bytes.Equal(data, r.policy) {
		return nil
	}
	if err := r.writeFile(data); err != nil {
		return err
	}
	r.policy = data
	r.log.Info("egress policy written", "networks", len(p.Networks))
	return nil
}

// writeFile replaces the policy file atomically. It holds no secrets: the
// egress proxy, as another user, reads it.
func (r *Reconciler) writeFile(data []byte) error {
	tmp, err := os.CreateTemp(r.cfg.PolicyDir, ".egress-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(r.cfg.PolicyDir, policyFile))
}

// crashlooping reports whether ref used its restarts in the last hour.
func (r *Reconciler) crashlooping(ref string) bool {
	cutoff := r.now().Add(-time.Hour)
	recent := slices.DeleteFunc(r.restarts[ref], func(t time.Time) bool { return t.Before(cutoff) })
	r.restarts[ref] = recent
	return len(recent) > restartsPerHour
}

// startAgent creates the owner's container, attaches to its stdin, starts
// it and hands over a fresh credential. On any failure it removes the
// container, so the next round starts clean.
func (r *Reconciler) startAgent(ctx context.Context, ref string, revision int64) bool {
	name := containerName(ref)
	if !r.act("create", ref, r.docker.create(ctx, name, agentSpec(r.cfg.AgentImage, r.cfg.ServerURL, ref, revision))) {
		return false
	}
	send, err := r.docker.attach(ctx, ref)
	ok := r.act("attach", ref, err) && r.act("start", ref, r.docker.start(ctx, name))
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
		r.act("remove failed start", ref, r.docker.remove(ctx, name))
	}
	return ok
}

// stopAgent stops and removes ref's container, then takes the egress proxy
// off its network and removes the network.
func (r *Reconciler) stopAgent(ctx context.Context, ref string, subnets map[string]netip.Prefix) {
	name := containerName(ref)
	if r.act("stop", ref, r.docker.stop(ctx, name)) && r.act("remove", ref, r.docker.remove(ctx, name)) &&
		r.act("disconnect egress", ref, r.docker.disconnectEgress(ctx, ref)) &&
		r.act("remove network", ref, r.docker.removeNetwork(ctx, ref)) {
		if subnet, ok := subnets[ref]; ok {
			r.freed[subnet] = r.now()
		}
		delete(subnets, ref)
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
