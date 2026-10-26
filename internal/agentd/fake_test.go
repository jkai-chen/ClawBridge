package agentd

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const (
	testImage  = "ghcr.io/example/clawbridge-dispatcher-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testServer = "https://bridge.example.test"
	refA       = "p_aaaaaaaa"
	refB       = "p_bbbbbbbb"
	refC       = "p_cccccccc"
)

// fakeDaemon is a Docker daemon on a unix socket that keeps containers and
// networks in memory and records every request it is sent.
type fakeDaemon struct {
	mu         sync.Mutex
	containers map[string]string // name → state
	networks   map[string]bool
	stdin      map[string]string // name → what attach received
	bodies     map[string][]byte // name → create body
	requests   []string
}

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
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
			out = append(out, map[string]any{"Names": []string{"/" + name}, "State": state})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/containers/create":
		name := r.URL.Query().Get("name")
		if _, ok := f.containers[name]; ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.containers[name], f.bodies[name] = "created", body
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"Id":"0123"}`)
	case r.URL.Path == "/networks/create":
		var n networkSpec
		_ = json.Unmarshal(body, &n)
		if f.networks[n.Name] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.networks[n.Name] = true
		w.WriteHeader(http.StatusCreated)
	case parts[0] == "networks" && r.Method == http.MethodDelete:
		if !f.networks[parts[1]] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.networks, parts[1])
		w.WriteHeader(http.StatusNoContent)
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
			w.WriteHeader(http.StatusNoContent)
		case "POST stop":
			f.containers[name] = "exited"
			w.WriteHeader(http.StatusNoContent)
		case "DELETE ":
			delete(f.containers, name)
			w.WriteHeader(http.StatusNoContent)
		case "GET json":
			_ = json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Status": state}})
		case "POST attach":
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
	daemon *fakeDaemon
	proxy  string // the proxy's socket
	log    *bytes.Buffer
	logger *slog.Logger
}

func newStack(t *testing.T) *testStack {
	t.Helper()
	dir := socketDir(t)
	s := &testStack{
		daemon: &fakeDaemon{containers: map[string]string{}, networks: map[string]bool{}, stdin: map[string]string{}, bodies: map[string][]byte{}},
		proxy:  filepath.Join(dir, "proxy.sock"),
		log:    &bytes.Buffer{},
	}
	s.logger = slog.New(slog.NewJSONHandler(&lockedWriter{w: s.log}, nil))
	serveUnix(t, filepath.Join(dir, "docker.sock"), s.daemon)
	p, err := NewProxy(testImage, testServer, filepath.Join(dir, "docker.sock"), s.logger)
	if err != nil {
		t.Fatal(err)
	}
	serveUnix(t, s.proxy, p)
	return s
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// send makes one raw request to the proxy and returns the status.
func (s *testStack) send(t *testing.T, method, path string, body []byte) int {
	t.Helper()
	conn, err := net.Dial("unix", s.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", method, path, len(body), body)
	resp, err := http.ReadResponse(bufioReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// fakeServer is the ClawBridge server's launcher endpoints.
type fakeServer struct {
	refs      []string
	max       int
	mintErr   error
	minted    []string
	reports   []string
	wantedErr error
}

func (s *fakeServer) Wanted(context.Context) ([]string, int, error) {
	return s.refs, s.max, s.wantedErr
}
func (s *fakeServer) Credential(_ context.Context, ref string) (string, error) {
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
