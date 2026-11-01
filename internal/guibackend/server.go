// Package guibackend is the local web UI's backend: a JSON-RPC 2.0 method
// registry served over HTTP, and a server-sent-events hub for pushes
// (live/GUI-DESIGN.md §12). It knows nothing of the CLI or the TUI; the
// command that serves it registers the methods.
package guibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// ProtocolVersion is what hello reports.
	ProtocolVersion = 1
	// MaxInFlight bounds the method calls running at once, so a slow call
	// does not block a fast one but a flood cannot start unbounded work.
	MaxInFlight = 4
	// MaxBatch caps the requests in one batch.
	MaxBatch = 32
	// MaxBody caps one POST body.
	MaxBody = 1 << 20
	// clientBuffer is how many events one SSE client may have waiting.
	clientBuffer = 16
	// maxLogName caps a method name in the log, in bytes.
	maxLogName = 64
	// DefaultHeartbeat is the SSE comment-line interval.
	DefaultHeartbeat = 15 * time.Second
)

// JSON-RPC error codes.
const (
	CodeParse    = -32700
	CodeInvalid  = -32600
	CodeNotFound = -32601
	CodeParams   = -32602
	CodeInternal = -32603
	CodeMethod   = -32000
)

// Class says what a method may do. A write or confirm method is never
// registered in P2; see GUI-DESIGN §4.2 for the confirmation rules.
type Class int

const (
	Read Class = iota
	Write
	Confirm
)

// Method is one allow-listed call.
type Method struct {
	Class Class
	Call  func(ctx context.Context, params json.RawMessage) (any, error)
}

// Error is an error a handler returns with its own JSON-RPC code. Any
// other error is reported as CodeMethod.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Hello is what the built-in hello method reports about this client.
type Hello struct {
	ClientVersion string
	// Mode is local or cloud; empty when no profile opened.
	Mode string
	// Platform is darwin, linux, windows or wsl; see Platform.
	Platform string
	// Profile is a small summary of the open profile, or an error.
	Profile any
	// Bundle names the UI bundle being served: "placeholder" or the
	// bundle's sha256 (webui.BundleID). Left out of hello when empty.
	Bundle string
}

// Server holds the allow-list and the event hub. Methods is the
// allow-list; hello is built in and cannot be replaced. Set its fields
// before Handler or Events is used.
type Server struct {
	Methods map[string]Method
	Hello   Hello
	// AllowWrites lets a registered Write or Confirm method be called. Off,
	// such a method is unreachable and answers as an unknown name does.
	AllowWrites bool
	// Log gets one line per call: the method name, the outcome code and
	// the duration. Params and results are never logged.
	Log io.Writer
	// Heartbeat is the SSE comment interval; zero means DefaultHeartbeat.
	Heartbeat time.Duration

	once sync.Once
	sem  chan struct{}

	mu             sync.Mutex
	clients        map[*client]struct{}
	lastDisconnect time.Time
}

type client struct{ events chan []byte }

func (s *Server) init() {
	s.once.Do(func() {
		s.sem = make(chan struct{}, MaxInFlight)
		s.clients = map[*client]struct{}{}
	})
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// MarshalJSON keeps a null result on a success; omitempty would drop it.
func (r response) MarshalJSON() ([]byte, error) {
	id := r.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if r.Error != nil {
		return json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *rpcError       `json:"error"`
		}{"2.0", id, r.Error})
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, r.Result})
}

func fail(id json.RawMessage, code int, msg string) *response {
	return &response{ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// Handler serves JSON-RPC 2.0 over POST: one request object or a batch
// array, up to MaxBody bytes and MaxBatch requests, one response per request that has an id.
//
// It does no authentication and checks no Host, Origin or CSRF token. It
// must never be mounted without the internal/webui middleware that does.
func (s *Server) Handler() http.Handler {
	s.init()
	return http.HandlerFunc(s.serveRPC)
}

func (s *Server) serveRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body over the 1 MiB cap", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(body, &items) != nil {
			write(w, fail(nil, CodeParse, "parse error"))
			return
		}
		if len(items) == 0 {
			write(w, fail(nil, CodeInvalid, "invalid request"))
			return
		}
		if len(items) > MaxBatch {
			write(w, fail(nil, CodeInvalid, "batch too large"))
			return
		}
		out := make([]*response, len(items))
		var wg sync.WaitGroup
		for i, item := range items {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = s.one(r.Context(), item)
			}()
		}
		wg.Wait()
		var answered []*response
		for _, o := range out {
			if o != nil {
				answered = append(answered, o)
			}
		}
		if len(answered) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		write(w, answered)
		return
	}
	if out := s.one(r.Context(), body); out != nil {
		write(w, out)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func write(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body, _ = json.Marshal(fail(nil, CodeInternal, "internal error"))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// one answers one request; it returns nil for a notification.
func (s *Server) one(ctx context.Context, raw json.RawMessage) *response {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return fail(nil, CodeInvalid, "invalid request")
		}
		return fail(nil, CodeParse, "parse error")
	}
	notify := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		if notify {
			return nil
		}
		return fail(req.ID, CodeInvalid, "invalid request")
	}
	// A request that goes away while it waits for a slot never takes one.
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		if notify {
			return nil
		}
		return fail(req.ID, CodeInternal, "request cancelled")
	}
	start := time.Now()
	result, code, err := s.call(ctx, req)
	<-s.sem
	s.logCall(req.Method, code, time.Since(start))
	if notify {
		return nil
	}
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return fail(req.ID, e.Code, e.Message)
		}
		return fail(req.ID, code, err.Error())
	}
	// Marshal here so an unencodable result fails this call, not the batch.
	enc, merr := json.Marshal(result)
	if merr != nil {
		return fail(req.ID, CodeInternal, "internal error")
	}
	return &response{ID: req.ID, Result: json.RawMessage(enc)}
}

// call runs a method; a panic becomes a CodeInternal error and the server
// keeps serving. The code is the outcome for the log.
func (s *Server) call(ctx context.Context, req request) (result any, code int, err error) {
	defer func() {
		if p := recover(); p != nil {
			// The panic value could carry a secret; say only that it
			// happened.
			result, code, err = nil, CodeInternal, errors.New("internal error")
		}
	}()
	if req.Method == "hello" {
		h := s.Hello
		out := map[string]any{
			"client_version": h.ClientVersion,
			"protocol":       ProtocolVersion,
			"mode":           h.Mode,
			"platform":       h.Platform,
			"profile":        h.Profile,
		}
		if h.Bundle != "" {
			out["bundle"] = h.Bundle
		}
		// On WSL the page names the distro ("Ubuntu (WSL)"); elsewhere the
		// field is left out.
		if h.Platform == "wsl" {
			if d := os.Getenv("WSL_DISTRO_NAME"); d != "" {
				out["distro"] = d
			}
		}
		return out, 0, nil
	}
	m, ok := s.Methods[req.Method]
	if !ok || m.Call == nil || (m.Class != Read && !s.AllowWrites) {
		return nil, CodeNotFound, errors.New("method not found")
	}
	result, err = m.Call(ctx, req.Params)
	if err == nil {
		return result, 0, nil
	}
	var e *Error
	if errors.As(err, &e) {
		return nil, e.Code, err
	}
	return nil, CodeMethod, err
}

// Events streams server-sent events: `event: <method>` and `data: <json>`
// for every Notify, and a comment line each Heartbeat. A client that falls
// clientBuffer events behind is dropped.
//
// It does no authentication and checks no Host, Origin or CSRF token. It
// must never be mounted without the internal/webui middleware that does.
func (s *Server) Events() http.Handler {
	s.init()
	return http.HandlerFunc(s.serveEvents)
}

func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	c := &client{events: make(chan []byte, clientBuffer)}
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	defer s.drop(c)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	beat := s.Heartbeat
	if beat <= 0 {
		beat = DefaultHeartbeat
	}
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-c.events:
			if !open {
				return
			}
			if _, err := w.Write(ev); err != nil {
				return
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

// drop removes a client once and records when the last one left.
func (s *Server) drop(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[c]; ok {
		delete(s.clients, c)
		close(c.events)
		s.lastDisconnect = time.Now()
	}
}

// Notify broadcasts an event to every connected client. It never blocks: a
// client whose buffer is full is dropped. It is safe to call while calls
// are being answered.
func (s *Server) Notify(method string, params any) error {
	if method == "" || strings.ContainsAny(method, "\r\n") {
		return errors.New("guibackend: bad event name")
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ev := []byte("event: " + method + "\ndata: " + string(data) + "\n\n")
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		select {
		case c.events <- ev:
		default:
			delete(s.clients, c)
			close(c.events)
			s.lastDisconnect = time.Now()
		}
	}
	return nil
}

// Connected is the number of event streams open now.
func (s *Server) Connected() int {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// LastDisconnect is when an event stream last closed; zero if none has.
func (s *Server) LastDisconnect() time.Time {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDisconnect
}

func (s *Server) logCall(method string, code int, d time.Duration) {
	if s.Log == nil {
		return
	}
	// Method names are the allow-list's, but an unknown one is the
	// caller's text: bound it, drop what does not print, then quote it.
	fmt.Fprintf(s.Log, "ui: %s code=%d took=%s\n", strconv.Quote(logName(method)), code, d.Round(time.Millisecond))
}

func logName(method string) string {
	// Cut before scanning, so a huge name costs little.
	cut := len(method) > 4*maxLogName
	if cut {
		method = strings.ToValidUTF8(method[:4*maxLogName], "")
	}
	var b strings.Builder
	for _, r := range method {
		if !unicode.IsPrint(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxLogName {
			cut = true
			break
		}
		b.WriteRune(r)
	}
	if cut {
		b.WriteString("…")
	}
	return b.String()
}

// Platform names the OS this process runs on: darwin, linux, windows, or
// wsl for a Linux under Windows.
func Platform() string {
	if runtime.GOOS == "linux" {
		if os.Getenv("WSL_DISTRO_NAME") != "" {
			return "wsl"
		}
		if b, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft") {
			return "wsl"
		}
	}
	return runtime.GOOS
}
