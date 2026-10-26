package agentd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// srcConn is a client connection from a given source address.
type srcConn struct {
	net.Conn
	src string
}

func (c srcConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(c.src), Port: 40000}
}

// egressHarness is an Egress with fake DNS and a fake upstream that echoes.
type egressHarness struct {
	e   *Egress
	log *lockedWriter

	mu     sync.Mutex
	dialed []string
	got    bytes.Buffer // what upstreams received
	sent   bytes.Buffer // what the proxy wrote upstream, recorded as it writes
}

// sentConn records the proxy's writes upstream before they go.
type sentConn struct {
	net.Conn
	h *egressHarness
}

func (c sentConn) Write(p []byte) (int, error) {
	c.h.mu.Lock()
	c.h.sent.Write(p)
	c.h.mu.Unlock()
	return c.Conn.Write(p)
}

var egressDNS = map[string][]string{
	"bridge.example.test": {"203.0.113.10"},
	"api.provider-a.test": {"203.0.113.20"},
	"api.provider-b.test": {"203.0.113.30"},
	"evil.test":           {"203.0.113.66"},
	"rebind.test":         {"10.0.0.5"},
	"mixed.test":          {"203.0.113.40", "169.254.169.254"},
	"cgnat.test":          {"100.64.0.9"},
	"loop.test":           {"::1"},
	"nat64.test":          {"64:ff9b::a00:5"},
	"reserved.test":       {"240.0.0.1"},
	"sixtofour.test":      {"2002:a00:5::1"},
}

func testPolicy() EgressPolicy {
	return EgressPolicy{Networks: []EgressNetwork{
		{OwnerRef: refA, Source: "172.30.1.0/24", Allow: []string{"bridge.example.test:443", "api.provider-a.test:443"}},
		{OwnerRef: refB, Source: "172.30.2.0/24", Allow: []string{"bridge.example.test:443", "api.provider-b.test:443"}},
		{OwnerRef: refC, Source: "172.30.3.0/24", Allow: []string{"rebind.test:443", "mixed.test:443"}},
		{OwnerRef: "p_eeeeeeee", Source: "172.30.5.0/24", Allow: []string{"nat64.test:443", "reserved.test:443"}},
		{OwnerRef: "p_ffffffff", Source: "172.30.6.0/24", Allow: []string{"sixtofour.test:443"}},
		{OwnerRef: "p_dddddddd", Source: "fd00:cb::/64", Allow: []string{"198.51.100.7:443", "cgnat.test:443"}},
	}}
}

func newEgressHarness(t *testing.T) *egressHarness {
	t.Helper()
	h := &egressHarness{log: &lockedWriter{w: &bytes.Buffer{}}}
	h.e = NewEgress(slog.New(slog.NewJSONHandler(h.log, nil)))
	h.e.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range egressDNS[host] {
			out = append(out, netip.MustParseAddr(a))
		}
		if out == nil {
			return nil, errors.New("no such host")
		}
		return out, nil
	}
	h.e.dial = func(_ context.Context, addr netip.AddrPort) (net.Conn, error) {
		h.mu.Lock()
		h.dialed = append(h.dialed, addr.String())
		h.mu.Unlock()
		proxySide, upstream := net.Pipe()
		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := upstream.Read(buf)
				if n > 0 {
					h.mu.Lock()
					h.got.Write(buf[:n])
					h.mu.Unlock()
					if _, err := upstream.Write(buf[:n]); err != nil {
						return
					}
				}
				if err != nil {
					upstream.Close()
					return
				}
			}
		}()
		return sentConn{Conn: proxySide, h: h}, nil
	}
	if err := h.e.SetPolicy(testPolicy()); err != nil {
		t.Fatal(err)
	}
	return h
}

// client is one connection to the proxy, after its answer.
type client struct {
	status int
	conn   net.Conn
	r      *bufio.Reader
}

// request sends raw as the request from src, followed by early, and reads
// the answer.
func (h *egressHarness) request(t *testing.T, src, raw, early string) client {
	t.Helper()
	c, s := net.Pipe()
	go h.e.handle(srcConn{Conn: s, src: src})
	go func() { _, _ = io.WriteString(c, raw+early) }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("%s %q: %v", src, raw, err)
	}
	return client{status: resp.StatusCode, conn: c, r: r}
}

func (h *egressHarness) connect(t *testing.T, src, target, early string) client {
	t.Helper()
	return h.request(t, src, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n", early)
}

func (h *egressHarness) dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

// An owner reaches its own server and provider through the tunnel, and the
// proxy dials the address it checked, not the name.
func TestEgressAllowsTheOwnersHosts(t *testing.T) {
	h := newEgressHarness(t)
	c := h.connect(t, "172.30.1.5", "api.provider-a.test:443", "early-hello")
	if c.status != http.StatusOK {
		t.Fatalf("status %d: %s", c.status, h.log.String())
	}
	buf := make([]byte, len("early-hello"))
	if _, err := io.ReadFull(c.r, buf); err != nil || string(buf) != "early-hello" {
		t.Fatalf("echo %q %v", buf, err)
	}
	if _, err := io.WriteString(c.conn, "s3cret-payload"); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, len("s3cret-payload"))
	if _, err := io.ReadFull(c.r, buf); err != nil || string(buf) != "s3cret-payload" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.conn.Close()
	for _, ok := range []struct{ src, target string }{
		{"172.30.1.6", "bridge.example.test:443"},
		{"172.30.1.7", "API.Provider-A.test:443"},
		{"172.30.2.5", "api.provider-b.test:443"},
		{"fd00:cb::5", "198.51.100.7:443"},
	} {
		if c := h.connect(t, ok.src, ok.target, ""); c.status != http.StatusOK {
			t.Fatalf("%v: status %d", ok, c.status)
		} else {
			c.conn.Close()
		}
	}
	want := []string{"203.0.113.20:443", "203.0.113.10:443", "203.0.113.20:443", "203.0.113.30:443", "198.51.100.7:443"}
	if got := h.dials(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("dialed %v", got)
	}
	// The tunnel's bytes are never logged.
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(h.log.String(), "s3cret") || strings.Contains(h.log.String(), "early-hello") {
		t.Fatalf("log: %s", h.log.String())
	}
}

// Everything else is refused, per source network, before any dial [AG-L3].
func TestEgressRefuses(t *testing.T) {
	h := newEgressHarness(t)
	for name, c := range map[string]struct {
		src, raw string
		status   int
	}{
		"A to B's provider":        {"172.30.1.5", "CONNECT api.provider-b.test:443 HTTP/1.1\r\n\r\n", 403},
		"B to A's provider":        {"172.30.2.5", "CONNECT api.provider-a.test:443 HTTP/1.1\r\n\r\n", 403},
		"another host":             {"172.30.1.5", "CONNECT evil.test:443 HTTP/1.1\r\n\r\n", 403},
		"another port":             {"172.30.1.5", "CONNECT api.provider-a.test:80 HTTP/1.1\r\n\r\n", 403},
		"no port":                  {"172.30.1.5", "CONNECT api.provider-a.test HTTP/1.1\r\n\r\n", 403},
		"padded port":              {"172.30.1.5", "CONNECT api.provider-a.test:0443 HTTP/1.1\r\n\r\n", 403},
		"trailing dot":             {"172.30.1.5", "CONNECT api.provider-a.test.:443 HTTP/1.1\r\n\r\n", 403},
		"the host's IP":            {"172.30.1.5", "CONNECT 203.0.113.20:443 HTTP/1.1\r\n\r\n", 403},
		"metadata IP":              {"172.30.1.5", "CONNECT 169.254.169.254:80 HTTP/1.1\r\n\r\n", 403},
		"no owner's network":       {"10.9.9.9", "CONNECT api.provider-a.test:443 HTTP/1.1\r\n\r\n", 403},
		"plain proxying":           {"172.30.1.5", "GET http://api.provider-a.test/ HTTP/1.1\r\nHost: api.provider-a.test\r\n\r\n", 405},
		"not HTTP":                 {"172.30.1.5", "\x16\x03\x01 hello\r\n\r\n", 400},
		"name to a private IP":     {"172.30.3.5", "CONNECT rebind.test:443 HTTP/1.1\r\n\r\n", 403},
		"name to metadata as well": {"172.30.3.5", "CONNECT mixed.test:443 HTTP/1.1\r\n\r\n", 403},
		"name to CGNAT":            {"fd00:cb::5", "CONNECT cgnat.test:443 HTTP/1.1\r\n\r\n", 403},
		"name to NAT64":            {"172.30.5.5", "CONNECT nat64.test:443 HTTP/1.1\r\n\r\n", 403},
		"name to reserved":         {"172.30.5.5", "CONNECT reserved.test:443 HTTP/1.1\r\n\r\n", 403},
		"name to 6to4":             {"172.30.6.5", "CONNECT sixtofour.test:443 HTTP/1.1\r\n\r\n", 403},
		"huge header":              {"172.30.1.5", "CONNECT api.provider-a.test:443 HTTP/1.1\r\nX: " + strings.Repeat("a", 8<<10) + "\r\n\r\n", 400},
	} {
		if got := h.request(t, c.src, c.raw, ""); got.status != c.status {
			t.Errorf("%s: status %d", name, got.status)
		}
	}
	if d := h.dials(); len(d) != 0 {
		t.Fatalf("dialed %v", d)
	}
	// A refused target is quoted and cut short in the log.
	h.request(t, "172.30.1.5", "CONNECT "+strings.Repeat("x", 200)+".test:443 HTTP/1.1\r\n\r\n", "")
	if strings.Contains(h.log.String(), strings.Repeat("x", 100)) {
		t.Fatal("a long target was logged whole")
	}
}

// A policy change closes the tunnels it no longer allows, at once.
func TestEgressPolicyChangeClosesTunnels(t *testing.T) {
	h := newEgressHarness(t)
	provider := h.connect(t, "172.30.1.5", "api.provider-a.test:443", "")
	server := h.connect(t, "172.30.1.6", "bridge.example.test:443", "")
	if provider.status != 200 || server.status != 200 {
		t.Fatal(provider.status, server.status)
	}
	p := testPolicy()
	p.Networks[0].Allow = []string{"bridge.example.test:443", "api.provider-c.test:443"}
	if err := h.e.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatal("the old provider's tunnel is still open")
	}
	if _, err := io.WriteString(server.conn, "x"); err != nil {
		t.Fatalf("the server tunnel was closed: %v", err)
	}
	if c := h.connect(t, "172.30.1.5", "api.provider-a.test:443", ""); c.status != 403 {
		t.Fatalf("status %d", c.status)
	}
	// An empty policy refuses everything.
	if err := h.e.SetPolicy(EgressPolicy{}); err != nil {
		t.Fatal(err)
	}
	if c := h.connect(t, "172.30.1.6", "bridge.example.test:443", ""); c.status != 403 {
		t.Fatalf("status %d", c.status)
	}
}

func TestEgressCapsTunnelsPerNetwork(t *testing.T) {
	h := newEgressHarness(t)
	h.e.maxTunnels = 2
	for range 2 {
		if c := h.connect(t, "172.30.1.5", "bridge.example.test:443", ""); c.status != 200 {
			t.Fatal(c.status)
		}
	}
	if c := h.connect(t, "172.30.1.5", "bridge.example.test:443", ""); c.status != http.StatusServiceUnavailable {
		t.Fatal(c.status)
	}
	if c := h.connect(t, "172.30.2.5", "bridge.example.test:443", ""); c.status != 200 {
		t.Fatalf("another owner was capped: %d", c.status)
	}
}

func TestEgressPolicyValidation(t *testing.T) {
	ok := EgressNetwork{OwnerRef: refA, Source: "172.30.1.0/24", Allow: []string{"bridge.example.test:443"}}
	for name, nets := range map[string][]EgressNetwork{
		"bad ref":      {{OwnerRef: "postgres", Source: ok.Source, Allow: ok.Allow}},
		"host source":  {{OwnerRef: refA, Source: "172.30.1.5/24", Allow: ok.Allow}},
		"no source":    {{OwnerRef: refA, Allow: ok.Allow}},
		"overlap":      {ok, {OwnerRef: refB, Source: "172.30.0.0/16", Allow: ok.Allow}},
		"twice":        {ok, {OwnerRef: refA, Source: "172.30.2.0/24", Allow: ok.Allow}},
		"no allow":     {{OwnerRef: refA, Source: ok.Source}},
		"three hosts":  {{OwnerRef: refA, Source: ok.Source, Allow: []string{"a.test:443", "b.test:443", "c.test:443"}}},
		"no port":      {{OwnerRef: refA, Source: ok.Source, Allow: []string{"bridge.example.test"}}},
		"port 0":       {{OwnerRef: refA, Source: ok.Source, Allow: []string{"bridge.example.test:0"}}},
		"wildcard":     {{OwnerRef: refA, Source: ok.Source, Allow: []string{"*.example.test:443"}}},
		"url":          {{OwnerRef: refA, Source: ok.Source, Allow: []string{"https://bridge.example.test:443"}}},
		"private IP":   {{OwnerRef: refA, Source: ok.Source, Allow: []string{"10.0.0.1:443"}}},
		"metadata IP":  {{OwnerRef: refA, Source: ok.Source, Allow: []string{"169.254.169.254:80"}}},
		"loopback":     {{OwnerRef: refA, Source: ok.Source, Allow: []string{"127.0.0.1:443"}}},
		"benchmark IP": {{OwnerRef: refA, Source: ok.Source, Allow: []string{"198.18.0.1:443"}}},
		"NAT64 IP":     {{OwnerRef: refA, Source: ok.Source, Allow: []string{"[64:ff9b::a9fe:a9fe]:80"}}},
		"single label": {{OwnerRef: refA, Source: ok.Source, Allow: []string{"localhost:443"}}},
		"numeric top":  {{OwnerRef: refA, Source: ok.Source, Allow: []string{"bridge.example.123:443"}}},
	} {
		if err := NewEgress(slog.New(slog.DiscardHandler)).SetPolicy(EgressPolicy{Networks: nets}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, raw := range map[string]string{
		"unknown field": `{"networks":[],"allow_all":true}`,
		"trailing":      `{"networks":[]}{}`,
		"not json":      `networks: []`,
	} {
		if _, err := ParseEgressPolicy([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if p, err := ParseEgressPolicy([]byte(`{"networks":[{"owner_ref":"p_aaaaaaaa","source":"172.30.1.0/24","allow":["bridge.example.test:443"]}]}`)); err != nil ||
		NewEgress(slog.New(slog.DiscardHandler)).SetPolicy(p) != nil {
		t.Fatal(err)
	}
}

// A policy change while a CONNECT is dialling still applies to it: the
// tunnel is checked again before it opens.
func TestEgressRechecksAfterDialling(t *testing.T) {
	h := newEgressHarness(t)
	dial := h.e.dial
	h.e.dial = func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
		p := testPolicy()
		p.Networks[0].Allow = []string{"bridge.example.test:443"}
		if err := h.e.SetPolicy(p); err != nil {
			t.Error(err)
		}
		return dial(ctx, addr)
	}
	if c := h.connect(t, "172.30.1.5", "api.provider-a.test:443", "early-hello"); c.status != http.StatusForbidden {
		t.Fatalf("status %d", c.status)
	}
	h.e.mu.Lock()
	open := len(h.e.tunnels)
	h.e.mu.Unlock()
	h.mu.Lock()
	sent := h.sent.String()
	h.mu.Unlock()
	if open != 0 || sent != "" {
		t.Fatalf("%d tunnels open, %q sent upstream", open, sent)
	}
}

// A subnet given to another owner closes the tunnels opened from it.
func TestEgressPolicyChangeOfSourceClosesTunnels(t *testing.T) {
	h := newEgressHarness(t)
	c := h.connect(t, "172.30.1.5", "bridge.example.test:443", "")
	if c.status != 200 {
		t.Fatal(c.status)
	}
	p := testPolicy()
	p.Networks[0].Source = "172.30.9.0/24"
	if err := h.e.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatal("a tunnel from a reassigned subnet is still open")
	}
}

// Half-sent requests count against the caps too, so one owner can't hold
// every slot; the proxy-wide cap holds across owners.
func TestEgressCapsPendingConnections(t *testing.T) {
	h := newEgressHarness(t)
	h.e.maxTunnels, h.e.maxConns = 2, 3
	var pending []net.Conn
	idle := func(src string) {
		c, s := net.Pipe()
		go h.e.handle(srcConn{Conn: s, src: src})
		pending = append(pending, c)
	}
	waitActive := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			h.e.mu.Lock()
			total := h.e.total
			h.e.mu.Unlock()
			if total == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d active, want %d", total, n)
			}
		}
	}
	idle("172.30.1.5")
	idle("172.30.1.6")
	waitActive(2)
	if c := h.connect(t, "172.30.1.7", "bridge.example.test:443", ""); c.status != http.StatusServiceUnavailable {
		t.Fatalf("a third pending connection from A: %d", c.status)
	}
	idle("172.30.2.5")
	waitActive(3)
	if c := h.connect(t, "172.30.3.5", "rebind.test:443", ""); c.status != http.StatusServiceUnavailable {
		t.Fatalf("past the proxy-wide cap: %d", c.status)
	}
	for _, c := range pending {
		c.Close()
	}
	waitActive(0)
	c := h.connect(t, "172.30.1.5", "bridge.example.test:443", "")
	if c.status != 200 {
		t.Fatalf("slots were not released: %d", c.status)
	}
	c.conn.Close()
}

// flakyListener fails its first Accept with a non-closed error.
type flakyListener struct {
	net.Listener
	failed bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

// An Accept error other than a closed listener is waited out.
func TestEgressServeSurvivesAcceptErrors(t *testing.T) {
	h := newEgressHarness(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Serve(&flakyListener{Listener: l}) }()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// 127.0.0.1 is on no owner's network: refused, which proves it was served.
	io.WriteString(conn, "CONNECT bridge.example.test:443 HTTP/1.1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%v %v", resp, err)
	}
	l.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
