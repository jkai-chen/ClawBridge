package keyagent

import "errors"

// The Windows pipe's name, access control and client check are plain
// strings and an interface so they run, and are tested, on every OS.

// pipeName is the named pipe for a user and a state directory.
func pipeName(sid, hash string) string {
	return `\\.\pipe\clawbridge-` + sid + "-" + hash
}

// pipeSDDL is the security descriptor of every instance of the pipe: a
// protected DACL (it inherits nothing) with one entry, full access for the
// user's SID. There is no Everyone, no Administrators and no SYSTEM entry.
func pipeSDDL(sid string) string {
	return "D:P(A;;GA;;;" + sid + ")"
}

var errPipeServer = errors.New("the key agent's pipe is served by another user")

// pipeDialer is the client side of the pipe, so the owner check can be
// tested away from Windows.
type pipeDialer interface {
	// open connects to the pipe.
	open() (conn, error)
	// serverOwner is the SID of the user the serving process runs as,
	// found from the connection (GetNamedPipeServerProcessId and that
	// process's token).
	serverOwner(c conn) (string, error)
	// self is this process's user SID.
	self() (string, error)
}

// connectPipe opens the pipe and checks who serves it before the caller
// sends anything: a squatter on the pipe name would otherwise be handed a
// passphrase. The connection is closed and unused if the check fails or
// cannot be made.
func connectPipe(d pipeDialer) (conn, error) {
	c, err := d.open()
	if err != nil {
		return nil, err
	}
	owner, err := d.serverOwner(c)
	if err != nil {
		c.Close()
		return nil, errPipeServer
	}
	self, err := d.self()
	if err != nil || owner == "" || owner != self {
		c.Close()
		return nil, errPipeServer
	}
	return c, nil
}
