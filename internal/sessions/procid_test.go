package sessions

import (
	"os"
	"testing"
)

func TestParseLinuxStat(t *testing.T) {
	// A command name with spaces and parentheses, as the kernel prints it.
	s := "4321 (my (odd) cmd) S 77 4321 4321 34816 4321 4194304 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 200 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	ppid, start, err := parseLinuxStat(s)
	if err != nil || ppid != 77 || start != 987654 {
		t.Fatalf("got ppid %d start %d err %v", ppid, start, err)
	}
	for _, bad := range []string{"", "1 cmd S 1", "1 (c) S 1 2", "1 (c) S x 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 5 1"} {
		if _, _, err := parseLinuxStat(bad); err == nil {
			t.Errorf("parseLinuxStat(%q) should fail", bad)
		}
	}
}

func TestSelfLookupAlive(t *testing.T) {
	self := Self()
	if self.PID != os.Getpid() {
		t.Fatalf("Self pid %d", self.PID)
	}
	if self.Start == 0 {
		t.Skip("no process start time on this platform")
	}
	if !Alive(self) {
		t.Error("this process should be alive")
	}
	if Alive(ProcID{PID: self.PID, Start: self.Start + 1}) {
		t.Error("a different start time must not be alive")
	}
	if p, ok := Lookup(self.PID); !ok || p != self {
		t.Errorf("Lookup = %v %v", p, ok)
	}
	if _, ok := Parent(self); !ok {
		t.Error("this process has a parent")
	}
}

func TestAliveChecksStart(t *testing.T) {
	f := newFakeProcs(t)
	p := f.add(fakeBase+1, 100)
	if !Alive(p) {
		t.Fatal("alive")
	}
	if Alive(ProcID{PID: p.PID, Start: 101}) {
		t.Error("PID reuse with another start time is not alive")
	}
	f.kill(p.PID)
	if Alive(p) {
		t.Error("a gone process is not alive")
	}
	if Alive(ProcID{}) || Alive(ProcID{PID: p.PID}) {
		t.Error("an empty identity is not alive")
	}
}

func chain(f *fakeProcs, pids ...int) []ProcID {
	var out []ProcID
	for i, pid := range pids {
		out = append(out, f.add(pid, int64(1000+pid)))
		if i > 0 {
			f.parents[pids[i-1]] = pid
		}
	}
	return out
}

func TestAgentAncestor(t *testing.T) {
	f := newFakeProcs(t)
	// server -> shim -> shim -> agent -> shell
	c := chain(f, fakeBase+10, fakeBase+11, fakeBase+12, fakeBase+13, fakeBase+14)
	agent := c[3]
	has := func(p ProcID) bool { return p == agent }

	got, hops, ok := AgentAncestor(c[0], has, 5)
	if !ok || got != agent || hops != 3 {
		t.Errorf("wrapper chain of 3: %v %d %v", got, hops, ok)
	}
	if _, _, ok := AgentAncestor(c[0], has, 2); ok {
		t.Error("limit 2 must not reach the agent 3 hops up")
	}
	// The limit is capped at 5 however large a caller asks.
	f2 := f
	long := chain(f2, fakeBase+20, fakeBase+21, fakeBase+22, fakeBase+23, fakeBase+24, fakeBase+25, fakeBase+26)
	far := long[6] // 6 hops up
	if _, _, ok := AgentAncestor(long[0], func(p ProcID) bool { return p == far }, 99); ok {
		t.Error("never looks beyond 5 hops")
	}
	near := long[5] // 5 hops up
	if g, h, ok := AgentAncestor(long[0], func(p ProcID) bool { return p == near }, 99); !ok || h != 5 || g != near {
		t.Errorf("5 hops: %v %d %v", g, h, ok)
	}
	// None has a link.
	if _, _, ok := AgentAncestor(c[0], func(ProcID) bool { return false }, 5); ok {
		t.Error("no link anywhere")
	}
	// The first with a link wins, not the highest.
	both := func(p ProcID) bool { return p == c[1] || p == c[3] }
	if g, h, _ := AgentAncestor(c[0], both, 5); g != c[1] || h != 1 {
		t.Errorf("first link wins: %v %d", g, h)
	}
}
