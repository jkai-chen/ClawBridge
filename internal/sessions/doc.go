// Package sessions holds the local identity state of ClawBridge agents
// (SESSION-CONFIG-SIMPLIFY §2): agent links, the per-user conversation index
// and per-project session records.
//
// A link ties a running agent process (PID plus start time) to its
// conversation. The index maps a conversation to the home (state directory)
// whose record owns it. A record is one named session in a project, with the
// instance, the conversation aliases and the live owner. Everything is
// 0700/0600 and written atomically; records are changed only under a
// per-project lock. This package does no messaging and starts no processes.
//
// A caller must not hold WithProjectLock across a network round trip: a lock
// is never taken from a live holder, so a slow call blocks every other claim
// in the project until the 15 s wait ends. Claim on the server first, then
// take the lock, re-check and write.
package sessions
