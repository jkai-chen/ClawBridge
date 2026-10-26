package agentd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestReconciler(s *testStack, server *fakeServer) *Reconciler {
	cfg := Config{ServerURL: testServer, Image: testImage, LauncherToken: "cbl_" + strings.Repeat("x", 24), Docker: s.proxy}
	return NewReconciler(cfg, server, s.logger)
}

func (f *fakeDaemon) state() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for name, state := range f.containers {
		out[name] = state
	}
	for name := range f.networks {
		out[name] = "network"
	}
	return out
}

// agentd's own calls all pass the proxy: a wanted owner gets its internal
// network and a container from the fixed spec, attached before it starts,
// and the credential arrives on stdin, then EOF. It is never logged.
func TestReconcileStartsAnAgentThroughTheProxy(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{refA}, max: 2}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	name := containerName(refA)
	if got := s.daemon.state(); !reflect.DeepEqual(got, map[string]string{name: "running", networkName(refA): "network"}) {
		t.Fatalf("%v\n%s", got, s.log.String())
	}
	if got, want := string(s.daemon.bodies[name]), string(mustJSON(t, agentSpec(testImage, testServer, refA))); got != want {
		t.Fatalf("created %s", got)
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
	seen := s.daemon.seen()
	order := []string{}
	for _, r := range seen {
		for _, step := range []string{"/networks/create", "/containers/create", "/attach", "/start"} {
			if strings.Contains(r, step) {
				order = append(order, step)
			}
		}
	}
	if !slices.Equal(order, []string{"/networks/create", "/containers/create", "/attach", "/start"}) {
		t.Fatalf("order %v", seen)
	}
	if strings.Contains(s.log.String(), "secretcredential") || strings.Contains(s.log.String(), `"refused"`) {
		t.Fatalf("log: %s", s.log.String())
	}
}

// Unwanted agents are stopped and removed with their network; containers
// with other names are never touched.
func TestReconcileStopsUnwantedAgentsOnly(t *testing.T) {
	s := newStack(t)
	s.daemon.containers = map[string]string{containerName(refA): "running", containerName(refB): "running", "postgres": "running",
		"clawbridge-agent-p_aaaaaaaa-extra": "running"}
	s.daemon.networks = map[string]bool{networkName(refA): true, networkName(refB): true}
	if err := newTestReconciler(s, &fakeServer{refs: []string{refA}, max: 2}).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{containerName(refA): "running", networkName(refA): "network", "postgres": "running",
		"clawbridge-agent-p_aaaaaaaa-extra": "running"}
	if got := s.daemon.state(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	for _, r := range s.daemon.seen() {
		if strings.Contains(r, "postgres") || strings.Contains(r, "-extra") {
			t.Fatalf("touched %s", r)
		}
	}
}

// The server says which refs run, never how, and its refs are validated:
// one that is not an owner ref is refused and the others still start.
func TestReconcileRefusesBadRefs(t *testing.T) {
	s := newStack(t)
	server := &fakeServer{refs: []string{"P_AAAAAAAA", "p_a", "../postgres", "p_aaaaaaaa --privileged", refA}, max: 4}
	if err := newTestReconciler(s, server).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); len(got) != 2 || got[containerName(refA)] != "running" {
		t.Fatalf("%v", got)
	}
	if !slices.Equal(server.minted, []string{refA}) || strings.Contains(s.log.String(), "postgres") {
		t.Fatalf("%v %s", server.minted, s.log.String())
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
	s.daemon.containers = map[string]string{containerName(refC): "running"}
	server := &fakeServer{refs: []string{refA, refB, refC}, max: 2}
	r := newTestReconciler(s, server)
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.daemon.state(); got[containerName(refA)] != "running" || got[containerName(refC)] != "running" || got[containerName(refB)] != "" {
		t.Fatalf("%v", got)
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
	if running != 1 {
		t.Fatalf("%v", s.daemon.state())
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
