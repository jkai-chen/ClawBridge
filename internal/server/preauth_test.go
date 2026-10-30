package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An IPv6 client counts as its /64; an IPv4 one as its address.
func TestAddressKey(t *testing.T) {
	key := func(peer, forwarded string) string {
		r := httptest.NewRequest("POST", IntakePath, nil)
		r.RemoteAddr = peer
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		return addressKey(mustProxies(t, "172.18.0.5").apply(r))
	}
	for _, c := range []struct{ peer, forwarded, want string }{
		{"203.0.113.9:4000", "", "203.0.113.9"},
		{"[2001:db8:1:2:3::4]:4000", "", "2001:db8:1:2::/64"},
		{"172.18.0.5:4000", "2001:db8:1:2:ffff::9", "2001:db8:1:2::/64"},
		{"172.18.0.5:4000", "2001:db8:1:3::9", "2001:db8:1:3::/64"},
		{"[::ffff:203.0.113.9]:4000", "", "203.0.113.9"},
	} {
		if got := key(c.peer, c.forwarded); got != c.want {
			t.Errorf("%s via %q: got %s, want %s", c.peer, c.forwarded, got, c.want)
		}
	}
}

// A full table evicts, never refuses a new address; an address over its
// limit waits for its window.
func TestPreauthLimiterEvicts(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newPreauthLimiter(2, func() time.Time { return now })
	for i := range preauthKeys {
		if !p.allow(fmt.Sprint("a", i)) {
			t.Fatal("refused", i)
		}
		now = now.Add(time.Millisecond)
	}
	if !p.allow("new") || len(p.seen) != preauthKeys {
		t.Fatal("a full table refused a new address", len(p.seen))
	}
	if _, kept := p.seen["a0"]; kept {
		t.Fatal("the oldest window was not the one evicted")
	}
	if !p.allow("new") || p.allow("new") {
		t.Fatal("the per-address limit")
	}
	now = now.Add(time.Minute)
	if !p.allow("new") {
		t.Fatal("a new window")
	}
}

// The join and failed-sign-in counters make room the same way.
func TestCounterEvictsWhenFull(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := newCounter(1, time.Hour, func() time.Time { return now })
	for i := range deviceCacheMax {
		c.add(fmt.Sprint("a", i))
	}
	if !c.add("new") || len(c.seen) != deviceCacheMax {
		t.Fatal("a full counter refused a new key", len(c.seen))
	}
}

// G9-1: intake traffic from rotating IPv6 addresses, never signed in,
// can't make /v1/execute refuse a signed-in principal; one /64 is limited
// as one client.
func TestIntakeFloodLeavesPrincipalsAlone(t *testing.T) {
	_, host := intakeServer(t)
	handler := host.Config.Handler
	post := func(path, forwarded, token string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set("X-Forwarded-For", forwarded)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := range 10001 {
		if code := post(IntakePath, fmt.Sprintf("2001:db8:%x:%x::1", i>>16, i&0xffff), "cbi_bad"); code != http.StatusUnauthorized {
			t.Fatal("intake answered", code, "at", i)
		}
	}
	if code := post("/v1/execute", "198.51.100.7", "google-token"); code == http.StatusTooManyRequests {
		t.Fatal("a signed-in principal was refused after an intake flood")
	}
	limited := 0
	for i := range 1201 {
		if post(IntakePath, fmt.Sprintf("2001:db8:ffff:1::%x", i+1), "cbi_bad") == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited != 1 {
		t.Fatal("one /64 is one client:", limited)
	}
}

// S2: with no trusted proxy configured (the binary's default), a Docker
// neighbour rotating X-Forwarded-For through the handler is one client.
func TestUntrustedPeerCannotRotateItsBucket(t *testing.T) {
	store := &fakeIntake{}
	handler := NewHandlerWithOptions(store, nil, HandlerOptions{})
	limited := 0
	for i := range 1201 {
		r := httptest.NewRequest("POST", IntakePath, strings.NewReader(`{}`))
		r.RemoteAddr = "172.18.0.7:4000"
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.%d.%d", i>>8, i&0xff))
		r.Header.Set("Authorization", "Bearer cbi_bad")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited != 1 {
		t.Fatal("the neighbour chose its own buckets:", limited)
	}
}

// G9-L7: the tick endpoint limits an address before it verifies a token.
func TestTickLimitsAnAddress(t *testing.T) {
	handler := NewHandlerWithOptions(&fakeIntake{}, nil, HandlerOptions{TickVerifier: tickCheck{"scheduler-token"}, Ticker: &countTicks{ticks: make(chan struct{}, 1)}})
	codes := map[int]int{}
	for range 61 {
		r := httptest.NewRequest("POST", TickPath, nil)
		r.RemoteAddr = "203.0.113.9:4000"
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		codes[w.Code]++
	}
	if codes[http.StatusUnauthorized] != 60 || codes[http.StatusTooManyRequests] != 1 {
		t.Fatal(codes)
	}
}

// blocked counts nothing; reject counts one refusal, per minute.
func TestPreauthLimiterBlockedAndReject(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newPreauthLimiter(2, func() time.Time { return now })
	for range 10 {
		if p.blocked("a") {
			t.Fatal("blocked with no refusal")
		}
	}
	p.reject("a")
	if p.blocked("a") {
		t.Fatal("blocked after one of two")
	}
	p.reject("a")
	if !p.blocked("a") || p.blocked("b") {
		t.Fatal("a not blocked at its limit, or b blocked")
	}
	now = now.Add(time.Minute)
	if p.blocked("a") {
		t.Fatal("still blocked after the minute")
	}
}
