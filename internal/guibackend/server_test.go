package guibackend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
)

func testServer() *Server {
	return &Server{
		Hello: Hello{ClientVersion: "1.0.0-test", Mode: "local", Platform: "linux", Profile: map[string]any{"workspace": "ws_example", "project": "prj_example", "waiting_for_project": false}},
		Methods: map[string]Method{
			"echo":  {Class: Read, Call: func(_ context.Context, p json.RawMessage) (any, error) { return json.RawMessage(p), nil }},
			"boom":  {Class: Read, Call: func(context.Context, json.RawMessage) (any, error) { panic("secret-token-value") }},
			"fails": {Class: Read, Call: func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("no such thing") }},
		},
	}
}

func post(t *testing.T, h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func rpc(t *testing.T, h http.Handler, body string) any {
	t.Helper()
	rec := post(t, h, "application/json", body)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q, body %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err, rec.Body.String())
	}
	return v
}

func errorCode(v any) float64 {
	m, _ := v.(map[string]any)
	e, _ := m["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return code
}

func TestSingleCall(t *testing.T) {
	h := testServer().Handler()
	m := rpc(t, h, `{"jsonrpc":"2.0","id":7,"method":"echo","params":{"a":"é\n"}}`).(map[string]any)
	if m["jsonrpc"] != "2.0" || m["id"] != float64(7) || !reflect.DeepEqual(m["result"], map[string]any{"a": "é\n"}) {
		t.Fatalf("reply %v", m)
	}
	// A string id, a handler error, and a null result.
	m = rpc(t, h, `{"jsonrpc":"2.0","id":"x","method":"fails"}`).(map[string]any)
	if m["id"] != "x" || errorCode(m) != CodeMethod || m["error"].(map[string]any)["message"] != "no such thing" {
		t.Fatalf("reply %v", m)
	}
	m = rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"echo","params":null}`).(map[string]any)
	if v, ok := m["result"]; !ok || v != nil {
		t.Fatalf("a null result must be present: %v", m)
	}
	// Content-Type parameters are fine.
	if rec := post(t, h, "application/json; charset=utf-8", `{"jsonrpc":"2.0","id":1,"method":"hello"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestBatch(t *testing.T) {
	h := testServer().Handler()
	v := rpc(t, h, `[
		{"jsonrpc":"2.0","id":1,"method":"echo","params":"one"},
		{"jsonrpc":"2.0","method":"echo","params":"a notification: no reply"},
		{"jsonrpc":"2.0","id":2,"method":"nope"},
		{"jsonrpc":"2.0","id":3,"method":"hello"},
		{"id":4,"method":"echo"},
		7
	]`)
	got, _ := v.([]any)
	if len(got) != 5 {
		t.Fatalf("want 5 replies, got %v", v)
	}
	byID := map[any]map[string]any{}
	invalid := 0
	for _, r := range got {
		m := r.(map[string]any)
		if m["id"] == nil {
			invalid++
			if errorCode(m) != CodeInvalid {
				t.Fatalf("reply %v", m)
			}
			continue
		}
		byID[m["id"]] = m
	}
	if byID[float64(1)]["result"] != "one" || errorCode(byID[float64(2)]) != CodeNotFound || byID[float64(3)]["result"] == nil || errorCode(byID[float64(4)]) != CodeInvalid || invalid != 1 {
		t.Fatalf("replies %v", got)
	}
	// An empty batch is one invalid request; a batch of notifications is 204.
	if errorCode(rpc(t, h, `[]`)) != CodeInvalid {
		t.Fatal("empty batch")
	}
	if rec := post(t, h, "application/json", `[{"jsonrpc":"2.0","method":"echo"}]`); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := post(t, h, "application/json", `{"jsonrpc":"2.0","method":"echo"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("a notification: status %d", rec.Code)
	}
}

func TestBatchOverTheCapIsRefusedAndNothingRuns(t *testing.T) {
	s := testServer()
	var mu sync.Mutex
	ran := 0
	s.Methods["count"] = Method{Class: Read, Call: func(context.Context, json.RawMessage) (any, error) {
		mu.Lock()
		ran++
		mu.Unlock()
		return nil, nil
	}}
	h := s.Handler()
	item := func(i int) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"count"}`, i) }
	var items []string
	for i := 1; i <= MaxBatch+1; i++ {
		items = append(items, item(i))
	}
	m := rpc(t, h, "["+strings.Join(items, ",")+"]").(map[string]any)
	if errorCode(m) != CodeInvalid || m["error"].(map[string]any)["message"] != "batch too large" || ran != 0 {
		t.Fatalf("reply %v, ran %d", m, ran)
	}
	// A batch at the cap runs.
	if got := rpc(t, h, "["+strings.Join(items[:MaxBatch], ",")+"]").([]any); len(got) != MaxBatch || ran != MaxBatch {
		t.Fatalf("replies %d, ran %d", len(got), ran)
	}
}

func TestCancelledRequestDoesNotTakeASlot(t *testing.T) {
	s := testServer()
	release := make(chan struct{})
	var mu sync.Mutex
	started, ran := 0, map[string]bool{}
	s.Methods["hold"] = Method{Class: Read, Call: func(context.Context, json.RawMessage) (any, error) {
		mu.Lock()
		started++
		mu.Unlock()
		<-release
		return "held", nil
	}}
	s.Methods["late"] = Method{Class: Read, Call: func(context.Context, json.RawMessage) (any, error) {
		mu.Lock()
		ran["late"] = true
		mu.Unlock()
		return nil, nil
	}}
	h := s.Handler()
	var wg sync.WaitGroup
	for i := 0; i < MaxInFlight; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"hold"}`) }()
	}
	waitFor(t, "every slot taken", func() bool { mu.Lock(); defer mu.Unlock(); return started == MaxInFlight })
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"late"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled request kept waiting for a slot")
	}
	mu.Lock()
	if ran["late"] {
		t.Fatal("a cancelled request ran its method")
	}
	mu.Unlock()
	close(release)
	wg.Wait()
	if n := len(s.sem); n != 0 {
		t.Fatalf("%d slots still held", n)
	}
	// All four slots are free again.
	if m := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"echo","params":1}`).(map[string]any); m["result"] != float64(1) {
		t.Fatalf("reply %v", m)
	}
}

func TestBadJSONIsAParseError(t *testing.T) {
	h := testServer().Handler()
	for _, body := range []string{`{not json`, ``, `[1,`} {
		if m := rpc(t, h, body); errorCode(m) != CodeParse || m.(map[string]any)["id"] != nil {
			t.Fatalf("%q: %v", body, m)
		}
	}
	if m := rpc(t, h, `{"jsonrpc":"2.0","id":2}`); errorCode(m) != CodeInvalid {
		t.Fatalf("reply %v", m)
	}
	if m := rpc(t, h, `{"jsonrpc":"1.0","id":2,"method":"echo"}`); errorCode(m) != CodeInvalid {
		t.Fatalf("reply %v", m)
	}
}

func TestUnknownMethodIsNotFound(t *testing.T) {
	h := testServer().Handler()
	for i, name := range []string{"nope", "messages.send", "linear.do", "updates.apply", "model.setup", "hello.again"} {
		m := rpc(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, i, name))
		if errorCode(m) != CodeNotFound {
			t.Fatalf("%s: %v", name, m)
		}
	}
}

func TestWriteMethodsAreUnreachableByDefault(t *testing.T) {
	for _, allow := range []bool{false, true} {
		s := testServer()
		s.AllowWrites = allow
		s.Methods["messages.send"] = Method{Class: Write, Call: func(context.Context, json.RawMessage) (any, error) { return "sent", nil }}
		s.Methods["linear.confirm"] = Method{Class: Confirm, Call: func(context.Context, json.RawMessage) (any, error) { return "done", nil }}
		h := s.Handler()
		for i, name := range []string{"messages.send", "linear.confirm"} {
			m := rpc(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, i+1, name)).(map[string]any)
			if allow && m["result"] == nil || !allow && (errorCode(m) != CodeNotFound || m["error"].(map[string]any)["message"] != "method not found") {
				t.Fatalf("allow=%v %s: %v", allow, name, m)
			}
		}
	}
}

func TestHelloCannotBeReplaced(t *testing.T) {
	s := testServer()
	s.Methods["hello"] = Method{Class: Read, Call: func(context.Context, json.RawMessage) (any, error) { return "replaced", nil }}
	m := rpc(t, s.Handler(), `{"jsonrpc":"2.0","id":1,"method":"hello"}`).(map[string]any)
	r, _ := m["result"].(map[string]any)
	if r["client_version"] != "1.0.0-test" || r["protocol"] != float64(ProtocolVersion) || r["mode"] != "local" || r["platform"] != "linux" || r["profile"] == nil {
		t.Fatalf("hello %v", m)
	}
}

func TestPanicIsRecoveredAndTheNextCallWorks(t *testing.T) {
	var log bytes.Buffer
	s := testServer()
	s.Log = &log
	h := s.Handler()
	m := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"boom","params":{"password":"hunter2"}}`)
	if errorCode(m) != CodeInternal || strings.Contains(fmt.Sprint(m), "secret-token-value") {
		t.Fatalf("reply %v", m)
	}
	if m = rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"echo","params":"still here"}`); m.(map[string]any)["result"] != "still here" {
		t.Fatalf("reply %v", m)
	}
	if !strings.Contains(log.String(), `"boom" code=-32603`) || strings.Contains(log.String(), "hunter2") || strings.Contains(log.String(), "secret-token-value") {
		t.Fatalf("log %q", log.String())
	}
}

func TestBodyOverTheCapIs413(t *testing.T) {
	h := testServer().Handler()
	pad := strings.Repeat("a", MaxBody)
	rec := post(t, h, "application/json", `{"jsonrpc":"2.0","id":1,"method":"echo","params":"`+pad+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", rec.Code)
	}
	// A body at the cap is read in full.
	head, tail := `{"jsonrpc":"2.0","id":1,"method":"echo","params":"`, `"}`
	body := head + strings.Repeat("a", MaxBody-len(head)-len(tail)) + tail
	if len(body) != MaxBody {
		t.Fatal(len(body))
	}
	if rec = post(t, h, "application/json", body); rec.Code != http.StatusOK || rec.Body.Len() < MaxBody-100 {
		t.Fatalf("status %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestOnlyPostWithJSON(t *testing.T) {
	h := testServer().Handler()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/rpc", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("%s: status %d, allow %q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/json-seq", "multipart/form-data; boundary=x", ";;"} {
		if rec := post(t, h, ct, `{"jsonrpc":"2.0","id":1,"method":"hello"}`); rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%q: status %d", ct, rec.Code)
		}
	}
}

func TestConcurrentCallsRunUnderTheSemaphore(t *testing.T) {
	s := testServer()
	var mu sync.Mutex
	running, peak := 0, 0
	release := make(chan struct{})
	s.Methods["work"] = Method{Class: Read, Call: func(context.Context, json.RawMessage) (any, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return "ok", nil
	}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	replies := make(chan string, 9)
	for i := 1; i <= 9; i++ {
		go func() {
			resp, err := http.Post(srv.URL, "application/json", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"work"}`, i)))
			if err != nil {
				replies <- err.Error()
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			replies <- string(b)
		}()
	}
	for {
		mu.Lock()
		n := running
		mu.Unlock()
		if n == MaxInFlight {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if running != MaxInFlight {
		t.Fatalf("running %d, want exactly %d while the rest wait", running, MaxInFlight)
	}
	mu.Unlock()
	close(release)
	for i := 0; i < 9; i++ {
		if r := <-replies; !strings.Contains(r, `"ok"`) {
			t.Fatalf("reply %q", r)
		}
	}
	if peak > MaxInFlight {
		t.Fatalf("peak %d", peak)
	}
}

func TestSlowCallDoesNotBlockAFastOne(t *testing.T) {
	s := testServer()
	started, release := make(chan struct{}), make(chan struct{})
	s.Methods["slow"] = Method{Class: Read, Call: func(context.Context, json.RawMessage) (any, error) {
		close(started)
		<-release
		return "slow", nil
	}}
	h := s.Handler()
	slow := make(chan any, 1)
	go func() { slow <- rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"slow"}`) }()
	<-started
	if m := rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"echo","params":"fast"}`).(map[string]any); m["result"] != "fast" {
		t.Fatalf("reply %v", m)
	}
	close(release)
	if m := (<-slow).(map[string]any); m["result"] != "slow" {
		t.Fatalf("reply %v", m)
	}
}

func TestLogHasMethodsAndDurationsOnly(t *testing.T) {
	var log bytes.Buffer
	s := testServer()
	s.Log = &log
	h := s.Handler()
	rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"echo","params":{"token":"s3cret"}}`)
	rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"fails"}`)
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `ui: "echo" code=0 took=`) || !strings.HasPrefix(lines[1], `ui: "fails" code=-32000 took=`) {
		t.Fatalf("log %q", log.String())
	}
	if strings.Contains(log.String(), "s3cret") || strings.Contains(log.String(), "no such thing") {
		t.Fatalf("log leaks: %q", log.String())
	}
}

func TestLogNameIsBoundedAndPrintable(t *testing.T) {
	var log bytes.Buffer
	s := testServer()
	s.Log = &log
	h := s.Handler()
	rpc(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, strings.Repeat("m", 5000)))
	rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"a\u001b[31m\u0000b\r\nfake: line\u202e"}`)
	if want := `"` + strings.Repeat("m", 64) + `…"`; !strings.Contains(log.String(), want) {
		t.Fatalf("log %q", log.String())
	}
	if !strings.Contains(log.String(), `"a[31mbfake: line"`) {
		t.Fatalf("log %q", log.String())
	}
	if n := strings.Count(log.String(), "\n"); n != 2 || strings.ContainsAny(strings.ReplaceAll(log.String(), "\n", ""), "\x1b\x00\r\u202e") {
		t.Fatalf("log has %d lines or controls: %q", n, log.String())
	}
	if len(logName(strings.Repeat("é", 1000))) > maxLogName+len("…") {
		t.Fatal("multibyte name not bounded")
	}
}

// sse is one connected event client.
type sse struct {
	resp *http.Response
	br   *bufio.Reader
}

func connectSSE(t *testing.T, url string) *sse {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, type %q, cache %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
	}
	c := &sse{resp: resp, br: bufio.NewReader(resp.Body)}
	if line := c.line(t); line != ": connected" {
		t.Fatalf("first line %q", line)
	}
	c.line(t)
	return c
}

func (c *sse) line(t *testing.T) string {
	t.Helper()
	l, err := c.br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(l, "\n")
}

// event reads one event block, skipping comment lines.
func (c *sse) event(t *testing.T) (name, data string) {
	t.Helper()
	for {
		l := c.line(t)
		switch {
		case strings.HasPrefix(l, "event: "):
			name = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			data = strings.TrimPrefix(l, "data: ")
		case l == "" && name != "":
			return name, data
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

func TestEventsDeliverToEveryClientAndTrackConnections(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.Events())
	t.Cleanup(srv.Close) // runs after the clients close
	if s.Connected() != 0 || !s.LastDisconnect().IsZero() {
		t.Fatal("a fresh hub has no clients and no disconnect")
	}
	a, b := connectSSE(t, srv.URL), connectSSE(t, srv.URL)
	waitFor(t, "two clients", func() bool { return s.Connected() == 2 })
	if err := s.Notify("outbox.count", map[string]any{"n": 3}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*sse{a, b} {
		if name, data := c.event(t); name != "outbox.count" || data != `{"n":3}` {
			t.Fatalf("event %q data %q", name, data)
		}
	}
	before := time.Now()
	a.resp.Body.Close()
	waitFor(t, "one client", func() bool { return s.Connected() == 1 })
	waitFor(t, "a disconnect time", func() bool { return !s.LastDisconnect().Before(before) })
	if err := s.Notify("messages.changed", nil); err != nil {
		t.Fatal(err)
	}
	if name, _ := b.event(t); name != "messages.changed" {
		t.Fatalf("event %q", name)
	}
	if err := s.Notify("bad\nname", nil); err == nil {
		t.Fatal("an event name with a newline must be refused")
	}
	if err := s.Notify("x", make(chan int)); err == nil {
		t.Fatal("unencodable params must fail")
	}
	b.resp.Body.Close()
	waitFor(t, "no clients", func() bool { return s.Connected() == 0 })
}

func TestEventsOnlyGet(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer().Events().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/events", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	s := testServer()
	s.Heartbeat = 10 * time.Millisecond
	srv := httptest.NewServer(s.Events())
	t.Cleanup(srv.Close) // runs after the clients close
	c := connectSSE(t, srv.URL)
	if l := c.line(t); l != ": heartbeat" {
		t.Fatalf("line %q", l)
	}
}

// stuckWriter is a client that stops reading: its Write blocks.
type stuckWriter struct {
	header  http.Header
	unstick chan struct{}
}

func (w *stuckWriter) Header() http.Header { return w.header }
func (w *stuckWriter) WriteHeader(int)     {}
func (w *stuckWriter) Flush()              {}
func (w *stuckWriter) Write(p []byte) (int, error) {
	<-w.unstick
	return len(p), nil
}

func TestSlowEventClientIsDroppedWithoutBlockingOthers(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.Events())
	t.Cleanup(srv.Close) // runs after the clients close
	good := connectSSE(t, srv.URL)
	stuck := &stuckWriter{header: http.Header{}, unstick: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Events().ServeHTTP(stuck, httptest.NewRequest(http.MethodGet, "/events", nil))
	}()
	waitFor(t, "both clients", func() bool { return s.Connected() == 2 })
	start := time.Now()
	const sent = 3 * clientBuffer
	// The good client reads each event as it comes, so it never falls behind
	// by a buffer; the stuck one does.
	for i := 0; i < sent; i++ {
		if err := s.Notify("tick", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
		if _, data := good.event(t); data != fmt.Sprintf(`{"i":%d}`, i) {
			t.Fatalf("event %d: %q", i, data)
		}
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Notify blocked on a slow client")
	}
	if s.Connected() != 1 || s.LastDisconnect().IsZero() {
		t.Fatalf("connected %d, last disconnect %v", s.Connected(), s.LastDisconnect())
	}
	// The dropped handler returns once its write unblocks.
	close(stuck.unstick)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the dropped client's handler did not return")
	}
}

type fakeExec struct {
	requests []bus.Request
}

func (f *fakeExec) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.requests = append(f.requests, r)
	return bus.Result{Source: "local-authority", Data: map[string]any{"sessions": []any{
		map[string]any{"id": "ses_1", "name": "engineer-a", "role": "engineer", "agent_kind": "claude-code", "status": "active", "presence": "active", "stale": false},
		map[string]any{"id": "ses_2", "name": "lead", "role": "lead", "agent_kind": "codex", "status": "active", "presence": "idle", "stale": true},
	}}}, nil
}

func (f *fakeExec) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("unused")
}

// TestRPCFixtures plays every testdata/rpc file against the handler with a
// fake executor. The frontend's tests read the same files.
func TestRPCFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "rpc", "*.json"))
	if err != nil || len(files) < 4 {
		t.Fatal(files, err)
	}
	exec := &fakeExec{}
	s := testServer()
	s.Methods["sessions.list"] = SessionsList(exec, Scope{Workspace: "ws_example", Project: "prj_example"})
	s.Methods["doctor.checks"] = DoctorChecks(func(context.Context) (doctor.Report, error) {
		return doctor.Report{Status: doctor.Warn, GeneratedAt: "2026-01-02T03:04:05Z", Checks: []doctor.Check{
			{ID: "profile", Title: "Profile", Status: doctor.Pass, Message: "profile found", Path: "/home/example/.config/clawbridge/config.toml"},
			{ID: "skill", Title: "Skill", Status: doctor.Warn, Message: "skill not installed", Action: "clawbridge skill install"},
		}}, nil
	})
	delete(s.Methods, "echo")
	h := s.Handler()
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Description string          `json:"description"`
			Request     json.RawMessage `json:"request"`
			Response    json.RawMessage `json:"response"`
		}
		if err = json.Unmarshal(raw, &fx); err != nil || fx.Description == "" {
			t.Fatal(file, err)
		}
		rec := post(t, h, "application/json", string(fx.Request))
		var got, want any
		if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(file, err, rec.Body.String())
		}
		if err = json.Unmarshal(fx.Response, &want); err != nil {
			t.Fatal(file, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %s\nwant %s", file, rec.Body.Bytes(), fx.Response)
		}
	}
	if len(exec.requests) != 1 {
		t.Fatalf("executor calls: %d", len(exec.requests))
	}
	if r := exec.requests[0]; r.Operation != "list_sessions" || r.Workspace != "ws_example" || r.Project != "prj_example" || r.Args["limit"] != 100 || r.Args["after"] != int64(0) || r.Args["instance"] != nil {
		t.Fatalf("request %+v", r)
	}
}

func TestSessionsListSendsTheInstanceOfASession(t *testing.T) {
	exec := &fakeExec{}
	m := SessionsList(exec, Scope{Workspace: "w", Project: "p", Session: "s", Instance: "i"})
	if _, err := m.Call(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if r := exec.requests[0]; r.Session != "s" || r.Args["instance"] != "i" {
		t.Fatalf("request %+v", r)
	}
}

func TestPlatformIsOneOfTheFour(t *testing.T) {
	switch Platform() {
	case "darwin", "linux", "windows", "wsl":
	default:
		t.Fatal(Platform())
	}
}
