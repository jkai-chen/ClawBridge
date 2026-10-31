package backend

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// maxLogLine bounds one kept log line.
const maxLogLine = 4096

// RPCUpdateNeeded is the JSON-RPC error code a backend answers hello with
// when the protocol versions do not overlap. Any other hello error means the
// client is broken, not old.
const RPCUpdateNeeded = -32002

// Protocol is the GUI backend protocol version this shell speaks.
const Protocol = 1

// State is where the backend connection stands.
type State string

const (
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateStopped      State = "stopped"
	StateNeedsSetup   State = "needs-setup"
	StateUpdateNeeded State = "update-needed"
)

// Status is what the UI is told about the backend.
type Status struct {
	State         State           `json:"state"`
	Code          string          `json:"code,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Target        string          `json:"target,omitempty"`
	Distro        string          `json:"distro,omitempty"`
	OS            string          `json:"os"`
	ClientVersion string          `json:"client_version,omitempty"`
	Hello         json.RawMessage `json:"hello,omitempty"`
}

// Event names the shell emits to the page. Notifications from the backend
// are forwarded as NotifyPrefix + method, so a backend can never spoof the
// shell's own state event.
const (
	EventState   = "backend:state"
	NotifyPrefix = "rpc/"
)

var methodName = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-zA-Z][a-zA-Z0-9]*)+$`)

// ErrStopped is returned for a call made while no backend is running, and
// for every call that was in flight when it stopped. Nothing is replayed.
var ErrStopped = errors.New("backend stopped")

// RPCError is a JSON-RPC error reply from the backend.
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// Bridge owns the backend child process and the calls made to it.
type Bridge struct {
	Resolve    func() (*Plan, error)
	Spawn      func(*Plan) (*Child, error) // default: StartCmd(plan.Cmd)
	Emit       func(name string, data any)
	GUIVersion string
	OS         string
	// Timeouts; zero values use the defaults.
	PreambleTimeout time.Duration
	CallTimeout     time.Duration

	mu     sync.Mutex
	gen    int
	cur    *conn
	status Status
	logs   *logRing
}

// New returns a Bridge. Emit may be nil.
func New(resolve func() (*Plan, error), emit func(string, any), guiVersion, goos string) *Bridge {
	b := &Bridge{Resolve: resolve, Emit: emit, GUIVersion: guiVersion, OS: goos, logs: newLogRing(2000)}
	b.status = Status{State: StateStopped, OS: goos}
	return b
}

func (b *Bridge) preambleTimeout() time.Duration {
	if b.PreambleTimeout > 0 {
		return b.PreambleTimeout
	}
	return 60 * time.Second
}

func (b *Bridge) callTimeout() time.Duration {
	if b.CallTimeout > 0 {
		return b.CallTimeout
	}
	return 60 * time.Second
}

// Logs returns what the shell kept for Help → Logs: output discarded before
// the preamble, the backend's stderr, and its backend.log notifications.
func (b *Bridge) Logs() []string { return b.logs.snapshot() }

// Status returns the current state.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *Bridge) setStatus(gen int, s Status) bool {
	b.mu.Lock()
	if gen != b.gen {
		b.mu.Unlock()
		return false
	}
	s.OS = b.OS
	if s.Target == "" {
		s.Target = b.status.Target
		s.Distro = b.status.Distro
	}
	b.status = s
	b.mu.Unlock()
	if b.Emit != nil {
		b.Emit(EventState, s)
	}
	return true
}

// Start launches the backend. It returns at once; progress is reported
// through the state event. A running backend is stopped first, and what was
// pending on it fails with ErrStopped: a restart never replays anything.
func (b *Bridge) Start() {
	b.mu.Lock()
	b.gen++
	gen := b.gen
	old := b.cur
	b.cur = nil
	b.mu.Unlock()
	if old != nil {
		old.shutdown()
	}
	b.setStatus(gen, Status{State: StateStarting})
	go b.run(gen)
}

// Stop ends the backend for good (app exit).
func (b *Bridge) Stop() {
	b.mu.Lock()
	b.gen++
	old := b.cur
	b.cur = nil
	b.mu.Unlock()
	if old != nil {
		old.shutdown()
	}
}

// detach makes c no longer the current connection, so its read loop's end
// is not reported over a status the shell has just set on purpose.
func (b *Bridge) detach(c *conn) {
	b.mu.Lock()
	if b.cur == c {
		b.cur = nil
	}
	b.mu.Unlock()
}

func (b *Bridge) fail(gen int, code, reason string, st State) {
	b.setStatus(gen, Status{State: st, Code: code, Reason: reason})
}

func (b *Bridge) run(gen int) {
	plan, err := b.Resolve()
	if err != nil {
		var se *SetupError
		if errors.As(err, &se) {
			st := StateNeedsSetup
			b.mu.Lock()
			b.status.Target, b.status.Distro = "", ""
			b.mu.Unlock()
			b.fail(gen, se.Code, se.Msg, st)
			return
		}
		b.fail(gen, CodeSpawn, err.Error(), StateStopped)
		return
	}
	b.mu.Lock()
	if gen == b.gen {
		b.status.Target, b.status.Distro = plan.Target, plan.Distro
	}
	b.mu.Unlock()
	spawn := b.Spawn
	if spawn == nil {
		spawn = func(p *Plan) (*Child, error) { return StartCmd(p.Cmd) }
	}
	child, err := spawn(plan)
	if err != nil {
		b.fail(gen, CodeSpawn, "Could not start the client: "+err.Error(), StateStopped)
		return
	}
	c := newConn(child, b)
	go c.readStderr() // from the start: a noisy stderr must not fill its pipe
	b.mu.Lock()
	if gen != b.gen {
		b.mu.Unlock()
		c.shutdown()
		return
	}
	b.cur = c
	b.mu.Unlock()

	nonce, err := NewNonce()
	if err == nil {
		err = c.writeLine(nonce)
	}
	if err != nil {
		c.shutdown()
		b.fail(gen, CodeSpawn, "Could not write to the client: "+err.Error(), StateStopped)
		return
	}
	r := newReader(child.Stdout)
	synced := make(chan error, 1)
	go func() { synced <- SyncPreamble(r, nonce, func(l string) { b.logs.add("discarded: " + l) }) }()
	select {
	case err = <-synced:
	case <-time.After(b.preambleTimeout()):
		err = fmt.Errorf("%w (no preamble within %s)", ErrNoPreamble, b.preambleTimeout())
		c.shutdown()
		<-synced
	}
	if err != nil {
		// Let stderr reach EOF first, so the verdict does not race the
		// preamble's own EOF: the usage line may still be in the pipe.
		select {
		case <-c.stderrDone:
		case <-time.After(3 * time.Second):
		}
		b.detach(c)
		c.shutdown()
		if c.oldClient.Load() {
			b.fail(gen, CodeUpdateNeeded, "The installed ClawBridge client has no GUI backend; this app needs a newer client.", StateUpdateNeeded)
			return
		}
		b.fail(gen, CodeNoPreamble, noPreambleReason(err), StateStopped)
		return
	}
	go c.readLoop(r, func(reason string, code string) {
		// The backend ended or broke the stream while running.
		b.mu.Lock()
		current := b.cur == c
		b.mu.Unlock()
		if current {
			b.fail(gen, code, reason, StateStopped)
		}
	})

	// The first call is hello {gui_version, protocol} (GUI-DESIGN §4.2).
	params, _ := json.Marshal(map[string]any{"gui_version": b.GUIVersion, "protocol": Protocol})
	raw, err := c.call("hello", params, b.callTimeout())
	if err != nil {
		if errors.Is(err, ErrStopped) {
			return // the read loop already reported why the stream ended
		}
		b.detach(c)
		c.shutdown()
		var re *RPCError
		if errors.As(err, &re) && re.Code == RPCUpdateNeeded {
			b.fail(gen, CodeUpdateNeeded, "The client did not accept this app: "+re.Message, StateUpdateNeeded)
			return
		}
		reason := "The client did not answer hello: " + err.Error()
		if re != nil {
			reason = "The client refused hello: " + re.Error()
		}
		b.fail(gen, CodeNoHello, reason, StateStopped)
		return
	}
	var hello struct {
		Protocol      int    `json:"protocol"`
		ClientVersion string `json:"client_version"`
	}
	_ = json.Unmarshal(raw, &hello)
	if hello.Protocol != Protocol {
		b.detach(c)
		c.shutdown()
		b.setStatus(gen, Status{State: StateUpdateNeeded, Code: CodeUpdateNeeded, ClientVersion: hello.ClientVersion,
			Reason: fmt.Sprintf("The client speaks protocol %d; this app needs %d.", hello.Protocol, Protocol)})
		return
	}
	c.setHello(raw)
	b.setStatus(gen, Status{State: StateRunning, ClientVersion: hello.ClientVersion, Hello: raw})
}

func noPreambleReason(err error) string {
	return "The client did not start as a ClawBridge GUI backend: " + err.Error() +
		". Check Help → Logs and that the installed clawbridge is up to date."
}

// Call is the only way the page reaches the backend. It enforces the
// allow-list and the no-nonce rule, then forwards the call.
func (b *Bridge) Call(method string, params json.RawMessage) (json.RawMessage, error) {
	send, err := CheckCall(method, params)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	c := b.cur
	running := b.status.State == StateRunning
	b.mu.Unlock()
	if c == nil || !running {
		return nil, ErrStopped
	}
	if method == "hello" {
		// The handshake already happened; answer it from the cache so the
		// page sees exactly what the shell negotiated.
		if h := c.helloReply(); h != nil {
			return h, nil
		}
	}
	return c.call(method, send, b.callTimeout())
}

// ---------------------------------------------------------------- conn

type reply struct {
	result json.RawMessage
	err    error
}

type conn struct {
	child *Child
	b     *Bridge

	wmu sync.Mutex // serialises writes

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	closed  bool
	hello   json.RawMessage
	once    sync.Once

	// stderrDone closes when stderr reached EOF; oldClient is set when the
	// client said it has no gui-backend command (a v1.0.0 client).
	stderrDone chan struct{}
	oldClient  atomic.Bool
}

// oldClientLine is what v1.0.0's cli.go prints for a subcommand it lacks.
// The release is immutable, so the text is stable. The line may carry a
// "clawbridge: " prefix; only the phrase is matched.
const oldClientLine = `unknown command "gui-backend"; use --help`

func newConn(child *Child, b *Bridge) *conn {
	return &conn{child: child, b: b, pending: map[int64]chan reply{}, stderrDone: make(chan struct{})}
}

func (c *conn) setHello(h json.RawMessage) {
	c.mu.Lock()
	c.hello = h
	c.mu.Unlock()
}

func (c *conn) helloReply() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

func (c *conn) writeLine(s string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := io.WriteString(c.child.Stdin, s+"\n")
	return err
}

// shutdown closes stdin, kills the process and fails every pending call.
func (c *conn) shutdown() {
	c.once.Do(func() {
		_ = c.child.Stdin.Close()
		c.child.Kill()
		c.failAll(ErrStopped)
		go func() { _ = c.child.Wait() }()
	})
}

func (c *conn) failAll(err error) {
	c.mu.Lock()
	c.closed = true
	p := c.pending
	c.pending = map[int64]chan reply{}
	c.mu.Unlock()
	for _, ch := range p {
		ch <- reply{err: err}
	}
}

func (c *conn) call(method string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrStopped
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", id, method, params})
	if err == nil {
		c.wmu.Lock()
		err = WriteFrame(c.child.Stdin, body)
		c.wmu.Unlock()
	}
	if err != nil {
		c.drop(id)
		return nil, ErrStopped
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-time.After(timeout):
		c.drop(id)
		return nil, fmt.Errorf("backend did not answer %s within %s", method, timeout)
	}
}

func (c *conn) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readStderr keeps the backend's stderr for Help → Logs and never stops
// draining it: a line over the cap is cut, the rest of it skipped, so a
// chatty client cannot fill the pipe and block.
func (c *conn) readStderr() {
	defer close(c.stderrDone)
	r := bufio.NewReaderSize(c.child.Stderr, 64<<10)
	for {
		line, err := readLineCapped(r, maxLogLine)
		if line != "" {
			c.b.logs.add("stderr: " + line)
			if strings.Contains(line, oldClientLine) {
				c.oldClient.Store(true)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_, _ = io.Copy(io.Discard, r)
			}
			return
		}
	}
}

// readLineCapped reads one line, keeping at most max bytes (cut on a rune
// boundary) and discarding the rest of it.
func readLineCapped(r *bufio.Reader, max int) (string, error) {
	var kept []byte
	cut := false
	for {
		chunk, err := r.ReadSlice('\n')
		room := max - len(kept)
		if len(chunk) > room {
			cut = true
		}
		if room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			kept = append(kept, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		line := strings.TrimRight(string(kept), "\r\n")
		if cut {
			line = trimPartialRune(line)
		}
		return line, err
	}
}

// trimPartialRune drops an incomplete rune a byte cut left at the end.
func trimPartialRune(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// truncateUTF8 cuts s to at most max bytes without splitting a rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// readLoop dispatches frames until the stream ends or breaks, then reports
// why through onEnd (once) and fails everything pending.
func (c *conn) readLoop(r *bufio.Reader, onEnd func(reason, code string)) {
	reason, code := "The ClawBridge client stopped.", CodeExited
	for {
		body, err := ReadFrame(r)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				reason = "The ClawBridge client stopped."
			case errors.Is(err, io.ErrUnexpectedEOF):
				reason, code = "The ClawBridge client stopped in the middle of an answer.", CodeFrame
			case errors.Is(err, ErrFrameTooLarge):
				reason, code = "The ClawBridge client sent an answer over the 8 MiB limit, so it was stopped.", CodeFrame
			default:
				reason, code = "The ClawBridge client sent a malformed answer: "+err.Error(), CodeFrame
			}
			break
		}
		c.dispatch(body)
	}
	c.shutdown()
	onEnd(reason, code)
}

type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func (c *conn) dispatch(body []byte) {
	var e envelope
	if json.Unmarshal(body, &e) != nil {
		return
	}
	hasID := len(e.ID) > 0 && string(e.ID) != "null"
	if e.Method != "" {
		if !hasID {
			c.notify(e.Method, e.Params)
		}
		// A request from the backend to the shell is not part of the
		// protocol; it is ignored.
		return
	}
	if !hasID {
		return
	}
	var id int64
	if json.Unmarshal(e.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	if e.Error != nil {
		ch <- reply{err: &RPCError{Code: e.Error.Code, Message: e.Error.Message, Data: e.Error.Data}}
		return
	}
	res := e.Result
	if res == nil {
		res = json.RawMessage("null")
	}
	ch <- reply{result: res}
}

func (c *conn) notify(method string, params json.RawMessage) {
	if !methodName.MatchString(method) {
		return
	}
	if method == "backend.log" {
		c.b.logs.add("log: " + strings.TrimSpace(string(params)))
	}
	if c.b.Emit != nil {
		var v any
		if len(params) > 0 {
			_ = json.Unmarshal(params, &v)
		}
		c.b.Emit(NotifyPrefix+method, v)
	}
}

// ---------------------------------------------------------------- logs

type logRing struct {
	mu   sync.Mutex
	max  int
	data []string
}

func newLogRing(max int) *logRing { return &logRing{max: max} }

func (l *logRing) add(s string) {
	if len(s) > maxLogLine {
		s = truncateUTF8(s, maxLogLine) + "…"
	}
	l.mu.Lock()
	l.data = append(l.data, s)
	if len(l.data) > l.max {
		l.data = l.data[len(l.data)-l.max:]
	}
	l.mu.Unlock()
}

func (l *logRing) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.data...)
}
