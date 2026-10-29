//go:build windows

package ctxwatch

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperSleep and TestHelperPrintAgentPID are run as child processes by
// the tests below, not as tests of their own.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_HELPER") != "sleep" {
		t.Skip("helper")
	}
	time.Sleep(30 * time.Second)
}

func TestHelperPrintAgentPID(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_HELPER") != "print" {
		t.Skip("helper")
	}
	fmt.Println("AGENTPID=" + strconv.Itoa(HookAgentPID()))
}

func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), "CLAWBRIDGE_TEST_HELPER=sleep")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

func TestParentPIDOfChildIsThisProcess(t *testing.T) {
	child := startSleeper(t)
	got, ok := ParentPID(child.Process.Pid)
	if !ok || got != os.Getpid() {
		t.Fatalf("ParentPID(child) = %d, %v; want %d", got, ok, os.Getpid())
	}
}

func TestProcessAliveFollowsTheChild(t *testing.T) {
	child := startSleeper(t)
	pid := child.Process.Pid
	if !ProcessAlive(pid) {
		t.Fatal("running child reported dead")
	}
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	if ProcessAlive(pid) {
		t.Fatal("waited child reported alive")
	}
	if ProcessAlive(0) || ProcessAlive(-1) || ProcessAlive(0x7ffffff0) {
		t.Fatal("a dead PID reported alive")
	}
}

// A hook started through PowerShell has the shell between it and the agent:
// the derivation names the process that started the shell.
func TestHookAgentPIDSkipsAShellParent(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("no powershell")
	}
	script := "& '" + os.Args[0] + "' '-test.run=^TestHelperPrintAgentPID$' '-test.v'"
	cmd := exec.Command(ps, "-NoProfile", "-Command", script)
	cmd.Env = append(os.Environ(), "CLAWBRIDGE_TEST_HELPER=print", "CLAWBRIDGE_AGENT_PID=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "AGENTPID=" + strconv.Itoa(os.Getpid())
	if !strings.Contains(string(out), want) {
		t.Fatalf("output lacks %q:\n%s", want, out)
	}
}
