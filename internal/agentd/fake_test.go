package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testImage       = "ghcr.io/example/clawbridge-dispatcher-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testEgressImage = "ghcr.io/example/clawbridge-agentd@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testServer      = "https://bridge.example.test"
	refA            = "p_aaaaaaaa"
	refB            = "p_bbbbbbbb"
	refC            = "p_cccccccc"
)

// fakeDaemon is a Docker daemon on a unix socket that keeps containers and
// networks in memory and records every request it is sent.
type fakeDaemon struct {
	mu         sync.Mutex
	containers map[string]string // name → state
	networks   map[string]*fakeNet
	stdin      map[string]string // name → what attach received
	bodies     map[string][]byte // name → create body
	requests   []string
	// onCreate, when set, runs as a container is created; refuse makes
	// that create fail.
	onCreate func(name string) (refuse bool)
	// failStop and failNetworkDelete name what can't be stopped or
	// removed; exitOnStart, agents that exit as soon as they start;
	// attachStatus, when set, is attach's answer instead of 101.
	failStop, failRemove, failNetworkDelete, exitOnStart map[string]bool
	// missingImages are images the daemon doesn't have; it has all others.
	missingImages map[string]bool
	attachStatus  int
	// slow delays every answer; set before the daemon serves.
	slow time.Duration
}

type fakeNet struct {
	subnet  string
	members map[string]bool
}

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	time.Sleep(f.slow)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if m := apiVersion.FindStringSubmatch(r.URL.Path); m != nil {
		r.URL.Path = m[1]
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
		var out []map[string]any
		for name, state := range f.containers {
			var created struct{ Labels map[string]string }
			_ = json.Unmarshal(f.bodies[name], &created)
			out = append(out, map[string]any{"Names": []string{"/" + name}, "State": state, "Labels": created.Labels})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/containers/create":
		name := r.URL.Query().Get("name")
		if _, ok := f.containers[name]; ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if f.onCreate != nil && f.onCreate(name) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.containers[name], f.bodies[name] = "created", body
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"Id":"0123"}`)
	case r.Method == http.MethodGet && parts[0] == "images":
		image := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		if f.missingImages[image] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{}`)
	case r.Method == http.MethodGet && r.URL.Path == "/networks":
		out := []networkSpec{}
		for name, n := range f.networks {
			out = append(out, networkSpec{Name: name, IPAM: ipamSpec{Config: []ipamConfig{{Subnet: n.subnet}}}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/networks/create":
		var n networkSpec
		_ = json.Unmarshal(body, &n)
		if f.networks[n.Name] != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		// Like dockerd: an overlapping pool is refused.
		for _, other := range f.networks {
			if other.subnet == n.IPAM.Config[0].Subnet {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		f.networks[n.Name] = &fakeNet{subnet: n.IPAM.Config[0].Subnet, members: map[string]bool{}}
		w.WriteHeader(http.StatusCreated)
		f.bodies[n.Name] = body
	case parts[0] == "networks" && len(parts) >= 2:
		n := f.networks[parts[1]]
		if n == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var c connectSpec
		_ = json.Unmarshal(body, &c)
		switch r.Method + " " + strings.Join(parts[2:], "/") {
		case "DELETE ":
			if f.failNetworkDelete[parts[1]] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if len(n.members) > 0 {
				w.WriteHeader(http.StatusForbidden) // "has active endpoints"
				return
			}
			delete(f.networks, parts[1])
			w.WriteHeader(http.StatusNoContent)
		case "GET ":
			members := map[string]any{}
			for name := range n.members {
				members["id-"+name] = map[string]string{"Name": name}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": parts[1], "Containers": members,
				"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": n.subnet}}}})
		case "POST connect":
			if _, ok := f.containers[c.Container]; !ok || n.members[c.Container] {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			n.members[c.Container] = true
			w.WriteHeader(http.StatusOK)
		case "POST disconnect":
			if !n.members[c.Container] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(n.members, c.Container)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	case parts[0] == "containers" && len(parts) >= 2:
		name := parts[1]
		state, ok := f.containers[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method + " " + strings.Join(parts[2:], "/") {
		case "POST start":
			f.containers[name] = "running"
			if f.exitOnStart[name] {
				f.containers[name] = "exited"
			}
			w.WriteHeader(http.StatusNoContent)
		case "POST stop":
			if f.failStop[name] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.containers[name] = "exited"
			w.WriteHeader(http.StatusNoContent)
		case "DELETE ":
			if f.failRemove[name] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			delete(f.containers, name)
			// A removed container leaves every network.
			for _, n := range f.networks {
				delete(n.members, name)
			}
			w.WriteHeader(http.StatusNoContent)
		case "GET json":
			var created struct{ Image string }
			_ = json.Unmarshal(f.bodies[name], &created)
			_ = json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Running": state == "running"}, "Config": map[string]any{"Image": created.Image}})
		case "POST attach":
			if f.attachStatus != 0 {
				w.WriteHeader(f.attachStatus)
				return
			}
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			fmt.Fprint(conn, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			f.mu.Unlock()
			got, _ := io.ReadAll(buf)
			f.mu.Lock()
			f.stdin[name] = string(got)
			conn.Close()
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeDaemon) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// socketDir is a short directory for unix sockets: macOS caps their paths
// at 104 bytes, which a test's TempDir can exceed.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agentd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serveUnix(t *testing.T, path string, h http.Handler) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

// testStack is a fake daemon behind the real proxy, and the log of both
// the proxy and agentd.
type testStack struct {
	daemon    *fakeDaemon
	proxy     string // the proxy's socket
	policyDir string
	log       *lockedWriter
	logger    *slog.Logger
}

func newStack(t *testing.T) *testStack {
	t.Helper()
	return newStackWith(t, testSpec)
}

// newStackWith is newStack with the proxy's spec made by spec.
func newStackWith(t *testing.T, spec func(policyDir string) Spec) *testStack {
	t.Helper()
	dir := socketDir(t)
	s := &testStack{
		daemon:    &fakeDaemon{containers: map[string]string{}, networks: map[string]*fakeNet{}, stdin: map[string]string{}, bodies: map[string][]byte{}},
		proxy:     filepath.Join(dir, "proxy.sock"),
		policyDir: t.TempDir(),
		log:       &lockedWriter{w: &bytes.Buffer{}},
	}
	s.logger = slog.New(slog.NewJSONHandler(s.log, nil))
	serveUnix(t, filepath.Join(dir, "docker.sock"), s.daemon)
	p, err := NewProxy(spec(s.policyDir), filepath.Join(dir, "docker.sock"), s.logger)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", s.proxy)
	if err != nil {
		t.Fatal(err)
	}
	srv := proxyServer(p)
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return s
}

func testSpec(policyDir string) Spec {
	return Spec{ServerURL: testServer, AgentImage: testImage, EgressImage: testEgressImage, PolicyDir: policyDir}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// String returns what was written so far.
func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprint(l.w)
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// send makes one request to the proxy and returns the status.
func (s *testStack) send(t *testing.T, method, path string, body []byte) int {
	t.Helper()
	return s.sendRaw(t, fmt.Sprintf("%s %s HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", method, path, len(body), body))
}

// sendRaw sends raw as the request and returns the status.
func (s *testStack) sendRaw(t *testing.T, raw string) int {
	t.Helper()
	conn, err := net.Dial("unix", s.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The proxy may refuse and close before a large body is all written;
	// its answer is still there to read.
	_, werr := io.WriteString(conn, raw)
	resp, err := http.ReadResponse(bufioReader(conn), nil)
	if err != nil {
		t.Fatal(errors.Join(err, werr))
	}
	resp.Body.Close()
	return resp.StatusCode
}

// fakeServer is the ClawBridge server's launcher endpoints.
type fakeServer struct {
	refs []string
	// providers overrides an owner's provider host:port; the default is
	// api.provider.test:443. revisions overrides its config revision, 1.
	providers map[string]string
	revisions map[string]int64
	max       int
	mintErr   error
	mintDelay time.Duration
	minted    []string
	reports   []string
	wantedErr error
}

func (s *fakeServer) Wanted(context.Context) ([]Wanted, int, error) {
	var out []Wanted
	for _, ref := range s.refs {
		provider := "api.provider.test:443"
		if p, ok := s.providers[ref]; ok {
			provider = p
		}
		revision := int64(1)
		if r, ok := s.revisions[ref]; ok {
			revision = r
		}
		out = append(out, Wanted{Ref: ref, Provider: provider, Revision: revision})
	}
	return out, s.max, s.wantedErr
}
func (s *fakeServer) Credential(_ context.Context, ref string) (string, error) {
	time.Sleep(s.mintDelay)
	if s.mintErr != nil {
		return "", s.mintErr
	}
	s.minted = append(s.minted, ref)
	return "cba_" + ref + "_secretcredential", nil
}
func (s *fakeServer) Report(_ context.Context, ref, code string) error {
	s.reports = append(s.reports, ref+" "+code)
	return nil
}
