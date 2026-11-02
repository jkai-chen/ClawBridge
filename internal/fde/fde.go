// Package fde detects whether the disk volume holding a directory is
// protected by full-disk encryption. The answer is a heuristic for the
// person's benefit, not a security boundary: only a positive, specific
// detection is "detected". Any error, missing tool, timeout, unparseable
// output or volume we cannot map is "unknown". Windows and WSL are never
// checked: key storage there uses a passphrase by default, so Detect returns
// "unknown" with the method "windows-policy".
package fde

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// The three states of a Result.
const (
	StateDetected    = "detected"
	StateNotDetected = "not_detected"
	StateUnknown     = "unknown"
)

// commandTimeout bounds every external command.
const commandTimeout = 5 * time.Second

// Result is one detection outcome. Detail never carries usernames, home
// paths or serial numbers: only a mount point, a drive letter or a device
// basename.
type Result struct {
	State  string    `json:"state"`
	Method string    `json:"method"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// run is the single place external commands are executed. Tests replace it.
var run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// A nil Stdin reads from the null device, so nothing can prompt.
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

var now = time.Now

type runOut struct {
	out []byte
	err error
}

// runCmd runs one command with a 5 s timeout derived from ctx. It returns
// when the deadline passes even if the runner ignores its context.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	ch := make(chan runOut, 1)
	// Copy the runner first: the goroutine outlives this call when the runner ignores its context,
	// and must not read the package variable after a test has put it back.
	runner := run
	go func() {
		out, err := runner(cctx, name, args...)
		ch <- runOut{out, err}
	}()
	select {
	case r := <-ch:
		return r.out, r.err
	case <-cctx.Done():
		return nil, cctx.Err()
	}
}

func result(state, method, detail string) Result {
	return Result{State: state, Method: method, Detail: detail, At: now().UTC()}
}

func unknown(method, detail string) Result {
	return result(StateUnknown, method, detail)
}

// Detect inspects the volume that holds dir. It needs no admin rights and
// never prompts.
func Detect(ctx context.Context, dir string) Result {
	r := detectPlatform(ctx, dir)
	if r.At.IsZero() {
		r.At = now().UTC()
	}
	return r
}
