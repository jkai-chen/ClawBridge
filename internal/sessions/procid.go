package sessions

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"clawbridge/internal/ctxwatch"
)

// MaxAgentHops is how far up AgentAncestor looks for an agent (F2).
const MaxAgentHops = 5

// ProcID names one process instance. Start is the platform's process start
// time in an opaque unit; it is only ever compared for equality, and guards
// against a PID being reissued to another process.
type ProcID struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"`
}

// procState is what a process-table lookup could tell.
type procState int

const (
	// procGone: no process has this PID.
	procGone procState = iota
	// procRunning: the process exists and its start time is known.
	procRunning
	// procUnknown: the lookup failed for another reason (for instance access
	// denied), so nothing is known either way.
	procUnknown
)

// lookupProc and parentPID are the seams to the platform; tests replace them.
var (
	lookupProc = platformLookup
	parentPID  = ctxwatch.ParentPID
)

// Lookup is the process with this PID, with its start time. It reports false
// when the process doesn't exist or its start time can't be read.
func Lookup(pid int) (ProcID, bool) {
	if pid <= 0 {
		return ProcID{}, false
	}
	start, st := lookupProc(pid)
	if st != procRunning {
		return ProcID{}, false
	}
	return ProcID{PID: pid, Start: start}, true
}

// Self is this process. If its start time can't be read, Start is 0, and
// such an identity is never Alive.
func Self() ProcID {
	pid := os.Getpid()
	if p, ok := Lookup(pid); ok {
		return p
	}
	return ProcID{PID: pid}
}

// Parent is the parent of p. It reports false when there is none or it
// can't be identified.
func Parent(p ProcID) (ProcID, bool) {
	pp, ok := parentPID(p.PID)
	if !ok {
		return ProcID{}, false
	}
	return Lookup(pp)
}

// Alive says whether p is still that process: it exists and its start time
// is the same. A reused PID is not alive.
func Alive(p ProcID) bool {
	if p.PID <= 0 || p.Start == 0 {
		return false
	}
	start, st := lookupProc(p.PID)
	return st == procRunning && start == p.Start
}

// gone says that p is surely not running any more: the PID is unused, or has
// a different start time. A failed lookup is not "gone", so a sweep never
// deletes on a guess.
func gone(p ProcID) bool {
	if p.PID <= 0 {
		return true
	}
	start, st := lookupProc(p.PID)
	switch st {
	case procGone:
		return true
	case procRunning:
		return start != p.Start
	}
	return false
}

// AgentAncestor walks up from the process from, at most maxHops parents
// (capped at MaxAgentHops), and returns the first one hasLink accepts. A
// wrapper or shim between the agent and its child is passed this way (F2).
// from itself is not tested. hops is the number of levels climbed, for
// doctor.
func AgentAncestor(from ProcID, hasLink func(ProcID) bool, maxHops int) (agent ProcID, hops int, ok bool) {
	maxHops = min(maxHops, MaxAgentHops)
	cur := from
	for hops = 1; hops <= maxHops; hops++ {
		p, found := Parent(cur)
		if !found || p.PID <= 1 {
			return ProcID{}, 0, false
		}
		if hasLink(p) {
			return p, hops, true
		}
		cur = p
	}
	return ProcID{}, 0, false
}

// parseLinuxStat reads the parent PID and the start time (field 22, in clock
// ticks since boot) from the text of /proc/<pid>/stat. The command name is in
// parentheses and may hold spaces and parentheses, so the fields start after
// the last closing one.
func parseLinuxStat(s string) (ppid int, start int64, err error) {
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, 0, errors.New("no command name in stat")
	}
	// Fields after the name: state (3), ppid (4), ..., starttime (22).
	fields := strings.Fields(s[end+1:])
	if len(fields) < 20 {
		return 0, 0, errors.New("stat is too short")
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, err
	}
	start, err = strconv.ParseInt(fields[19], 10, 64)
	if err != nil || start <= 0 {
		return 0, 0, errors.New("bad start time in stat")
	}
	return ppid, start, nil
}
