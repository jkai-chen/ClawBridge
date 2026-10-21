package hostobserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
)

const MaxMessageBytes = 1 << 20
const MaxUnrelatedMessages = 128

// Transport owns a single already-connected host channel. Each method must honor
// its context; Close unblocks pending IO. Tests can inject bounded protocol peers.
type Transport interface {
	Send(context.Context, []byte) error
	Receive(context.Context) ([]byte, error)
	Close() error
}
type client struct {
	mu        sync.Mutex
	transport Transport
	binding   Binding
	next      int64
}

// NewObserver initializes only the supplied existing connection. It owns the
// transport, closing it on initialization failure or caller Close.
func NewObserver(ctx context.Context, b Binding, t Transport) (Observer, error) {
	var err error
	b, err = NormalizeBinding(b)
	if err != nil {
		t.Close()
		return nil, err
	}
	c := &client{transport: t, binding: b}
	_, err = c.rpc(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "clawbridge-host-observer", "version": "1"}})
	if err == nil {
		raw, _ := json.Marshal(map[string]any{"method": "initialized"})
		if t.Send(ctx, raw) != nil {
			err = Unavailable()
		}
	}
	if err != nil {
		t.Close()
		return nil, err
	}
	return c, nil
}
func (c *client) Close() error { return c.transport.Close() }
func (c *client) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, Unavailable()
	}
	c.next++
	raw, _ := json.Marshal(map[string]any{"id": c.next, "method": method, "params": params})
	if err := c.transport.Send(ctx, raw); err != nil {
		return nil, Unavailable()
	}
	for skipped := 0; skipped <= MaxUnrelatedMessages; skipped++ {
		raw, err := c.transport.Receive(ctx)
		if err != nil {
			var classified *observationError
			if errors.As(err, &classified) {
				return nil, err
			}
			return nil, Unavailable()
		}
		if ctx.Err() != nil {
			return nil, Unavailable()
		}
		if len(raw) > MaxMessageBytes {
			return nil, Unsupported()
		}
		var reply map[string]json.RawMessage
		if json.Unmarshal(raw, &reply) != nil || reply == nil {
			return nil, Unsupported()
		}
		if _, ok := reply["method"]; ok {
			if skipped == MaxUnrelatedMessages {
				return nil, Unsupported()
			}
			continue
		} // never answer host requests.
		var id int64
		if json.Unmarshal(reply["id"], &id) != nil || id != c.next {
			return nil, Unsupported()
		}
		if value, ok := reply["error"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, Unavailable()
		}
		value, ok := reply["result"]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, Unsupported()
		}
		return value, nil
	}
	return nil, Unsupported()
}
func (c *client) Observe(ctx context.Context) (Observation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := c.rpc(ctx, "thread/read", map[string]any{"threadId": c.binding.Thread, "includeTurns": false})
	if err != nil {
		return Observation{}, err
	}
	var response struct {
		Thread struct {
			ID     string          `json:"id"`
			Status json.RawMessage `json:"status"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != c.binding.Thread {
		return Observation{}, Unsupported()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(response.Thread.Status, &fields) != nil || fields == nil {
		return Observation{}, Unsupported()
	}
	var state string
	if json.Unmarshal(fields["type"], &state) != nil {
		return Observation{}, Unsupported()
	}
	flags := []string{}
	if state == "active" {
		if len(fields) != 2 || bytes.Equal(bytes.TrimSpace(fields["activeFlags"]), []byte("null")) || json.Unmarshal(fields["activeFlags"], &flags) != nil || flags == nil {
			return Observation{}, Unsupported()
		}
	} else if len(fields) != 1 {
		return Observation{}, Unsupported()
	}
	o := Observation{Adapter: "codex", ThreadID: c.binding.Thread, State: state, ActiveFlags: flags, QueueState: "unknown"}
	if err = validateObservation(o, c.binding); err != nil {
		return Observation{}, err
	}
	return o, nil
}
