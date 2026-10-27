package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// preauthKeys bounds each table of client addresses kept before any token
// is checked (G9-1).
const preauthKeys = 4096

// addressKey is the client's address as a rate-limit key: an IPv4 address
// as it is, an IPv6 address as its /64, the block one subscriber gets, so
// rotating through it counts as one client (G9-1).
func addressKey(r *http.Request) string {
	address := clientAddress(r)
	ip := net.ParseIP(address)
	if ip == nil {
		return address
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
}

// preauthLimiter limits requests per client address before any token is
// checked. It is its own table, never the principals' limiter, so traffic
// that never signs in can't fill theirs; when full it evicts the oldest
// window rather than refuse a new address, so rotating addresses can't
// lock anyone out (G9-1).
type preauthLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	perMinute int
	seen      map[string]window
}

func newPreauthLimiter(perMinute int, now func() time.Time) *preauthLimiter {
	return &preauthLimiter{now: now, perMinute: perMinute, seen: map[string]window{}}
}

func (p *preauthLimiter) allow(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	v, held := p.seen[key]
	if !held || now.Sub(v.start) >= time.Minute {
		v = window{start: now}
	}
	if v.count >= p.perMinute {
		return false
	}
	if !held {
		evictOldest(p.seen, preauthKeys, now, time.Minute)
	}
	v.count++
	p.seen[key] = v
	return true
}

// evictOldest makes room for one more key in a table of at most max: it
// drops the windows past period, and when none is, the window that started
// first.
func evictOldest(seen map[string]window, max int, now time.Time, period time.Duration) {
	if len(seen) < max {
		return
	}
	oldest, first := "", now
	for k, w := range seen {
		if now.Sub(w.start) >= period {
			delete(seen, k)
		} else if !w.start.After(first) {
			oldest, first = k, w.start
		}
	}
	if len(seen) >= max {
		delete(seen, oldest)
	}
}
