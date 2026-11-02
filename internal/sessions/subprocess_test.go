package sessions

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const helperEnv = "SESSIONS_TEST_CLAIM_DIR"

// TestHelperClaimProcess is the child of TestClaimConcurrentProcesses, not a
// test of its own. It waits for "go" on stdin, claims, prints the outcome,
// and stays alive (so it stays the owner) until stdin closes.
func TestHelperClaimProcess(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper for TestClaimConcurrentProcesses")
	}
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		os.Exit(2)
	}
	_, err := Claim(dir, "web", convID(os.Getpid()%1000), Self(), "inst", Hint{})
	var inUse ErrInUse
	switch {
	case err == nil:
		fmt.Println("RESULT won")
	case errors.As(err, &inUse):
		fmt.Println("RESULT inuse")
	default:
		fmt.Println("RESULT error", err)
	}
	io.Copy(io.Discard, in)
}

// Three real processes claim one name at once: exactly one wins.
func TestClaimConcurrentProcesses(t *testing.T) {
	if Self().Start == 0 {
		t.Skip("no process start time on this platform")
	}
	dir := testRecordsDir(t)
	type child struct {
		cmd *exec.Cmd
		in  io.WriteCloser
		out *bufio.Scanner
	}
	var kids []child
	for i := 0; i < 3; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperClaimProcess$")
		cmd.Env = append(os.Environ(), helperEnv+"="+dir)
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		kids = append(kids, child{cmd, in, bufio.NewScanner(out)})
	}
	defer func() {
		for _, k := range kids {
			k.in.Close()
			k.cmd.Wait()
		}
	}()
	for _, k := range kids {
		fmt.Fprintln(k.in, "go")
	}
	wins := 0
	for _, k := range kids {
		done := make(chan string, 1)
		go func() {
			for k.out.Scan() {
				if s, ok := strings.CutPrefix(k.out.Text(), "RESULT "); ok {
					done <- s
					return
				}
			}
			done <- "no result"
		}()
		select {
		case s := <-done:
			switch s {
			case "won":
				wins++
			case "inuse":
			default:
				t.Errorf("child result: %s", s)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a child did not answer")
		}
	}
	if wins != 1 {
		t.Fatalf("%d processes won, want exactly 1", wins)
	}
}
