//go:build !windows

package keyagent

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"clawbridge/internal/fsprivate"
)

// peerUIDSeam, when set, replaces peerUIDOf; tests use it. It is atomic
// because the server's accept loop reads it while a test swaps it.
var peerUIDSeam atomic.Pointer[func(*net.UnixConn) (uint32, error)]

// peerUID is the uid of the process on the other end of c. It fails closed:
// a system with no way to read it refuses every peer.
func peerUID(c *net.UnixConn) (uint32, error) {
	if f := peerUIDSeam.Load(); f != nil {
		return (*f)(c)
	}
	return peerUIDOf(c)
}

// umaskMu serialises the process-wide umask change around a bind.
var umaskMu sync.Mutex

var errPeer = errors.New("the key agent's socket is served by another user")

// unixListener drops every peer that is not this user.
type unixListener struct{ l *net.UnixListener }

func (u *unixListener) Accept() (conn, error) {
	for {
		c, err := u.l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err != nil || uid != uint32(fsprivate.CurrentUID()) {
			c.Close()
			continue
		}
		return c, nil
	}
}

func (u *unixListener) Close() error { return u.l.Close() }

// isOurSocket is ownedSocket; tests replace it.
var isOurSocket = ownedSocket

// ownedSocket reports whether path is a socket owned by this user.
func ownedSocket(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode()&os.ModeSocket != 0 && int(st.Uid) == fsprivate.CurrentUID(), nil
}

// listen binds the agent's socket. A socket file left by a dead agent that
// is ours is removed and rebound; a live agent, or a file that is not
// provably our socket, stops it.
func listen(state string) (listener, error) {
	path, err := endpointFor(state, true)
	if err != nil {
		return nil, err
	}
	// The lock makes "check for a stale socket, remove it, bind" one step
	// against another Serve doing the same.
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("cannot open the key agent's lock file")
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, ErrAlreadyRunning
	}
	if _, err = os.Lstat(path); err == nil {
		mine, _ := isOurSocket(path)
		if !mine {
			return nil, errors.New("something that is not this user's socket is at " + path + "; refusing to remove it")
		}
		c, derr := net.DialTimeout("unix", path, 2*time.Second)
		if derr == nil {
			c.Close()
			return nil, ErrAlreadyRunning
		}
		if !errors.Is(derr, syscall.ECONNREFUSED) {
			return nil, errors.New("cannot tell whether a key agent is running at " + path)
		}
		if err = os.Remove(path); err != nil {
			return nil, errors.New("cannot remove the stale socket at " + path)
		}
	}
	umaskMu.Lock()
	old := unix.Umask(0o177)
	l, err := net.Listen("unix", path)
	unix.Umask(old)
	umaskMu.Unlock()
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrAlreadyRunning
		}
		return nil, errors.New("cannot listen at " + path)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != 0o600 {
		l.Close()
		return nil, errors.New("the key agent's socket is not 0600")
	}
	return &unixListener{l: l.(*net.UnixListener)}, nil
}

// dial connects to the agent for state and confirms the server is this
// user's process. A missing or refusing socket is ErrLocked.
func dial(state string) (conn, error) {
	path, err := endpointFor(state, false)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrLocked
		}
		return nil, errors.New("cannot reach the key agent")
	}
	uid, err := peerUID(c.(*net.UnixConn))
	if err != nil || uid != uint32(fsprivate.CurrentUID()) {
		c.Close()
		return nil, errPeer
	}
	return c, nil
}
