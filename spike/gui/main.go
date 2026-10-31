// SPIKE (live/GUI-P1-SPIKE.md Q1/Q2/Q4): throwaway Wails v3 shell. It starts
// `clawbridge gui-backend --stdio` as a child, syncs on its preamble, and
// forwards allow-listed calls. pty.* is honoured only from the terminal
// window (GUI-DESIGN §4.4, R2 H1).
//
//	CB_BACKEND=/path/to/clawbridge go run .   # local child
//	CB_WSL=Ubuntu-24.04 go run .              # Windows: backend in WSL
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed assets
var assets embed.FS

const wslLaunch = `L=$(getent passwd "$(id -un)" | cut -d: -f7); exec "${L:-/bin/sh}" -lc "exec clawbridge gui-backend --stdio"`

// mainMethods are what the main window may call; the terminal window may
// call only termMethods.
var mainMethods = map[string]bool{"hello": true, "sessions.list": true, "doctor.checks": true}
var termMethods = map[string]bool{"pty.open": true, "pty.write": true, "pty.resize": true, "pty.close": true}

type Bridge struct {
	app     *application.App
	mu      sync.Mutex
	w       io.Writer
	r       *bufio.Reader
	id      int
	waiting map[int]chan json.RawMessage
	logs    []string
}

func (b *Bridge) start() error {
	var cmd *exec.Cmd
	if d := os.Getenv("CB_WSL"); d != "" {
		cmd = exec.Command("wsl.exe", "-d", d, "--cd", "~", "--", "sh", "-c", wslLaunch)
	} else {
		bin := os.Getenv("CB_BACKEND")
		if bin == "" {
			bin = "clawbridge"
		}
		cmd = exec.Command(bin, "gui-backend", "--stdio")
	}
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	nonce := hex.EncodeToString(raw)
	if _, err := io.WriteString(in, nonce+"\n"); err != nil {
		return err
	}
	r := bufio.NewReader(out)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("backend ended before its preamble: %w", err)
		}
		if strings.TrimRight(line, "\r\n") == "CLAWBRIDGE-GUI-BACKEND/1 "+nonce {
			break
		}
		b.logs = append(b.logs, "discarded: "+strings.TrimRight(line, "\r\n"))
	}
	b.w, b.r, b.waiting = in, r, map[int]chan json.RawMessage{}
	go b.readLoop()
	return nil
}

func (b *Bridge) readLoop() {
	for {
		length := -1
		for {
			line, err := b.r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
				length, _ = strconv.Atoi(v)
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(b.r, body); err != nil {
			return
		}
		var m struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		if m.ID == nil {
			// Notifications go only to the terminal window.
			if strings.HasPrefix(m.Method, "pty.") {
				if w, ok := b.app.Window.GetByName("terminal"); ok {
					w.EmitEvent(m.Method, string(m.Params))
				}
			}
			continue
		}
		b.mu.Lock()
		ch := b.waiting[*m.ID]
		delete(b.waiting, *m.ID)
		b.mu.Unlock()
		if ch != nil {
			ch <- body
		}
	}
}

func windowName(ctx context.Context) string {
	if w, ok := ctx.Value(application.WindowKey).(application.Window); ok {
		return w.Name()
	}
	return ""
}

// Call forwards one allow-listed method for the calling window.
func (b *Bridge) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	switch windowName(ctx) {
	case "main":
		if !mainMethods[method] {
			return nil, errors.New("not allowed from this window: " + method)
		}
	case "terminal":
		if !termMethods[method] {
			return nil, errors.New("not allowed from this window: " + method)
		}
	default:
		return nil, errors.New("unknown window")
	}
	if _, ok := params["nonce"]; ok {
		return nil, errors.New("a call from the page may not carry a nonce")
	}
	if b.w == nil {
		return nil, errors.New("backend not running")
	}
	ch := make(chan json.RawMessage, 1)
	b.mu.Lock()
	b.id++
	id := b.id
	b.waiting[id] = ch
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	_, err := fmt.Fprintf(b.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply, nil
	case <-time.After(60 * time.Second):
		return nil, errors.New("backend timeout")
	}
}

// Logs returns what the shell discarded before the preamble.
func (b *Bridge) Logs(ctx context.Context) []string { return b.logs }

// OpenTerminal opens the terminal window; only the main window may ask.
func (b *Bridge) OpenTerminal(ctx context.Context) error {
	if windowName(ctx) != "main" {
		return errors.New("not allowed from this window")
	}
	if w, ok := b.app.Window.GetByName("terminal"); ok {
		w.Focus()
		return nil
	}
	b.app.Window.NewWithOptions(application.WebviewWindowOptions{Name: "terminal", Title: "Agent terminal", Width: 900, Height: 560, URL: "/terminal.html"})
	return nil
}

func main() {
	bridge := &Bridge{}
	app := application.New(application.Options{
		Name:     "ClawBridge GUI spike",
		Services: []application.Service{application.NewService(bridge)},
		Assets:   application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Mac:      application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
	})
	bridge.app = app
	if err := bridge.start(); err != nil {
		bridge.logs = append(bridge.logs, "backend: "+err.Error())
	}
	app.Window.NewWithOptions(application.WebviewWindowOptions{Name: "main", Title: "ClawBridge (spike)", Width: 1000, Height: 700, URL: "/"})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
