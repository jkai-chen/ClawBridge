package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"clawbridge/internal/guibackend"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeOpener records what it is given and opens nothing.
type fakeOpener struct {
	mu      sync.Mutex
	targets []string
}

func (f *fakeOpener) open(_ context.Context, target string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, target)
	return true, nil
}

func (f *fakeOpener) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.targets...)
}

type rig struct {
	t       *testing.T
	srv     *Server
	backend *guibackend.Server
	log     *safeBuf
	opener  *fakeOpener
	done    chan error
	cancel  context.CancelFunc
	client  *http.Client
	base    string
	profile string
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "cbui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func testBundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><html><head><title>t</title></head><body>shell</body></html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
}

func start(t *testing.T, mut func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, log: &safeBuf{}, opener: &fakeOpener{}, done: make(chan error, 1)}
	r.backend = &guibackend.Server{
		Hello: guibackend.Hello{ClientVersion: "1.0.0-test", Mode: "local", Platform: "linux", Profile: map[string]any{"workspace": "ws_example"}},
		Methods: map[string]guibackend.Method{
			"echo": {Class: guibackend.Read, Call: func(_ context.Context, p json.RawMessage) (any, error) { return json.RawMessage(p), nil }},
		},
		Heartbeat: 50 * time.Millisecond,
	}
	cfg := Config{Backend: r.backend, StateDir: filepath.Join(shortDir(t), "ui"), Profile: "/profiles/a.toml", Platform: "linux", Opener: r.opener.open, Log: r.log, Bundle: testBundle(), Poll: 10 * time.Millisecond}
	if mut != nil {
		mut(&cfg)
	}
	r.base, r.profile = cfg.StateDir, cfg.Profile
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- srv.Serve(ctx) }()
	r.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	return r
}

type resp struct {
	*http.Response
	body string
}

// do sends a request with the right Host unless hdr says otherwise.
func (r *rig) do(method, path string, hdr map[string]string, body string) resp {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL()+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res, string(b)}
}

var metaRe = regexp.MustCompile(`<meta name="cb-session" content="([0-9a-f]{64})">`)

// launch redeems a fresh token and returns the session secret.
func (r *rig) launch() string {
	r.t.Helper()
	link, err := r.srv.NoOpenURL()
	if err != nil {
		r.t.Fatal(err)
	}
	u, _ := url.Parse(link)
	res := r.do("GET", "/?"+u.RawQuery, nil, "")
	m := metaRe.FindStringSubmatch(res.body)
	if res.StatusCode != 200 || m == nil {
		r.t.Fatalf("exchange: %d %q", res.StatusCode, res.body)
	}
	return m[1]
}

func (r *rig) rpc(secret, body string) resp {
	return r.do("POST", "/rpc", map[string]string{SessionHeader: secret, "Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, body)
}

const hello = `{"jsonrpc":"2.0","id":1,"method":"hello"}`

func TestAcceptanceWrongHost(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	for _, host := range []string{"evil.example", "evil.example:" + r.srv.port, "127.0.0.1", "127.0.0.1:1", "[::1]:" + r.srv.port, "localhost.evil.example:" + r.srv.port, "127.0.0.1.evil.example:" + r.srv.port} {
		for _, p := range []string{"/", "/rpc", "/events", "/assets/app.js"} {
			res := r.do("POST", p, map[string]string{"Host": host, SessionHeader: sec, "Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, hello)
			if res.StatusCode != 421 {
				t.Errorf("Host %q %s: %d", host, p, res.StatusCode)
			}
		}
	}
	// HTTP/1.0 with no Host at all.
	c, err := net.Dial("tcp4", r.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if !strings.Contains(line, " 421 ") {
		t.Errorf("no Host: %q", line)
	}
	// Both accepted spellings work.
	for _, host := range []string{"127.0.0.1:" + r.srv.port, "localhost:" + r.srv.port} {
		if res := r.rpc(sec, hello); res.StatusCode != 200 {
			t.Errorf("Host %q: %d", host, res.StatusCode)
		}
		res := r.do("POST", "/rpc", map[string]string{"Host": host, SessionHeader: sec, "Content-Type": "application/json", "Origin": "http://" + host}, hello)
		if res.StatusCode != 200 {
			t.Errorf("Host %q with Origin: %d", host, res.StatusCode)
		}
	}
}

func TestAcceptanceCrossSiteOrigin(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	host := "127.0.0.1:" + r.srv.port
	for _, h := range []map[string]string{
		{"Origin": "http://evil.example"},
		{"Origin": "null"},
		{"Origin": "http://127.0.0.1:1"},
		{"Origin": "http://localhost:" + r.srv.port},
		{"Origin": "https://" + host},
		{"Origin": "http://" + host, "Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{},
	} {
		h[SessionHeader], h["Content-Type"] = sec, "application/json"
		if res := r.do("POST", "/rpc", h, hello); res.StatusCode != 403 {
			t.Errorf("POST /rpc %v: %d", h, res.StatusCode)
		}
		if res := r.do("GET", "/events", h, ""); res.StatusCode != 403 {
			t.Errorf("GET /events %v: %d", h, res.StatusCode)
		}
	}
}

func TestAcceptanceMissingOrWrongSessionOnRPCAndEvents(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	same := map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	with := func(v string, set bool) map[string]string {
		h := map[string]string{}
		for k, x := range same {
			h[k] = x
		}
		if set {
			h[SessionHeader] = v
		}
		return h
	}
	wrongSame := strings.Repeat("0", 64)
	for _, c := range []struct{ method, path string }{{"POST", "/rpc"}, {"GET", "/events"}, {"GET", "/rpc"}} {
		missing := r.do(c.method, c.path, with("", false), hello)
		for _, w := range []string{wrongSame, sec[:63], sec + "0", "x", strings.ToUpper(sec)} {
			wrong := r.do(c.method, c.path, with(w, true), hello)
			// Nit d: the same status and body whether missing or wrong.
			if missing.StatusCode != 401 || wrong.StatusCode != missing.StatusCode || wrong.body != missing.body || wrong.Header.Get("Content-Type") != missing.Header.Get("Content-Type") {
				t.Errorf("%s %s: missing %d %q, wrong %d %q", c.method, c.path, missing.StatusCode, missing.body, wrong.StatusCode, wrong.body)
			}
		}
	}
	if res := r.rpc(sec, hello); res.StatusCode != 200 || !strings.Contains(res.body, `"protocol"`) {
		t.Fatalf("right secret: %d %q", res.StatusCode, res.body)
	}
}

func TestAcceptanceReusedOrExpiredLaunchToken(t *testing.T) {
	r := start(t, func(c *Config) { c.LaunchTTL = 150 * time.Millisecond })
	link, _ := r.srv.NoOpenURL()
	u, _ := url.Parse(link)
	if res := r.do("GET", "/?"+u.RawQuery, nil, ""); !metaRe.MatchString(res.body) {
		t.Fatal("first use failed")
	}
	reuse := r.do("GET", "/?"+u.RawQuery, nil, "")
	if reuse.StatusCode != 403 || metaRe.MatchString(reuse.body) || strings.Contains(reuse.body, "cb-session") {
		t.Errorf("reuse: %d %q", reuse.StatusCode, reuse.body)
	}
	link, _ = r.srv.NoOpenURL()
	u, _ = url.Parse(link)
	time.Sleep(400 * time.Millisecond)
	late := r.do("GET", "/?"+u.RawQuery, nil, "")
	if late.StatusCode != 403 || strings.Contains(late.body, "cb-session") {
		t.Errorf("expired: %d %q", late.StatusCode, late.body)
	}
	for _, q := range []string{"t=", "t=nonsense", "t=" + strings.Repeat("a", 64)} {
		if res := r.do("GET", "/?"+q, nil, ""); res.StatusCode != 403 || strings.Contains(res.body, "cb-session") {
			t.Errorf("%q: %d", q, res.StatusCode)
		}
	}
}

func TestAcceptanceLaunchTokenInSecondTab(t *testing.T) {
	r := start(t, nil)
	link, _ := r.srv.NoOpenURL()
	u, _ := url.Parse(link)
	tab1 := r.do("GET", "/?"+u.RawQuery, nil, "")
	tab2 := r.do("GET", "/?"+u.RawQuery, nil, "")
	if !metaRe.MatchString(tab1.body) {
		t.Fatal("tab one has no secret")
	}
	if tab2.StatusCode != 403 || strings.Contains(tab2.body, "cb-session") {
		t.Errorf("tab two: %d %q", tab2.StatusCode, tab2.body)
	}
	// The plain shell (a reload, or tab two after the replaceState) has none.
	if plain := r.do("GET", "/", nil, ""); strings.Contains(plain.body, "cb-session") {
		t.Error("plain shell has a secret")
	}
}

func TestAcceptanceCORSPreflight(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	for _, p := range []string{"/rpc", "/events", "/", "/assets/app.js"} {
		for _, h := range []map[string]string{
			{"Origin": "http://evil.example", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "x-clawbridge-session,content-type"},
			{"Origin": "http://127.0.0.1:" + r.srv.port, "Access-Control-Request-Method": "POST", SessionHeader: sec},
			{"Access-Control-Request-Method": "POST"},
		} {
			res := r.do("OPTIONS", p, h, "")
			if res.StatusCode < 400 {
				t.Errorf("OPTIONS %s %v: %d", p, h, res.StatusCode)
			}
			for k := range res.Header {
				if strings.HasPrefix(k, "Access-Control-") {
					t.Errorf("OPTIONS %s sent %s", p, k)
				}
			}
		}
	}
}

// R1-T1: the right Host, no Origin, every header the server ever set, and
// no secret: no endpoint returns a secret, and /rpc refuses.
func TestAcceptanceRightHostNoOriginEveryHeaderNoSecretHoldsNothing(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	seen := map[string]string{}
	var bodies []string
	collect := func(res resp) {
		for k, v := range res.Header {
			seen[k] = strings.Join(v, ",")
		}
		bodies = append(bodies, res.body)
		if res.Header.Get("Set-Cookie") != "" {
			t.Error("the server set a cookie")
		}
	}
	for _, p := range []string{"/", "/missing", "/assets/app.js", "/rpc", "/events"} {
		collect(r.do("GET", p, nil, ""))
	}
	collect(r.rpc(sec, hello)) // headers of a real answer
	delete(seen, SessionHeader)
	// Replay all of them, plus the browser's same-origin hint, as a plain
	// local process would.
	replay := map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	for k, v := range seen {
		if k != "Content-Type" {
			replay[k] = v
		}
	}
	for _, c := range []struct{ m, p string }{{"POST", "/rpc"}, {"GET", "/events"}, {"GET", "/rpc"}} {
		if res := r.do(c.m, c.p, replay, hello); res.StatusCode != 401 {
			t.Errorf("%s %s with every header: %d", c.m, c.p, res.StatusCode)
		}
	}
	for _, p := range []string{"/", "/missing", "/assets/app.js", "/index.html", "/?x=1"} {
		res := r.do("GET", p, replay, "")
		collect(res)
	}
	for _, b := range bodies {
		if strings.Contains(b, sec) || strings.Contains(b, "cb-session") || regexp.MustCompile(`[0-9a-f]{64}`).MatchString(b) {
			t.Errorf("a body holds a secret: %q", b)
		}
	}
	for k, v := range seen {
		if strings.Contains(v, sec) {
			t.Errorf("header %s holds the secret", k)
		}
	}
}

func TestAcceptanceShellWithoutTokenHasNoSecret(t *testing.T) {
	r := start(t, nil)
	r.launch() // a live secret exists
	res := r.do("GET", "/", nil, "")
	if res.StatusCode != 200 || !strings.Contains(res.body, "shell") || strings.Contains(res.body, "cb-session") || regexp.MustCompile(`[0-9a-f]{32}`).MatchString(res.body) {
		t.Errorf("%d %q", res.StatusCode, res.body)
	}
	// With a token: exactly one meta, inside <head>, and the plain shell is
	// otherwise the bundle's index.
	link, _ := r.srv.NoOpenURL()
	u, _ := url.Parse(link)
	ex := r.do("GET", "/?"+u.RawQuery, nil, "")
	if n := strings.Count(ex.body, "cb-session"); n != 1 || !strings.Contains(ex.body, "<head><meta name=\"cb-session\"") {
		t.Errorf("exchange page: %q", ex.body)
	}
	if strings.Replace(metaRe.ReplaceAllString(ex.body, ""), "", "", 1) != res.body {
		t.Error("exchange page differs from the shell beyond the meta")
	}
}

func TestAcceptanceOpenerArgvHasNoTokenOnDarwinAndLinux(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		r := start(t, func(c *Config) { c.Platform = platform })
		if ok, err := r.srv.Open(context.Background()); err != nil || !ok {
			t.Fatal(ok, err)
		}
		got := r.opener.all()
		if len(got) != 1 || !strings.HasPrefix(got[0], "file://") {
			t.Fatalf("%s: %v", platform, got)
		}
		page, err := os.ReadFile(strings.TrimPrefix(got[0], "file://"))
		if err != nil {
			t.Fatal(err)
		}
		tok := regexp.MustCompile(`\?t=([0-9a-f]{64})`).FindStringSubmatch(string(page))
		if tok == nil {
			t.Fatalf("no token in the launch file: %s", page)
		}
		if strings.Contains(got[0], tok[1]) || strings.Contains(got[0], "?t=") || strings.Contains(got[0], "http") {
			t.Errorf("%s: the opener's argv holds the token: %q", platform, got[0])
		}
	}
}

// The explicit exception of C2: where argv is private, the URL is given.
func TestOpenerGetsURLOnWindowsAndWSL(t *testing.T) {
	for _, platform := range []string{"windows", "wsl"} {
		r := start(t, func(c *Config) { c.Platform = platform })
		r.srv.Open(context.Background())
		got := r.opener.all()
		if len(got) != 1 || !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/\?t=[0-9a-f]{64}$`).MatchString(got[0]) {
			t.Errorf("%s: %v", platform, got)
		}
		if files, _ := filepath.Glob(filepath.Join(r.srv.dir, "launch-*")); len(files) != 0 {
			t.Errorf("%s wrote a launch file", platform)
		}
	}
}

func TestAcceptanceLaunchFileModeDirModeAndGoneAfterExchange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	// A loose pre-existing dir is tightened.
	dir := filepath.Join(shortDir(t), "ui")
	os.Mkdir(dir, 0o755)
	r := start(t, func(c *Config) { c.StateDir = dir })
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o", fi.Mode().Perm())
	}
	target, err := r.srv.OpenTarget()
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(target, "file://")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || filepath.Dir(path) != r.srv.dir {
		t.Errorf("launch file %o in %s", fi.Mode().Perm(), filepath.Dir(path))
	}
	page, _ := os.ReadFile(path)
	if strings.Contains(string(page), "<script") || !strings.Contains(string(page), `http-equiv="refresh" content="0;url=http://127.0.0.1:`+r.srv.port+`/?t=`) {
		t.Errorf("launch page: %s", page)
	}
	u := regexp.MustCompile(`url=([^"]+)"`).FindStringSubmatch(string(page))[1]
	pu, _ := url.Parse(u)
	if res := r.do("GET", "/?"+pu.RawQuery, nil, ""); !metaRe.MatchString(res.body) {
		t.Fatal("the launch file's link does not exchange")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("launch file still there after the exchange: %v", err)
	}
	// Unredeemed: gone at expiry.
	r2 := start(t, func(c *Config) { c.LaunchTTL = 100 * time.Millisecond })
	tg, _ := r2.srv.OpenTarget()
	p2 := strings.TrimPrefix(tg, "file://")
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Errorf("launch file still there after expiry: %v", err)
	}
	// And gone when the server stops.
	r3 := start(t, nil)
	tg, _ = r3.srv.OpenTarget()
	r3.cancel()
	<-r3.done
	r3.done <- nil
	if _, err := os.Stat(strings.TrimPrefix(tg, "file://")); !os.IsNotExist(err) {
		t.Error("launch file survived the server")
	}
}

func TestAcceptanceBindAddressOtherThanLoopback(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "::1", "localhost", "192.168.1.5", "127.0.0.2", "0.0.0.0:0"} {
		_, err := New(Config{Backend: &guibackend.Server{}, StateDir: filepath.Join(shortDir(t), "ui"), Host: host, Bundle: testBundle()})
		if err == nil {
			t.Errorf("Host %q was accepted", host)
		}
	}
	r := start(t, nil)
	if a := r.srv.Addr(); !strings.HasPrefix(a, "127.0.0.1:") {
		t.Errorf("listening on %s", a)
	}
	// A fixed port is honoured.
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	fixed := start(t, func(c *Config) { c.Port = p })
	if fixed.srv.Addr() != net.JoinHostPort("127.0.0.1", itoa(p)) {
		t.Errorf("fixed port: %s", fixed.srv.Addr())
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func headerRoutes(r *rig, sec string) map[string]resp {
	port := r.srv.port
	same := map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	authed := map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json", SessionHeader: sec}
	link, _ := r.srv.NoOpenURL()
	u, _ := url.Parse(link)
	big := `{"jsonrpc":"2.0","id":1,"method":"echo","params":"` + strings.Repeat("a", guibackend.MaxBody+10) + `"}`
	out := map[string]resp{
		"GET / plain":                r.do("GET", "/", nil, ""),
		"GET /?t= exchange (nit c)":  r.do("GET", "/?"+u.RawQuery, nil, ""),
		"GET /?t= bad token":         r.do("GET", "/?t=bad", nil, ""),
		"POST / 405":                 r.do("POST", "/", same, ""),
		"GET /assets/app.js":         r.do("GET", "/assets/app.js", nil, ""),
		"GET /missing 404":           r.do("GET", "/missing", nil, ""),
		"GET /index.html 404":        r.do("GET", "/index.html", nil, ""),
		"GET /assets/ dir 404":       r.do("GET", "/assets/", nil, ""),
		"POST /assets/app.js 405":    r.do("POST", "/assets/app.js", same, ""),
		"wrong Host 421":             r.do("GET", "/", map[string]string{"Host": "evil.example:" + port}, ""),
		"wrong Host /rpc 421":        r.do("POST", "/rpc", map[string]string{"Host": "evil.example"}, hello),
		"POST /rpc no secret 401":    r.do("POST", "/rpc", same, hello),
		"GET /events no secret 401":  r.do("GET", "/events", same, ""),
		"POST /rpc cross origin 403": r.do("POST", "/rpc", map[string]string{"Origin": "http://evil.example", SessionHeader: sec, "Content-Type": "application/json"}, hello),
		"OPTIONS /rpc preflight":     r.do("OPTIONS", "/rpc", map[string]string{"Origin": "http://evil.example", "Access-Control-Request-Method": "POST"}, ""),
		"POST /rpc 200":              r.do("POST", "/rpc", authed, hello),
		"POST /rpc 415":              r.do("POST", "/rpc", map[string]string{"Sec-Fetch-Site": "same-origin", SessionHeader: sec, "Content-Type": "text/plain"}, hello),
		"POST /rpc 413":              r.do("POST", "/rpc", authed, big),
		"GET /rpc 405":               r.do("GET", "/rpc", authed, ""),
		"POST /events 403 or 405":    r.do("POST", "/events", authed, ""),
	}
	return out
}

func TestAcceptanceExactHeaderSetOnEveryRoute(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	routes := headerRoutes(r, sec)
	want := map[int]bool{200: true, 403: true, 404: true, 405: true, 413: true, 415: true, 421: true, 401: true}
	seenCodes := map[int]bool{}
	check := func(name string, h http.Header, code int) {
		seenCodes[code] = true
		if !want[code] {
			t.Errorf("%s: unexpected status %d", name, code)
		}
		for k, v := range SecurityHeaders {
			if vals := h.Values(k); len(vals) != 1 || vals[0] != v {
				t.Errorf("%s (%d): %s = %q, want %q", name, code, k, vals, v)
			}
		}
		for k := range h {
			if strings.HasPrefix(k, "Access-Control-") || k == "Set-Cookie" {
				t.Errorf("%s: sent %s", name, k)
			}
		}
		if strings.Contains(h.Get("Content-Security-Policy"), "unsafe-") {
			t.Errorf("%s: unsafe in CSP", name)
		}
	}
	for name, res := range routes {
		check(name, res.Header, res.StatusCode)
	}
	for _, c := range []int{200, 401, 403, 404, 405, 413, 415, 421} {
		if !seenCodes[c] {
			t.Errorf("no route produced %d", c)
		}
	}
	// The event stream too, whose handler sets its own Cache-Control.
	req, _ := http.NewRequest("GET", r.srv.URL()+"/events", nil)
	req.Header.Set(SessionHeader, sec)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	check("GET /events 200", res.Header, res.StatusCode)
}

func TestSessionSecretsCapAtSixteen(t *testing.T) {
	r := start(t, nil)
	var secrets []string
	for i := 0; i < maxSessions+1; i++ {
		secrets = append(secrets, r.launch())
	}
	if r.rpc(secrets[0], hello).StatusCode != 401 {
		t.Error("the oldest secret still works")
	}
	for i, s := range secrets[1:] {
		if r.rpc(s, hello).StatusCode != 200 {
			t.Errorf("secret %d was dropped", i+1)
		}
	}
	r.srv.tokens.mu.Lock()
	n := len(r.srv.tokens.sessions)
	r.srv.tokens.mu.Unlock()
	if n != maxSessions {
		t.Errorf("%d live secrets", n)
	}
}

func TestEventsStreamWithSecret(t *testing.T) {
	r := start(t, nil)
	sec := r.launch()
	req, _ := http.NewRequest("GET", r.srv.URL()+"/events", nil)
	req.Header.Set(SessionHeader, sec)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	waitFor(t, func() bool { return r.backend.Connected() == 1 })
	r.backend.Notify("ping", map[string]int{"n": 1})
	br := bufio.NewReader(res.Body)
	var got string
	for i := 0; i < 8 && !strings.Contains(got, "event: ping"); i++ {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		got += l
	}
	if !strings.Contains(got, "event: ping") {
		t.Errorf("no event: %q", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func (r *rig) events(sec string) (cancel func()) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", r.srv.URL()+"/events", nil)
	req.Header.Set(SessionHeader, sec)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	go io.Copy(io.Discard, res.Body)
	return cancel
}

func TestAcceptanceIdleExitAfterTheTabCloses(t *testing.T) {
	r := start(t, func(c *Config) { c.IdleGrace = 300 * time.Millisecond; c.StartGrace = time.Minute })
	sec := r.launch()
	cancel := r.events(sec)
	waitFor(t, func() bool { return r.backend.Connected() == 1 })
	// While the tab is connected, longer than the grace, it keeps running.
	select {
	case err := <-r.done:
		t.Fatalf("stopped with a tab open: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	closed := time.Now()
	cancel()
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrIdle) {
			t.Errorf("err = %v", err)
		}
		if d := time.Since(closed); d < 250*time.Millisecond {
			t.Errorf("stopped %v after the tab closed, before the grace", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no idle exit")
	}
	r.done <- nil // for the cleanup
}

func TestReconnectWithinTheGraceKeepsRunning(t *testing.T) {
	r := start(t, func(c *Config) { c.IdleGrace = 400 * time.Millisecond; c.StartGrace = time.Minute })
	sec := r.launch()
	cancel := r.events(sec)
	waitFor(t, func() bool { return r.backend.Connected() == 1 })
	cancel()
	waitFor(t, func() bool { return r.backend.Connected() == 0 })
	time.Sleep(150 * time.Millisecond)
	again := r.events(sec)
	defer again()
	select {
	case err := <-r.done:
		t.Fatalf("stopped despite the reconnect: %v", err)
	case <-time.After(900 * time.Millisecond):
	}
}

func TestAcceptanceNeverConnectedExit(t *testing.T) {
	began := time.Now()
	r := start(t, func(c *Config) { c.StartGrace = 300 * time.Millisecond; c.IdleGrace = 10 * time.Millisecond })
	// Still zero at the backend before any connection.
	if !r.backend.LastDisconnect().IsZero() {
		t.Fatal("LastDisconnect not zero")
	}
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrNeverConnected) {
			t.Errorf("err = %v", err)
		}
		if d := time.Since(began); d < 250*time.Millisecond {
			t.Errorf("stopped after %v, before the start grace", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no never-connected exit")
	}
	r.done <- nil
}

// R1: the never-connected exit counts from the start, not from the
// backend's LastDisconnect (zero until the first connection), and a tab that
// connects in time is never cut off by it.
func TestNeverConnectedCountsFromStartNotLastDisconnect(t *testing.T) {
	r := start(t, func(c *Config) { c.StartGrace = 300 * time.Millisecond; c.IdleGrace = time.Minute })
	cancel := r.events(r.launch())
	defer cancel()
	select {
	case err := <-r.done:
		t.Fatalf("stopped with a connected tab: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
}

type deadlineWriter struct {
	http.ResponseWriter
	deadlines []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestEventsWriteDeadlineRefreshedOnEveryWrite(t *testing.T) {
	// The wrapper sets the deadline on the header, each write and each flush.
	rec := &deadlineWriter{ResponseWriter: newRecorder()}
	g := &guard{ResponseWriter: rec, deadline: 200 * time.Millisecond}
	g.WriteHeader(200)
	n := len(rec.deadlines)
	if n == 0 {
		t.Fatal("no deadline at the header")
	}
	for i := 0; i < 3; i++ {
		time.Sleep(5 * time.Millisecond)
		g.Write([]byte(": heartbeat\n\n"))
		g.Flush()
	}
	if len(rec.deadlines) != n+6 {
		t.Fatalf("%d deadline sets", len(rec.deadlines))
	}
	for i := 1; i < len(rec.deadlines); i++ {
		if !rec.deadlines[i].After(rec.deadlines[i-1]) && !rec.deadlines[i].Equal(rec.deadlines[i-1]) {
			t.Error("deadline moved back")
		}
	}
	if last := rec.deadlines[len(rec.deadlines)-1]; last.Before(time.Now()) || last.After(time.Now().Add(250*time.Millisecond)) {
		t.Errorf("deadline %v", time.Until(last))
	}
	// Only /events is wired with one.
	r := start(t, func(c *Config) { c.EventWriteTimeout = 7 * time.Second })
	if r.srv.cfg.EventWriteTimeout != 7*time.Second {
		t.Error("timeout not configurable")
	}
}

// A client that stops reading is cut off once the write deadline passes.
func TestStalledEventsClientIsDropped(t *testing.T) {
	r := start(t, func(c *Config) { c.EventWriteTimeout = 300 * time.Millisecond })
	sec := r.launch()
	conn, err := net.Dial("tcp4", r.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024)
	io.WriteString(conn, "GET /events HTTP/1.1\r\nHost: "+r.srv.Addr()+"\r\nSec-Fetch-Site: same-origin\r\n"+SessionHeader+": "+sec+"\r\n\r\n")
	waitFor(t, func() bool { return r.backend.Connected() == 1 })
	// Never read; push big events a few at a time so the buffer fills but
	// the hub's queue does not overflow.
	big := strings.Repeat("x", 512<<10)
	deadline := time.Now().Add(10 * time.Second)
	for r.backend.Connected() == 1 && time.Now().Before(deadline) {
		r.backend.Notify("fill", big)
		time.Sleep(200 * time.Millisecond)
	}
	if r.backend.Connected() != 0 {
		t.Fatal("the stalled client still holds its goroutine")
	}
}

func TestAcceptanceNoQueryStringInAnyLogLine(t *testing.T) {
	r := start(t, nil)
	link, _ := r.srv.NoOpenURL()
	u, _ := url.Parse(link)
	tok := u.Query().Get("t")
	r.do("GET", "/?"+u.RawQuery, nil, "")
	r.do("GET", "/?t="+tok, nil, "")
	r.do("GET", "/assets/app.js?v=SECRETQ", nil, "")
	sec := r.launch()
	r.do("POST", "/rpc?s="+sec, map[string]string{SessionHeader: sec, "Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, `{"jsonrpc":"2.0","id":1,"method":"echo","params":"PARAMVALUE"}`)
	r.do("GET", "/missing?x=SECRETQ", nil, "")
	r.do("GET", "/events?secret="+sec, nil, "")
	r.do("GET", "/", map[string]string{"Host": "evil.example"}, "")
	log := r.log.String()
	if log == "" {
		t.Fatal("nothing logged")
	}
	for _, bad := range []string{"?", "SECRETQ", "PARAMVALUE", tok, sec, "t=", "Session"} {
		if strings.Contains(log, bad) {
			t.Errorf("the log holds %q:\n%s", bad, log)
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if !regexp.MustCompile(`^ui: "[A-Z]+" "[^"]*" \d{3}$|^ui: "[^"]*" code=-?\d+ took=\S+$`).MatchString(l) {
			t.Errorf("odd log line %q", l)
		}
	}
}

func TestSecondInstanceGetsAFreshLaunchViaTheSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no control socket on Windows")
	}
	r := start(t, nil)
	dir := r.srv.dir
	base := r.base
	// A second New on the same dir is refused with AlreadyRunning.
	_, err := New(Config{Backend: &guibackend.Server{}, StateDir: base, Profile: r.profile, Bundle: testBundle()})
	var ar *AlreadyRunning
	if !errors.As(err, &ar) {
		t.Fatalf("second New: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, socketName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	h, err := RequestHandoff(context.Background(), base, r.profile, "open")
	if err != nil {
		t.Fatal(err)
	}
	if h.URL != r.srv.URL() || !strings.HasPrefix(h.Target, "file://") || strings.Contains(h.Target, "?t=") {
		t.Fatalf("%+v", h)
	}
	page, err := os.ReadFile(strings.TrimPrefix(h.Target, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(regexp.MustCompile(`url=([^"]+)"`).FindStringSubmatch(string(page))[1])
	if res := r.do("GET", "/?"+pu.RawQuery, nil, ""); !metaRe.MatchString(res.body) {
		t.Error("the fresh launch does not exchange")
	}
	// And the URL form for --no-open.
	h2, err := RequestHandoff(context.Background(), base, r.profile, "url")
	if err != nil || !strings.Contains(h2.Target, "/?t=") {
		t.Fatalf("%+v %v", h2, err)
	}
	// A bad request is refused.
	c, _ := net.Dial("unix", filepath.Join(dir, socketName))
	io.WriteString(c, "{\"want\":\"shell\"}\n")
	var rep controlReply
	json.NewDecoder(c).Decode(&rep)
	c.Close()
	if rep.Error == "" || rep.Target != "" {
		t.Errorf("bad request answered %+v", rep)
	}
	// After the server stops the socket and info are gone, and the lock is free.
	r.cancel()
	<-r.done
	r.done <- nil
	if _, err := os.Stat(filepath.Join(dir, socketName)); !os.IsNotExist(err) {
		t.Error("socket left behind")
	}
	if s, err := New(Config{Backend: &guibackend.Server{}, StateDir: base, Profile: r.profile, Bundle: testBundle()}); err != nil {
		t.Errorf("lock not released: %v", err)
	} else {
		s.Close()
	}
}

func TestRequestHandoffWithNothingRunning(t *testing.T) {
	_, err := RequestHandoff(context.Background(), shortDir(t), "/profiles/a.toml", "open")
	var nc *NoControlError
	if !errors.As(err, &nc) {
		t.Fatalf("%v", err)
	}
}

func TestPlaceholderBundleIsEmbedded(t *testing.T) {
	r := start(t, func(c *Config) { c.Bundle = nil })
	res := r.do("GET", "/", nil, "")
	if res.StatusCode != 200 || !strings.Contains(res.body, "isn't built into this binary") {
		t.Errorf("%d %q", res.StatusCode, res.body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type %q", ct)
	}
	if strings.Contains(res.body, "<script") {
		t.Error("the placeholder has a script")
	}
}

func TestStaticAssetsAreServedFromTheBundle(t *testing.T) {
	r := start(t, nil)
	res := r.do("GET", "/assets/app.js", nil, "")
	if res.StatusCode != 200 || res.body != "console.log(1)" || !strings.Contains(res.Header.Get("Content-Type"), "javascript") {
		t.Errorf("%d %q %q", res.StatusCode, res.body, res.Header.Get("Content-Type"))
	}
	for _, p := range []string{"/../x", "/assets/../../x", "/%2e%2e/index.html"} {
		if res := r.do("GET", p, nil, ""); res.StatusCode == 200 && strings.Contains(res.body, "isn't") {
			t.Errorf("%s served", p)
		}
	}
}

func newRecorder() http.ResponseWriter { return &recorder{h: http.Header{}} }

type recorder struct{ h http.Header }

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(int)             {}
func (r *recorder) Flush()                      {}

// U1: one UI per profile, not per state home.
func TestTwoProfilesRunTwoIndependentServers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no control socket on Windows")
	}
	base := filepath.Join(shortDir(t), "ui")
	a := start(t, func(c *Config) { c.StateDir, c.Profile = base, "/profiles/a.toml" })
	b := start(t, func(c *Config) { c.StateDir, c.Profile = base, "/profiles/b.toml" })
	if a.srv.dir == b.srv.dir || a.srv.URL() == b.srv.URL() {
		t.Fatalf("shared: %s %s", a.srv.dir, b.srv.dir)
	}
	// The same profile, by another spelling of its path, is the same UI.
	_, err := New(Config{Backend: &guibackend.Server{}, StateDir: base, Profile: "/profiles/../profiles/a.toml", Bundle: testBundle()})
	var ar *AlreadyRunning
	if !errors.As(err, &ar) {
		t.Fatalf("same profile: %v", err)
	}
	ha, err := RequestHandoff(context.Background(), base, "/profiles/a.toml", "url")
	if err != nil || ha.URL != a.srv.URL() {
		t.Fatalf("a: %+v %v", ha, err)
	}
	hb, err := RequestHandoff(context.Background(), base, "/profiles/b.toml", "url")
	if err != nil || hb.URL != b.srv.URL() {
		t.Fatalf("b: %+v %v", hb, err)
	}
	// Each token works only on its own server.
	pa, _ := url.Parse(ha.Target)
	if res := b.do("GET", "/?"+pa.RawQuery, nil, ""); res.StatusCode != 403 {
		t.Errorf("a's token on b: %d", res.StatusCode)
	}
	// Defence in depth: a handoff to a directory whose server serves another
	// profile is refused.
	_, err = requestHandoffAt(context.Background(), a.srv.dir, "/profiles/b.toml", "url")
	var pm *ProfileMismatchError
	if !errors.As(err, &pm) {
		t.Errorf("mismatch: %v", err)
	}
	// And ui.json names the profile.
	raw, _ := os.ReadFile(filepath.Join(a.srv.dir, infoName))
	if !strings.Contains(string(raw), `"profile":"/profiles/a.toml"`) {
		t.Errorf("ui.json: %s", raw)
	}
}

// A hashed directory too deep for a socket path falls back to printing
// where the UI runs.
func TestLongStateDirFallsBackToNoControl(t *testing.T) {
	base := filepath.Join(shortDir(t), strings.Repeat("d", 60), strings.Repeat("e", 60))
	r := start(t, func(c *Config) { c.StateDir = base })
	if _, err := os.Stat(filepath.Join(r.srv.dir, socketName)); err == nil {
		t.Fatal("a socket was made on a path over the limit")
	}
	_, err := RequestHandoff(context.Background(), base, r.profile, "open")
	var nc *NoControlError
	if !errors.As(err, &nc) || nc.URL != r.srv.URL() {
		t.Fatalf("%v", err)
	}
	// The server itself works.
	if res := r.do("GET", "/", nil, ""); res.StatusCode != 200 {
		t.Errorf("%d", res.StatusCode)
	}
}

// U3: a slow request body is cut off.
func TestSlowBodyPostIsCutOff(t *testing.T) {
	r := start(t, func(c *Config) { c.BodyReadTimeout = 300 * time.Millisecond })
	sec := r.launch()
	conn, err := net.Dial("tcp4", r.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /rpc HTTP/1.1\r\nHost: "+r.srv.Addr()+"\r\nSec-Fetch-Site: same-origin\r\nContent-Type: application/json\r\nContent-Length: 500\r\n"+SessionHeader+": "+sec+"\r\n\r\n{\"jsonrpc\"")
	began := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, conn) // ends when the server answers and closes, or cuts it
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("the slow body held the connection for %v", d)
	}
	// A normal POST still works.
	if res := r.rpc(sec, hello); res.StatusCode != 200 {
		t.Errorf("%d", res.StatusCode)
	}
}
