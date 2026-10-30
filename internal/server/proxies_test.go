package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mustProxies parses a trusted-proxy list a test depends on.
func mustProxies(t *testing.T, raw string) *TrustedProxies {
	t.Helper()
	p, err := ParseTrustedProxies(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// proxied is the client address and scheme the handler keys a request
// from peer with these forwarded headers on, once p has read them.
func proxied(p *TrustedProxies, peer, xff, xfp string) (address, scheme string) {
	r := httptest.NewRequest("GET", ModelHelloPath, nil)
	r.RemoteAddr = peer
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if xfp != "" {
		r.Header.Set("X-Forwarded-Proto", xfp)
	}
	r = p.apply(r)
	return clientAddress(r), requestScheme(r)
}

// S2: each entry is an IP, a CIDR or a host name; anything else stops
// serve at startup, naming the entry. Unset or empty trusts no peer.
func TestParseTrustedProxies(t *testing.T) {
	for _, raw := range []string{"", "  "} {
		if p, err := ParseTrustedProxies(raw); err != nil || p.configured() {
			t.Fatalf("%q: %v %v", raw, p, err)
		}
	}
	p, err := ParseTrustedProxies(" 127.0.0.1, 172.18.0.0/16 ,caddy,proxy.example.test, ::1, 2001:db8::/32")
	if err != nil || !p.configured() || len(p.entries) != 6 {
		t.Fatalf("a valid list: %v %v", p, err)
	}
	for _, bad := range []string{"10.0.0.300", "10.0.0.0/33", "bad host", "http://caddy", "caddy:443", "-caddy", "caddy_1", "a..b", "1.2.3", strings.Repeat("a", 64)} {
		_, err := ParseTrustedProxies("127.0.0.1," + bad)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := ParseTrustedProxies("127.0.0.1,,caddy"); err == nil || !strings.Contains(err.Error(), "empty entry") {
		t.Errorf("an empty entry: %v", err)
	}
}

// S2: without a trusted proxy, forwarded headers are ignored from every
// peer, loopback and private ones included: a process that reaches the
// server around the proxy can't choose its rate-limit bucket or claim
// https.
func TestForwardedHeadersIgnoredFromUntrustedPeers(t *testing.T) {
	for _, p := range []*TrustedProxies{nil, mustProxies(t, ""), mustProxies(t, "10.9.0.0/16, 192.0.2.1")} {
		for _, peer := range []string{"127.0.0.1:4000", "[::1]:4000", "172.18.0.5:4000", "10.0.0.2:4000", "203.0.113.9:4000"} {
			host, _, _ := net.SplitHostPort(peer)
			address, scheme := proxied(p, peer, "198.51.100.1", "https")
			if address != host || scheme != "http" {
				t.Errorf("%v from %s: %s %s", p, peer, address, scheme)
			}
		}
	}
}

// S2: a Docker neighbour on the compose network (a private peer not in
// the list) sending X-Forwarded-For is keyed on its own address.
func TestDockerNeighbourKeyedOnItsOwnAddress(t *testing.T) {
	p := mustProxies(t, "172.18.0.2")
	r := httptest.NewRequest("POST", IntakePath, nil)
	r.RemoteAddr = "172.18.0.7:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := addressKey(p.apply(r)); got != "172.18.0.7" {
		t.Fatal(got)
	}
}

// S2: a peer in the list, by IP, CIDR or resolved host name, is the proxy:
// the rightmost X-Forwarded-For and X-Forwarded-Proto entries are the
// client's; with no header, the peer and the connection's scheme.
func TestForwardedHeadersHonouredFromTrustedProxies(t *testing.T) {
	byName := mustProxies(t, "caddy")
	byName.lookup = func(_ context.Context, host string) ([]net.IP, error) {
		if host != "caddy" {
			return nil, errors.New("unknown")
		}
		return []net.IP{net.ParseIP("172.18.0.9"), net.ParseIP("172.18.0.5")}, nil
	}
	for name, p := range map[string]*TrustedProxies{
		"ip":       mustProxies(t, "192.0.2.1, 172.18.0.5"),
		"cidr":     mustProxies(t, "172.18.0.0/16"),
		"hostname": byName,
	} {
		for _, c := range []struct{ xff, xfp, address, scheme string }{
			{"198.51.100.1", "https", "198.51.100.1", "https"},
			{"10.0.0.1, 198.51.100.2", "http, HTTPS", "198.51.100.2", "https"},
			{"", "", "172.18.0.5", "http"},
			{"not-an-ip", "", "172.18.0.5", "http"},
		} {
			address, scheme := proxied(p, "172.18.0.5:4000", c.xff, c.xfp)
			if address != c.address || scheme != c.scheme {
				t.Errorf("%s %q %q: %s %s", name, c.xff, c.xfp, address, scheme)
			}
		}
	}
	if address, _ := proxied(mustProxies(t, "::1"), "[::1]:4000", "2001:db8::9", ""); address != "2001:db8::9" {
		t.Error("an IPv6 proxy:", address)
	}
}

// S2: a host name that doesn't resolve trusts nothing; the failure is
// logged once per cache period, never per request, and the answer is
// cached about 30 seconds either way.
func TestTrustedProxyLookup(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := mustProxies(t, "caddy")
	calls, fails := 0, true
	p.now = func() time.Time { return now }
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		calls++
		if fails {
			return nil, errors.New("no such host")
		}
		return []net.IP{net.ParseIP("172.18.0.5")}, nil
	}
	var logged []string
	p.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	for range 5 {
		if address, scheme := proxied(p, "172.18.0.5:4000", "198.51.100.1", "https"); address != "172.18.0.5" || scheme != "http" {
			t.Fatal("trusted after a failed lookup:", address, scheme)
		}
	}
	if calls != 1 || len(logged) != 1 || !strings.Contains(logged[0], `"caddy"`) {
		t.Fatalf("calls %d, logged %q", calls, logged)
	}
	fails = false
	now = now.Add(trustedProxyTTL + time.Second)
	if address, _ := proxied(p, "172.18.0.5:4000", "198.51.100.1", ""); address != "198.51.100.1" || calls != 2 {
		t.Fatal("not looked up again after the period:", address, calls)
	}
	proxied(p, "172.18.0.5:4000", "198.51.100.1", "")
	if calls != 2 || len(logged) != 1 {
		t.Fatalf("a good answer was not cached: calls %d, logged %q", calls, logged)
	}
	// A request with no forwarded header never needs a lookup.
	now = now.Add(trustedProxyTTL + time.Second)
	proxied(p, "172.18.0.5:4000", "", "")
	if calls != 2 {
		t.Fatal("looked up for a request with nothing to trust:", calls)
	}
}

// S2: a failed lookup after a good one keeps the proxy's last addresses
// (a DNS hiccup must not drop it for everyone) and tries again after
// trustedProxyRetry; a name that resolves to two addresses trusts both.
func TestTrustedProxyKeepsLastGoodAddresses(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := mustProxies(t, "caddy-backend")
	calls, fails := 0, false
	p.now = func() time.Time { return now }
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		calls++
		if fails {
			return nil, errors.New("server misbehaving")
		}
		return []net.IP{net.ParseIP("172.19.0.2"), net.ParseIP("172.20.0.2")}, nil
	}
	var logged []string
	p.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	for _, peer := range []string{"172.19.0.2:4000", "172.20.0.2:4000"} {
		if address, scheme := proxied(p, peer, "198.51.100.1", "https"); address != "198.51.100.1" || scheme != "https" {
			t.Fatal("both addresses must be trusted:", peer, address, scheme)
		}
	}
	fails = true
	now = now.Add(trustedProxyTTL + time.Second)
	if address, _ := proxied(p, "172.19.0.2:4000", "198.51.100.1", ""); address != "198.51.100.1" || calls != 2 || len(logged) != 1 {
		t.Fatal("dropped after one failed lookup:", address, calls, logged)
	}
	// Within the retry period the last addresses stand, with no lookup.
	now = now.Add(trustedProxyRetry - time.Second)
	if address, _ := proxied(p, "172.20.0.2:4000", "198.51.100.1", ""); address != "198.51.100.1" || calls != 2 {
		t.Fatal("retried too soon or dropped:", address, calls)
	}
	// After it, one more lookup; recovered, the normal period applies.
	fails = false
	now = now.Add(2 * time.Second)
	if address, _ := proxied(p, "172.19.0.2:4000", "198.51.100.1", ""); address != "198.51.100.1" || calls != 3 || len(logged) != 1 {
		t.Fatal("not retried after the retry period:", address, calls, logged)
	}
	now = now.Add(trustedProxyRetry + time.Second)
	proxied(p, "172.19.0.2:4000", "198.51.100.1", "")
	if calls != 3 {
		t.Fatal("a recovered name is looked up again before its period:", calls)
	}
}

// S2: a lookup in flight doesn't hold up requests that have good
// addresses; one with none yet waits for the first answer.
func TestTrustedProxyLookupDoesNotBlock(t *testing.T) {
	p := mustProxies(t, "caddy-backend")
	release, started := make(chan struct{}), make(chan struct{}, 4)
	var mu sync.Mutex
	calls := 0
	p.lookup = func(context.Context, string) ([]net.IP, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			started <- struct{}{}
			<-release
		}
		return []net.IP{net.ParseIP("172.19.0.2")}, nil
	}
	if address, _ := proxied(p, "172.19.0.2:4000", "198.51.100.1", ""); address != "198.51.100.1" {
		t.Fatal("first answer:", address)
	}
	p.entries[0].mu.Lock()
	p.entries[0].until = time.Time{}
	p.entries[0].mu.Unlock()
	go proxied(p, "172.19.0.2:4000", "198.51.100.1", "")
	<-started
	done := make(chan string)
	go func() { address, _ := proxied(p, "172.19.0.2:4000", "198.51.100.2", ""); done <- address }()
	select {
	case address := <-done:
		if address != "198.51.100.2" {
			t.Fatal("stale addresses not used:", address)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request waited for a lookup in flight although good addresses were known")
	}
	close(release)
}

// S2: serve --trusted-proxies, or CLAWBRIDGE_TRUSTED_PROXIES; the flag
// wins. A malformed entry stops serve, naming it.
func TestParseServeTrustedProxies(t *testing.T) {
	base := []string{"--auth", "device", "--public-origin", "https://clawbridge.example.test", "--bootstrap-owner", "owner@example.test"}
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "CLAWBRIDGE_TRUSTED_PROXIES" {
				return v
			}
			return ""
		}
	}
	s, err := ParseServe(base, env(""))
	if err != nil || s.TrustedProxies.configured() {
		t.Fatal("unset:", s.TrustedProxies, err)
	}
	s, err = ParseServe(base, env("caddy"))
	if err != nil || !s.TrustedProxies.configured() || s.TrustedProxies.entries[0].name != "caddy" {
		t.Fatal("from the environment:", s.TrustedProxies, err)
	}
	s, err = ParseServe(append([]string{"--trusted-proxies", "127.0.0.1"}, base...), env("caddy"))
	if err != nil || len(s.TrustedProxies.entries) != 1 || s.TrustedProxies.entries[0].name != "" {
		t.Fatal("the flag:", s.TrustedProxies, err)
	}
	if _, err = ParseServe(base, env("caddy,not a host")); err == nil || !strings.Contains(err.Error(), "CLAWBRIDGE_TRUSTED_PROXIES") || !strings.Contains(err.Error(), `"not a host"`) {
		t.Fatal("a malformed entry:", err)
	}
	if _, err = ParseServe(append([]string{"--trusted-proxies", "10.0.0.0/40"}, base...), env("")); err == nil || !strings.Contains(err.Error(), "--trusted-proxies") {
		t.Fatal("a malformed flag:", err)
	}
}
