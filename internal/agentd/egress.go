package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Egress is the per-owner egress proxy (§5.3, AG-L3). It is the one process
// attached to every owner's internal network, so it is kept minimal:
//   - CONNECT only, with no TLS interception and no secrets.
//   - Each source network may reach exactly its own allow-list: the
//     ClawBridge server and that owner's provider, as host:port.
//   - A host name is resolved by the proxy, and every address must be
//     public. The proxy then dials the address it checked, so a DNS answer
//     can't be rebound in between.
//   - An IP literal passes only when it is listed itself.
//
// Its policy comes only from agentd, which writes it from the server's
// per-owner config.
type Egress struct {
	log *slog.Logger
	// resolve and dial are the network; tests replace them.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
	// maxTunnels caps one source network's connections, from accept to close:
	// waiting for the request, dialling, or tunnelling. maxConns caps them all.
	maxTunnels, maxConns int

	mu      sync.Mutex
	policy  []egressNetwork
	tunnels map[*tunnel]struct{}
	active  map[string]int
	total   int
}

// EgressPolicy is the file agentd writes for the egress proxy.
type EgressPolicy struct {
	Networks []EgressNetwork `json:"networks"`
}

// EgressNetwork is one owner's network: its source subnet and the host:port
// pairs it may reach.
type EgressNetwork struct {
	OwnerRef string   `json:"owner_ref"`
	Source   string   `json:"source"`
	Allow    []string `json:"allow"`
}

type egressNetwork struct {
	ref    string
	source netip.Prefix
	allow  map[string]bool
}

type tunnel struct {
	ref, target      string
	src              netip.Addr
	client, upstream net.Conn
}

const (
	// maxAllow: the server and one provider host (§5.3).
	maxAllow         = 2
	egressHeaderWait = 10 * time.Second
	egressDialWait   = 10 * time.Second
	egressIdle       = 10 * time.Minute
	maxConnectHeader = 4 << 10
	maxTunnelsPerNet = 32
	maxEgressConns   = 256
)

var hostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NewEgress returns an Egress with no policy: it refuses everything until
// SetPolicy.
func NewEgress(log *slog.Logger) *Egress {
	return &Egress{
		log: log,
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr.String())
		},
		maxTunnels: maxTunnelsPerNet,
		maxConns:   maxEgressConns,
		tunnels:    map[*tunnel]struct{}{},
		active:     map[string]int{},
	}
}

// ParseEgressPolicy reads a policy, refusing unknown fields.
func ParseEgressPolicy(data []byte) (EgressPolicy, error) {
	var p EgressPolicy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return EgressPolicy{}, fmt.Errorf("the egress policy is not valid JSON: %w", err)
	}
	if dec.More() {
		return EgressPolicy{}, errors.New("the egress policy has trailing data")
	}
	return p, nil
}

// SetPolicy validates and installs p, and closes every open tunnel that p
// no longer allows, by target or by source: a changed provider, or a subnet
// given to another owner, takes effect at once.
func (e *Egress) SetPolicy(p EgressPolicy) error {
	compiled, err := compileEgress(p)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.policy = compiled
	var closing []*tunnel
	for t := range e.tunnels {
		if n := e.networkByRef(t.ref); n == nil || !n.allow[t.target] || !n.source.Contains(t.src) {
			closing = append(closing, t)
		}
	}
	e.mu.Unlock()
	for _, t := range closing {
		e.log.Info("egress tunnel closed by a policy change", "ref", t.ref, "target", t.target)
		t.client.Close()
		t.upstream.Close()
	}
	return nil
}

func compileEgress(p EgressPolicy) ([]egressNetwork, error) {
	var out []egressNetwork
	for _, n := range p.Networks {
		if !validOwnerRef(n.OwnerRef) {
			return nil, errors.New("an egress network's owner_ref is not an owner ref")
		}
		source, err := netip.ParsePrefix(n.Source)
		if err != nil || source != source.Masked() {
			return nil, fmt.Errorf("%s: source must be a network prefix such as 172.30.1.0/24", n.OwnerRef)
		}
		for _, o := range out {
			if o.ref == n.OwnerRef {
				return nil, fmt.Errorf("%s is listed twice", n.OwnerRef)
			}
			if o.source.Overlaps(source) {
				return nil, fmt.Errorf("the sources of %s and %s overlap", o.ref, n.OwnerRef)
			}
		}
		if len(n.Allow) == 0 || len(n.Allow) > maxAllow {
			return nil, fmt.Errorf("%s: allow must list the server and the provider (1 or 2 host:port entries)", n.OwnerRef)
		}
		allow := map[string]bool{}
		for _, a := range n.Allow {
			target, err := normalTarget(a)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", n.OwnerRef, err)
			}
			if ip, err := netip.ParseAddr(hostOf(target)); err == nil && !publicAddr(ip) {
				return nil, fmt.Errorf("%s: %s is not a public address", n.OwnerRef, ip)
			}
			allow[target] = true
		}
		out = append(out, egressNetwork{ref: n.OwnerRef, source: source, allow: allow})
	}
	return out, nil
}

// normalTarget checks a host:port and returns it with the host in lower
// case. The host is a DNS name or an IP literal; the port is explicit.
func normalTarget(target string) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", fmt.Errorf("%q is not host:port", target)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("%q has no valid port", target)
	}
	host = strings.ToLower(host)
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", fmt.Errorf("%q has a zone", target)
		}
		return net.JoinHostPort(ip.String(), port), nil
	}
	if len(host) > 253 || !hostPattern.MatchString(host) {
		return "", fmt.Errorf("%q is not a DNS name or an IP address", target)
	}
	return net.JoinHostPort(host, port), nil
}

func hostOf(target string) string {
	host, _, _ := net.SplitHostPort(target)
	return host
}

// notPublic are ranges netip doesn't call private but that never reach a
// public host directly: shared CGNAT space, "this network", IETF protocol
// assignments, benchmarking, reserved, and the IPv6 translation and
// tunnelling prefixes that can carry a private IPv4 address.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("100::/64"),
}

// publicAddr reports whether ip is a public unicast address: not loopback,
// private, link-local (cloud metadata), multicast, unspecified or one of
// notPublic.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func (e *Egress) networkByRef(ref string) *egressNetwork {
	for i := range e.policy {
		if e.policy[i].ref == ref {
			return &e.policy[i]
		}
	}
	return nil
}

func (e *Egress) networkFor(src netip.Addr) *egressNetwork {
	src = src.Unmap()
	for i := range e.policy {
		if e.policy[i].source.Contains(src) {
			return &e.policy[i]
		}
	}
	return nil
}

// Serve accepts connections on l until it is closed. Any other Accept
// error, such as running out of file descriptors, is waited out, as
// net/http does, rather than taking the proxy down for every owner.
func (e *Egress) Serve(l net.Listener) error {
	var wait time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			wait = min(max(2*wait, 5*time.Millisecond), time.Second)
			e.log.Warn("egress accept failed; retrying", "error", err.Error(), "wait_ms", wait.Milliseconds())
			time.Sleep(wait)
			continue
		}
		wait = 0
		go e.handle(conn)
	}
}

// handle serves one client connection: one CONNECT, then the tunnel. The
// connection counts against its network's cap, and the proxy's, from the
// start: one owner can't hold every slot with half-sent requests.
func (e *Egress) handle(conn net.Conn) {
	start := time.Now()
	t, refusal := e.open(conn)
	if t != nil || (refusal != nil && refusal.counted) {
		defer e.release(refusal, t)
	}
	if refusal != nil {
		e.log.Warn("egress refused", "ref", refusal.ref, "target", refusal.target, "status", refusal.status, "reason", refusal.reason)
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", refusal.status, http.StatusText(refusal.status))
		conn.Close()
		return
	}
	up, down := e.splice(t)
	e.mu.Lock()
	delete(e.tunnels, t)
	e.mu.Unlock()
	e.log.Info("egress tunnel", "ref", t.ref, "target", t.target, "bytes_up", up, "bytes_down", down, "ms", time.Since(start).Milliseconds())
}

type egressRefusal struct {
	ref, target, reason string
	status              int
	// counted: the connection held a slot, released when it ends.
	counted bool
}

// reserve takes a slot for one connection from ref's network.
func (e *Egress) reserve(ref string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.total >= e.maxConns || e.active[ref] >= e.maxTunnels {
		return false
	}
	e.total++
	e.active[ref]++
	return true
}

func (e *Egress) release(refusal *egressRefusal, t *tunnel) {
	ref := ""
	if t != nil {
		ref = t.ref
	} else {
		ref = refusal.ref
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.total--
	if e.active[ref]--; e.active[ref] <= 0 {
		delete(e.active, ref)
	}
}

// open reads the CONNECT, checks it, dials the checked address and
// registers the tunnel.
func (e *Egress) open(conn net.Conn) (*tunnel, *egressRefusal) {
	refuse := func(ref, target string, status int, reason string) (*tunnel, *egressRefusal) {
		if len(target) > 64 {
			target = target[:64] + "…"
		}
		return nil, &egressRefusal{ref: ref, target: strconv.Quote(target), status: status, reason: reason}
	}
	_ = conn.SetDeadline(time.Now().Add(egressHeaderWait))
	src, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		return refuse("", "", http.StatusForbidden, "unknown source")
	}
	e.mu.Lock()
	n := e.networkFor(src.Addr())
	var ref string
	var allow map[string]bool
	if n != nil {
		ref, allow = n.ref, n.allow
	}
	e.mu.Unlock()
	if n == nil {
		return refuse("", "", http.StatusForbidden, "the source is on no owner's network")
	}
	if !e.reserve(ref) {
		return refuse(ref, "", http.StatusServiceUnavailable, "too many connections")
	}
	counted := refuse
	refuse = func(ref, target string, status int, reason string) (*tunnel, *egressRefusal) {
		t, r := counted(ref, target, status, reason)
		r.counted = true
		return t, r
	}
	br := bufio.NewReaderSize(io.LimitReader(conn, maxConnectHeader), maxConnectHeader)
	req, err := http.ReadRequest(br)
	if err != nil {
		return refuse(ref, "", http.StatusBadRequest, "not an HTTP request")
	}
	if req.Method != http.MethodConnect {
		return refuse(ref, req.RequestURI, http.StatusMethodNotAllowed, "only CONNECT")
	}
	target, err := normalTarget(req.RequestURI)
	if err != nil || !allow[target] {
		return refuse(ref, req.RequestURI, http.StatusForbidden, "not on this owner's allow-list")
	}
	// The request's deadline ends here; the dial has its own.
	_ = conn.SetDeadline(time.Time{})
	host, portText, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portText)
	ctx, cancel := context.WithTimeout(context.Background(), egressDialWait)
	defer cancel()
	addrs, err := e.addresses(ctx, host)
	if err != nil {
		return refuse(ref, target, http.StatusForbidden, err.Error())
	}
	var upstream net.Conn
	for _, addr := range addrs {
		if upstream, err = e.dial(ctx, netip.AddrPortFrom(addr, uint16(port))); err == nil {
			break
		}
	}
	if upstream == nil {
		return refuse(ref, target, http.StatusBadGateway, "unreachable")
	}
	t := &tunnel{ref: ref, target: target, src: src.Addr().Unmap(), client: conn, upstream: upstream}
	e.mu.Lock()
	// The policy may have changed while dialling.
	n = e.networkByRef(ref)
	if n == nil || !n.allow[target] || !n.source.Contains(src.Addr().Unmap()) {
		e.mu.Unlock()
		upstream.Close()
		return refuse(ref, target, http.StatusForbidden, "not on this owner's allow-list")
	}
	e.tunnels[t] = struct{}{}
	e.mu.Unlock()
	fail := func(reason string) (*tunnel, *egressRefusal) {
		e.mu.Lock()
		delete(e.tunnels, t)
		e.mu.Unlock()
		upstream.Close()
		return refuse(ref, target, http.StatusBadGateway, reason)
	}
	// Bytes the client sent after the request (a TLS hello) go up only once
	// the tunnel is allowed and registered.
	if early, _ := br.Peek(br.Buffered()); len(early) > 0 {
		if _, err := upstream.Write(early); err != nil {
			return fail("unreachable")
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(egressHeaderWait))
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return fail("client gone")
	}
	return t, nil
}

// addresses resolves host and refuses the lot if any address is not public:
// a name that answers with a private address is refused, not filtered.
func (e *Egress) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		// Listed itself, and public (checked in compileEgress).
		return []netip.Addr{ip}, nil
	}
	addrs, err := e.resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, errors.New("the name does not resolve")
	}
	for _, a := range addrs {
		if !publicAddr(a) {
			return nil, errors.New("the name resolves to an address that is not public")
		}
	}
	return addrs, nil
}

// splice copies both ways until either side ends or stays idle for
// egressIdle, and returns the bytes sent up and down.
func (e *Egress) splice(t *tunnel) (up, down int64) {
	_ = t.client.SetDeadline(time.Time{})
	var wg sync.WaitGroup
	copyIdle := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(egressIdle))
			r, err := src.Read(buf)
			if r > 0 {
				w, werr := dst.Write(buf[:r])
				*n += int64(w)
				if werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		// Either side ending ends the tunnel.
		t.client.Close()
		t.upstream.Close()
	}
	wg.Add(2)
	go copyIdle(t.upstream, t.client, &up)
	go copyIdle(t.client, t.upstream, &down)
	wg.Wait()
	return up, down
}
