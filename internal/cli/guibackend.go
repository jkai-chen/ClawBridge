package cli

// SPIKE (live/GUI-P1-SPIKE.md): throwaway, never merged. `clawbridge
// gui-backend --stdio` serves the GUI over its stdin/stdout.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/bus"
)

const guiProtocol = 1

var guiNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)

// guiPTYCommand builds the agent command a pty.open runs; tests replace it.
var guiPTYCommand = func(kind, dir string) (*exec.Cmd, error) {
	if kind != "claude" && kind != "codex" {
		return nil, errors.New("kind must be claude or codex")
	}
	shell := loginShell()
	cmd := exec.Command(shell, "-lc", "exec "+kind)
	cmd.Dir = dir
	return cmd, nil
}

func loginShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func guiPlatform() string {
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

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcConn frames JSON-RPC messages with Content-Length headers.
type rpcConn struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  io.Writer
}

func (c *rpcConn) read() (rpcMessage, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return rpcMessage{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			if length, err = strconv.Atoi(v); err != nil || length < 0 || length > 8<<20 {
				return rpcMessage{}, errors.New("bad Content-Length")
			}
		}
	}
	if length < 0 {
		return rpcMessage{}, errors.New("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return rpcMessage{}, err
	}
	var m rpcMessage
	return m, json.Unmarshal(body, &m)
}

func (c *rpcConn) write(m rpcMessage) error {
	m.JSONRPC = "2.0"
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func runGUIBackend(ctx context.Context, flags Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 1 || args[0] != "--stdio" {
		return errors.New("usage: clawbridge gui-backend --stdio")
	}
	if marker := agentMarker(os.Environ()); marker != "" {
		return errors.New("gui-backend is a person's tool; it does not run in an agent's environment (" + marker + " is set)")
	}
	r := bufio.NewReader(stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return errors.New("gui-backend: no nonce line on stdin")
	}
	nonce := strings.TrimRight(line, "\r\n")
	if !guiNonce.MatchString(nonce) {
		return errors.New("gui-backend: the first stdin line must be 32 lowercase hex characters")
	}
	if _, err = fmt.Fprintf(stdout, "CLAWBRIDGE-GUI-BACKEND/%d %s\n", guiProtocol, nonce); err != nil {
		return err
	}
	conn := &rpcConn{r: r, w: stdout}
	in, openErr := openUIInputs(ctx, flags)
	if in != nil {
		defer in.Close()
	}
	ptys := newGUIPTYs(conn)
	defer ptys.closeAll()
	for {
		m, err := conn.read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		result, callErr := guiCall(ctx, in, openErr, ptys, m.Method, m.Params)
		if m.ID == nil {
			continue
		}
		reply := rpcMessage{ID: m.ID, Result: result}
		if callErr != nil {
			reply = rpcMessage{ID: m.ID, Error: &rpcError{Code: -32000, Message: callErr.Error()}}
		}
		if err = conn.write(reply); err != nil {
			return err
		}
	}
}

func guiCall(ctx context.Context, in *uiInputs, openErr error, ptys *guiPTYs, method string, params json.RawMessage) (any, error) {
	switch method {
	case "hello":
		out := map[string]any{"protocol": guiProtocol, "client_version": buildinfo.Current().Version, "platform": guiPlatform()}
		if in != nil {
			out["mode"] = in.Config.Mode
			out["waiting_for_project"] = in.Waiting
		} else {
			out["profile_error"] = openErr.Error()
		}
		return out, nil
	case "pty.open", "pty.write", "pty.resize", "pty.close":
		return ptys.call(method, params)
	}
	if in == nil {
		return nil, openErr
	}
	switch method {
	case "sessions.list":
		c := in.Config
		r, err := in.Executor.Execute(in.Context, bus.Request{Operation: "list_sessions", Workspace: c.Workspace, Project: c.Project, Args: map[string]any{"limit": 100}})
		return r.Data, err
	case "doctor.checks":
		report, err := in.Doctor(in.Context)
		return report, err
	}
	return nil, errors.New("unknown method " + strconv.Quote(method))
}

type guiPTYs struct {
	conn *rpcConn
	mu   sync.Mutex
	next int
	open map[int]*guiPTY
}

func newGUIPTYs(conn *rpcConn) *guiPTYs {
	return &guiPTYs{conn: conn, open: map[int]*guiPTY{}}
}

func (p *guiPTYs) call(method string, params json.RawMessage) (any, error) {
	var in struct {
		ID   int    `json:"id"`
		Kind string `json:"kind"`
		Dir  string `json:"dir"`
		Data string `json:"data"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
	}
	if method == "pty.open" {
		cmd, err := guiPTYCommand(in.Kind, in.Dir)
		if err != nil {
			return nil, err
		}
		t, err := startGUIPTY(cmd, in.Cols, in.Rows)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.next++
		id := p.next
		p.open[id] = t
		p.mu.Unlock()
		go p.pump(id, t)
		return map[string]any{"id": id}, nil
	}
	p.mu.Lock()
	t := p.open[in.ID]
	p.mu.Unlock()
	if t == nil {
		return nil, errors.New("no such terminal")
	}
	switch method {
	case "pty.write":
		b, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			return nil, err
		}
		_, err = t.Write(b)
		return map[string]any{}, err
	case "pty.resize":
		return map[string]any{}, t.Resize(in.Cols, in.Rows)
	default:
		p.mu.Lock()
		delete(p.open, in.ID)
		p.mu.Unlock()
		return map[string]any{}, t.Close()
	}
}

func (p *guiPTYs) pump(id int, t *guiPTY) {
	buf := make([]byte, 32<<10)
	for {
		n, err := t.Read(buf)
		if n > 0 {
			params, _ := json.Marshal(map[string]any{"id": id, "data": base64.StdEncoding.EncodeToString(buf[:n])})
			_ = p.conn.write(rpcMessage{Method: "pty.data", Params: params})
		}
		if err != nil {
			params, _ := json.Marshal(map[string]any{"id": id})
			_ = p.conn.write(rpcMessage{Method: "pty.exit", Params: params})
			return
		}
	}
}

func (p *guiPTYs) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, t := range p.open {
		_ = t.Close()
		delete(p.open, id)
	}
}
