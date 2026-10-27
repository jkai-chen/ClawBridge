package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestReconciler(s *testStack, server *fakeServer) *Reconciler {
	cfg := Config{Spec: testSpec(s.policyDir), LauncherToken: "cbl_" + strings.Repeat("x", 24), Docker: s.proxy}
	r := NewReconciler(cfg, server, s.logger)
	r.sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

// state is every container's state and every network's subnet and
// members.
func (f *fakeDaemon) state() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for name, state := range f.containers {
		out[name] = state
	}
	for name, n := range f.networks {
		members := []string{}
		for m := range n.members {
			members = append(members, m)
		}
		slices.Sort(members)
		out[name] = n.subnet + " " + strings.Join(members, ",")
	}
	return out
}

// readPolicy is the egress policy agentd wrote.
func (s *testStack) readPolicy(t *testing.T) EgressPolicy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.policyDir, policyFile))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseEgressPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	subnet0 = "172.30.0.0/28"
	subnet1 = "172.30.0.16/28"
	subnet2 = "172.30.0.32/28"
	egressN = "172.31.255.0/28 "
)

// agentd's own calls all pass the proxy: the egress proxy and its network
// first, then the owner's network on a fixed subnet with the egress proxy
// connected, the policy allowing it, and only then the agent's container,
// attached before it starts. The credential arrives on stdin, then EOF, and
// is never logged.
func TestReconcileStartsAnAgentThroughTheProxy(t *testing.T) {
	s := newStack(t)
	name := containerName(refA)
	var policyAtCreate EgressPolicy
	s.daemon.onCreate = func(created string) bool {
		if created == name {
			data, _ := os.ReadFile(filepath.Join(s.policyDir, policyFile))
			_ = json.Unmarshal(data, &policyAtCreate)
		}
		return false
	}
	server := &fakeServer{refs: []string{refA}, max: 2}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err, s.log.String())
	}
	want := map[string]string{name: "running", networkName(refA): subnet0 + " " + egressName,
		egressName: "running", egressNetName: egressN}
	if got := s.daemon.state(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n%s", got, s.log.String())
	}
	s.daemon.mu.Lock()
	bodies := map[string]string{}
	for k, v := range s.daemon.bodies {
		bodies[k] = string(v)
	}
	s.daemon.mu.Unlock()
	for created, spec := range map[string]any{
		name:              agentSpec(testImage, testServer, refA, 1),
		egressName:        egressSpec(testEgressImage, s.policyDir),
		networkName(refA): agentNetwork(refA, netip.MustParsePrefix(subnet0)),
		egressNetName:     egressNetSpec(),
	} {
		if got := bodies[created]; got != string(mustJSON(t, spec)) {
			t.Fatalf("%s created as %s", created, got)
		}
	}
	wantPolicy := EgressPolicy{Networks: []EgressNetwork{{OwnerRef: refA, Source: subnet0, Allow: []string{"bridge.example.test:443", "api.provider.test:443"}}}}
	if !reflect.DeepEqual(policyAtCreate, wantPolicy) || !reflect.DeepEqual(s.readPolicy(t), wantPolicy) {
		t.Fatalf("policy at create %v, now %v", policyAtCreate, s.readPolicy(t))
	}
	// The daemon reads stdin on its own after agentd's write returns.
	var stdin string
	for deadline := time.Now().Add(5 * time.Second); stdin == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.daemon.mu.Lock()
		stdin = s.daemon.stdin[name]
		s.daemon.mu.Unlock()
	}
	if stdin != "cba_"+refA+"_secretcredential\n" {
		t.Fatalf("stdin %q", stdin)
	}
	var order []string
	for _, r := range s.daemon.seen() {
		for _, step := range []string{"/networks/cbnet-" + refA + "/connect", "/containers/create?name=" + name, name + "/attach", name + "/start"} {
			if strings.Contains(r, step) {
				order = append(order, step)
			}
		}
	}
	if len(order) != 4 || !strings.HasSuffix(order[0], "/connect") || !strings.HasSuffix(order[3], "/start") || !strings.HasSuffix(order[2], "/attach") {
		t.Fatalf("order %v", order)
	}
	if strings.Contains(s.log.String(), "secretcredential") || strings.Contains(s.log.String(), `"refused"`) {
		t.Fatalf("log: %s", s.log.String())
	}
}

// Owners get the first free subnets, the policy follows the wanted set, a
// stopped owner's network is emptied and removed, and its subnet is reused.
func TestReconcileAllocatesSubnetsAndWritesThePolicy(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA, refB}, max: 4, providers: map[string]string{refB: "Bridge.Example.Test:443"}}
	r := newTestReconciler(s, server)
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.daemon.state()
	if got[networkName(refA)] != subnet0+" "+egressName || got[networkName(refB)] != subnet1+" "+egressName {
		t.Fatalf("%v", got)
	}
	// B's provider is the server itself: listed once.
	want := EgressPolicy{Networks: []EgressNetwork{
		{OwnerRef: refA, Source: subnet0, Allow: []string{"bridge.example.test:443", "api.provider.test:443"}},
		{OwnerRef: refB, Source: subnet1, Allow: []string{"bridge.example.test:443"}},
	}}
	if p := s.readPolicy(t); !reflect.DeepEqual(p, want) {
		t.Fatalf("%v", p)
	}
	// A leaves. Its subnet is quarantined, so C, starting in the same
	// round, gets the next one.
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	server.refs = []string{refB, refC}
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = s.daemon.state()
	if _, ok := got[networkName(refA)]; ok || got[networkName(refC)] != subnet2+" "+egressName {
		t.Fatalf("%v", got)
	}
	if p := s.readPolicy(t); len(p.Networks) != 2 || p.Networks[1].OwnerRef != refC || p.Networks[1].Source != subnet2 {
		t.Fatalf("%v", p)
	}
	// A minute later the subnet is free again, and the next owner gets it.
	now = now.Add(subnetQuarantine)
	server.refs = []string{refB, refC, "p_dddddddd"}
	server.max = 4
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state()[networkName("p_dddddddd")]; got != subnet0+" "+egressName {
		t.Fatalf("%v", s.daemon.state())
	}
	// Unchanged rounds don't rewrite the file.
	before := strings.Count(s.log.String(), "egress policy written")
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(s.log.String(), "egress policy written") != before {
		t.Fatal("the policy was rewritten without a change")
	}
}

// A recreated egress proxy has left every network: agentd reconnects it to
// each live owner's network.
func TestReconcileReconnectsARecreatedEgress(t *testing.T) {
	s := newStack(t)
	r := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2})
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.daemon.mu.Lock()
	s.daemon.containers[egressName] = "exited"
	s.daemon.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[egressName] != "running" || got[networkName(refA)] != subnet0+" "+egressName || got[containerName(refA)] != "running" {
		t.Fatalf("%v", got)
	}
}

// Without the egress proxy, no agent starts.
func TestReconcileNeedsTheEgressProxy(t *testing.T) {
	s := newStack(t)
	s.daemon.onCreate = func(name string) bool { return name == egressName }
	if err := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2}).Step(context.Background()); err == nil {
		t.Fatal("a round without the egress proxy succeeded")
	}
	if _, ok := s.daemon.state()[containerName(refA)]; ok {
		t.Fatal("an agent started without the egress proxy")
	}
}

// Unwanted agents are stopped and removed with their network, the egress
// proxy disconnected first; containers with other names are never touched.
func TestReconcileStopsUnwantedAgentsOnly(t *testing.T) {
	s := newStack(t)
	s.daemon.containers = map[string]string{containerName(refA): "running", containerName(refB): "running", "postgres": "running",
		"clawbridge-agent-p_aaaaaaaa-extra": "running", egressName: "running"}
	s.daemon.networks = map[string]*fakeNet{
		egressNetName:     {subnet: "172.31.255.0/28", members: map[string]bool{}},
		networkName(refA): {subnet: subnet0, members: map[string]bool{egressName: true}},
		networkName(refB): {subnet: subnet1, members: map[string]bool{egressName: true}},
	}
	if err := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2}).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{containerName(refA): "running", networkName(refA): subnet0 + " " + egressName, "postgres": "running",
		"clawbridge-agent-p_aaaaaaaa-extra": "running", egressName: "running", egressNetName: egressN}
	if got := s.daemon.state(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n%s", got, s.log.String())
	}
	for _, r := range s.daemon.seen() {
		if strings.Contains(r, "postgres") || strings.Contains(r, "-extra") {
			t.Fatalf("touched %s", r)
		}
	}
}

// The server says which refs run and their provider, never how, and both
// are validated: a bad one is refused and the others still start.
func TestReconcileRefusesBadRefs(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{"P_AAAAAAAA", "p_a", "../postgres", "p_aaaaaaaa --privileged", refA, refB, refC, "p_dddddddd"}, max: 8,
		providers: map[string]string{refB: "10.0.0.1:443", refC: "https://api.provider.test", "p_dddddddd": "*.provider.test:443"}}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.daemon.state()
	for _, ref := range []string{refB, refC, "p_dddddddd"} {
		if _, ok := got[containerName(ref)]; ok {
			t.Fatalf("%s started: %v", ref, got)
		}
	}
	if got[containerName(refA)] != "running" || !slices.Equal(server.minted, []string{refA}) {
		t.Fatalf("%v %v", got, server.minted)
	}
	if strings.Contains(s.log.String(), "postgres") || strings.Contains(s.log.String(), "10.0.0.1") {
		t.Fatalf("a server value was echoed: %s", s.log.String())
	}
	for name, server := range map[string]*fakeServer{
		"no cap":     {refs: []string{refA}, max: 0},
		"huge cap":   {refs: []string{refA}, max: 1000},
		"server off": {wantedErr: errors.New("down"), max: 2},
	} {
		if newTestReconciler(newStack(t), server).Step(context.Background()) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Never more than the cap, counted from Docker's own list.
func TestReconcileKeepsToTheCap(t *testing.T) {
	s := newStack(t)
	r := newTestReconciler(s, &fakeServer{refs: []string{refC}, max: 2})
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := &fakeServer{refs: []string{refA, refB, refC}, max: 2}
	r.server = server
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refA)] != "running" || got[containerName(refC)] != "running" || got[containerName(refB)] != "" {
		t.Fatalf("%v", got)
	}
	if p := s.readPolicy(t); len(p.Networks) != 2 {
		t.Fatalf("the policy names agents over the cap: %v", p)
	}
	// A lowered cap stops the extra agent.
	server.max = 1
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	running := 0
	for name, state := range s.daemon.state() {
		if strings.HasPrefix(name, containerPrefix) && state == "running" {
			running++
		}
	}
	if running != 1 || len(s.readPolicy(t).Networks) != 1 {
		t.Fatalf("%v %v", s.daemon.state(), s.readPolicy(t))
	}
}

// A crashed agent is restarted at most 3 times an hour, then reported as
// agent_crashloop once; an hour later it is tried again.
func TestReconcileStopsACrashloop(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA}, max: 2}
	r := newTestReconciler(s, server)
	now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	crash := func() {
		s.daemon.mu.Lock()
		s.daemon.containers[containerName(refA)] = "exited"
		s.daemon.mu.Unlock()
	}
	for range 5 {
		if err := r.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		crash()
		now = now.Add(time.Minute)
	}
	if len(server.minted) != 4 || !slices.Equal(server.reports, []string{refA + " " + codeCrashloop}) {
		t.Fatalf("minted %v reports %v", server.minted, server.reports)
	}
	now = now.Add(time.Hour)
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.minted) != 5 {
		t.Fatalf("not retried: %v", server.minted)
	}
}

// A start that fails after the create leaves no container behind, and the
// credential is not minted unless the container started.
func TestReconcileCleansUpAFailedStart(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA}, max: 2, mintErr: errors.New("no")}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.daemon.state()[containerName(refA)]; ok {
		t.Fatalf("left behind: %v", s.daemon.state())
	}
	if !strings.Contains(s.log.String(), "the server did not mint an agent credential") {
		t.Fatal(s.log.String())
	}
}

// The pool holds 4,096 owner subnets; the first free one is used.
func TestFreeSubnet(t *testing.T) {
	used := map[string]netip.Prefix{}
	if p, ok := freeSubnet(used, nil); !ok || p.String() != subnet0 {
		t.Fatal(p, ok)
	}
	base := subnetPool.Addr().As4()
	for i := range 4096 {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{base[0], base[1], byte(i >> 4), byte(i << 4)}), subnetBits)
		if _, ok := validAgentSubnet(p.String()); !ok {
			t.Fatalf("%s is not a pool /28", p)
		}
		used[p.String()] = p
	}
	if p, ok := freeSubnet(used, nil); ok {
		t.Fatalf("a 4097th subnet: %s", p)
	}
	delete(used, "172.30.200.48/28")
	if p, ok := freeSubnet(used, nil); !ok || p.String() != "172.30.200.48/28" {
		t.Fatal(p, ok)
	}
	// A quarantined subnet is not handed out.
	if p, ok := freeSubnet(used, map[netip.Prefix]time.Time{netip.MustParsePrefix("172.30.200.48/28"): {}}); ok {
		t.Fatalf("a quarantined subnet: %s", p)
	}
}

// A new config revision restarts the owner's agent in a fresh instance with
// a fresh credential (§11.6); its network stays, and a rotation is not a
// crash. An agentd restart with the same revision restarts nothing.
func TestReconcileRestartsOnANewRevision(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA}, max: 2, revisions: map[string]int64{refA: 1}}
	r := newTestReconciler(s, server)
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for revision := int64(2); revision <= 6; revision++ {
		server.revisions[refA] = revision
		if err := r.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.daemon.mu.Lock()
		var created struct{ Labels map[string]string }
		_ = json.Unmarshal(s.daemon.bodies[containerName(refA)], &created)
		state := s.daemon.containers[containerName(refA)]
		s.daemon.mu.Unlock()
		if state != "running" || created.Labels[revisionLabel] != strconv.FormatInt(revision, 10) {
			t.Fatalf("revision %d: %s %v", revision, state, created.Labels)
		}
	}
	// Five rotations in a few seconds: five fresh credentials, no crash loop,
	// and the same network throughout.
	if len(server.minted) != 6 || len(server.reports) != 0 {
		t.Fatalf("minted %v reports %v", server.minted, server.reports)
	}
	if got := s.daemon.state()[networkName(refA)]; got != subnet0+" "+egressName {
		t.Fatalf("network %q", got)
	}
	again := newTestReconciler(s, server)
	if err := again.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.minted) != 6 {
		t.Fatalf("a restarted agentd restarted the agent: %v", server.minted)
	}
	// An owner without a revision is refused.
	server.revisions[refA] = 0
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.daemon.state()[containerName(refA)]; ok {
		t.Fatal("an owner without a config revision still runs")
	}
}

// running is how many agent containers Docker runs.
func (f *fakeDaemon) running() int {
	n := 0
	for name, state := range f.state() {
		if strings.HasPrefix(name, containerPrefix) && state == "running" {
			n++
		}
	}
	return n
}

// A failed stop is followed by a forced removal. Only an agent that can be
// neither stopped nor removed still runs: it counts toward the cap, and
// nothing starts in that round. A stopped agent that couldn't be removed
// doesn't run, so it doesn't count [R86-M1, R94-F1].
func TestReconcileCountsAFailedStopTowardTheCap(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA, refB}, max: 2}
	r := newTestReconciler(s, server)
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	set := func(failStop, failRemove []string) {
		s.daemon.mu.Lock()
		defer s.daemon.mu.Unlock()
		s.daemon.failStop, s.daemon.failRemove = map[string]bool{}, map[string]bool{}
		for _, ref := range failStop {
			s.daemon.failStop[containerName(ref)] = true
		}
		for _, ref := range failRemove {
			s.daemon.failRemove[containerName(ref)] = true
		}
	}
	// The stop fails, the forced removal ends it, and C starts.
	set([]string{refB}, nil)
	server.refs = []string{refA, refC}
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refB)] != "" || got[containerName(refC)] != "running" {
		t.Fatalf("%v", got)
	}
	// Neither works: C still runs, so B doesn't start.
	set([]string{refC}, []string{refC})
	server.refs = []string{refA, refB}
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refC)] != "running" || got[containerName(refB)] != "" || s.daemon.running() != 2 {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(s.log.String(), "starting none this round") {
		t.Fatal(s.log.String())
	}
	// A lowered cap with an agent that can't be stopped: the next in line is
	// stopped instead, never one more started.
	server.refs, server.max = []string{refA, refB, refC}, 1
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refA)] != "" || got[containerName(refB)] != "" || s.daemon.running() != 1 {
		t.Fatalf("%v", got)
	}
	// It stops but can't be removed: it no longer runs, so B starts.
	set(nil, []string{refC})
	server.refs, server.max = []string{refA, refB}, 2
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refC)] != "exited" || got[containerName(refA)] != "running" || got[containerName(refB)] != "running" {
		t.Fatalf("%v", got)
	}
	// Once it can be removed, it is.
	set(nil, nil)
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refC)] != "" {
		t.Fatalf("%v", got)
	}
}

// Failed starts back off, 1, 2, 4… rounds, and are reported once as
// agent_start_failing after three in a row; a start that works resets the
// count [R86-M2].
func TestReconcileBacksOffFailedStarts(t *testing.T) {
	s := newStack(t)
	failing := true
	s.daemon.onCreate = func(name string) bool { return failing && name == containerName(refA) }
	server := &fakeServer{refs: []string{refA}, max: 2}
	r := newTestReconciler(s, server)
	var tried []int
	for round := 1; round <= 16; round++ {
		if reported := len(server.reports) > 0; reported != (round > 4) { // the third failure is in round 4
			t.Fatalf("round %d: reports %v", round, server.reports)
		}
		before := strings.Count(strings.Join(s.daemon.seen(), "\n"), "/containers/create?name="+containerName(refA))
		if err := r.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Count(strings.Join(s.daemon.seen(), "\n"), "/containers/create?name="+containerName(refA)) > before {
			tried = append(tried, round)
		}
	}
	if !slices.Equal(tried, []int{1, 2, 4, 8, 16}) || !slices.Equal(server.reports, []string{refA + " " + codeStartFailing}) {
		t.Fatalf("tried in rounds %v, reports %v", tried, server.reports)
	}
	if _, ok := s.daemon.state()[containerName(refA)]; ok || len(server.minted) != 0 {
		t.Fatal("a failed start left a container or minted a credential")
	}
	failing = false
	for range 16 {
		if err := r.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if s.daemon.state()[containerName(refA)] != "running" || r.failed[refA] != nil {
		t.Fatalf("%v %v", s.daemon.state(), r.failed)
	}
}

// The one-time credential is minted only for an agent that is up: one that
// exits at once gets none, and its stdin is closed [R86-M2, R86-L1].
func TestReconcileMintsOnlyForARunningAgent(t *testing.T) {
	s := newStack(t)
	s.daemon.exitOnStart = map[string]bool{containerName(refA): true}
	server := &fakeServer{refs: []string{refA}, max: 2}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.minted) != 0 || s.daemon.state()[containerName(refA)] != "" {
		t.Fatalf("minted %v: %v", server.minted, s.daemon.state())
	}
	waitStdin(t, s, containerName(refA), "")
}

// waitStdin waits for the daemon to see the end of name's stdin, and
// checks what it read.
func waitStdin(t *testing.T, s *testStack, name, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.daemon.mu.Lock()
		got, ok := s.daemon.stdin[name]
		s.daemon.mu.Unlock()
		if ok {
			if got != want {
				t.Fatalf("stdin %q", got)
			}
			return
		}
	}
	t.Fatal("the attach stream was never closed")
}

// A failed mint closes the attach stream too [R86-L1], and only a 101
// opens it [R86-L2].
func TestReconcileClosesTheAttachStream(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA}, max: 2, mintErr: errors.New("no")}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStdin(t, s, containerName(refA), "")

	s = newStack(t)
	s.daemon.attachStatus = http.StatusOK
	server = &fakeServer{refs: []string{refA}, max: 2}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.daemon.state()[containerName(refA)]; ok || len(server.minted) != 0 || !strings.Contains(s.log.String(), "attach: status 200") {
		t.Fatalf("%v %v %s", s.daemon.state(), server.minted, s.log.String())
	}
}

// The attach stream outlives the proxy's read timeout, while the server
// mints the credential [R86-L4].
func TestReconcileHandsOverPastTheProxyReadTimeout(t *testing.T) {
	old := proxyReadTimeout
	proxyReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { proxyReadTimeout = old })
	s := newStack(t)
	server := &fakeServer{refs: []string{refA}, max: 2, mintDelay: 300 * time.Millisecond}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStdin(t, s, containerName(refA), "cba_"+refA+"_secretcredential\n")
}

// A network whose container is gone, of an owner no longer wanted, is
// removed, and a failed removal is retried the next round [R86-M4, R90-M2].
func TestReconcileRemovesLeftoverNetworks(t *testing.T) {
	s := newStack(t)
	s.daemon.containers = map[string]string{egressName: "running"}
	s.daemon.networks = map[string]*fakeNet{
		egressNetName:     {subnet: "172.31.255.0/28", members: map[string]bool{}},
		networkName(refA): {subnet: subnet0, members: map[string]bool{egressName: true}},
		networkName(refB): {subnet: subnet1, members: map[string]bool{egressName: true}},
	}
	s.daemon.failNetworkDelete = map[string]bool{networkName(refB): true}
	r := newTestReconciler(s, &fakeServer{max: 2})
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.daemon.state()
	if _, ok := got[networkName(refA)]; ok || got[networkName(refB)] != subnet1+" " {
		t.Fatalf("%v", got)
	}
	if _, ok := r.freed[netip.MustParsePrefix(subnet0)]; !ok {
		t.Fatal("the freed subnet is not quarantined")
	}
	s.daemon.mu.Lock()
	s.daemon.failNetworkDelete = nil
	s.daemon.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.daemon.state()[networkName(refB)]; ok {
		t.Fatalf("not retried: %v", s.daemon.state())
	}
}

// An owner network already there on another subnet is replaced, so the
// policy names the subnet the agent is really on [R90-M3].
func TestReconcileReplacesANetworkOnAnotherSubnet(t *testing.T) {
	s := newStack(t)
	s.daemon.networks = map[string]*fakeNet{networkName(refA): {subnet: "10.99.0.0/24", members: map[string]bool{}}}
	if err := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2}).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[networkName(refA)] != subnet0+" "+egressName || got[containerName(refA)] != "running" {
		t.Fatalf("%v", got)
	}
	if p := s.readPolicy(t); len(p.Networks) != 1 || p.Networks[0].Source != subnet0 {
		t.Fatalf("%v", p)
	}
	if !strings.Contains(s.log.String(), "not on its subnet") {
		t.Fatal(s.log.String())
	}
}

// A new egress image replaces the running egress proxy, which is
// reconnected to the owners' networks [R90-L4].
func TestReconcileReplacesAnOldEgressImage(t *testing.T) {
	s := newStack(t)
	r := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2})
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.daemon.mu.Lock()
	s.daemon.bodies[egressName] = []byte(`{"Image":"ghcr.io/example/clawbridge-agentd@sha256:` + strings.Repeat("0", 64) + `"}`)
	s.daemon.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.daemon.mu.Lock()
	body := string(s.daemon.bodies[egressName])
	s.daemon.mu.Unlock()
	if body != string(mustJSON(t, egressSpec(testEgressImage, s.policyDir))) {
		t.Fatalf("not replaced: %s", body)
	}
	if got := s.daemon.state(); got[networkName(refA)] != subnet0+" "+egressName || got[containerName(refA)] != "running" {
		t.Fatalf("%v", got)
	}
}

// Without the egress proxy no agent starts, but unwanted agents are still
// stopped [R90-L4].
func TestReconcileStopsAgentsWithoutTheEgressProxy(t *testing.T) {
	s := newStack(t)
	s.daemon.containers = map[string]string{containerName(refA): "running"}
	s.daemon.onCreate = func(name string) bool { return name == egressName }
	if err := newTestReconciler(s, &fakeServer{refs: []string{refB}, max: 2}).Step(context.Background()); err == nil {
		t.Fatal("a round without the egress proxy succeeded")
	}
	if got := s.daemon.state(); got[containerName(refA)] != "" || got[containerName(refB)] != "" {
		t.Fatalf("%v", got)
	}
}

// A new agent starts only after the egress proxy can have reread a changed
// policy; an unchanged policy needs no wait [R90-M1].
func TestReconcileWaitsForThePolicyBeforeStarting(t *testing.T) {
	if policySettle <= egressPoll {
		t.Fatalf("policySettle %v is not longer than the egress poll %v", policySettle, egressPoll)
	}
	s := newStack(t)
	r := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2})
	var waits []string
	r.sleep = func(_ context.Context, d time.Duration) error {
		_, started := s.daemon.state()[containerName(refA)]
		waits = append(waits, d.String()+" "+strconv.FormatBool(started))
		return nil
	}
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	// It crashes; its restart is on an unchanged policy.
	s.daemon.mu.Lock()
	s.daemon.containers[containerName(refA)] = "exited"
	s.daemon.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(waits, []string{policySettle.String() + " false"}) || s.daemon.state()[containerName(refA)] != "running" {
		t.Fatalf("waits %v", waits)
	}
}

// A network on the wrong subnet with a container on it is left alone and
// logged, not torn down and failed again every round [R94-F2].
func TestReconcileKeepsAMisplacedNetworkWithAContainer(t *testing.T) {
	s := newStack(t)
	s.daemon.containers = map[string]string{containerName(refA): "running"}
	s.daemon.bodies[containerName(refA)] = mustJSON(t, agentSpec(testImage, testServer, refA, 1))
	s.daemon.networks = map[string]*fakeNet{networkName(refA): {subnet: "10.99.0.0/24", members: map[string]bool{containerName(refA): true}}}
	r := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2})
	for range 2 {
		if err := r.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range s.daemon.seen() {
		if strings.Contains(q, "DELETE /networks/") || strings.Contains(q, "/networks/"+networkName(refA)+"/disconnect") {
			t.Fatalf("tore down a network under its agent: %s", q)
		}
	}
	if got := s.daemon.state(); got[containerName(refA)] != "running" || !strings.Contains(s.log.String(), "a container is on it") {
		t.Fatalf("%v %s", got, s.log.String())
	}
	if p := s.readPolicy(t); len(p.Networks) != 0 {
		t.Fatalf("the policy names a subnet the agent isn't on: %v", p)
	}
}

// A new egress image that isn't loaded leaves the old egress proxy running
// rather than removing it for nothing [R94-F3].
func TestReconcileKeepsTheEgressWhenTheNewImageIsMissing(t *testing.T) {
	s := newStack(t)
	r := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2})
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"Image":"ghcr.io/example/clawbridge-agentd@sha256:` + strings.Repeat("0", 64) + `"}`)
	s.daemon.mu.Lock()
	s.daemon.bodies[egressName] = old
	s.daemon.missingImages = map[string]bool{testEgressImage: true}
	s.daemon.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.daemon.mu.Lock()
	body := string(s.daemon.bodies[egressName])
	s.daemon.mu.Unlock()
	if body != string(old) || s.daemon.state()[egressName] != "running" || !strings.Contains(s.log.String(), "the new egress image is not loaded") {
		t.Fatalf("%s %v", body, s.daemon.state())
	}
}

// The policy lists a provider's compiled-in companion beside it (Q3), and
// the private server when the operator listed one (Q2).
func TestReconcileWritesCompanionsAndThePrivateServer(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA, refB, refC}, max: 4, providers: map[string]string{
		refA: "bedrock-runtime.eu-west-1.amazonaws.com:443",
		refB: "europe-west4-aiplatform.googleapis.com:443",
		refC: "acme.openai.azure.com:443",
	}}
	r := newTestReconciler(s, server)
	r.cfg.PrivateServer = "bridge.example.test:443"
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err, s.log.String())
	}
	p := s.readPolicy(t)
	want := map[string][]string{
		refA: {"bridge.example.test:443", "bedrock-runtime.eu-west-1.amazonaws.com:443", "sts.eu-west-1.amazonaws.com:443"},
		refB: {"bridge.example.test:443", "europe-west4-aiplatform.googleapis.com:443", "oauth2.googleapis.com:443"},
		refC: {"bridge.example.test:443", "acme.openai.azure.com:443", "login.microsoftonline.com:443"},
	}
	if len(p.Networks) != 3 || p.PrivateServer != "bridge.example.test:443" {
		t.Fatalf("%+v", p)
	}
	for _, n := range p.Networks {
		if !slices.Equal(n.Allow, want[n.OwnerRef]) {
			t.Fatalf("%s: %v", n.OwnerRef, n.Allow)
		}
	}
}

func TestCompanionOf(t *testing.T) {
	for provider, want := range map[string]string{
		"aiplatform.googleapis.com:443":                         "oauth2.googleapis.com:443",
		"us-central1-aiplatform.googleapis.com:443":             "oauth2.googleapis.com:443",
		"bedrock-runtime.us-east-1.amazonaws.com:443":           "sts.us-east-1.amazonaws.com:443",
		"bedrock-runtime-fips.us-gov-west-1.amazonaws.com:443":  "sts.us-gov-west-1.amazonaws.com:443",
		"acme.cognitiveservices.azure.com:443":                  "login.microsoftonline.com:443",
		"api.anthropic.com:443":                                 "",
		"notaiplatform.googleapis.com:443":                      "",
		"x.acme.openai.azure.com:443":                           "",
		"generativelanguage.googleapis.com:443":                 "",
		"aiplatform.googleapis.com.evil.test:443":               "",
		"aiplatform.googleapis.com:8443":                        "",
		"bedrock-runtime.evil.amazonaws.com:443":                "",
		"bedrock-runtime.us-east-1.amazonaws.com.evil.test:443": "",
		"acme.openai.azure.com.evil.test:443":                   "",
	} {
		if got := companionOf(provider); got != want {
			t.Fatalf("%s: %q, not %q", provider, got, want)
		}
	}
}
