package agentd

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "cbl_s3cr3tlaunchertoken0123456789"

// fakeLauncher is the server's launcher endpoints over TLS, as agreed with
// the server side: Bearer cbl_…, ClawBridge-Model-API: 1, 401 for any auth
// failure.
type fakeLauncher struct {
	mu       sync.Mutex
	agents   []map[string]any
	max      int
	minted   []string
	reports  []string
	calls    []string
	revoked  bool
	override func(w http.ResponseWriter, r *http.Request) bool
}

func (f *fakeLauncher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.override != nil && f.override(w, r) {
		return
	}
	if f.revoked || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("ClawBridge-Model-API") != "1" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"unauthorized","echo":"`+r.Header.Get("Authorization")+`"}`)
		return
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/dispatcher/launcher/wanted":
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": f.agents, "max_agents": f.max, "server_time": "2026-10-26T22:00:00Z"})
	case "POST /v1/dispatcher/launcher/credential":
		for _, a := range f.agents {
			if a["owner_ref"] == body["owner_ref"] {
				f.minted = append(f.minted, body["owner_ref"])
				cred := "cba_" + strings.Repeat("A", 39) + strconv.Itoa(1000+len(f.minted))
				_ = json.NewEncoder(w).Encode(map[string]string{"credential": cred, "expires_at": "2026-10-26T22:05:00Z"})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"not_found"}`)
	case "POST /v1/dispatcher/launcher/report":
		f.reports = append(f.reports, body["owner_ref"]+" "+body["code"])
		io.WriteString(w, `{"ok":true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeLauncher(t *testing.T) (*fakeLauncher, *launcherClient) {
	t.Helper()
	f := &fakeLauncher{max: 2}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c := NewLauncherClient(srv.URL, testToken).(*launcherClient)
	transport := srv.Client().Transport
	c.http.Transport = transport
	return f, c
}

func TestLauncherClientSpeaksTheWire(t *testing.T) {
	f, c := newFakeLauncher(t)
	f.agents = []map[string]any{{"owner_ref": refA, "provider": "api.provider.test:443", "config_revision": 7, "future_field": true}}
	wanted, max, err := c.Wanted(context.Background())
	if err != nil || max != 2 || len(wanted) != 1 || wanted[0] != (Wanted{Ref: refA, Provider: "api.provider.test:443", Revision: 7}) {
		t.Fatal(wanted, max, err)
	}
	cred, err := c.Credential(context.Background(), refA)
	if err != nil || !credentialPattern.MatchString(cred) {
		t.Fatal(cred, err)
	}
	if _, err := c.Credential(context.Background(), refB); !errors.Is(err, errNotWanted) {
		t.Fatal(err)
	}
	if err := c.Report(context.Background(), refA, codeCrashloop); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.reports, ",") != refA+" "+codeCrashloop {
		t.Fatal(f.reports)
	}
}

// Failures name the call and a status or fixed reason: never a body, a
// credential or the token.
func TestLauncherClientErrorsAreSafe(t *testing.T) {
	f, c := newFakeLauncher(t)
	f.agents = []map[string]any{{"owner_ref": refA, "provider": "api.provider.test:443", "config_revision": 1}}
	f.revoked = true
	if _, _, err := c.Wanted(context.Background()); !errors.Is(err, errLauncherRefused) {
		t.Fatal(err)
	}
	f.revoked = false
	var errs []error
	for name, answer := range map[string]func(w http.ResponseWriter){
		"server error": func(w http.ResponseWriter) {
			w.WriteHeader(500)
			io.WriteString(w, `{"error":"boom `+testToken+`"}`)
		},
		"not json":      func(w http.ResponseWriter) { io.WriteString(w, `<html>`+testToken) },
		"no max_agents": func(w http.ResponseWriter) { io.WriteString(w, `{"agents":[]}`) },
		"too large": func(w http.ResponseWriter) {
			// Valid JSON however it is cut: only the size check refuses it.
			io.WriteString(w, `{"agents":[],"max_agents":2}`+strings.Repeat(" ", maxLauncherBody))
		},
		"redirect": func(w http.ResponseWriter) {
			// The same server, so a followed redirect would arrive and be seen.
			w.Header().Set("Location", "/steal")
			w.WriteHeader(http.StatusFound)
		},
	} {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/steal" {
				return false
			}
			answer(w)
			return true
		}
		_, _, err := c.Wanted(context.Background())
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		errs = append(errs, err)
	}
	// A malformed credential is refused without echoing it.
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/credential") {
			io.WriteString(w, `{"credential":"cba_not a credential `+testToken+`"}`)
			return true
		}
		return false
	}
	if _, err := c.Credential(context.Background(), refA); err == nil {
		t.Fatal("a malformed credential was accepted")
	} else {
		errs = append(errs, err)
	}
	f.override = func(w http.ResponseWriter, r *http.Request) bool { io.WriteString(w, `{"ok":false}`); return true }
	if err := c.Report(context.Background(), refA, codeCrashloop); err == nil {
		t.Fatal("a report the server didn't take was accepted")
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "cba_") || strings.Contains(err.Error(), "elsewhere") {
			t.Fatalf("an error echoed a secret or a body: %v", err)
		}
	}
	for _, call := range f.calls {
		if strings.Contains(call, "steal") {
			t.Fatal("the redirect was followed")
		}
	}
}

// agentd end to end with the launcher client: the server's wanted set
// starts an agent with a credential the server minted, a new revision
// restarts it, and an unwanted owner is stopped.
func TestReconcileWithTheLauncherClient(t *testing.T) {
	f, c := newFakeLauncher(t)
	s := newStack(t)
	cfg := Config{Spec: testSpec(s.policyDir), LauncherToken: testToken, Docker: s.proxy}
	r := NewReconciler(cfg, c, s.logger)
	r.sleep = func(context.Context, time.Duration) error { return nil }
	f.agents = []map[string]any{{"owner_ref": refA, "provider": "api.provider.test:443", "config_revision": 1}}
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err, s.log.String())
	}
	if got := s.daemon.state()[containerName(refA)]; got != "running" || len(f.minted) != 1 {
		t.Fatalf("%q %v", got, f.minted)
	}
	f.mu.Lock()
	f.agents[0]["config_revision"] = 2
	f.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.minted) != 2 {
		t.Fatalf("a new revision did not restart the agent: %v", f.minted)
	}
	f.mu.Lock()
	f.agents = []map[string]any{}
	f.mu.Unlock()
	if err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.daemon.state()[containerName(refA)]; ok {
		t.Fatal("an unwanted agent still runs")
	}
	if strings.Contains(s.log.String(), "s3cr3t") || strings.Contains(s.log.String(), "cba_") {
		t.Fatalf("the log carries a secret: %s", s.log.String())
	}
	// A refused token fails the round with a safe reason, and touches nothing.
	f.revoked = true
	if err := r.Step(context.Background()); err == nil || !strings.Contains(err.Error(), "refused the launcher token") {
		t.Fatal(err)
	}
}

// The client never sends the token through an environment proxy, and
// waits longer than the server's 15 s request deadline.
func TestLauncherClientTransport(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	c := NewLauncherClient("https://server.example.invalid", testToken).(*launcherClient)
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || c.http.Timeout <= 15*time.Second {
		t.Fatalf("%#v %v", c.http.Transport, c.http.Timeout)
	}
}

func TestLauncherClientRateLimited(t *testing.T) {
	f, c := newFakeLauncher(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate_limited"}`)
		return true
	}
	_, _, err := c.Wanted(context.Background())
	if err == nil || errors.Is(err, errLauncherRefused) || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("%v", err)
	}
}

// A wanted answer without its agents list is refused, not read as an empty
// set that would stop every agent [R94-L1].
func TestLauncherClientNeedsTheAgentsList(t *testing.T) {
	f, c := newFakeLauncher(t)
	for body, ok := range map[string]bool{
		`{"agents":[],"max_agents":3}`:   true,
		`{"max_agents":3}`:               false,
		`{"agents":null,"max_agents":3}`: false,
	} {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			io.WriteString(w, body)
			return true
		}
		wanted, max, err := c.Wanted(context.Background())
		if ok != (err == nil) || len(wanted) != 0 || ok && max != 3 {
			t.Fatalf("%s: %v %d %v", body, wanted, max, err)
		}
	}
}

// A request with no answer says why from a fixed set: tls, timeout, dns or
// unreachable, never the raw error [R94-L2].
func TestLauncherClientNamesTransportFailures(t *testing.T) {
	// A certificate the client doesn't trust.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	c := NewLauncherClient(srv.URL, testToken).(*launcherClient)
	if _, _, err := c.Wanted(context.Background()); err == nil || err.Error() != "launcher GET v1/dispatcher/launcher/wanted: tls" {
		t.Fatalf("%v", err)
	}
	// An answer slower than the client's timeout.
	f, c := newFakeLauncher(t)
	f.override = func(http.ResponseWriter, *http.Request) bool {
		time.Sleep(300 * time.Millisecond)
		return true
	}
	c.http.Timeout = 50 * time.Millisecond
	if _, _, err := c.Wanted(context.Background()); err == nil || err.Error() != "launcher GET v1/dispatcher/launcher/wanted: timeout" {
		t.Fatalf("%v", err)
	}
	// A refused connection, and a name that doesn't resolve.
	srv.Close()
	if _, _, err := c.Wanted(context.Background()); err == nil || !strings.HasSuffix(err.Error(), ": unreachable") && !strings.HasSuffix(err.Error(), ": timeout") {
		t.Fatalf("%v", err)
	}
	for err, want := range map[error]string{
		&url.Error{Op: "Get", Err: &net.DNSError{Err: "no such host", Name: "x.invalid"}}: "dns",
		&url.Error{Op: "Get", Err: x509.UnknownAuthorityError{}}:                          "tls",
		&url.Error{Op: "Get", Err: context.DeadlineExceeded}:                              "timeout",
		&url.Error{Op: "Get", Err: errors.New("connection refused")}:                      "unreachable",
	} {
		if got := transportReason(err); got != want {
			t.Fatalf("%v: %s, not %s", err, got, want)
		}
	}
}
