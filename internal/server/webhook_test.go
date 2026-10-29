package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// Synthetic IDs and secrets only.
const (
	whTeam     = "11111111-1111-4111-8111-111111111111"
	whTeam2    = "33333333-3333-4333-8333-333333333333"
	whTeam3    = "55555555-5555-4555-8555-555555555555"
	whOrg      = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	whOrgOther = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	whHook     = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	whIssueID  = "00000000-0000-4000-8000-0000000000e1"
	whSecret   = "whsec-synthetic-0123456789abcdef"
	whSecret2  = "whsec-synthetic-rotated-fedcba9876"
	whSecret3  = "whsec-synthetic-team-two-00000000"
	whSecret4  = "whsec-synthetic-team-three-111111"
	whAddr     = "203.0.113.5"
	whAddr2    = "198.51.100.7"
	whFarPoll  = "2999-01-01T00:00:00.000000000Z"
)

var whStart = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fakeWebhook is the bus's webhook store: dedup on the delivery ID, an
// outcome from the bound teams and the gate, and every count.
type fakeWebhook struct {
	mu      sync.Mutex
	rows    map[string]bus.WebhookDelivery
	records int
	counts  map[string]int
	proven  bool
	bound   map[string][]string
	fail    bool
	calls   int
}

func newFakeWebhook() *fakeWebhook {
	return &fakeWebhook{rows: map[string]bus.WebhookDelivery{}, counts: map[string]int{}, bound: map[string][]string{whTeam: {"pid-1"}}}
}

func (f *fakeWebhook) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("not used")
}

func (f *fakeWebhook) WebhookRecord(_ context.Context, d bus.WebhookDelivery) (bus.WebhookResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records++
	if f.fail {
		return bus.WebhookResult{}, errors.New("database down")
	}
	if _, ok := f.rows[d.DeliveryID]; ok {
		f.counts[d.TeamID+"/duplicate"]++
		return bus.WebhookResult{Outcome: "duplicate"}, nil
	}
	f.rows[d.DeliveryID] = d
	projects := f.bound[d.TeamID]
	switch {
	case len(projects) == 0:
		return bus.WebhookResult{Outcome: "unbound_team"}, nil
	case !f.proven:
		return bus.WebhookResult{Outcome: "inert"}, nil
	}
	return bus.WebhookResult{Outcome: "applied", Projects: append([]string(nil), projects...)}, nil
}

func (f *fakeWebhook) WebhookCount(_ context.Context, team, reason string, n int, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.counts[team+"/"+reason] += n
	return nil
}

func (f *fakeWebhook) count(team, reason string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[team+"/"+reason]
}

func (f *fakeWebhook) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeWebhook) recordCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records
}

// whEnv is a webhook handler on a test clock, its nudges on a channel.
type whEnv struct {
	t      *testing.T
	store  WebhookStore
	h      *webhookHandler
	clock  *drainClock
	nudges chan string
}

func newWhEnv(t *testing.T, store WebhookStore, opts ...func(*WebhookOptions, *HandlerOptions)) *whEnv {
	t.Helper()
	clock := &drainClock{now: whStart}
	e := &whEnv{t: t, store: store, clock: clock, nudges: make(chan string, 1024)}
	wo := &WebhookOptions{
		Secrets: map[string][]string{whTeam: {whSecret}},
		OrgID:   func(context.Context) (string, error) { return whOrg, nil },
		Nudge:   func(pid string) { e.nudges <- pid },
	}
	ho := &HandlerOptions{Now: clock.Now}
	for _, o := range opts {
		o(wo, ho)
	}
	e.h = newWebhookHandler(store, wo, ho.Now, ho.Drain)
	// Counter flushes run in the request, so a test reads them at once.
	e.h.spawn = func(f func()) { f() }
	return e
}

func whSign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// whPayload is an Issue update by default; n makes data.id unique.
func whPayload(n int, kind, action, team string, at time.Time) map[string]any {
	return map[string]any{
		"type": kind, "action": action, "organizationId": whOrg, "webhookTimestamp": at.UnixMilli(), "webhookId": whHook,
		"url":   "https://linear.example.test/issue/X-1",
		"actor": map[string]any{"id": "00000000-0000-4000-8000-0000000000a1", "name": "Some Person"},
		"data": map[string]any{"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", n), "teamId": team, "issueId": whIssueID,
			"title": "a title", "description": "a description", "body": "a comment body"},
		"updatedFrom": map[string]any{"stateId": "00000000-0000-4000-8000-0000000000d1"},
	}
}

func whBody(t *testing.T, p map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// post sends body signed under secret ("" sends no signature) from addr.
// whSecretTeam is each test secret's team; any other secret posts to
// whTeam's URL.
var whSecretTeam = map[string]string{whSecret: whTeam, whSecret2: whTeam, whSecret3: whTeam2, whSecret4: whTeam3}

func (e *whEnv) post(addr string, body []byte, secret string) *httptest.ResponseRecorder {
	return e.postWith(addr, body, func(r *http.Request) {
		if team, ok := whSecretTeam[secret]; ok {
			r.URL.Path = LinearWebhookPath + team
		}
		if secret != "" {
			r.Header.Set("Linear-Signature", whSign(secret, body))
		}
	})
}

func (e *whEnv) postWith(addr string, body []byte, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, LinearWebhookPath+whTeam, bytes.NewReader(body))
	r.RemoteAddr = addr + ":443"
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// good is a valid signed delivery n for whTeam at the clock's time.
func (e *whEnv) good(n int) []byte {
	return whBody(e.t, whPayload(n, "Issue", "update", whTeam, e.clock.Now()))
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error body: %d %q", w.Code, w.Body.String())
	}
	return b.Error.Code
}

func (e *whEnv) rejectsOf(addr string) int {
	e.h.rejects.mu.Lock()
	defer e.h.rejects.mu.Unlock()
	return e.h.rejects.seen[addr].count
}

// countingMAC counts every HMAC the handler computes.
func countingMAC(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	old := webhookMAC
	webhookMAC = func(key []byte) hash.Hash {
		n.Add(1)
		return old(key)
	}
	t.Cleanup(func() { webhookMAC = old })
	return &n
}

// R2 step 1 [C1].
func TestWebhookPerIPRejectLimit(t *testing.T) {
	e := newWhEnv(t, newFakeWebhook())
	for i := 1; i <= 31; i++ {
		w := e.post(whAddr, e.good(i), "wrong-secret-wrong-secret")
		want := 401
		if i == 31 {
			want = 429
		}
		if w.Code != want {
			t.Fatal("post", i, w.Code)
		}
	}
	if w := e.post(whAddr2, e.good(100), whSecret); w.Code != 200 {
		t.Fatal("another address was refused:", w.Code, w.Body.String())
	}
}

// R2 steps 1 and 8a [C1]: verified deliveries never count as rejects.
func TestWebhookVerifiedBurstFromOneAddress(t *testing.T) {
	e := newWhEnv(t, newFakeWebhook())
	for i := 0; i < 200; i++ {
		if w := e.post(whAddr, e.good(i), whSecret); w.Code != 200 {
			t.Fatal("delivery", i, w.Code, w.Body.String())
		}
	}
	if n := e.rejectsOf(whAddr); n != 0 {
		t.Fatal("verified deliveries counted as rejects:", n)
	}
}

// R2 step 8a: the server-wide verified ceiling.
func TestWebhookVerifiedCeiling(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	for i := 0; i < 600; i++ {
		// Spread over addresses: the ceiling is server-wide.
		if w := e.post(fmt.Sprintf("203.0.113.%d", i%200), e.good(i), whSecret); w.Code != 200 {
			t.Fatal("delivery", i, w.Code)
		}
	}
	w := e.post(whAddr2, e.good(600), whSecret)
	if w.Code != 429 || errCode(t, w) != "verified_throttled" {
		t.Fatal("the 601st:", w.Code, w.Body.String())
	}
	if e.rejectsOf(whAddr2) != 0 {
		t.Fatal("a throttled verified delivery counted as a reject")
	}
	e.h.flushCounts(context.Background())
	if store.count(whTeam, "verified_throttled") != 1 {
		t.Fatal("verified_throttled not counted:", store.counts)
	}
	if store.rowCount() != 600 {
		t.Fatal("the throttled delivery was recorded")
	}
	// A minute on, the ceiling resets.
	e.clock.add(time.Minute)
	if w := e.post(whAddr2, e.good(601), whSecret); w.Code != 200 {
		t.Fatal("after the minute:", w.Code)
	}
}

// R2 step 2: a drain answers 503 before the body is read.
func TestWebhookDrain(t *testing.T) {
	drain, _, _ := newTestDrain(on(drainStart, 10*time.Minute))
	store := newFakeWebhook()
	mac := countingMAC(t)
	e := newWhEnv(t, store, func(_ *WebhookOptions, h *HandlerOptions) { h.Drain = drain })
	body := &countingReader{r: bytes.NewReader(e.good(1))}
	r := httptest.NewRequest(http.MethodPost, LinearWebhookPath+whTeam, body)
	r.RemoteAddr = whAddr + ":443"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Linear-Signature", whSign(whSecret, e.good(1)))
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	var retry int
	if _, err := fmt.Sscan(w.Header().Get("Retry-After"), &retry); err != nil || retry < 1 || retry > 30 {
		t.Fatal("Retry-After:", w.Header().Get("Retry-After"))
	}
	if body.n != 0 || mac.Load() != 0 || store.recordCalls() != 0 || store.rowCount() != 0 {
		t.Fatal("drained delivery read or recorded:", body.n, mac.Load(), store.recordCalls())
	}
}

type countingReader struct {
	r *bytes.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// R2 step 3.
func TestWebhookContentType(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	body := e.good(1)
	for _, ct := range []string{"text/plain", "", "application/jsonx"} {
		w := e.postWith(whAddr, body, func(r *http.Request) {
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Linear-Signature", whSign(whSecret, body))
		})
		if w.Code != 415 {
			t.Fatal(ct, w.Code)
		}
	}
	if store.rowCount() != 0 || e.rejectsOf(whAddr) != 3 {
		t.Fatal("recorded, or not counted as rejects", store.rowCount(), e.rejectsOf(whAddr))
	}
}

// R2 step 4: the cap refuses before any HMAC.
func TestWebhookBodyCap(t *testing.T) {
	store := newFakeWebhook()
	mac := countingMAC(t)
	e := newWhEnv(t, store)
	big := bytes.Repeat([]byte(" "), webhookBodyCap+1)
	w := e.post(whAddr, big, whSecret)
	if w.Code != 413 || errCode(t, w) != "too_large" {
		t.Fatal(w.Code, w.Body.String())
	}
	// Without a Content-Length too: the reader's cap catches it.
	w = e.postWith(whAddr, big, func(r *http.Request) {
		r.ContentLength = -1
		r.Header.Set("Linear-Signature", strings.Repeat("0", 64))
	})
	if w.Code != 413 {
		t.Fatal("streamed:", w.Code)
	}
	if mac.Load() != 0 {
		t.Fatal("an HMAC ran on an oversized body:", mac.Load())
	}
	e.h.flushCounts(context.Background())
	if store.count("", "too_large") != 2 || store.rowCount() != 0 {
		t.Fatal("too_large not counted unattributed:", store.counts)
	}
}

// R2 step 5.
func TestWebhookSignatureHeader(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	body := e.good(1)
	good := whSign(whSecret, body)
	for name, edit := range map[string]func(*http.Request){
		"missing": func(*http.Request) {},
		"doubled": func(r *http.Request) {
			r.Header.Add("Linear-Signature", good)
			r.Header.Add("Linear-Signature", good)
		},
		"non-hex": func(r *http.Request) { r.Header.Set("Linear-Signature", "zz"+good[2:]) },
		"short":   func(r *http.Request) { r.Header.Set("Linear-Signature", good[:62]) },
	} {
		w := e.postWith(whAddr, body, edit)
		if w.Code != 401 || errCode(t, w) != "signature" {
			t.Fatal(name, w.Code, w.Body.String())
		}
	}
	e.h.flushCounts(context.Background())
	if store.rowCount() != 0 || store.count("", "signature") != 4 {
		t.Fatal(store.rowCount(), store.counts)
	}
}

// R2 step 6: every secret is tried every time.
func TestWebhookHMAC(t *testing.T) {
	store := newFakeWebhook()
	mac := countingMAC(t)
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Secrets = map[string][]string{whTeam: {whSecret, whSecret2}, whTeam2: {whSecret3}}
	})
	body := e.good(1)
	sig := whSign("not-a-configured-secret-at-all", body)
	before := mac.Load()
	w := e.postWith(whAddr, body, func(r *http.Request) { r.Header.Set("Linear-Signature", sig) })
	if w.Code != 401 || errCode(t, w) != "signature" {
		t.Fatal("wrong key:", w.Code)
	}
	if mac.Load()-before != 2 {
		t.Fatal("not exactly the team's two secrets were tried:", mac.Load()-before)
	}
	sig = whSign(whSecret2, body)
	before = mac.Load()
	w = e.postWith(whAddr, body, func(r *http.Request) { r.Header.Set("Linear-Signature", sig) })
	if w.Code != 200 {
		t.Fatal("the rotation's second entry:", w.Code, w.Body.String())
	}
	if mac.Load()-before != 2 {
		t.Fatal("a match stopped the loop early, or another team's secret ran:", mac.Load()-before)
	}
	// whTeam2's URL: its one secret only.
	before = mac.Load()
	if w = e.post(whAddr, whBody(t, whPayload(2, "Issue", "update", whTeam2, whStart)), whSecret3); w.Code != 200 || mac.Load()-before != 1 {
		t.Fatal("team two:", w.Code, mac.Load()-before)
	}
	e.h.flushCounts(context.Background())
	if store.count("", "signature") != 1 {
		t.Fatal(store.counts)
	}
}

// R2 step 7.
func TestWebhookOrgTeam(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Secrets = map[string][]string{whTeam: {whSecret}, whTeam2: {whSecret3}}
	})
	p := whPayload(1, "Issue", "update", whTeam, whStart)
	p["organizationId"] = whOrgOther
	w := e.post(whAddr, whBody(t, p), whSecret)
	if w.Code != 401 || errCode(t, w) != "org_mismatch" {
		t.Fatal("org:", w.Code, w.Body.String())
	}
	// whTeam's payload signed with whTeam2's secret.
	w = e.post(whAddr, e.good(2), whSecret3)
	if w.Code != 401 || errCode(t, w) != "team_mismatch" {
		t.Fatal("team:", w.Code, w.Body.String())
	}
	e.h.flushCounts(context.Background())
	if store.count(whTeam, "org_mismatch") != 1 || store.count(whTeam2, "team_mismatch") != 1 {
		t.Fatal("not counted under the matched team:", store.counts)
	}
	if store.rowCount() != 0 || e.rejectsOf(whAddr) != 0 {
		t.Fatal(store.rowCount(), e.rejectsOf(whAddr))
	}
	// An org in another case is the same org.
	p = whPayload(3, "Issue", "update", strings.ToUpper(whTeam), whStart)
	p["organizationId"] = strings.ToUpper(whOrg)
	if w = e.post(whAddr, whBody(t, p), whSecret); w.Code != 200 {
		t.Fatal("upper case:", w.Code, w.Body.String())
	}
	// The identity unreadable: 503, nothing recorded.
	e2 := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.OrgID = func(context.Context) (string, error) { return "", errors.New("down") }
	})
	w = e2.post(whAddr2, e2.good(4), whSecret)
	if w.Code != 503 || errCode(t, w) != "unavailable" || w.Header().Get("Retry-After") == "" {
		t.Fatal("identity down:", w.Code, w.Body.String())
	}
	if store.rowCount() != 1 || e2.rejectsOf(whAddr2) != 0 {
		t.Fatal("identity down recorded or rejected:", store.rowCount())
	}
}

// A delivery with no data.teamId is its secret's team's; another team is
// still refused.
func TestWebhookEmptyTeamIsTheSecrets(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Secrets = map[string][]string{whTeam: {whSecret}, whTeam2: {whSecret3}}
	})
	p := whPayload(1, "Comment", "create", "", whStart)
	delete(p["data"].(map[string]any), "teamId")
	if w := e.post(whAddr, whBody(t, p), whSecret3); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, d := range store.rows {
		if d.TeamID != whTeam2 {
			t.Fatal("recorded under", d.TeamID)
		}
	}
	if w := e.post(whAddr, whBody(t, whPayload(2, "Comment", "create", whTeam, whStart)), whSecret3); w.Code != 401 || errCode(t, w) != "team_mismatch" {
		t.Fatal(w.Code, w.Body.String())
	}
}

// C1: refusals after the HMAC verified come from Linear itself and never
// count against its address.
func TestWebhookVerifiedRefusalsNeverThrottle(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	for i := 0; i < 40; i++ {
		body := whBody(t, whPayload(i, "Issue", "update", whTeam, whStart.Add(-10*time.Minute)))
		if w := e.post(whAddr, body, whSecret); w.Code != 401 || errCode(t, w) != "stale" {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	p := whPayload(50, "Issue", "update", whTeam, whStart)
	p["organizationId"] = whOrgOther
	e.post(whAddr, whBody(t, p), whSecret)
	e.post(whAddr, []byte("not json"), whSecret)
	if w := e.post(whAddr, e.good(100), whSecret); w.Code != 200 {
		t.Fatal("a fresh delivery after:", w.Code, w.Body.String())
	}
	e.h.flushCounts(context.Background())
	if e.rejectsOf(whAddr) != 0 || store.count(whTeam, "stale") != 40 {
		t.Fatal(e.rejectsOf(whAddr), store.counts)
	}
}

// R2 step 8.
func TestWebhookWindow(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	for i, off := range []time.Duration{61 * time.Second, -61 * time.Second} {
		w := e.post(whAddr, whBody(t, whPayload(i, "Issue", "update", whTeam, whStart.Add(off))), whSecret)
		if w.Code != 401 || errCode(t, w) != "stale" {
			t.Fatal(off, w.Code, w.Body.String())
		}
	}
	for i, off := range []time.Duration{59 * time.Second, -59 * time.Second} {
		if w := e.post(whAddr, whBody(t, whPayload(10+i, "Issue", "update", whTeam, whStart.Add(off))), whSecret); w.Code != 200 {
			t.Fatal(off, w.Code, w.Body.String())
		}
	}
	e.h.flushCounts(context.Background())
	if store.count(whTeam, "stale") != 2 || store.rowCount() != 2 {
		t.Fatal(store.counts, store.rowCount())
	}
	// Dedup must outlast the window, or a replay inside it could pass
	// (bus.webhookRetention is 7 days; TestWebhookRetentionLongerThanWindow
	// pins the bus side).
	if webhookWindow != bus.WebhookWindow || webhookWindow >= 7*24*time.Hour {
		t.Fatal("the window is not the bus's, or outlasts the retention")
	}
}

// R2 step 9: one Linear-Delivery is one delivery, whatever its body (a
// retry may carry a fresh webhookTimestamp).
func TestWebhookDedupOnLinearDelivery(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	const id = "0000000A-0000-4000-8000-00000000DD01"
	for i := 0; i < 2; i++ {
		body := whBody(t, whPayload(1, "Issue", "update", whTeam, whStart.Add(time.Duration(i)*time.Second)))
		w := e.postWith(whAddr, body, func(r *http.Request) {
			r.Header.Set("Linear-Signature", whSign(whSecret, body))
			r.Header.Set("Linear-Delivery", id)
		})
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if _, ok := store.rows[strings.ToLower(id)]; !ok || store.rowCount() != 1 || store.count(whTeam, "duplicate") != 1 {
		t.Fatal("the second body was not a duplicate:", store.rows, store.counts)
	}
	// A malformed or doubled header falls back to the body's hash.
	for i, edit := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Linear-Delivery", "not-a-uuid") },
		func(r *http.Request) { r.Header.Add("Linear-Delivery", id); r.Header.Add("Linear-Delivery", id) },
	} {
		body := e.good(10 + i)
		w := e.postWith(whAddr, body, func(r *http.Request) { r.Header.Set("Linear-Signature", whSign(whSecret, body)); edit(r) })
		sum := sha256.Sum256(body)
		if _, ok := store.rows["sha256:"+hex.EncodeToString(sum[:])]; w.Code != 200 || !ok {
			t.Fatal(i, w.Code, "no fallback key")
		}
	}
}

// R2 step 9, without the header: the body's hash is the key.
func TestWebhookDedup(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	body := e.good(1)
	for i := 0; i < 2; i++ {
		if w := e.post(whAddr, body, whSecret); w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if store.rowCount() != 1 || store.count(whTeam, "duplicate") != 1 {
		t.Fatal(store.rowCount(), store.counts)
	}
	sum := sha256.Sum256(body)
	d, ok := store.rows["sha256:"+hex.EncodeToString(sum[:])]
	if !ok {
		t.Fatal("the dedup key is not the body's hash:", store.rows)
	}
	// The body's length goes to the store for the probe's evidence (Q2).
	if d.Size != len(body) {
		t.Fatal("size:", d.Size, len(body))
	}
}

// whReal is a SQLite bus store and a raw connection to its file.
func whReal(t *testing.T) (*bus.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus.db")
	store, err := bus.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return store, db
}

func whScalar(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(q, err)
	}
	return n
}

// whBind binds project pid to teams with far-future intake cursors, as
// rows: the binding's operations are the bus's, tested there.
func whBind(t *testing.T, db *sql.DB, pid string, teams ...string) {
	t.Helper()
	mapping, _ := json.Marshal(map[string]any{"linear_team_ids": teams})
	stamp := whStart.Format("2006-01-02T15:04:05.000000000Z")
	if _, err := db.Exec(`INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at) VALUES(?,1,?,?)`, pid, string(mapping), stamp); err != nil {
		t.Fatal(err)
	}
	cursors := [][2]string{{"intake-s2", "app"}, {"intake-s3", "app"}}
	for _, team := range teams {
		cursors = append(cursors, [2]string{"intake-s1", team})
	}
	for _, c := range cursors {
		if _, err := db.Exec(`INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,updated_at) VALUES(?,?,?,?,?,?,?)`, pid, c[0], c[1], stamp, stamp, whFarPoll, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func whNext(t *testing.T, db *sql.DB, pid, source, subject string) string {
	t.Helper()
	var next string
	if err := db.QueryRow(`SELECT next_poll_at FROM watch_cursors WHERE project_id=? AND source=? AND subject=?`, pid, source, subject).Scan(&next); err != nil {
		t.Fatal(err)
	}
	return next
}

// R2 step 10 [C2]: nothing is written before the HMAC verifies.
func TestWebhookWriteOnlyAfterVerify(t *testing.T) {
	store, db := whReal(t)
	if err := store.WebhookTeamsConfigured(context.Background(), []string{whTeam}, whStart); err != nil {
		t.Fatal(err)
	}
	e := newWhEnv(t, store)
	e.h.rejects = newPreauthLimiter(1<<30, e.clock.Now) // the flood is the point here
	for i := 0; i < 1000; i++ {
		secret := ""
		if i%2 == 1 {
			secret = "mis-signed-secret-mis-signed"
		}
		if w := e.post(fmt.Sprintf("203.0.113.%d", i%250), e.good(i), secret); w.Code != 401 {
			t.Fatal(i, w.Code)
		}
	}
	e.clock.add(time.Minute)
	e.h.flushCounts(context.Background())
	if n := whScalar(t, db, `SELECT COUNT(*) FROM webhook_deliveries`); n != 0 {
		t.Fatal("deliveries:", n)
	}
	if n := whScalar(t, db, `SELECT COUNT(*) FROM webhook_teams`); n != 1 {
		t.Fatal("team rows:", n)
	}
	if n := whScalar(t, db, `SELECT COALESCE(SUM(n),0) FROM webhook_counts WHERE team_id='' AND reason='signature'`); n != 1000 {
		t.Fatal("signature count:", n)
	}
	if n := whScalar(t, db, `SELECT COUNT(*) FROM webhook_counts`); n > 2 {
		t.Fatal("the counters grew per request:", n)
	}
}

// R2 step 10, the gate.
func TestWebhookGate(t *testing.T) {
	store, db := whReal(t)
	pid := "00000000-0000-4000-8000-0000000000p1"
	whBind(t, db, pid, whTeam)
	e := newWhEnv(t, store)
	if w := e.post(whAddr, e.good(1), whSecret); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if n := whScalar(t, db, `SELECT COUNT(*) FROM webhook_deliveries WHERE outcome='inert'`); n != 1 {
		t.Fatal("not inert:", n)
	}
	if whNext(t, db, pid, "intake-s1", whTeam) != whFarPoll || whNext(t, db, pid, "intake-s2", "app") != whFarPoll {
		t.Fatal("the gate off marked a cursor")
	}
	select {
	case got := <-e.nudges:
		t.Fatal("the gate off nudged", got)
	case <-time.After(50 * time.Millisecond):
	}
	store.SetWebhooksProven(true)
	if w := e.post(whAddr, e.good(2), whSecret); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if whNext(t, db, pid, "intake-s1", whTeam) == whFarPoll || whNext(t, db, pid, "intake-s2", "app") == whFarPoll {
		t.Fatal("the gate on marked no cursor")
	}
	select {
	case got := <-e.nudges:
		if got != pid {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no nudge")
	}
	select {
	case got := <-e.nudges:
		t.Fatal("two nudges", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// R2 step 11: 200 with no body, and a blocked nudger never holds it.
func TestWebhookAnswer(t *testing.T) {
	store := newFakeWebhook()
	store.proven = true
	release := make(chan struct{})
	defer close(release)
	var called atomic.Int64
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Nudge = func(string) { called.Add(1); <-release }
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.post(whAddr, e.good(1), whSecret) }()
	select {
	case w := <-done:
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked nudger held the answer")
	}
	deadline := time.Now().Add(2 * time.Second)
	for called.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if called.Load() != 1 {
		t.Fatal("nudged", called.Load())
	}
	// A nil nudger is fine.
	e2 := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) { o.Nudge = nil })
	if w := e2.post(whAddr, e2.good(2), whSecret); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

// R4 [C5]: updatedFrom's values are never read, kept or echoed.
func TestWebhookUpdatedFromValuesNeverKept(t *testing.T) {
	store, db := whReal(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := newWhEnv(t, store)
	const marker = "MARKER-7f3a"
	p := whPayload(1, "Issue", "update", whTeam, whStart)
	p["updatedFrom"] = map[string]any{"title": marker + "-title", "labelIds": []string{marker + "-label"}, "description": map[string]any{"x": marker}}
	w := e.post(whAddr, whBody(t, p), whSecret)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// And a refused one's answer.
	p["organizationId"] = whOrgOther
	refused := e.post(whAddr, whBody(t, p), whSecret)
	if strings.Contains(w.Body.String()+refused.Body.String(), marker) || strings.Contains(logs.String(), marker) {
		t.Fatal("the marker reached an answer or the log")
	}
	for _, table := range []string{"webhook_deliveries", "webhook_teams", "webhook_counts"} {
		rows, err := db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(fmt.Sprint(vals...), marker) || strings.Contains(fmt.Sprint(vals...), "a title") {
				t.Fatal("payload text stored in", table, vals)
			}
		}
		rows.Close()
	}
	// The decoder keeps the key names only.
	d, ok := decodeWebhook(whBody(t, whPayload(2, "Issue", "update", whTeam, whStart)))
	if !ok || strings.Join(d.UpdatedKeys, ",") != "stateId" {
		t.Fatal(d, ok)
	}
}

// §5 [C4]: the payload's team picks the bound project; an unbound team is
// recorded as such.
func TestWebhookBoundTeam(t *testing.T) {
	store, db := whReal(t)
	store.SetWebhooksProven(true)
	pid := "00000000-0000-4000-8000-0000000000p1"
	whBind(t, db, pid, whTeam, whTeam2)
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Secrets = map[string][]string{whTeam: {whSecret}, whTeam2: {whSecret3}, whTeam3: {whSecret4}}
	})
	if w := e.post(whAddr, whBody(t, whPayload(1, "Issue", "create", whTeam2, whStart)), whSecret3); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if whNext(t, db, pid, "intake-s1", whTeam2) == whFarPoll {
		t.Fatal("the new team's cursor was not marked")
	}
	if whNext(t, db, pid, "intake-s1", whTeam) != whFarPoll {
		t.Fatal("another team's cursor was marked")
	}
	if w := e.post(whAddr, whBody(t, whPayload(2, "Issue", "create", whTeam3, whStart)), whSecret4); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if n := whScalar(t, db, `SELECT COUNT(*) FROM webhook_deliveries WHERE team_id=? AND outcome='unbound_team'`, whTeam3); n != 1 {
		t.Fatal("unbound:", n)
	}
}

// §5 Q10: a comment storm for one project makes one pass a minute.
func TestWebhookCommentBurstOnePass(t *testing.T) {
	store := newFakeWebhook()
	store.proven = true
	p := &passes{}
	n, _ := testNudger(p, oneAtATime(&countTicks{ticks: make(chan struct{}, 8)}))
	var wg sync.WaitGroup
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Nudge = func(pid string) { n.NudgeProject(pid); wg.Done() }
	})
	wg.Add(100)
	for i := 0; i < 100; i++ {
		if w := e.post(whAddr, whBody(t, whPayload(i, "Comment", "create", whTeam, whStart)), whSecret); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	wg.Wait()
	n.serve(context.Background())
	n.serve(context.Background())
	if p.count() != 1 {
		t.Fatal("passes:", p.pids)
	}
}

// Freshness is checked before dedup: a replay outside the window is stale,
// never a duplicate, and is never recorded.
func TestWebhookFreshnessBeforeDedup(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	recorded := e.good(1)
	if w := e.post(whAddr, recorded, whSecret); w.Code != 200 {
		t.Fatal(w.Code)
	}
	calls := store.recordCalls()
	e.clock.add(10 * time.Minute)
	fresh := whBody(t, whPayload(2, "Issue", "update", whTeam, whStart))
	for name, body := range map[string][]byte{"recorded": recorded, "new": fresh} {
		w := e.post(whAddr2, body, whSecret)
		if w.Code != 401 || errCode(t, w) != "stale" {
			t.Fatal(name, w.Code, w.Body.String())
		}
	}
	if store.recordCalls() != calls {
		t.Fatal("WebhookRecord ran for a stale delivery")
	}
}

// A store error answers 503 for Linear to retry; the retry is applied.
func TestWebhookStoreErrorIsRetried(t *testing.T) {
	store := newFakeWebhook()
	store.proven = true
	store.fail = true
	e := newWhEnv(t, store)
	body := e.good(1)
	w := e.post(whAddr, body, whSecret)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || errCode(t, w) != "unavailable" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	select {
	case got := <-e.nudges:
		t.Fatal("nudged on a failure", got)
	case <-time.After(50 * time.Millisecond):
	}
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	if w = e.post(whAddr, body, whSecret); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if store.count(whTeam, "duplicate") != 0 || store.rowCount() != 1 {
		t.Fatal("the retry was deduplicated:", store.counts)
	}
	select {
	case <-e.nudges:
	case <-time.After(2 * time.Second):
		t.Fatal("the retry did not nudge")
	}
}

// A signed body that isn't the typed shape is refused and records nothing.
func TestWebhookVerifiedButUndecodable(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	bad := map[string][]byte{
		"not json":     []byte("not json"),
		"array":        []byte("[1,2]"),
		"no entity":    []byte(`{"type":"Issue","action":"update","organizationId":"` + whOrg + `","webhookTimestamp":1,"data":{"teamId":"` + whTeam + `"}}`),
		"bad entity":   whBody(t, map[string]any{"type": "Issue", "action": "update", "organizationId": whOrg, "webhookTimestamp": whStart.UnixMilli(), "data": map[string]any{"id": "x'; DROP", "teamId": whTeam}}),
		"bad type":     whBody(t, map[string]any{"type": "Issue<script>", "action": "update", "organizationId": whOrg, "webhookTimestamp": whStart.UnixMilli(), "data": map[string]any{"id": whIssueID, "teamId": whTeam}}),
		"wrong typing": []byte(`{"type":1,"action":"update"}`),
	}
	for name, body := range bad {
		w := e.post(whAddr, body, whSecret)
		if w.Code != 400 || errCode(t, w) != "bad_payload" {
			t.Fatal(name, w.Code, w.Body.String())
		}
	}
	e.h.flushCounts(context.Background())
	if store.recordCalls() != 0 || store.count(whTeam, "bad_payload") != len(bad) {
		t.Fatal(store.recordCalls(), store.counts)
	}
}

// The route exists only with a secret and the app; any other method is
// 405.
func TestWebhookRouteExistsOnlyWhenConfigured(t *testing.T) {
	org := func(context.Context) (string, error) { return whOrg, nil }
	for name, o := range map[string]*WebhookOptions{
		"none":       nil,
		"no secrets": {OrgID: org},
		"no app":     {Secrets: map[string][]string{whTeam: {whSecret}}},
	} {
		h := NewHandlerWithOptions(newFakeWebhook(), nil, HandlerOptions{Webhook: o})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, LinearWebhookPath+whTeam, strings.NewReader("{}")))
		if w.Code != 404 || errCode(t, w) != "not_found" {
			t.Fatal(name, w.Code)
		}
	}
	h := NewHandlerWithOptions(newFakeWebhook(), nil, HandlerOptions{Webhook: &WebhookOptions{Secrets: map[string][]string{whTeam: {whSecret}}, OrgID: org}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, LinearWebhookPath+whTeam, nil))
	if w.Code != 405 {
		t.Fatal("GET:", w.Code)
	}
	// The bare path names no team: 404, like an unknown path.
	for _, path := range []string{"/linear/webhook", LinearWebhookPath} {
		w = httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, r)
		if w.Code != 404 || errCode(t, w) != "not_found" {
			t.Fatal(path, w.Code)
		}
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, LinearWebhookPath+whTeam, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("POST:", w.Code)
	}
}

// The org ID is read once per 10 minutes; a failure isn't cached.
func TestWebhookOrgIDCached(t *testing.T) {
	clock := &drainClock{now: whStart}
	calls, fail := 0, true
	get := cachedOrgID(func(context.Context) (string, error) {
		calls++
		if fail {
			return "", errors.New("down")
		}
		return strings.ToUpper(whOrg), nil
	}, clock.Now)
	if _, err := get(context.Background()); err == nil {
		t.Fatal("a failure passed")
	}
	fail = false
	for i := 0; i < 3; i++ {
		if id, err := get(context.Background()); err != nil || id != whOrg {
			t.Fatal(id, err)
		}
	}
	if calls != 2 {
		t.Fatal("calls:", calls)
	}
	clock.add(10 * time.Minute)
	if _, _ = get(context.Background()); calls != 3 {
		t.Fatal("not re-read after 10 min:", calls)
	}
}

func TestParseServeWebhookSecrets(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	env := func(secrets string, app bool) func(string) string {
		return func(k string) string {
			switch {
			case k == "CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS":
				return secrets
			case k == "CLAWBRIDGE_LINEAR_APP_CLIENT_ID" && app:
				return "client-id"
			}
			return ""
		}
	}
	upper := strings.ToUpper(whTeam)
	s, err := ParseServe(base, env(upper+"="+whSecret+","+whTeam+"="+whSecret2+","+whTeam2+"="+whSecret3, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.WebhookSecrets) != 2 || strings.Join(s.WebhookSecrets[whTeam], " ") != whSecret+" "+whSecret2 || s.WebhookSecrets[whTeam2][0] != whSecret3 {
		t.Fatal(s.WebhookSecrets)
	}
	// A secret may hold '='.
	if s, err = ParseServe(base, env(whTeam+"=abc=def=ghi=jkl=mnop", true)); err != nil || s.WebhookSecrets[whTeam][0] != "abc=def=ghi=jkl=mnop" {
		t.Fatal(s.WebhookSecrets, err)
	}
	if s, err = ParseServe(base, env("", false)); err != nil || len(s.WebhookSecrets) != 0 {
		t.Fatal("none:", err)
	}
	many := []string{}
	for i := 0; i < 33; i++ {
		many = append(many, fmt.Sprintf("00000000-0000-4000-8000-%012d=%s-%02d", i, whSecret, i))
	}
	for name, v := range map[string]string{
		"no team":        "=" + whSecret,
		"not a uuid":     "team-one=" + whSecret,
		"no secret":      whTeam + "=",
		"no equals":      whTeam,
		"short":          whTeam + "=short",
		"long":           whTeam + "=" + strings.Repeat("x", 257),
		"space":          whTeam + "=whsec synthetic 0123456789",
		"control":        whTeam + "=whsec-synthetic-\t0123456789",
		"non-ascii":      whTeam + "=whsec-synthetic-é0123456789",
		"empty entry":    whTeam + "=" + whSecret + ",",
		"three per team": whTeam + "=" + whSecret + "," + whTeam + "=" + whSecret2 + "," + whTeam + "=" + whSecret3,
		"same twice":     whTeam + "=" + whSecret + "," + whTeam + "=" + whSecret,
		"shared":         whTeam + "=" + whSecret + "," + whTeam2 + "=" + whSecret,
		"33 in total":    strings.Join(many, ","),
	} {
		_, err := ParseServe(base, env(v, true))
		if err == nil {
			t.Fatal(name, "was accepted")
		}
		if strings.Contains(err.Error(), "whsec") || strings.Contains(err.Error(), "short") {
			t.Fatal(name, "error shows the secret:", err)
		}
	}
	if _, err = ParseServe(base, env(whTeam+"="+whSecret, false)); err == nil || !strings.Contains(err.Error(), "Linear app") {
		t.Fatal("secrets without an app:", err)
	}
	// The gate.
	gate := func(v string) func(string) string {
		return func(k string) string {
			if k == "CLAWBRIDGE_WEBHOOKS_PROVEN" {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true} {
		if s, err = ParseServe(base, gate(v)); err != nil || s.WebhooksProven != want {
			t.Fatal(v, s.WebhooksProven, err)
		}
	}
	if _, err = ParseServe(base, gate("yes")); err == nil {
		t.Fatal("yes was accepted")
	}
	if s, err = ParseServe(append(base, "--webhooks-proven"), gate("")); err != nil || !s.WebhooksProven {
		t.Fatal("flag:", err)
	}
}

// webhookStart is fake startup state.
type webhookStart struct {
	teams  []string
	called bool
	gateOn bool
}

func (s *webhookStart) WebhookTeamsConfigured(_ context.Context, teams []string, _ time.Time) error {
	s.teams, s.called = teams, true
	return nil
}
func (s *webhookStart) WebhookGateOn(context.Context, time.Time) error { s.gateOn = true; return nil }

// The start records the configured teams and the gate, refuses secrets
// without the app, and logs the team count only.
func TestWebhookStartup(t *testing.T) {
	org := func(context.Context) (string, error) { return whOrg, nil }
	s := &webhookStart{}
	o, err := webhookStartup(context.Background(), s, map[string][]string{whTeam2: {whSecret3}, whTeam: {whSecret}}, true, org, whStart)
	if err != nil || o == nil || len(o.Secrets) != 2 || o.OrgID == nil {
		t.Fatal(o, err)
	}
	if strings.Join(s.teams, ",") != whTeam+","+whTeam2 || !s.gateOn {
		t.Fatal(s.teams, s.gateOn)
	}
	s = &webhookStart{}
	if o, err = webhookStartup(context.Background(), s, nil, false, org, whStart); err != nil || o != nil || !s.called || len(s.teams) != 0 || s.gateOn {
		t.Fatal("none:", o, err, s)
	}
	if _, err = webhookStartup(context.Background(), &webhookStart{}, map[string][]string{whTeam: {whSecret}}, false, nil, whStart); err == nil {
		t.Fatal("secrets without the app started")
	}
}

// No secret ever reaches the log: not at startup, not from a request.
func TestWebhookNoSecretInLogs(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	org := func(context.Context) (string, error) { return whOrg, nil }
	secrets := map[string][]string{whTeam: {whSecret, whSecret2}}
	if _, err := webhookStartup(context.Background(), &webhookStart{}, secrets, true, org, whStart); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "linear webhooks: 1 teams configured") {
		t.Fatal("startup log:", logs.String())
	}
	store := newFakeWebhook()
	store.fail = true
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) { o.Secrets = secrets })
	e.post(whAddr, e.good(1), whSecret)
	e.post(whAddr, e.good(2), "wrong-secret-wrong-secret")
	e.post(whAddr, e.good(3), whSecret2)
	failing := &failingCounts{fakeWebhook: store}
	e2 := newWhEnv(t, failing, func(o *WebhookOptions, _ *HandlerOptions) { o.Secrets = secrets })
	e2.post(whAddr, e2.good(4), "wrong-secret-wrong-secret")
	for _, s := range []string{whSecret, whSecret2, whSign(whSecret, e.good(1))} {
		if strings.Contains(logs.String(), s) {
			t.Fatal("the log holds a secret or signature:", logs.String())
		}
	}
}

type failingCounts struct{ *fakeWebhook }

func (f *failingCounts) WebhookCount(context.Context, string, string, int, time.Time) error {
	return errors.New("refused " + whSecret)
}

// The sweeper purges old webhook rows at most hourly.
func TestSweeperPurgesWebhooks(t *testing.T) {
	s := &purgingSweeps{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runSweeper(ctx, s, 2*time.Millisecond, nil); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for s.sweepCount() < 5 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if s.sweepCount() < 5 || s.purgeCount() != 1 {
		t.Fatal("sweeps", s.sweepCount(), "purges", s.purgeCount())
	}
}

type purgingSweeps struct {
	mu             sync.Mutex
	sweeps, purges int
}

func (p *purgingSweeps) SweepDeliveries(context.Context) (bus.SweepResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweeps++
	return bus.SweepResult{}, nil
}
func (p *purgingSweeps) PurgeWebhooks(context.Context, time.Time) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purges++
	return 0, nil
}
func (p *purgingSweeps) sweepCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.sweeps }
func (p *purgingSweeps) purgeCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.purges }

// A team the path names that isn't configured, or isn't a lower-case
// UUID, is answered exactly as a bad signature, with no MAC computed.
func TestWebhookPathTeam(t *testing.T) {
	store := newFakeWebhook()
	mac := countingMAC(t)
	// whOrg's UUID as a second team, with letters to upper-case.
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Secrets = map[string][]string{whTeam: {whSecret}, whOrg: {whSecret2}}
	})
	body := e.good(1)
	wrong := e.post(whAddr2, body, "wrong-secret-wrong-secret")
	if wrong.Code != 401 || errCode(t, wrong) != "signature" {
		t.Fatal(wrong.Code)
	}
	for _, team := range []string{whTeam2, strings.ToUpper(whOrg), "not-a-team", whTeam + "/x"} {
		before := mac.Load()
		w := e.postWith(whAddr, body, func(r *http.Request) {
			r.URL.Path = LinearWebhookPath + team
			r.Header.Set("Linear-Signature", whSign(whSecret, body))
		})
		if mac.Load() != before {
			t.Fatal(team, "computed a MAC")
		}
		if w.Code != wrong.Code || !bytes.Equal(w.Body.Bytes(), wrong.Body.Bytes()) || fmt.Sprint(w.Header()) != fmt.Sprint(wrong.Header()) {
			t.Fatal(team, "answered unlike a bad signature:", w.Code, w.Body.String(), w.Header())
		}
	}
	e.h.flushCounts(context.Background())
	if e.rejectsOf(whAddr) != 4 || store.count("", "signature") != 5 || store.rowCount() != 0 {
		t.Fatal(e.rejectsOf(whAddr), store.counts)
	}
}

// Verified refusals are counted in memory, by team and reason: no write
// until the flush, then one per counter with its sum.
func TestWebhookRefusalsCountedInMemory(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store, func(o *WebhookOptions, _ *HandlerOptions) {
		o.Secrets = map[string][]string{whTeam: {whSecret}, whTeam2: {whSecret3}}
	})
	for i := 0; i < 7; i++ {
		e.post(whAddr, whBody(t, whPayload(i, "Issue", "update", whTeam, whStart.Add(-5*time.Minute))), whSecret)
	}
	for i := 0; i < 3; i++ {
		e.post(whAddr, whBody(t, whPayload(10+i, "Issue", "update", whTeam, whStart)), whSecret3)
	}
	e.post(whAddr, e.good(20), "wrong-secret-wrong-secret")
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if calls != 0 {
		t.Fatal("refusals wrote before the flush:", calls)
	}
	e.clock.add(webhookFlushEvery)
	e.post(whAddr, e.good(21), "wrong-secret-wrong-secret") // due: flushes
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.calls != 3 || store.counts[whTeam+"/stale"] != 7 || store.counts[whTeam2+"/team_mismatch"] != 3 || store.counts["/signature"] != 2 {
		t.Fatal(store.calls, store.counts)
	}
}
