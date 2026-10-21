//go:build !windows

package hostobserve

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Bound the HTTP upgrade response as well as subsequent WebSocket frames.
// The library still owns all HTTP/WebSocket parsing and framing.
type handshakeConn struct {
	net.Conn
	remaining int64
	ready     bool
}

func (c *handshakeConn) Read(p []byte) (int, error) {
	if c.ready {
		return c.Conn.Read(p)
	}
	if c.remaining <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.Conn.Read(p)
	c.remaining -= int64(n)
	return n, err
}

type wsTransport struct {
	conn *websocket.Conn
	raw  net.Conn
	once sync.Once
}

func (t *wsTransport) Close() error {
	var err error
	t.once.Do(func() { err = t.raw.Close() })
	return err
}
func (t *wsTransport) deadline(ctx context.Context, read bool) func() {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if read {
		_ = t.conn.SetReadDeadline(deadline)
	} else {
		_ = t.conn.SetWriteDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { t.Close() })
	return func() { stop() }
}
func (t *wsTransport) Send(ctx context.Context, data []byte) error {
	stop := t.deadline(ctx, false)
	defer stop()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return websocket.Message.Send(t.conn, string(data))
}
func (t *wsTransport) Receive(ctx context.Context) ([]byte, error) {
	stop := t.deadline(ctx, true)
	defer stop()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var data []byte
	err := websocket.Message.Receive(t.conn, &data)
	if errors.Is(err, websocket.ErrFrameTooLarge) {
		return nil, Unsupported()
	}
	return data, err
}
func Open(ctx context.Context, b Binding) (Observer, error) {
	b, err := NormalizeBinding(b)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(b.Endpoint)
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
	if err != nil {
		return nil, Unavailable()
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err = raw.SetDeadline(deadline); err != nil {
		raw.Close()
		return nil, Unavailable()
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	config, err := websocket.NewConfig("ws://localhost/", "http://localhost/")
	if err != nil {
		stop()
		raw.Close()
		return nil, Unavailable()
	}
	limited := &handshakeConn{Conn: raw, remaining: MaxMessageBytes}
	ws, err := websocket.NewClient(config, limited)
	stop()
	if err != nil {
		raw.Close()
		return nil, Unavailable()
	}
	limited.ready = true
	ws.MaxPayloadBytes = MaxMessageBytes
	return NewObserver(ctx, b, &wsTransport{conn: ws, raw: raw})
}
