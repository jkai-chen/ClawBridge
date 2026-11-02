// Package keyagent is the passphrase-mode key agent: a per-user,
// per-state-directory process that holds the unwrapped data key in memory
// and answers a small set of operations over an owner-only local endpoint,
// so the data key never leaves it (live/KEY-STORAGE-REDESIGN.md §3.6). It
// is the ssh-agent model: a person unlocks once with the passphrase, the
// agent keeps the key, and every client asks the agent instead of holding
// the key.
//
// Serve is the agent; Dial gives a Client. The CLI detaches the process and
// reads the passphrase; neither is done here.
//
// Endpoint. macOS: $TMPDIR/clawbridge-<statehash>/agent.sock. Linux:
// $XDG_RUNTIME_DIR/clawbridge/<statehash>.sock, else
// /tmp/clawbridge-<uid>/<statehash>.sock. Windows: the named pipe
// \\.\pipe\clawbridge-<user SID>-<statehash>. <statehash> is the first 16
// hex digits of the SHA-256 of the state directory's canonical path. The
// directory holding a socket is 0700, owned by this user, not a symlink, and
// checked on every use; the socket is 0600 and the server drops a peer whose
// uid is not its own. The Windows pipe has a DACL granting only this user,
// refuses remote clients and is made as the first instance of its name, and
// the client confirms the serving process belongs to this user before it
// sends anything.
//
// Protocol. One request per connection: a 4-byte big-endian length and a
// JSON object, at most 64 KiB each way, read under a 5 second deadline.
// Every request carries "v":1 and an "op". Fields an op does not take, and
// fields the package does not know, are refused. The operations are status,
// device_new, sign, unseal, seal, rotate and lock. A failure is one of a
// fixed set of codes with a fixed message; nothing in a request is echoed.
//
// What the agent will not do. The device seed is made inside the agent
// (device_new) and only ever leaves as a signature: sign builds the server's
// canonical request string itself (devicekey.RequestBaseSum, the function
// devicekey.RequestBase and so devicesign.Signer.Authorization are built on)
// from structured fields, and never signs a caller's bytes. unseal and seal
// refuse the device kind. unseal of a Linear API key needs a purpose tag
// (defence in depth only: a same-user program can send either tag).
//
// Memory. The data key is handed to Serve in a buffer. Serve gives it to a
// sealed.Store, which keeps its own copy, and zeroes the caller's buffer at
// once, so the only live copy is the store's, zeroed by Store.Lock when the
// agent exits. That copy sits in an ordinary Go allocation inside package
// sealed, so it is not mlocked; core dumps are disabled and, where the OS
// allows, debugger attach is refused (see Harden). The Go runtime may have
// copied the key while it ran and the garbage collector will not clear
// those copies: zeroing is best effort and the docs say so.
package keyagent
