//go:build !windows

package integration

import (
	"bytes"
	"context"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/hostobserve"
	"golang.org/x/net/websocket"
)

// Inspect only this fixture process. Require a sustained real socket write wait,
// not just a pending read or an assumption that enough ping frames were sent.
func coordinationBlockedPong() string {
	var stacks bytes.Buffer
	if pprof.Lookup("goroutine").WriteTo(&stacks, 2) != nil {
		return ""
	}
	for _, stack := range strings.Split(stacks.String(), "\n\n") {
		if strings.Contains(stack, "[IO wait]") && strings.Contains(stack, "(*hybiFrameHandler).WritePong") && strings.Contains(stack, "(*wsTransport).Receive") && strings.Contains(stack, ".Write(") {
			return strings.SplitN(stack, "\n", 2)[0]
		}
	}
	return ""
}

func TestCoordinationHostCancellationClosesBackpressuredPong(t *testing.T) {
	entered := make(chan struct{})
	peerDone := make(chan struct{})
	binding, _ := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
		defer close(peerDone)
		close(entered)
		// After reading the metadata request, deliberately never read again. Pings
		// force the client's library to write pongs until its socket fills. Use
		// the library codec rather than constructing WebSocket frames by hand.
		_ = ws.SetWriteDeadline(time.Now().Add(4 * time.Second))
		ping := websocket.Codec{Marshal: func(any) ([]byte, byte, error) { return bytes.Repeat([]byte("p"), 125), websocket.PingFrame, nil }}
		for i := 0; i < 65536; i++ {
			if ping.Send(ws, nil) != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan hostobserve.Result, 1)
	go func() {
		r, _ := hostobserve.Run(ctx, hostobserve.Config{Binding: binding, Timeout: 10 * time.Second, Interval: time.Second})
		done <- r
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owned peer never reached metadata RPC")
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	previous := ""
	blocked := false
	for !blocked {
		select {
		case <-tick.C:
			current := coordinationBlockedPong()
			blocked = current != "" && current == previous
			previous = current
		case <-deadline.C:
			cancel()
			t.Fatal("fixture did not establish sustained pong/socket write backpressure")
		case result := <-done:
			t.Fatalf("observer ended before establishing backpressure: %s", result.Outcome)
		}
	}
	start := time.Now()
	cancel()
	select {
	case result := <-done:
		if result.Outcome != "cancelled" {
			t.Fatalf("cancel outcome=%s", result.Outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited on WebSocket pong/write lock instead of closing raw connection")
	}
	select {
	case <-peerDone:
	case <-time.After(time.Second):
		t.Fatal("owned peer retained a blocked write after observer cancellation")
	}
	t.Logf("sustained pong write backpressure established; cancellation released observer+peer in %s", time.Since(start))
}
