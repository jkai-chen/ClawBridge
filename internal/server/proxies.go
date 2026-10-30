package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// trustedProxyTTL is how long a trusted proxy's resolved host name, or its
// failed lookup, is kept (S2).
const trustedProxyTTL = 30 * time.Second

// trustedProxyRetry is how soon a host name whose lookup failed is looked
// up again while its last good addresses stand in.
const trustedProxyRetry = 5 * time.Second

// trustedProxyLookupTimeout bounds one host-name lookup on a request's
// path.
const trustedProxyLookupTimeout = 2 * time.Second

// TrustedProxies are the peers whose X-Forwarded-For and X-Forwarded-Proto
// the server reads: serve --trusted-proxies or CLAWBRIDGE_TRUSTED_PROXIES,
// each entry an IP, a CIDR or a host name. Any other peer's forwarded
// headers are ignored, a loopback or private one's too, so a process that
// reaches the server around the proxy can't choose its per-IP bucket or
// claim https (S2). The zero value and nil trust no peer.
type TrustedProxies struct {
	entries []*proxyEntry
	// lookup resolves a host-name entry to all its addresses; nil is the
	// system resolver.
	lookup func(ctx context.Context, host string) ([]net.IP, error)
	// now is the cache's clock; nil is time.Now.
	now func() time.Time
	// logf logs a failed lookup; nil is log.Printf.
	logf func(format string, args ...any)
}

// proxyEntry is one entry: a network (an IP is a one-address network) or
// a host name, resolved on use and cached.
type proxyEntry struct {
	network *net.IPNet
	name    string

	mu    sync.Mutex
	ips   []net.IP
	until time.Time
	// lookup is the lookup in flight, closed when it ends: requests meanwhile
	// use the last good addresses, or wait for it when there are none yet.
	lookup chan struct{}
}

// ParseTrustedProxies reads a comma-separated list of IPs, CIDRs and host
// names. Empty trusts no peer; a malformed entry is an error naming it.
func ParseTrustedProxies(raw string) (*TrustedProxies, error) {
	p := &TrustedProxies{}
	if strings.TrimSpace(raw) == "" {
		return p, nil
	}
	for _, field := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(field)
		switch {
		case entry == "":
			return nil, errors.New("the list has an empty entry")
		case strings.Contains(entry, "/"):
			_, network, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("%q is not an IP, a CIDR or a host name", entry)
			}
			p.entries = append(p.entries, &proxyEntry{network: network})
		case net.ParseIP(entry) != nil:
			ip := net.ParseIP(entry)
			bits := 128
			if v4 := ip.To4(); v4 != nil {
				ip, bits = v4, 32
			}
			p.entries = append(p.entries, &proxyEntry{network: &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}})
		case hostName(entry):
			p.entries = append(p.entries, &proxyEntry{name: strings.ToLower(entry)})
		default:
			return nil, fmt.Errorf("%q is not an IP, a CIDR or a host name", entry)
		}
	}
	return p, nil
}

// hostName reports a DNS name: dot-separated labels of letters, digits
// and inner hyphens, at most 253 bytes, whose last label isn't numeric,
// so a mistyped address is never taken for a name.
func hostName(s string) bool {
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != ""
}

// configured reports a list with at least one entry.
func (p *TrustedProxies) configured() bool { return p != nil && len(p.entries) > 0 }

// trusts is the one check of whether a peer is a trusted proxy, for both
// forwarded headers. A host name that doesn't resolve trusts nothing.
func (p *TrustedProxies) trusts(peer net.IP) bool {
	if !p.configured() || peer == nil {
		return false
	}
	for _, e := range p.entries {
		if e.network != nil {
			if e.network.Contains(peer) {
				return true
			}
			continue
		}
		for _, ip := range p.resolve(e) {
			if ip.Equal(peer) {
				return true
			}
		}
	}
	return false
}

// resolve is a host-name entry's addresses, looked up at most once per
// trustedProxyTTL, by one request at a time and without holding the lock:
// the others use the last good addresses meanwhile, or wait for the first
// answer. A failed lookup keeps the last good addresses and tries again
// after trustedProxyRetry, so one DNS hiccup never drops the proxy; only a
// name that never resolved trusts nothing, for trustedProxyTTL. A failure is
// logged once per lookup, never per request.
func (p *TrustedProxies) resolve(e *proxyEntry) []net.IP {
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	e.mu.Lock()
	for {
		if now().Before(e.until) {
			ips := e.ips
			e.mu.Unlock()
			return ips
		}
		if e.lookup == nil {
			break
		}
		if e.ips != nil {
			ips := e.ips
			e.mu.Unlock()
			return ips
		}
		wait := e.lookup
		e.mu.Unlock()
		<-wait
		e.mu.Lock()
	}
	done := make(chan struct{})
	e.lookup = done
	e.mu.Unlock()

	lookup := p.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustedProxyLookupTimeout)
	ips, err := lookup(ctx, e.name)
	cancel()

	e.mu.Lock()
	defer e.mu.Unlock()
	defer close(done)
	e.lookup = nil
	if err == nil && len(ips) > 0 {
		e.ips, e.until = ips, now().Add(trustedProxyTTL)
		return e.ips
	}
	if err == nil {
		err = errors.New("no addresses")
	}
	logf := log.Printf
	if p.logf != nil {
		logf = p.logf
	}
	if e.ips != nil {
		logf("clawbridge: trusted proxy %q did not resolve; keeping its last addresses, retrying in %s: %v", e.name, trustedProxyRetry, err)
		e.until = now().Add(trustedProxyRetry)
		return e.ips
	}
	logf("clawbridge: trusted proxy %q does not resolve; its forwarded headers are ignored (logged at most every %s): %v", e.name, trustedProxyTTL, err)
	e.until = now().Add(trustedProxyTTL)
	return nil
}

// forwardedKey holds a request's client address and scheme, read once at
// the handler's door.
type forwardedKey struct{}

type forwarded struct{ address, scheme string }

// apply reads r's client address and scheme: a trusted proxy's rightmost
// X-Forwarded-For and X-Forwarded-Proto entries, otherwise the peer and
// its connection. Only a request with a forwarded header asks whether its
// peer is trusted.
func (p *TrustedProxies) apply(r *http.Request) *http.Request {
	v := forwarded{address: peerHost(r), scheme: connScheme(r)}
	addresses, protos := r.Header.Values("X-Forwarded-For"), r.Header.Values("X-Forwarded-Proto")
	if (len(addresses) > 0 || len(protos) > 0) && p.trusts(net.ParseIP(v.address)) {
		if ip := net.ParseIP(rightmost(addresses)); ip != nil {
			v.address = ip.String()
		}
		if proto := strings.ToLower(rightmost(protos)); proto != "" {
			v.scheme = proto
		}
	}
	return r.WithContext(context.WithValue(r.Context(), forwardedKey{}, v))
}

// rightmost is the last comma-separated entry of a header's last line:
// the one the nearest proxy wrote.
func rightmost(values []string) string {
	if len(values) == 0 {
		return ""
	}
	entries := strings.Split(values[len(values)-1], ",")
	return strings.TrimSpace(entries[len(entries)-1])
}

func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func connScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// requestScheme is the scheme the client used: a trusted proxy's
// X-Forwarded-Proto, otherwise the connection's own.
func requestScheme(r *http.Request) string {
	if v, ok := r.Context().Value(forwardedKey{}).(forwarded); ok {
		return v.scheme
	}
	return connScheme(r)
}
