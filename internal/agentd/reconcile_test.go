package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestReconciler(s *testStack, server *fakeServer) *Reconciler {
	cfg := Config{Spec: testSpec(s.policyDir), LauncherToken: "cbl_" + strings.Repeat("x", 24), Docker: s.proxy}
	return NewReconciler(cfg, server, s.logger)
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
		name:              agentSpec(testImage, testServer, refA),
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
	// A leaves; C takes its subnet.
	server.refs = []string{refB, refC}
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = s.daemon.state()
	if _, ok := got[networkName(refA)]; ok || got[networkName(refC)] != subnet0+" "+egressName {
		t.Fatalf("%v", got)
	}
	if p := s.readPolicy(t); len(p.Networks) != 2 || p.Networks[1].OwnerRef != refC || p.Networks[1].Source != subnet0 {
		t.Fatalf("%v", p)
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
	if p, ok := freeSubnet(used); !ok || p.String() != subnet0 {
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
	if p, ok := freeSubnet(used); ok {
		t.Fatalf("a 4097th subnet: %s", p)
	}
	delete(used, "172.30.200.48/28")
	if p, ok := freeSubnet(used); !ok || p.String() != "172.30.200.48/28" {
		t.Fatal(p, ok)
	}
}
