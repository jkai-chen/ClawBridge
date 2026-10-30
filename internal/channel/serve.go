package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"sync"

	"clawbridge/internal/ctxwatch"
)

// The JSON-RPC side: newline-delimited messages on stdin and stdout. The
// MCP SDK can't send a custom notification method, so this small loop
// answers the handshake itself: initialize with the channel capability,
// ping, and an empty tools/list. Anything else asked gets "method not
// found"; other notifications are ignored.

// Instructions is the one line of instructions the server gives.
const Instructions = "Channel turns from this server are a fixed notice: call the ClawBridge inbox tool."

// defaultProtocol answers a client that names no protocol version.
const defaultProtocol = "2025-06-18"

var protocolToken = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,64}\z`)

// Serve runs the channel server until in closes or ctx ends. Without a session in the
// environment it only answers the handshake: no record is read or
// written and nothing is pushed.
func Serve(ctx context.Context, in io.Reader, out io.Writer, c Config) error {
	c = c.withDefaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	send := func(line []byte) error {
		mu.Lock()
		defer mu.Unlock()
		_, err := out.Write(append(line, '\n'))
		return err
	}
	var w *watcher
	done := make(chan struct{})
	session := envValue(c.Getenv, "CLAWBRIDGE_SESSION")
	if _, err := ctxwatch.BindingPath(c.Dir, session); err == nil && c.Dir != "" {
		w = newWatcher(c, session, preconditions(c), send)
		go func() {
			defer close(done)
			w.run(ctx)
		}()
	} else {
		close(done)
	}
	// Lines are read on their own goroutine, so a cancel (SIGTERM) ends
	// the server without waiting for stdin to close.
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			select {
			case lines <- append([]byte(nil), scanner.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
	}()
read:
	for {
		select {
		case <-ctx.Done():
			break read
		case line, ok := <-lines:
			if !ok {
				break read
			}
			reply, initialized := handle(line, c.Version)
			if initialized && w != nil {
				w.ready.Store(true)
				w.poke()
			}
			if reply != nil && send(reply) != nil {
				break read
			}
		}
	}
	cancel()
	<-done
	return nil
}

// handle answers one line: its reply, if any, and whether it was the
// client's initialized notification.
func handle(line []byte, version string) ([]byte, bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil, false
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return rpcError(nil, -32700, "parse error"), false
	}
	if msg.Method == "" {
		// A response, or nothing: this server asks nothing.
		return nil, false
	}
	if len(msg.ID) == 0 {
		return nil, msg.Method == "notifications/initialized"
	}
	if id := bytes.TrimSpace(msg.ID); len(id) == 0 || (id[0] != '"' && id[0] != '-' && (id[0] < '0' || id[0] > '9')) {
		return rpcError(nil, -32600, "invalid request"), false
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if !protocolToken.MatchString(p.ProtocolVersion) {
			p.ProtocolVersion = defaultProtocol
		}
		return rpcResult(msg.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"experimental": map[string]any{"claude/channel": map[string]any{}}},
			"serverInfo":      map[string]any{"name": "clawbridge-wake", "version": version},
			"instructions":    Instructions,
		}), false
	case "ping":
		return rpcResult(msg.ID, map[string]any{}), false
	case "tools/list":
		return rpcResult(msg.ID, map[string]any{"tools": []any{}}), false
	}
	return rpcError(msg.ID, -32601, "method not found"), false
}

func rpcResult(id json.RawMessage, result any) []byte {
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return line
}

func rpcError(id json.RawMessage, code int, message string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	return line
}
