//go:build !windows

package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestL1CLIImportSIGINTWithOpenStdinExitsWithoutEOF(t *testing.T) {
	p := newProcessFixture(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	cmd := exec.Command(p.binary, "linear", "import", "--name", "canceled-pipe-fixture", "--token-stdin")
	cmd.Dir = p.dir
	emptyPath := filepath.Join(t.TempDir(), "empty-path")
	if err := os.Mkdir(emptyPath, 0700); err != nil {
		t.Fatal(err)
	}
	for _, v := range p.env {
		if strings.HasPrefix(v, "HOME=") || strings.HasPrefix(v, "PATH=") || strings.HasPrefix(v, "XDG_") || strings.HasPrefix(v, "DBUS_") {
			continue
		}
		cmd.Env = append(cmd.Env, v)
	}
	cmd.Env = append(cmd.Env, "HOME="+filepath.Join(p.dir, "private-home"), "XDG_CONFIG_HOME="+filepath.Join(p.dir, "private-xdg"), "PATH="+emptyPath, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(p.dir, "absent-bus"))
	cmd.Stdin = r
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		r.Close()
		t.Fatal(err)
	}
	r.Close() // Child owns its duplicated descriptor; writer deliberately stays open.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	joined := false
	defer func() {
		if !joined {
			w.Close()
			cmd.Process.Kill()
			<-done
		}
	}()
	// The public CLI has no pre-read readiness line. Allow startup, then require
	// its own context-canceled diagnostic to exclude default signal termination.
	select {
	case err := <-done:
		joined = true
		t.Fatalf("import exited before cancellation despite open stdin: %v", err)
	case <-time.After(time.Second):
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	prompt := true
	var waitErr error
	select {
	case waitErr = <-done:
		joined = true
	case <-time.After(time.Second):
		prompt = false
	}
	if !joined {
		w.Close() // Owned fixture cleanup must unblock the failed child before stopping.
		select {
		case waitErr = <-done:
			joined = true
		case <-time.After(time.Second):
			cmd.Process.Kill()
			waitErr = <-done
			joined = true
		}
	}
	if !prompt {
		t.Error("SIGINT-canceled import required parent EOF/cleanup to exit")
	}
	if waitErr == nil || !strings.Contains(output.String(), "context canceled") {
		t.Fatalf("CLI did not report context cancellation (default signal exit is not acceptance): err=%v output=%q", waitErr, output.String())
	}
	for _, path := range []string{filepath.Join(p.dir, "state"), filepath.Join(p.dir, "private-home"), filepath.Join(p.dir, "private-xdg"), p.db} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("canceled empty-stdin import created app/credential/config state")
		}
	}
}
