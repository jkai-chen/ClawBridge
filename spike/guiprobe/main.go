// SPIKE (live/GUI-P1-SPIKE.md): throwaway. guiprobe starts a gui-backend
// the way the GUI will, syncs on its preamble, and makes RPC round trips.
//
//	guiprobe -bin ./clawbridge           # local child
//	guiprobe -wsl Ubuntu                  # Windows: backend inside WSL
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// wslLaunch is the design's fixed login-shell string (GUI-DESIGN §6.2):
// no user input in it; only the distro name varies.
const wslLaunch = `L=$(getent passwd "$(id -un)" | cut -d: -f7); exec "${L:-/bin/sh}" -lc "exec clawbridge gui-backend --stdio"`

type client struct {
	w  io.Writer
	r  *bufio.Reader
	id int
}

func (c *client) send(method string, params any) error {
	c.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	_, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (c *client) next() (map[string]any, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
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
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(body, &m)
}

// call sends one request and returns its reply, printing notifications.
func (c *client) call(method string, params any) (map[string]any, error) {
	if err := c.send(method, params); err != nil {
		return nil, err
	}
	for {
		m, err := c.next()
		if err != nil {
			return nil, err
		}
		if _, ok := m["id"]; ok {
			return m, nil
		}
		printNote(m)
	}
}

func printNote(m map[string]any) {
	p, _ := m["params"].(map[string]any)
	if d, ok := p["data"].(string); ok {
		b, _ := base64.StdEncoding.DecodeString(d)
		fmt.Printf("  note %v: %q\n", m["method"], b)
		return
	}
	fmt.Printf("  note %v %v\n", m["method"], p)
}

// wslDistros decodes `wsl.exe -l -q`, which prints UTF-16LE.
func wslDistros() ([]string, error) {
	out, err := exec.Command("wsl.exe", "-l", "-q").Output()
	if err != nil {
		return nil, err
	}
	u := make([]uint16, 0, len(out)/2)
	for i := 0; i+1 < len(out); i += 2 {
		u = append(u, uint16(out[i])|uint16(out[i+1])<<8)
	}
	var names []string
	for _, n := range strings.Split(string(utf16.Decode(u)), "\n") {
		if n = strings.TrimSpace(strings.TrimPrefix(n, "\uFEFF")); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

func main() {
	bin := flag.String("bin", "clawbridge", "local backend binary")
	distro := flag.String("wsl", "", "run the backend inside this WSL distro")
	ptyKind := flag.String("pty", "", "open a terminal of this kind (claude|codex) and type into it")
	flag.Parse()
	if err := run(*bin, *distro, *ptyKind); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run(bin, distro, ptyKind string) error {
	var cmd *exec.Cmd
	if distro != "" {
		names, err := wslDistros()
		if err != nil {
			return err
		}
		fmt.Printf("distros: %q\n", names)
		found := false
		for _, n := range names {
			found = found || n == distro
		}
		if !found {
			return errors.New("distro not in wsl.exe -l -q")
		}
		cmd = exec.Command("wsl.exe", "-d", distro, "--cd", "~", "--", "sh", "-c", wslLaunch)
	} else {
		cmd = exec.Command(bin, "gui-backend", "--stdio")
	}
	fmt.Printf("argv: %q\n", cmd.Args)
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	nonce := hex.EncodeToString(raw)
	if _, err := io.WriteString(stdin, nonce+"\n"); err != nil {
		return err
	}
	r := bufio.NewReader(stdout)
	want := "CLAWBRIDGE-GUI-BACKEND/1 " + nonce
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return errors.New("no preamble within 60 s")
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("before the preamble: %w", err)
		}
		if strings.TrimRight(line, "\r\n") == want {
			fmt.Println("preamble: synced")
			break
		}
		fmt.Printf("discarded before preamble: %q\n", line)
	}
	c := &client{w: stdin, r: r}
	for _, m := range []string{"hello", "sessions.list", "doctor.checks"} {
		reply, err := c.call(m, map[string]any{})
		if err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
		b, _ := json.Marshal(reply)
		if len(b) > 400 {
			b = append(b[:400], "…"...)
		}
		fmt.Printf("%s -> %s\n", m, b)
		if m == "hello" && reply["result"] == nil {
			return errors.New("hello failed")
		}
	}
	if ptyKind != "" {
		reply, err := c.call("pty.open", map[string]any{"kind": ptyKind, "dir": ".", "cols": 100, "rows": 30})
		if err != nil {
			return err
		}
		res, _ := reply["result"].(map[string]any)
		if res == nil {
			return fmt.Errorf("pty.open: %v", reply["error"])
		}
		id := res["id"]
		if _, err = c.call("pty.write", map[string]any{"id": id, "data": base64.StdEncoding.EncodeToString([]byte("ping\r"))}); err != nil {
			return err
		}
		if _, err = c.call("pty.resize", map[string]any{"id": id, "cols": 120, "rows": 40}); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		if _, err = c.call("pty.close", map[string]any{"id": id}); err != nil {
			return err
		}
	}
	return nil
}
