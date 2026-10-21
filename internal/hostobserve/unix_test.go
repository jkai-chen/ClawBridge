//go:build !windows

package hostobserve

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestCoordinationActualUnixWebSocketMetadataAndCancellation(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cb-host-ws-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		for {
			var raw []byte
			if websocket.Message.Receive(ws, &raw) != nil {
				return
			}
			var req struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			switch req.Method {
			case "initialize":
				_ = websocket.Message.Send(ws, `{"id":1,"result":{}}`)
			case "initialized":
			case "thread/read":
				if req.Params["includeTurns"] != false {
					t.Error("history requested")
				}
				_ = websocket.Message.Send(ws, string(response(req.ID, "idle")))
			default:
				t.Error("unexpected method", req.Method)
				return
			}
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	c := Config{Binding: Binding{"codex", "unix://" + socket, testThread}, For: "idle", Timeout: time.Second, Interval: time.Second}
	r, err := Run(context.Background(), c)
	if err != nil || r.Outcome != "matched" || r.LastObservation.State != "idle" {
		t.Fatal(r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err = Run(ctx, c)
	if err != nil || r.Outcome != "cancelled" {
		t.Fatal(r, err)
	}
}

func TestCoordinationUnixCancellationClosesBlockedRead(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cb-host-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "host.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	server := &http.Server{Handler: websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		for {
			var raw []byte
			if websocket.Message.Receive(ws, &raw) != nil {
				return
			}
			var request struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(raw, &request) != nil {
				return
			}
			if request.Method == "initialize" {
				_ = websocket.Message.Send(ws, `{"id":1,"result":{}}`)
			}
			if request.Method == "thread/read" {
				close(waiting)
				var ignored []byte
				_ = websocket.Message.Receive(ws, &ignored)
				return
			}
		}
	})}
	go server.Serve(ln)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() {
		r, _ := Run(ctx, Config{Binding: Binding{"codex", "unix://" + socket, testThread}, For: "idle", Timeout: time.Second, Interval: time.Second})
		done <- r
	}()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("fake host not reached")
	}
	cancel()
	select {
	case r := <-done:
		if r.Outcome != "cancelled" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close blocked socket")
	}
}
