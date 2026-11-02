package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/sessions"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rtServer is a fake ClawBridge server: it mints instances, wants the stored
// one for an existing name, and records every request.
type rtServer struct {
	mu        sync.Mutex
	calls     []bus.Request
	instances map[string]string
	rotated   []string
	n         int
}

func newRTServer() *rtServer { return &rtServer{instances: map[string]string{}} }

func (s *rtServer) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r)
	switch r.Operation {
	case "register_session":
		cur, exists := s.instances[r.Session]
		if resume, _ := r.Args["resume"].(bool); resume {
			s.n++
			s.rotated = append(s.rotated, r.Session)
			s.instances[r.Session] = fmt.Sprintf("rotated-%d", s.n)
			return bus.Result{Data: map[string]any{"instance": s.instances[r.Session]}}, nil
		}
		if exists {
			if r.Args["instance"] != cur {
				return bus.Result{}, errors.New("the session exists: give its current instance")
			}
			return bus.Result{Data: map[string]any{"instance": cur}}, nil
		}
		s.n++
		s.instances[r.Session] = fmt.Sprintf("minted-%d", s.n)
		return bus.Result{Data: map[string]any{"instance": s.instances[r.Session]}}, nil
	case "retire_session":
		delete(s.instances, r.Session)
	}
	return bus.Result{Source: "local-authority", Data: map[string]any{"ok": true}}, nil
}

func (s *rtServer) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, c.Operation+":"+c.Session)
	}
	return out
}

// callAt returns the i-th recorded request (negative counts from the end) under the lock, so a test can read it
// while the queue's goroutine is still executing.
func (s *rtServer) callAt(i int) bus.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 {
		i += len(s.calls)
	}
	return s.calls[i]
}

func (s *rtServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// rtEnv is one test's user config dir, homes and servers.
type rtEnv struct {
	t      *testing.T
	homes  map[string]*rtServer
	opened []string
	mu     sync.Mutex
	source string
}

func newRTEnv(t *testing.T) *rtEnv {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("HOME", cfg)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cfg, "xdg"))
	t.Setenv("APPDATA", filepath.Join(cfg, "appdata"))
	old := linkWait
	linkWait = 80 * time.Millisecond
	t.Cleanup(func() { linkWait = old })
	return &rtEnv{t: t, homes: map[string]*rtServer{}, source: ".clawbridge.toml at /repo"}
}

// home makes a home with its own fake server.
func (e *rtEnv) home(name string) string {
	h := filepath.Join(e.t.TempDir(), name)
	e.homes[h] = newRTServer()
	return h
}

func (e *rtEnv) dir(home string) string {
	d, err := sessions.RecordsDir(home, "ws", "proj")
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *rtEnv) openedHomes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.opened...)
}

func conv(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }

func fakeAgent(n int) sessions.ProcID { return sessions.ProcID{PID: 4_000_000 + n, Start: int64(n)} }

// writeLink writes the link for agent.
func (e *rtEnv) writeLink(agent sessions.ProcID, c string, mod func(*sessions.Link)) {
	l := sessions.Link{Agent: agent, Kind: sessions.KindClaudeCode, ConversationID: c, CWD: "/work/web"}
	if mod != nil {
		mod(&l)
	}
	if err := sessions.WriteLink(l); err != nil {
		e.t.Fatal(err)
	}
}

// record makes a released record with its instance known to the server.
func (e *rtEnv) record(home, name, instance string, convs ...string) {
	dir := e.dir(home)
	owner := fakeAgent(900)
	for i, c := range convs {
		if _, err := sessions.Claim(dir, name, c, owner, instance, sessions.Hint{}); err != nil {
			e.t.Fatalf("claim %d: %v", i, err)
		}
	}
	if len(convs) == 0 {
		if _, err := sessions.Claim(dir, name, "", owner, instance, sessions.Hint{}); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := sessions.Release(dir, name, owner); err != nil {
		e.t.Fatal(err)
	}
	if instance != "" {
		e.homes[home].instances[name] = instance
	}
}

type rtClient struct {
	e    *rtEnv
	rt   *runtime
	cs   *mcp.ClientSession
	done func()
}

// start builds a runtime server for agent and connects a client to it.
func (e *rtEnv) start(defaultHome string, agent sessions.ProcID, mod func(*RuntimeOptions)) *rtClient {
	e.t.Helper()
	o := RuntimeOptions{
		DefaultHome: defaultHome, Folder: "web",
		Open: func(_ context.Context, home string) (*RuntimeRoute, error) {
			e.mu.Lock()
			e.opened = append(e.opened, home)
			e.mu.Unlock()
			srv, ok := e.homes[home]
			if !ok {
				return nil, errors.New("no profile in " + home)
			}
			return &RuntimeRoute{Store: srv, Workspace: "ws", Project: "proj", ProjectSource: e.source}, nil
		},
	}
	if mod != nil {
		mod(&o)
	}
	rt := newRuntime(o)
	rt.walk = func(has func(sessions.ProcID) bool) (sessions.ProcID, bool) {
		if has(agent) {
			return agent, true
		}
		return sessions.ProcID{}, false
	}
	rt.direct = func() (sessions.ProcID, bool) { return agent, true }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	server, done := newRuntimeServer(rt, Options{Context: ctx})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close(); ss.Close(); cancel() })
	return &rtClient{e: e, rt: rt, cs: cs, done: done}
}

func (c *rtClient) call(name string, args map[string]any) (string, bool) {
	c.e.t.Helper()
	r, err := c.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.e.t.Fatal(err)
	}
	var sb strings.Builder
	for _, x := range r.Content {
		if t, ok := x.(*mcp.TextContent); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String(), r.IsError
}

func TestRuntimeStartsWithoutLinkOrProfile(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	c := e.start(home, fakeAgent(1), nil)
	tools, err := c.cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 21 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	if got := e.openedHomes(); len(got) != 0 {
		t.Fatalf("a profile was loaded for tools/list: %v", got)
	}
	if e.homes[home].count() != 0 {
		t.Fatal("the server was called")
	}
}

func TestRuntimeBoundOnFirstCallWithStoredInstance(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	e.record(home, "web", "stored-1", conv(1))
	agent := fakeAgent(1)
	e.writeLink(agent, conv(1), nil)
	if err := sessions.IndexSet(conv(1), home); err != nil {
		t.Fatal(err)
	}
	c := e.start("/nowhere", agent, nil)
	if msg, isErr := c.call("send", map[string]any{"to": "peer", "body": "hi", "operation_id": "op1"}); isErr {
		t.Fatalf("send failed on a bound session: %q", msg)
	}
	srv := e.homes[home]
	if srv.count() != 2 {
		t.Fatalf("calls: %v", srv.ops())
	}
	reg := srv.callAt(0)
	if reg.Operation != "register_session" || reg.Session != "web" || reg.Args["instance"] != "stored-1" {
		t.Fatalf("register: %+v", reg)
	}
	if _, has := reg.Args["resume"]; has || len(srv.rotated) != 0 {
		t.Fatalf("a re-bind rotated the instance: %+v", reg)
	}
	if s := srv.callAt(1); s.Operation != "send" || s.Session != "web" || s.Args["instance"] != "stored-1" {
		t.Fatalf("send: %+v", s)
	}
	rec, _ := sessions.Get(e.dir(home), "web")
	if rec.Owner == nil || *rec.Owner != agent {
		t.Fatalf("owner = %+v", rec.Owner)
	}
}

func TestRuntimeUnboundMessageAndWhoami(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	e.record(home, "web", "i", conv(9)) // the folder's name is taken
	agent := fakeAgent(1)
	e.writeLink(agent, conv(1), func(l *sessions.Link) { l.Home = home })
	c := e.start("/nowhere", agent, nil)
	msg, isErr := c.call("inbox", nil)
	if !isErr || !strings.Contains(msg, "this agent has no ClawBridge session yet: call register_session with a name (suggested: web-2)") {
		t.Fatalf("inbox: %q %v", msg, isErr)
	}
	who, isErr := c.call("whoami", nil)
	if isErr || !strings.Contains(who, "no ClawBridge session yet") || !strings.Contains(who, ".clawbridge.toml at /repo") ||
		!strings.Contains(who, `"project":"proj"`) || !strings.Contains(who, home) {
		t.Fatalf("whoami: %q %v", who, isErr)
	}
	if e.homes[home].count() != 0 {
		t.Fatalf("the server was called while unbound: %v", e.homes[home].ops())
	}
	// The link's home is used when the index has no entry.
	if got := e.openedHomes(); len(got) != 1 || got[0] != home {
		t.Fatalf("opened %v", got)
	}
}

func TestRuntimeClaimNewNameThenLiveOwnerRefusedBeforeServer(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	live := sessions.Self() // alive for the whole test
	e.writeLink(live, conv(1), nil)
	c1 := e.start(home, live, nil)
	out, isErr := c1.call("register_session", map[string]any{"name": "web"})
	if isErr || !strings.Contains(out, "minted-1") || !strings.Contains(out, ".clawbridge.toml at /repo") {
		t.Fatalf("claim: %q %v", out, isErr)
	}
	rec, err := sessions.Get(e.dir(home), "web")
	if err != nil || rec.Instance != "minted-1" || rec.Owner == nil || *rec.Owner != live ||
		len(rec.Conversations) != 1 || rec.Conversations[0] != conv(1) {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if h, ok := sessions.IndexGet(conv(1)); !ok || h != home {
		t.Fatalf("index = %q %v", h, ok)
	}
	// Another agent claims the same name while the first runs.
	other := fakeAgent(2)
	e.writeLink(other, conv(2), nil)
	before := e.homes[home].count()
	c2 := e.start(home, other, nil)
	msg, isErr := c2.call("register_session", map[string]any{"name": "web"})
	if !isErr || !strings.Contains(msg, "in use by another running agent") {
		t.Fatalf("second claim: %q %v", msg, isErr)
	}
	if got := e.homes[home].count(); got != before {
		t.Fatalf("the server heard of a refused claim: %v", e.homes[home].ops())
	}
}

func TestRuntimeRaceExistingNameNeverRotatedOrRetired(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	live := sessions.Self()
	dir := e.dir(home)
	if _, err := sessions.Claim(dir, "web", conv(1), live, "inst-owner", sessions.Hint{}); err != nil {
		t.Fatal(err)
	}
	e.homes[home].instances["web"] = "inst-owner"
	racer := fakeAgent(2)
	e.writeLink(racer, conv(2), func(l *sessions.Link) { l.AgentStartName = "web" })
	c := e.start(home, racer, nil)
	// The pre-check saw the owner dead; the re-check under the lock sees it alive.
	rec, _ := sessions.Get(dir, "web")
	c.rt.precheck = func(string, string, string, sessions.ProcID, sessions.Hint) (sessions.Record, bool, error) {
		return rec, true, nil
	}
	msg, isErr := c.call("register_session", map[string]any{"name": "web", "resume": true})
	if !isErr || !strings.Contains(msg, "in use by another running agent") {
		t.Fatalf("race: %q %v", msg, isErr)
	}
	srv := e.homes[home]
	if srv.count() != 1 {
		t.Fatalf("calls: %v", srv.ops())
	}
	reg := srv.callAt(0)
	if reg.Args["instance"] != "inst-owner" {
		t.Fatalf("existing name sent without its stored instance: %+v", reg)
	}
	if _, has := reg.Args["resume"]; has || len(srv.rotated) != 0 {
		t.Fatalf("resume reached the server: %+v", reg)
	}
	if srv.instances["web"] != "inst-owner" {
		t.Fatalf("the owner's instance changed: %q", srv.instances["web"])
	}
	after, _ := sessions.Get(dir, "web")
	if after.Owner == nil || *after.Owner != live || len(after.Conversations) != 1 {
		t.Fatalf("the owner's record changed: %+v", after)
	}
}

func TestRuntimeBrandNewNameRetiredWhenClaimFails(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	dir := e.dir(home)
	// A local record for another conversation, unknown to the server: the
	// pre-check is bypassed, the re-check under the lock refuses.
	if _, err := sessions.Claim(dir, "ghost", conv(1), fakeAgent(900), "", sessions.Hint{}); err != nil {
		t.Fatal(err)
	}
	_ = sessions.Release(dir, "ghost", fakeAgent(900))
	agent := fakeAgent(2)
	e.writeLink(agent, conv(2), nil)
	c := e.start(home, agent, nil)
	c.rt.precheck = func(string, string, string, sessions.ProcID, sessions.Hint) (sessions.Record, bool, error) {
		return sessions.Record{}, false, nil
	}
	msg, isErr := c.call("register_session", map[string]any{"name": "ghost"})
	if !isErr || !strings.Contains(msg, "belongs to another conversation") {
		t.Fatalf("claim: %q %v", msg, isErr)
	}
	srv := e.homes[home]
	if got := strings.Join(srv.ops(), ","); got != "register_session:ghost,retire_session:ghost" {
		t.Fatalf("calls: %s", got)
	}
	if _, still := srv.instances["ghost"]; still {
		t.Fatal("the name was not retired")
	}
}

func TestRuntimeEnvNameIsNotAHintButLinkStartNameIs(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	e.record(home, "web", "stored-1", conv(1))
	// CLAWBRIDGE_SESSION=web, a fresh conversation: no adoption.
	a1 := fakeAgent(2)
	e.writeLink(a1, conv(2), nil)
	c := e.start(home, a1, func(o *RuntimeOptions) { o.Suggest = "web" })
	before := e.homes[home].count()
	msg, isErr := c.call("register_session", map[string]any{"name": "web"})
	if !isErr || !strings.Contains(msg, "belongs to another conversation") {
		t.Fatalf("env name: %q %v", msg, isErr)
	}
	rec, _ := sessions.Get(e.dir(home), "web")
	if len(rec.Conversations) != 1 || e.homes[home].count() != before {
		t.Fatalf("the env name adopted a session: %+v", rec)
	}
	// The same name from `agent start NAME` (the link's field) adopts it.
	a2 := fakeAgent(3)
	e.writeLink(a2, conv(3), func(l *sessions.Link) { l.AgentStartName = "web" })
	c2 := e.start(home, a2, nil)
	if msg, isErr := c2.call("register_session", map[string]any{"name": "web"}); isErr {
		t.Fatalf("start name: %q", msg)
	}
	rec, _ = sessions.Get(e.dir(home), "web")
	if len(rec.Conversations) != 2 || rec.Owner == nil || *rec.Owner != a2 {
		t.Fatalf("not adopted: %+v", rec)
	}
	last := e.homes[home].callAt(-1)
	if last.Args["instance"] != "stored-1" || last.Args["resume"] != nil {
		t.Fatalf("adoption register: %+v", last)
	}
}

func TestRuntimeSuggestionFromEnvForRecordWithoutConversations(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	e.record(home, "pre", "i")
	agent := fakeAgent(1)
	e.writeLink(agent, conv(1), nil)
	c := e.start(home, agent, func(o *RuntimeOptions) { o.Suggest = "pre" })
	msg, _ := c.call("inbox", nil)
	if !strings.Contains(msg, "(suggested: pre)") {
		t.Fatalf("%q", msg)
	}
	if msg, isErr := c.call("register_session", map[string]any{"name": "pre"}); isErr {
		t.Fatalf("a record with no conversations is adoptable: %q", msg)
	}
}

func TestRuntimeNoLinkClaimsByPID(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	agent := fakeAgent(1)
	// No link is written for this agent.
	c := e.start(home, agent, func(o *RuntimeOptions) { o.EnvID = "env-diag-id" })
	out, isErr := c.call("register_session", map[string]any{"name": "web"})
	if isErr || !strings.Contains(out, "hooks aren't running here") {
		t.Fatalf("claim: %q %v", out, isErr)
	}
	rec, err := sessions.Get(e.dir(home), "web")
	if err != nil || rec.Instance != "minted-1" || rec.Owner == nil || *rec.Owner != agent ||
		len(rec.Conversations) != 0 || rec.EnvIDDiag != "env-diag-id" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if _, ok := sessions.IndexGet("env-diag-id"); ok {
		t.Fatal("the env ID was indexed")
	}
	if msg, isErr := c.call("send", map[string]any{"to": "p", "body": "b", "operation_id": "o"}); isErr {
		t.Fatalf("send after claim: %q", msg)
	}
}

func TestRuntimeTwoHomesResolveByIndex(t *testing.T) {
	e := newRTEnv(t)
	ha, hb := e.home("a"), e.home("b")
	e.record(ha, "alpha", "inst-a", conv(1))
	e.record(hb, "beta", "inst-b", conv(2))
	a1, a2 := fakeAgent(1), fakeAgent(2)
	e.writeLink(a1, conv(1), nil)
	e.writeLink(a2, conv(2), nil)
	_ = sessions.IndexSet(conv(1), ha)
	_ = sessions.IndexSet(conv(2), hb)
	def := e.home("default")
	c1 := e.start(def, a1, nil)
	c2 := e.start(def, a2, nil)
	for _, c := range []*rtClient{c1, c2} {
		if msg, isErr := c.call("inbox", nil); isErr {
			t.Fatalf("inbox: %q", msg)
		}
	}
	if got := e.homes[ha].callAt(0); got.Session != "alpha" || got.Args["instance"] != "inst-a" {
		t.Fatalf("home a: %+v", got)
	}
	if got := e.homes[hb].callAt(0); got.Session != "beta" || got.Args["instance"] != "inst-b" {
		t.Fatalf("home b: %+v", got)
	}
	if e.homes[def].count() != 0 {
		t.Fatalf("the default home was used: %v", e.homes[def].ops())
	}
	if ra, _ := sessions.Get(e.dir(ha), "alpha"); ra.Owner == nil || *ra.Owner != a1 {
		t.Fatalf("alpha: %+v", ra)
	}
	if rb, _ := sessions.Get(e.dir(hb), "beta"); rb.Owner == nil || *rb.Owner != a2 {
		t.Fatalf("beta: %+v", rb)
	}
}

func TestRuntimeReleasesOwnerOnExit(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	agent := fakeAgent(1)
	e.writeLink(agent, conv(1), nil)
	c := e.start(home, agent, nil)
	if msg, isErr := c.call("register_session", map[string]any{"name": "web"}); isErr {
		t.Fatal(msg)
	}
	if rec, _ := sessions.Get(e.dir(home), "web"); rec.Owner == nil {
		t.Fatal("no owner after the claim")
	}
	c.done()
	if rec, _ := sessions.Get(e.dir(home), "web"); rec.Owner != nil {
		t.Fatalf("owner kept after exit: %+v", rec.Owner)
	}
}

func TestRuntimeRegisterNeedsAValidName(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	agent := fakeAgent(1)
	e.writeLink(agent, conv(1), nil)
	c := e.start(home, agent, nil)
	msg, isErr := c.call("register_session", map[string]any{})
	if !isErr || !strings.Contains(msg, "call register_session with a name") {
		t.Fatalf("%q", msg)
	}
	if msg, isErr := c.call("register_session", map[string]any{"name": "../x"}); !isErr || !strings.Contains(msg, "not a valid session name") {
		t.Fatalf("%q", msg)
	}
	if e.homes[home].count() != 0 {
		t.Fatal("the server was called")
	}
}

func TestRuntimeRouteErrorRetriesAfterBackoff(t *testing.T) {
	e := newRTEnv(t)
	home := e.home("h")
	agent := fakeAgent(1)
	e.writeLink(agent, conv(1), nil)
	broken := true
	clock := time.Now()
	c := e.start(home, agent, func(o *RuntimeOptions) {
		inner := o.Open
		o.Open = func(ctx context.Context, h string) (*RuntimeRoute, error) {
			if broken {
				e.mu.Lock()
				e.opened = append(e.opened, "broken")
				e.mu.Unlock()
				return nil, errors.New("bad profile")
			}
			return inner(ctx, h)
		}
	})
	c.rt.now = func() time.Time { return clock }
	msg, isErr := c.call("inbox", nil)
	if !isErr || !strings.Contains(msg, "couldn't load your profile (bad profile); it will retry") {
		t.Fatalf("first call: %q %v", msg, isErr)
	}
	// The profile is fixed, but the back-off has not passed: no new attempt.
	broken = false
	if msg, _ := c.call("inbox", nil); !strings.Contains(msg, "couldn't load your profile") {
		t.Fatalf("retried too early: %q", msg)
	}
	clock = clock.Add(6 * time.Second)
	msg, isErr = c.call("inbox", nil)
	if !isErr || !strings.Contains(msg, "no ClawBridge session yet") {
		t.Fatalf("after the back-off: %q %v", msg, isErr)
	}
	if out, isErr := c.call("register_session", map[string]any{"name": "web"}); isErr {
		t.Fatalf("claim after the retry: %q", out)
	}
}

func TestRuntimeBackoffDoublesToAMinute(t *testing.T) {
	var b backoff
	now := time.Unix(0, 0)
	var got []time.Duration
	for i := 0; i < 6; i++ {
		b.bump(now)
		got = append(got, b.at.Sub(now))
	}
	want := []time.Duration{5, 10, 20, 40, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("%v", got)
		}
	}
}
