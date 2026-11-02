package keyagent

// Harden applies the process hardening that does not need the key, and
// returns one line for each step that did not apply (best effort; none is
// fatal):
//
//   - core dumps off (RLIMIT_CORE=0), on every Unix;
//   - Linux: prctl(PR_SET_DUMPABLE, 0), which also blocks same-uid ptrace and
//     /proc/<pid>/mem reads;
//   - macOS: ptrace(PT_DENY_ATTACH);
//   - Windows and other systems: nothing is applied.
//
// These are partial hardening against a same-user program, not a guarantee.
func Harden() []string { return hardenPlatform() }
