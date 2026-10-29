package ctxwatch

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func TestAcceptParent(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name          string
		parent, child time.Time
		exists, want  bool
	}{
		{"older parent", base, base.Add(time.Second), true, true},
		// C3: the parent exited and its PID now belongs to a younger process.
		{"reused id, younger", base.Add(time.Minute), base, true, false},
		{"equal times", base, base, true, false},
		{"parent gone", base, base.Add(time.Second), false, false},
		{"parent gone, zero time", time.Time{}, base, false, false},
	}
	for _, c := range cases {
		if got := acceptParent(c.parent, c.child, c.exists); got != c.want {
			t.Errorf("%s: acceptParent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsShellImage(t *testing.T) {
	for name, want := range map[string]bool{
		"bash.exe": true, "SH.EXE": true, "PowerShell.exe": true, "pwsh.exe": true,
		"claude.exe": false, "node.exe": false, "cmd.exe": false, "": false,
	} {
		if got := isShellImage(name); got != want {
			t.Errorf("isShellImage(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestHookAgentPIDFromEnvironment(t *testing.T) {
	t.Setenv("CLAWBRIDGE_AGENT_PID", "4242")
	if got := HookAgentPID(); got != 4242 {
		t.Fatalf("HookAgentPID = %d, want 4242", got)
	}
}

// wantParent is this process's parent: os.Getppid on unix, ParentPID on
// Windows, where Getppid can name a parent that has exited.
func wantParent() int {
	if runtime.GOOS != "windows" {
		return os.Getppid()
	}
	p, _ := ParentPID(os.Getpid())
	return p
}

func TestHookAgentPIDIgnoresInvalidVariable(t *testing.T) {
	want := wantParent()
	for _, v := range []string{"abc", "0", "-5", ""} {
		t.Setenv("CLAWBRIDGE_AGENT_PID", v)
		if got := HookAgentPID(); got != want {
			t.Errorf("CLAWBRIDGE_AGENT_PID=%q: HookAgentPID = %d, want the parent %d", v, got, want)
		}
	}
}

func TestHookAgentPIDUnsetIsTheParent(t *testing.T) {
	t.Setenv("CLAWBRIDGE_AGENT_PID", "")
	want := wantParent()
	if got := HookAgentPID(); got != want || got == 0 {
		t.Fatalf("HookAgentPID = %d, want the parent %d", got, want)
	}
}
