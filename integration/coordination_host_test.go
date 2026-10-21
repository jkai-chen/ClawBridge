//go:build !windows

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/hostobserve"
	"golang.org/x/net/websocket"
)

const coordinationHostThread = "11111111-2222-4333-8444-555555555555"

// This is an owned temporary protocol peer, never a real host control socket.
func coordinationHost(t *testing.T, respond func(*websocket.Conn, int, int)) (hostobserve.Binding, func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cb-review-ws-")
	if err != nil {
		t.Fatal(err)
	}
	// macOS temporary roots can exceed the Unix socket path limit.
	if len(dir) > 65 {
		os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "cb-review-ws-")
		if err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	var mu sync.Mutex
	var methods []string
	connections := 0
	srv := &http.Server{Handler: websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		mu.Lock()
		connections++
		mu.Unlock()
		reads := 0
		for {
			var raw []byte
			if websocket.Message.Receive(ws, &raw) != nil {
				return
			}
			var r struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(raw, &r) != nil {
				t.Error("malformed client request")
				return
			}
			mu.Lock()
			methods = append(methods, r.Method)
			mu.Unlock()
			switch r.Method {
			case "initialize":
				_ = websocket.JSON.Send(ws, map[string]any{"id": r.ID, "result": map[string]any{}})
			case "initialized":
			case "thread/read":
				if !reflect.DeepEqual(r.Params, map[string]any{"threadId": coordinationHostThread, "includeTurns": false}) {
					t.Errorf("host read requests more than bound metadata: %v", r.Params)
					return
				}
				reads++
				respond(ws, r.ID, reads)
			default:
				t.Errorf("unexpected outbound host method %q (including approval response)", r.Method)
				return
			}
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close(); os.RemoveAll(dir) })
	return hostobserve.Binding{Adapter: "codex", Endpoint: "unix://" + socket, Thread: coordinationHostThread}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		if connections != 1 {
			t.Errorf("observer silently reconnected: %d connections", connections)
		}
		return append([]string(nil), methods...)
	}
}

func coordinationHostReply(ws *websocket.Conn, id int, state string, flags ...string) {
	status := map[string]any{"type": state}
	if state == "active" {
		if flags == nil {
			flags = []string{}
		}
		status["activeFlags"] = flags
	}
	_ = websocket.JSON.Send(ws, map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": coordinationHostThread, "status": status, "turns": []string{"PRIVATE_HOST_BODY_MUST_NOT_ESCAPE"}}}})
}

func TestCoordinationHostActualProtocolAndLevelWait(t *testing.T) {
	for _, tc := range []struct {
		name, predicate, final string
		first                  bool
	}{
		{"initial_idle", "idle", "idle", true}, {"becomes_idle", "idle", "idle", false}, {"unloaded_is_not_exit", "unloaded", "notLoaded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding, methods := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
				// Unsolicited approval request is discarded, never answered.
				_ = websocket.Message.Send(ws, `{"id":"approval","method":"item/commandExecution/requestApproval","params":{"private":"PRIVATE_HOST_BODY_MUST_NOT_ESCAPE"}}`)
				if n == 1 && !tc.first {
					coordinationHostReply(ws, id, "active", "waitingOnUserInput")
					return
				}
				coordinationHostReply(ws, id, tc.final)
			})
			waits := 0
			r, err := hostobserve.Run(context.Background(), hostobserve.Config{Binding: binding, For: tc.predicate, Timeout: time.Second, Interval: 250 * time.Millisecond, Wait: func(ctx context.Context, d time.Duration) error { waits++; return ctx.Err() }})
			if err != nil || r.Outcome != "matched" || r.LastObservation == nil || r.LastObservation.State != tc.final || r.LastObservation.QueueState != "unknown" {
				t.Fatalf("result=%+v err=%v", r, err)
			}
			if _, err = time.Parse(time.RFC3339Nano, r.LastObservation.ObservedAt); err != nil {
				t.Fatal("not UTC timestamp", err)
			}
			want := []string{"initialize", "initialized", "thread/read"}
			if !tc.first {
				want = append(want, "thread/read")
			}
			if !reflect.DeepEqual(methods(), want) || waits != len(want)-3 {
				t.Fatalf("protocol/wait sequence differs: %v waits=%d", methods(), waits)
			}
		})
	}
}

func TestCoordinationHostExactUnrelatedBoundary(t *testing.T) {
	binding, methods := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
		for i := 0; i < 128; i++ {
			if websocket.Message.Send(ws, `{"method":"unrelated","params":{"private":"PRIVATE_HOST_BODY_MUST_NOT_ESCAPE"}}`) != nil {
				return
			}
		}
		coordinationHostReply(ws, id, "idle")
	})
	r, err := hostobserve.Run(context.Background(), hostobserve.Config{Binding: binding, For: "idle", Timeout: time.Second, Interval: time.Second})
	if err != nil || r.Outcome != "matched" {
		t.Fatalf("exact allowed unrelated limit refused: %+v %v", r, err)
	}
	if !reflect.DeepEqual(methods(), []string{"initialize", "initialized", "thread/read"}) {
		t.Fatal("unrelated events induced outbound host effects")
	}
}

func TestCoordinationHostValidationBeforeDial(t *testing.T) {
	base := hostobserve.Config{Binding: hostobserve.Binding{Adapter: "codex", Endpoint: "unix:///tmp/owned-review-placeholder.sock", Thread: coordinationHostThread}, For: "idle", Timeout: time.Second, Interval: 250 * time.Millisecond, Dial: func(context.Context, hostobserve.Binding) (hostobserve.Observer, error) {
		t.Fatal("invalid options reached dial")
		return nil, nil
	}}
	for _, endpoint := range []string{"", "unix:relative", "unix://host/socket", "unix://user@/socket", "unix:///tmp/socket?", "unix:///tmp/socket#", "unix:///tmp/socket?q=1", "unix:///tmp/socket#fragment", "tcp://127.0.0.1:1", "wss://example.test/", "unix:///tmp/socket%00"} {
		t.Run("endpoint_"+endpoint, func(t *testing.T) {
			c := base
			c.Binding.Endpoint = endpoint
			if _, err := hostobserve.Run(context.Background(), c); err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
		})
	}
	for _, change := range []func(*hostobserve.Config){
		func(c *hostobserve.Config) { c.Binding.Thread = "latest" }, func(c *hostobserve.Config) { c.Binding.Thread = "AAAAAAAA-2222-4333-8444-555555555555" }, func(c *hostobserve.Config) { c.Binding.Adapter = "shell" }, func(c *hostobserve.Config) { c.For = "exit" },
		func(c *hostobserve.Config) { c.Timeout = time.Second - time.Nanosecond }, func(c *hostobserve.Config) { c.Timeout = time.Hour + time.Nanosecond }, func(c *hostobserve.Config) { c.Interval = 250*time.Millisecond - time.Nanosecond }, func(c *hostobserve.Config) { c.Interval = 30*time.Second + time.Nanosecond },
	} {
		c := base
		change(&c)
		if _, err := hostobserve.Run(context.Background(), c); err == nil {
			t.Fatal("invalid binding/predicate/duration accepted")
		}
	}
	clean, err := hostobserve.NormalizeBinding(hostobserve.Binding{Adapter: "codex", Endpoint: "unix:///tmp/parent/../owned%20socket", Thread: coordinationHostThread})
	if err != nil || clean.Endpoint != "unix:///tmp/owned%20socket" {
		t.Fatalf("canonical socket selection wrong: %+v %v", clean, err)
	}
}

func TestCoordinationHostMalformedAndDisconnectNeverMatch(t *testing.T) {
	for _, mode := range []string{"wrong-id", "wrong-thread", "unknown-state", "unknown-flag", "systemError", "disconnect", "too-many-unrelated", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			binding, methods := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
				switch mode {
				case "wrong-id":
					coordinationHostReply(ws, id+1, "idle")
				case "wrong-thread":
					_ = websocket.Message.Send(ws, fmt.Sprintf(`{"id":%d,"result":{"thread":{"id":"OTHER","status":{"type":"idle"}}}}`, id))
				case "unknown-state":
					coordinationHostReply(ws, id, "exited")
				case "unknown-flag":
					coordinationHostReply(ws, id, "active", "queueDrained")
				case "systemError":
					coordinationHostReply(ws, id, "systemError")
				case "disconnect":
					ws.Close()
				case "too-many-unrelated":
					for i := 0; i < 129; i++ {
						if websocket.Message.Send(ws, `{"method":"unrelated","params":"PRIVATE_HOST_BODY_MUST_NOT_ESCAPE"}`) != nil {
							return
						}
					}
					coordinationHostReply(ws, id, "idle")
				case "oversized":
					_ = websocket.Message.Send(ws, strings.Repeat("S", (1<<20)+1))
				}
			})
			r, err := hostobserve.Run(context.Background(), hostobserve.Config{Binding: binding, For: "idle", Timeout: time.Second, Interval: time.Second})
			if err != nil || r.Outcome == "matched" {
				t.Fatalf("invalid host matched: %+v %v", r, err)
			}
			if mode == "systemError" && (r.Outcome != "unavailable" || r.LastObservation == nil || r.LastObservation.State != "systemError") {
				t.Fatalf("systemError lost valid observation: %+v", r)
			}
			if got := methods(); !reflect.DeepEqual(got, []string{"initialize", "initialized", "thread/read"}) {
				t.Fatalf("unexpected host effects: %v", got)
			}
		})
	}
}

func TestCoordinationHostCancellationClosesPendingRPC(t *testing.T) {
	entered := make(chan struct{})
	closed := make(chan struct{})
	binding, _ := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
		close(entered)
		var ignored []byte
		_ = websocket.Message.Receive(ws, &ignored)
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan hostobserve.Result, 1)
	go func() {
		r, _ := hostobserve.Run(ctx, hostobserve.Config{Binding: binding, For: "idle", Timeout: 3 * time.Second, Interval: time.Second})
		done <- r
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("did not reach RPC")
	}
	cancel()
	select {
	case r := <-done:
		if r.Outcome != "cancelled" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("host socket retained after cancel")
	}
}

func TestCoordinationHostLostConnectionRetainsLastObservation(t *testing.T) {
	binding, methods := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
		if n == 1 {
			coordinationHostReply(ws, id, "active", "waitingOnApproval")
			return
		}
		ws.Close()
	})
	r, err := hostobserve.Run(context.Background(), hostobserve.Config{Binding: binding, For: "idle", Timeout: time.Second, Interval: 250 * time.Millisecond, Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil || r.Outcome != "unavailable" || r.LastObservation == nil || r.LastObservation.State != "active" || !reflect.DeepEqual(r.LastObservation.ActiveFlags, []string{"waitingOnApproval"}) || r.LastObservation.QueueState != "unknown" {
		t.Fatalf("disconnect became idle/exit or lost valid metadata: %+v %v", r, err)
	}
	if !reflect.DeepEqual(methods(), []string{"initialize", "initialized", "thread/read", "thread/read"}) {
		t.Fatal("disconnect silently restarted host baseline")
	}
}

func TestCoordinationHostFiniteDeadlineAndInitialNonMatch(t *testing.T) {
	binding, methods := coordinationHost(t, func(ws *websocket.Conn, id, n int) { coordinationHostReply(ws, id, "notLoaded") })
	start := time.Now()
	r, err := hostobserve.Run(context.Background(), hostobserve.Config{Binding: binding, For: "idle", Timeout: time.Second, Interval: 30 * time.Second})
	if err != nil || r.Outcome != "timeout" || r.LastObservation == nil || r.LastObservation.State != "notLoaded" || time.Since(start) > 2*time.Second {
		t.Fatalf("wait ignored finite deadline or treated unload as idle: %+v %v", r, err)
	}
	if !reflect.DeepEqual(methods(), []string{"initialize", "initialized", "thread/read"}) {
		t.Fatal("wait exceeded overall deadline with another RPC")
	}
}

func TestCoordinationHostCLIEnvelopeAndConfigIsolation(t *testing.T) {
	p := newProcessFixture(t)
	home := filepath.Join(p.dir, "poisoned-home")
	os.MkdirAll(home, 0700)
	sentinel := []byte("invalid trusted config; PRIVATE_CONFIG_MUST_NOT_ESCAPE")
	path := filepath.Join(home, "config.toml")
	os.WriteFile(path, sentinel, 0600)
	run := func(args ...string) ([]byte, []byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = []string{"HOME=" + p.dir, "CLAWBRIDGE_HOME=" + home, "PATH=" + p.dir}
		var out, diag bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diag
		err := cmd.Run()
		return out.Bytes(), diag.Bytes(), err
	}
	for _, command := range []string{"host-status", "host-wait"} {
		out, diag, err := run(command, "--help")
		if err != nil || len(diag)+len(out) == 0 {
			t.Fatalf("help reads config or fails: %v", err)
		}
	}
	for _, args := range [][]string{
		{"host-wait", "--for", "exit"}, {"host-status", "--adapter", "codex", "--endpoint", "https://unsafe.invalid/PRIVATE_URL", "--thread", coordinationHostThread},
		{"host-status", "--adapter", "codex", "--endpoint", "unix:///tmp/unused?PRIVATE_URL", "--thread", coordinationHostThread},
	} {
		out, diag, err := run(args...)
		if err == nil {
			t.Fatal("invalid options accepted")
		}
		if len(out) != 0 || bytes.Contains(diag, []byte("PRIVATE_")) {
			t.Fatal("unsafe option/config reflected")
		}
	}
	binding, _ := coordinationHost(t, func(ws *websocket.Conn, id, n int) { coordinationHostReply(ws, id, "idle") })
	out, diag, err := run("host-status", "--adapter", binding.Adapter, "--endpoint", binding.Endpoint, "--thread", binding.Thread, "--timeout", "1s")
	if err != nil {
		t.Fatalf("host command accessed poisoned config: %v %s", err, diag)
	}
	var envelope struct {
		Source string                     `json:"source"`
		Data   map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(out, &envelope) != nil || envelope.Source != "host-observation" || len(envelope.Data) != 2 || string(envelope.Data["outcome"]) != `"matched"` || envelope.Data["last_observation"] == nil {
		t.Fatalf("wrong standalone terminal envelope: %s", out)
	}
	if bytes.Contains(out, []byte("PRIVATE_")) || len(diag) != 0 {
		t.Fatal("host private text escaped")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(sentinel, after) {
		t.Fatal("config mutated")
	}
}

func TestCoordinationHostCLITerminalFailureOutcomes(t *testing.T) {
	p := newProcessFixture(t)
	for _, mode := range []string{"timeout", "unsupported", "unavailable", "systemError", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			binding, _ := coordinationHost(t, func(ws *websocket.Conn, id, n int) {
				switch mode {
				case "timeout":
					coordinationHostReply(ws, id, "active")
				case "unsupported":
					coordinationHostReply(ws, id, "active", "PRIVATE_UNRECOGNIZED_HOST_TEXT")
				case "unavailable":
					_ = websocket.JSON.Send(ws, map[string]any{"id": id, "error": map[string]any{"message": "PRIVATE_HOST_ERROR_MUST_NOT_ESCAPE"}})
				case "systemError":
					coordinationHostReply(ws, id, "systemError")
				case "cancelled":
					select {
					case entered <- struct{}{}:
					default:
					}
					var ignored []byte
					_ = websocket.Message.Receive(ws, &ignored)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			command := "host-status"
			if mode == "timeout" || mode == "cancelled" {
				command = "host-wait"
			}
			args := []string{command, "--adapter", binding.Adapter, "--endpoint", binding.Endpoint, "--thread", binding.Thread, "--timeout", "1s"}
			if command == "host-wait" {
				args = append(args, "--for", "idle", "--interval", "30s")
			}
			cmd := exec.CommandContext(ctx, p.binary, args...)
			cmd.Dir = p.dir
			cmd.Env = []string{"HOME=" + p.dir, "CLAWBRIDGE_HOME=" + filepath.Join(p.dir, "unused-state"), "PATH=" + p.dir}
			var out, diag bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &diag
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if mode == "cancelled" {
				select {
				case <-entered:
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						cmd.Process.Kill()
						t.Fatal(err)
					}
				case <-ctx.Done():
					cmd.Process.Kill()
					t.Fatal("fake RPC never entered")
				}
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("nonmatched host outcome exited zero")
			}
			if ctx.Err() != nil {
				t.Fatal("host command exceeded finite/cancel bound")
			}
			var envelope struct {
				Source string             `json:"source"`
				Data   hostobserve.Result `json:"data"`
			}
			if json.Unmarshal(out.Bytes(), &envelope) != nil || envelope.Source != "host-observation" {
				t.Fatalf("not exactly one terminal envelope: %q", out.String())
			}
			want := mode
			if mode == "systemError" {
				want = "unavailable"
			}
			if envelope.Data.Outcome != want {
				t.Fatalf("outcome=%s want=%s", envelope.Data.Outcome, want)
			}
			if mode == "systemError" && (envelope.Data.LastObservation == nil || envelope.Data.LastObservation.State != "systemError") {
				t.Fatal("host-status discarded last valid systemError observation")
			}
			if strings.Contains(out.String()+diag.String(), "PRIVATE_") || strings.Contains(out.String()+diag.String(), binding.Endpoint) {
				t.Fatal("host text/private endpoint echoed")
			}
			if _, err := os.Stat(filepath.Join(p.dir, "unused-state")); !os.IsNotExist(err) {
				t.Fatal("standalone failure created app state")
			}
		})
	}
}
