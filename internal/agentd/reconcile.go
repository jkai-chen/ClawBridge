package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// codeStartFailing is reported once an owner's agent failed to start
	// startFailuresReported times in a row.
	codeStartFailing      = "agent_start_failing"
	startFailuresReported = 3
	// maxStartBackoff caps the rounds skipped after failed starts: 64
	// rounds, about half an hour.
	maxStartBackoff = 64
	// policySettle is how long a new agent waits after a policy change: the
	// egress proxy rereads the file every 2 s (egressPoll), and before then
	// it refuses the new agent's egress.
	policySettle = 3 * time.Second
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
	// failed holds, per ref, its agent's failed starts in a row.
	failed map[string]*startFailures
	// policy is the egress policy last written.
	policy []byte
	// freed holds when each owner subnet was given up. It isn't handed to
	// another owner for subnetQuarantine, so no tunnel or policy line of the
	// old owner can still match the new owner's agent. It is kept in
	// memory only: a subnet freed in the minute before agentd restarts can
	// be handed out again at once, guarded then only by the egress proxy's
	// policy reload.
	freed map[netip.Prefix]time.Time
	// sleep waits; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// startFailures counts an owner's failed starts. Each one doubles the
// rounds skipped before the next try.
type startFailures struct {
	count, skip int
	reported    bool
}

// NewReconciler returns a Reconciler for cfg, which LoadConfig validated.
func NewReconciler(cfg Config, server Server, log *slog.Logger) *Reconciler {
	target, _ := serverTarget(cfg.ServerURL)
	return &Reconciler{cfg: cfg, server: server, docker: newDocker(cfg.Docker), log: log, now: time.Now,
		serverTarget: target, restarts: map[string][]time.Time{}, reported: map[string]bool{}, failed: map[string]*startFailures{},
		freed: map[netip.Prefix]time.Time{}, sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
		// Server errors name the call and a status or fixed reason only.
		return fmt.Errorf("the server's wanted set could not be read: %w", err)
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
	for ref := range r.failed {
		if provider[ref] == "" {
			delete(r.failed, ref)
		}
	}
	// No agent starts without the egress proxy, but unwanted agents are
	// still stopped.
	egressErr := r.ensureEgress(ctx)
	subnets, err := r.docker.listNetworks(ctx)
	if err != nil {
		return err
	}
	containers, err := r.docker.list(ctx)
	if err != nil {
		return err
	}
	// live are the wanted agents running on their revision; present, every
	// ref whose container is still there after this round's stops. running
	// counts every running container, so one whose stop failed still
	// counts toward the cap.
	live, present := map[string]bool{}, map[string]bool{}
	running := 0
	stopFailed := false
	for _, c := range containers {
		gone := false
		switch {
		case provider[c.ref] == "":
			gone = r.removeAgent(ctx, c.ref, "")
		case c.revision != revision[c.ref]:
			// A rotated key or a new model runs in a fresh instance (§11.6),
			// which fetches the new revision; not a crash. Its network stays.
			r.log.Info("config revision changed", "ref", c.ref, "from", c.revision, "to", revision[c.ref])
			gone = r.removeAgent(ctx, c.ref, " for new revision")
		case c.running:
			live[c.ref] = true
		default:
			// It exited: remove it, and count the restart that replaces it.
			if gone = r.act("remove exited", c.ref, r.docker.remove(ctx, containerName(c.ref))); gone {
				r.restarts[c.ref] = append(r.restarts[c.ref], r.now())
			}
		}
		if !gone {
			present[c.ref] = true
			if c.running {
				running++
				stopFailed = stopFailed || !live[c.ref]
			}
		}
	}
	// A lowered cap stops the extra agents, so Docker never runs more than it.
	sorted := slices.Sorted(maps.Keys(live))
	for i := len(sorted) - 1; i >= 0 && running > maxAgents; i-- {
		ref := sorted[i]
		r.log.Warn("over the agent cap", "ref", ref, "max_agents", maxAgents)
		if !r.removeAgent(ctx, ref, " over the cap") {
			stopFailed = true
			continue
		}
		delete(live, ref)
		delete(present, ref)
		running--
	}
	// A network outlives its container when a stop, a failed start or an
	// exit removed only the container: an owner no longer wanted, with no
	// container left, loses its network, egress attachment and subnet.
	for _, ref := range slices.Sorted(maps.Keys(subnets)) {
		if provider[ref] == "" && !present[ref] {
			r.removeNetwork(ctx, ref, subnets)
		}
	}
	// Keep the egress proxy on every live owner's network: a recreated
	// egress container has lost them.
	for ref := range live {
		if egressErr == nil && !r.ensureNetwork(ctx, ref, subnets) {
			r.log.Warn("the owner's agent has no egress this round", "ref", ref)
		}
	}
	var starting []string
	switch {
	case egressErr != nil:
	case stopFailed:
		// What failed to stop may still run: start nothing until it is gone.
		r.log.Warn("an agent could not be stopped; starting none this round")
	default:
		for _, ref := range slices.Sorted(maps.Keys(provider)) {
			if present[ref] {
				continue
			}
			if running+len(starting) >= maxAgents {
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
			if f := r.failed[ref]; f != nil && f.skip > 0 {
				f.skip--
				continue
			}
			if r.ensureNetwork(ctx, ref, subnets) {
				starting = append(starting, ref)
			}
		}
	}
	// The policy allows a new agent's egress before the agent starts.
	changed, err := r.writePolicy(append(slices.Collect(maps.Keys(live)), starting...), subnets, provider)
	if err != nil {
		return err
	}
	if changed && len(starting) > 0 {
		if err := r.sleep(ctx, policySettle); err != nil {
			return err
		}
	}
	for _, ref := range starting {
		if r.startAgent(ctx, ref, revision[ref]) {
			delete(r.failed, ref)
		} else {
			r.startFailed(ctx, ref)
		}
	}
	return egressErr
}

// startFailed counts a failed start of ref's agent, and backs off: 1, 2,
// 4… rounds are skipped before the next try. After startFailuresReported
// in a row it reports codeStartFailing, once.
func (r *Reconciler) startFailed(ctx context.Context, ref string) {
	f := r.failed[ref]
	if f == nil {
		f = &startFailures{}
		r.failed[ref] = f
	}
	f.count++
	f.skip = min(1<<min(f.count-1, 6), maxStartBackoff) - 1
	if f.count >= startFailuresReported && !f.reported && r.server.Report(ctx, ref, codeStartFailing) == nil {
		f.reported = true
	}
	r.log.Warn("start failed", "ref", ref, "failures", f.count, "rounds_skipped", f.skip)
}

// ensureEgress creates the egress network and keeps the egress proxy
// running, from its fixed spec. Its policy file exists before it starts.
func (r *Reconciler) ensureEgress(ctx context.Context) error {
	if _, err := r.docker.createNetwork(ctx, egressNetSpec()); err != nil {
		return err
	}
	c, err := r.docker.inspect(ctx, egressName)
	if err != nil || c.running && c.image == r.cfg.EgressImage {
		return err
	}
	if c.running {
		// A new egress image takes effect at once: the old proxy is replaced,
		// and agents reconnect through the new one.
		r.log.Info("egress image changed", "ref", egressName)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.PolicyDir, policyFile)); errors.Is(err, os.ErrNotExist) {
		if err := r.writeFile([]byte(`{"networks":[]}` + "\n")); err != nil {
			return err
		}
	}
	if c.exists && !r.act("remove", egressName, r.docker.remove(ctx, egressName)) {
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
	if _, ok := subnets[ref]; !ok && !r.createNetwork(ctx, ref, subnets) {
		return false
	}
	n, err := r.docker.inspectNetwork(ctx, ref)
	if !r.act("inspect network", ref, err) {
		return false
	}
	if n.subnet != subnets[ref] {
		// A cbnet-<ref> was already there on another subnet (made before
		// fixed subnets, or by hand). The egress proxy would refuse its agent
		// every round: replace it.
		r.log.Error("the owner's network is not on its subnet; replacing it", "ref", ref)
		if n.egress && !r.act("disconnect egress", ref, r.docker.disconnectEgress(ctx, ref)) ||
			!r.act("remove network", ref, r.docker.removeNetwork(ctx, ref)) {
			return false
		}
		delete(subnets, ref)
		if !r.createNetwork(ctx, ref, subnets) {
			return false
		}
		n.egress = false
	}
	return n.egress || r.act("connect egress", ref, r.docker.connectEgress(ctx, ref))
}

// createNetwork creates ref's network on a free subnet of the pool, and
// records it in subnets.
func (r *Reconciler) createNetwork(ctx context.Context, ref string, subnets map[string]netip.Prefix) bool {
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
	_, err := r.docker.createNetwork(ctx, agentNetwork(ref, subnet))
	if !r.act("create network", ref, err) {
		return false
	}
	subnets[ref] = subnet
	return true
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
func (r *Reconciler) writePolicy(refs []string, subnets map[string]netip.Prefix, provider map[string]string) (changed bool, err error) {
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
		return false, errors.New("the egress policy is not valid: " + err.Error())
	}
	data, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if bytes.Equal(data, r.policy) {
		return false, nil
	}
	if err := r.writeFile(data); err != nil {
		return false, err
	}
	r.policy = data
	r.log.Info("egress policy written", "networks", len(p.Networks))
	return true, nil
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
	ok := r.handOver(ctx, ref)
	if !ok {
		r.act("remove failed start", ref, r.docker.remove(ctx, name))
	}
	return ok
}

// handOver attaches to the created container's stdin, starts it, and
// writes a credential minted only once the agent is up: each one is
// one-time, and one minted for an agent that never runs is wasted. The
// stream is closed on every path.
func (r *Reconciler) handOver(ctx context.Context, ref string) bool {
	name := containerName(ref)
	stdin, err := r.docker.attach(ctx, ref)
	if !r.act("attach", ref, err) {
		return false
	}
	defer stdin.close()
	if !r.act("start", ref, r.docker.start(ctx, name)) {
		return false
	}
	c, err := r.docker.inspect(ctx, name)
	if err == nil && !c.running {
		err = errors.New("it exited before its credential was sent")
	}
	if !r.act("inspect started", ref, err) {
		return false
	}
	credential, err := r.server.Credential(ctx, ref)
	if err != nil {
		err = fmt.Errorf("the server did not mint an agent credential: %w", err)
	} else {
		err = stdin.send(credential)
	}
	return r.act("hand over credential", ref, err)
}

// removeAgent stops and removes ref's container, and reports whether it
// is gone. why is added to the logged actions.
func (r *Reconciler) removeAgent(ctx context.Context, ref, why string) bool {
	name := containerName(ref)
	return r.act("stop"+why, ref, r.docker.stop(ctx, name)) && r.act("remove"+why, ref, r.docker.remove(ctx, name))
}

// removeNetwork takes the egress proxy off ref's network and removes it.
// Its subnet is quarantined before another owner gets it.
func (r *Reconciler) removeNetwork(ctx context.Context, ref string, subnets map[string]netip.Prefix) {
	if r.act("disconnect egress", ref, r.docker.disconnectEgress(ctx, ref)) &&
		r.act("remove network", ref, r.docker.removeNetwork(ctx, ref)) {
		r.freed[subnets[ref]] = r.now()
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
