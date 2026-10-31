package backend

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fake is an in-process stand-in for `clawbridge gui-backend --stdio`.
type fake struct {
	noise    []string // lines printed before the preamble, like rc files
	protocol int      // hello's protocol; 0 means Protocol
	noHello  bool     // answer hello with an error
	helloErr int      // that error's code; 0 means RPCUpdateNeeded
	silent   bool     // never answer hello
	usage    string   // print this to stderr and exit before the preamble
	stderrN  int      // bytes in one stderr line written before serving
	// handle answers one request; returning false means "say nothing".
	handle func(f *fake, method string, id int64, w io.Writer) bool

	mu       sync.Mutex
	received []string
	started  int
}

func (f *fake) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func (f *fake) spawn(*Plan) (*Child, error) {
	f.mu.Lock()
	f.started++
	f.mu.Unlock()
	inR, inW := io.Pipe()   // shell -> backend
	outR, outW := io.Pipe() // backend -> shell
	errR, errW := io.Pipe()
	go func() {
		defer outW.Close()
		defer errW.Close()
		r := bufio.NewReader(inR)
		nonce, err := r.ReadString('\n')
		if err != nil {
			return
		}
		nonce = strings.TrimSpace(nonce)
		if f.usage != "" {
			fmt.Fprintln(errW, f.usage)
			return // exits before any preamble, like a client without gui-backend
		}
		for _, l := range f.noise {
			fmt.Fprintln(outW, l)
		}
		fmt.Fprintf(errW, "backend ready\n")
		if f.noise == nil || f.noise[len(f.noise)-1] != "NO PREAMBLE" {
			fmt.Fprintf(outW, "%s %s\n", PreambleWord, nonce)
		}
		if f.stderrN > 0 {
			fmt.Fprintln(errW, strings.Repeat("e", f.stderrN))
		}
		for {
			body, err := ReadFrame(r)
			if err != nil {
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			f.received = append(f.received, req.Method)
			f.mu.Unlock()
			if req.Method == "hello" && f.silent {
				continue
			}
			if req.Method == "hello" {
				p := f.protocol
				if p == 0 {
					p = Protocol
				}
				if f.noHello {
					code := f.helloErr
					if code == 0 {
						code = RPCUpdateNeeded
					}
					writeJSON(outW, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": "protocols do not overlap"}})
					continue
				}
				writeJSON(outW, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocol": p, "client_version": "1.1.0", "mode": "local", "platform": "linux"}})
				continue
			}
			if f.handle != nil && f.handle(f, req.Method, req.ID, outW) {
				continue
			}
			writeJSON(outW, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"method": req.Method}})
		}
	}()
	return &Child{
		Stdin: inW, Stdout: outR, Stderr: errR,
		Wait: func() error { return nil },
		Kill: func() { _ = outR.Close(); _ = inR.Close() },
	}, nil
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_ = WriteFrame(w, b)
}

type events struct {
	mu  sync.Mutex
	all []emitted
	ch  chan emitted
}

type emitted struct {
	name string
	data any
}

func newEvents() *events { return &events{ch: make(chan emitted, 64)} }
func (e *events) emit(name string, data any) {
	e.mu.Lock()
	e.all = append(e.all, emitted{name, data})
	e.mu.Unlock()
	select {
	case e.ch <- emitted{name, data}:
	default:
	}
}

// waitState blocks until a backend:state event with the state arrives.
func (e *events) waitState(t *testing.T, want State) Status {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-e.ch:
			if s, ok := ev.data.(Status); ok && ev.name == EventState && s.State == want {
				return s
			}
		case <-deadline:
			t.Fatalf("no %q state; saw %+v", want, e.all)
		}
	}
}

// waitStateFor is waitState with a longer wait, for a real WSL start.
func (e *events) waitStateFor(t *testing.T, want State, d time.Duration) Status {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-e.ch:
			if s, ok := ev.data.(Status); ok && ev.name == EventState {
				if s.State == want {
					return s
				}
				if s.State != StateStarting {
					t.Fatalf("wanted %q, got %+v; logs %q", want, s, "")
				}
			}
		case <-deadline:
			t.Fatalf("no %q state in %s", want, d)
		}
	}
}

func (e *events) waitNamed(t *testing.T, name string) emitted {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-e.ch:
			if ev.name == name {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %q event", name)
		}
	}
}

func newTestBridge(f *fake) (*Bridge, *events) {
	ev := newEvents()
	b := New(func() (*Plan, error) { return &Plan{Target: "this computer", Cmd: exec.Command("true")}, nil }, ev.emit, "0.0.1", "linux")
	b.Spawn = f.spawn
	b.PreambleTimeout = 2 * time.Second
	b.CallTimeout = 2 * time.Second
	return b, ev
}

func TestBridgeStartsAndCalls(t *testing.T) {
	f := &fake{noise: []string{"motd: hi", "nvm: using node v22"}}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.Start()
	st := ev.waitState(t, StateRunning)
	if st.ClientVersion != "1.1.0" || st.Target != "this computer" {
		t.Fatalf("status: %+v", st)
	}
	// The noise before the preamble is kept for Help → Logs.
	logs := strings.Join(b.Logs(), "\n")
	if !strings.Contains(logs, "discarded: motd: hi") || !strings.Contains(logs, "discarded: nvm: using node v22") {
		t.Fatalf("logs: %q", logs)
	}
	out, err := b.Call("sessions.list", json.RawMessage(`{"limit":5}`))
	if err != nil || !strings.Contains(string(out), `"sessions.list"`) {
		t.Fatalf("sessions.list: %s %v", out, err)
	}
	// hello is answered from the handshake; the backend saw it only once.
	h, err := b.Call("hello", nil)
	if err != nil || !strings.Contains(string(h), `"client_version"`) {
		t.Fatalf("hello: %s %v", h, err)
	}
	if got := strings.Join(f.calls(), ","); got != "hello,sessions.list" {
		t.Fatalf("backend saw %q", got)
	}
}

func TestBridgeRefusalsNeverReachTheBackend(t *testing.T) {
	f := &fake{}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.Start()
	ev.waitState(t, StateRunning)
	if _, err := b.Call("sessions.list", json.RawMessage(`{"nonce":"abc"}`)); !errors.Is(err, ErrNonce) {
		t.Fatalf("nonce: %v", err)
	}
	if _, err := b.Call("messages.send", json.RawMessage(`{}`)); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("write: %v", err)
	}
	if got := strings.Join(f.calls(), ","); got != "hello" {
		t.Fatalf("backend saw %q after refusals", got)
	}
}

func TestBridgeForwardsNotificationsUnderAPrefix(t *testing.T) {
	f := &fake{handle: func(f *fake, method string, id int64, w io.Writer) bool {
		if method == "sessions.list" {
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "method": "outbox.count", "params": map[string]any{"queued": 3}})
			// A notification that tries to spoof the shell's own event is
			// dropped by its name; one with a bad name too.
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "method": "backend:state", "params": map[string]any{"state": "running"}})
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "method": "../x", "params": 1})
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			return true
		}
		return false
	}}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.Start()
	ev.waitState(t, StateRunning)
	if _, err := b.Call("sessions.list", nil); err != nil {
		t.Fatal(err)
	}
	got := ev.waitNamed(t, NotifyPrefix+"outbox.count")
	if m, _ := got.data.(map[string]any); m["queued"] != float64(3) {
		t.Fatalf("payload: %#v", got.data)
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	for _, e := range ev.all {
		if e.name == "backend:state" {
			if s, ok := e.data.(Status); !ok || s.State == "" {
				t.Fatalf("a spoofed state event reached the page: %#v", e)
			}
		}
		if strings.Contains(e.name, "..") {
			t.Fatalf("bad name forwarded: %q", e.name)
		}
	}
}

func TestBridgeCrashIsBackendStoppedAndRestartReplaysNothing(t *testing.T) {
	var crashes sync.WaitGroup
	crashes.Add(1)
	f := &fake{handle: func(f *fake, method string, id int64, w io.Writer) bool {
		if method == "doctor.checks" {
			// Die with the call in flight: no answer, stream closes.
			if pw, ok := w.(*io.PipeWriter); ok {
				_ = pw.Close()
			}
			crashes.Done()
			return true
		}
		return false
	}}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.Start()
	ev.waitState(t, StateRunning)

	errc := make(chan error, 1)
	go func() { _, err := b.Call("doctor.checks", nil); errc <- err }()
	crashes.Wait()
	if err := <-errc; !errors.Is(err, ErrStopped) {
		t.Fatalf("in-flight call: %v", err)
	}
	st := ev.waitState(t, StateStopped)
	if st.Code != CodeExited {
		t.Fatalf("status: %+v", st)
	}
	if _, err := b.Call("sessions.list", nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("call while stopped: %v", err)
	}

	before := len(f.calls())
	b.Start() // Restart
	ev.waitState(t, StateRunning)
	// The new process got a hello and nothing else: the in-flight
	// doctor.checks was not replayed.
	if got := f.calls()[before:]; len(got) != 1 || got[0] != "hello" {
		t.Fatalf("after restart the backend saw %v", got)
	}
}

func TestBridgeFramingFailuresStopTheBackend(t *testing.T) {
	cases := map[string]func(w io.Writer){
		"oversized frame": func(w io.Writer) {
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", MaxFrame+1)
		},
		"eof mid-frame": func(w io.Writer) {
			fmt.Fprint(w, "Content-Length: 50\r\n\r\nabc")
			if pw, ok := w.(*io.PipeWriter); ok {
				_ = pw.Close()
			}
		},
		"garbage header": func(w io.Writer) {
			fmt.Fprint(w, "this is not a frame\r\n\r\n")
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fake{handle: func(f *fake, method string, id int64, w io.Writer) bool {
				if method == "sessions.list" {
					bad(w)
					return true
				}
				return false
			}}
			b, ev := newTestBridge(f)
			defer b.Stop()
			b.Start()
			ev.waitState(t, StateRunning)
			errc := make(chan error, 1)
			go func() { _, err := b.Call("sessions.list", nil); errc <- err }()
			st := ev.waitState(t, StateStopped)
			if st.Code != CodeFrame {
				t.Fatalf("status: %+v", st)
			}
			if err := <-errc; !errors.Is(err, ErrStopped) {
				t.Fatalf("pending call: %v", err)
			}
		})
	}
}

func TestBridgeEOFBeforeThePreamble(t *testing.T) {
	// The backend writes noise and exits: "backend stopped", not a hang.
	f := &fake{noise: []string{"bash: clawbridge: command not found", "NO PREAMBLE"}}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.PreambleTimeout = 300 * time.Millisecond
	b.Start()
	st := ev.waitState(t, StateStopped)
	if st.Code != CodeNoPreamble {
		t.Fatalf("status: %+v", st)
	}
	if !strings.Contains(strings.Join(b.Logs(), "\n"), "command not found") {
		t.Fatalf("the noise must reach the logs: %q", b.Logs())
	}
}

func TestBridgeProtocolMismatchNeedsAnUpdate(t *testing.T) {
	for name, f := range map[string]*fake{
		"other protocol": {protocol: 7},
		"hello refused":  {noHello: true},
	} {
		t.Run(name, func(t *testing.T) {
			b, ev := newTestBridge(f)
			defer b.Stop()
			b.Start()
			st := ev.waitState(t, StateUpdateNeeded)
			if st.Code != CodeUpdateNeeded || st.Reason == "" {
				t.Fatalf("status: %+v", st)
			}
			if _, err := b.Call("sessions.list", nil); !errors.Is(err, ErrStopped) {
				t.Fatalf("calls must not pass: %v", err)
			}
		})
	}
}

func TestBridgeSetupErrorsAreStates(t *testing.T) {
	ev := newEvents()
	b := New(func() (*Plan, error) { return nil, &SetupError{CodeNeedsDistro, "pick a distro"} }, ev.emit, "0", "windows")
	b.Start()
	st := ev.waitState(t, StateNeedsSetup)
	if st.Code != CodeNeedsDistro || st.OS != "windows" {
		t.Fatalf("status: %+v", st)
	}
}

func TestBridgeCallTimeout(t *testing.T) {
	f := &fake{handle: func(f *fake, method string, id int64, w io.Writer) bool { return method == "doctor.checks" }}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.CallTimeout = 150 * time.Millisecond
	b.Start()
	ev.waitState(t, StateRunning)
	if _, err := b.Call("doctor.checks", nil); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("got %v", err)
	}
}

func TestBridgeRPCErrorsKeepTheirCode(t *testing.T) {
	f := &fake{handle: func(f *fake, method string, id int64, w io.Writer) bool {
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return true
	}}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.Start()
	ev.waitState(t, StateRunning)
	_, err := b.Call("doctor.checks", nil)
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -32601 {
		t.Fatalf("got %v", err)
	}
}

func TestBridgeHelloNotAnsweredStopsTheBackend(t *testing.T) {
	f := &fake{silent: true}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.CallTimeout = 200 * time.Millisecond
	b.Start()
	st := ev.waitState(t, StateStopped)
	if st.Code != CodeNoHello || !strings.Contains(st.Reason, "did not answer hello") {
		t.Fatalf("status: %+v", st)
	}
	// The child is gone, and the late read-loop end does not rewrite the status.
	time.Sleep(100 * time.Millisecond)
	if got := b.Status(); got.State != StateStopped || got.Code != CodeNoHello {
		t.Fatalf("status changed after the stop: %+v", got)
	}
	if _, err := b.Call("sessions.list", nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("call: %v", err)
	}
}

func TestBridgeOnlyMinus32002MeansUpdateNeeded(t *testing.T) {
	b, ev := newTestBridge(&fake{noHello: true, helloErr: -32000})
	defer b.Stop()
	b.Start()
	st := ev.waitState(t, StateStopped)
	if st.Code != CodeNoHello || !strings.Contains(st.Reason, "protocols do not overlap") {
		t.Fatalf("status: %+v", st)
	}
}

func TestBridgeSurvivesAnOverlongStderrLine(t *testing.T) {
	// One 300 KiB stderr line must not stop the drain: the child would
	// block on a full pipe and never answer.
	f := &fake{stderrN: 300 << 10}
	b, ev := newTestBridge(f)
	defer b.Stop()
	b.Start()
	ev.waitState(t, StateRunning)
	if _, err := b.Call("sessions.list", nil); err != nil {
		t.Fatal(err)
	}
	var long string
	for _, l := range b.Logs() {
		if strings.HasPrefix(l, "stderr: eee") {
			long = l
		}
	}
	if len(long) == 0 || len(long) > maxLogLine+len("stderr: ")+len("…") {
		t.Fatalf("the long line must be kept, cut: %d bytes", len(long))
	}
}

func TestTruncateOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	for max := 1; max < len(s); max++ {
		got := truncateUTF8(s, max)
		if !utf8.ValidString(got) || len(got) > max {
			t.Fatalf("max %d: %q", max, got)
		}
	}
	// A capped line read cuts the same way.
	r := newReader(strings.NewReader(strings.Repeat("日", 5000) + "\nnext\n"))
	line, err := readLineCapped(r, maxLogLine)
	if err != nil || !utf8.ValidString(line) || len(line) > maxLogLine {
		t.Fatalf("%d %v", len(line), err)
	}
	if next, _ := readLineCapped(r, maxLogLine); next != "next" {
		t.Fatalf("the reader must continue after a long line: %q", next)
	}
	// Preamble discards too.
	var junk []string
	in := strings.Repeat("日", 5000) + "\n" + PreambleWord + " " + testNonce + "\n"
	if err := SyncPreamble(newReader(strings.NewReader(in)), testNonce, func(l string) { junk = append(junk, l) }); err != nil || len(junk) != 1 || !utf8.ValidString(junk[0]) {
		t.Fatalf("%v %d", err, len(junk))
	}
}

func TestBridgeOldClientIsUpdateNeededNotStopped(t *testing.T) {
	// A v1.0.0 client prints its unknown-command line and exits.
	for _, line := range []string{
		`unknown command "gui-backend"; use --help`,
		`clawbridge: unknown command "gui-backend"; use --help`,
	} {
		b, ev := newTestBridge(&fake{usage: line})
		b.Start()
		st := ev.waitState(t, StateUpdateNeeded)
		b.Stop()
		if st.Code != CodeUpdateNeeded || !strings.Contains(st.Reason, "newer client") {
			t.Fatalf("%q: %+v", line, st)
		}
	}
}

func TestBridgeSilentExitIsStoppedNoPreamble(t *testing.T) {
	for _, usage := range []string{"clawbridge: something else broke", "unknown command \"other\"; use --help"} {
		b, ev := newTestBridge(&fake{usage: usage})
		b.Start()
		st := ev.waitState(t, StateStopped)
		b.Stop()
		if st.Code != CodeNoPreamble {
			t.Fatalf("%q: %+v", usage, st)
		}
	}
}
