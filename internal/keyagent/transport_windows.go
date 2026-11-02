//go:build windows

package keyagent

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pipeConn is one end of a synchronous byte-mode pipe. A deadline is a
// timer that cancels the pending I/O.
type pipeConn struct {
	h      windows.Handle
	server bool

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

func (p *pipeConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var n uint32
	err := windows.ReadFile(p.h, b, &n, nil)
	switch {
	case err == nil:
		return int(n), nil
	case errors.Is(err, windows.ERROR_BROKEN_PIPE), errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED):
		return int(n), io.EOF
	case errors.Is(err, windows.ERROR_MORE_DATA):
		return int(n), nil
	case errors.Is(err, windows.ERROR_OPERATION_ABORTED):
		return int(n), os.ErrDeadlineExceeded
	}
	return int(n), err
}

func (p *pipeConn) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(p.h, b, &n, nil)
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		return int(n), os.ErrDeadlineExceeded
	}
	return int(n), err
}

func (p *pipeConn) SetDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if t.IsZero() || p.closed {
		return nil
	}
	h := p.h
	p.timer = time.AfterFunc(time.Until(t), func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.closed {
			windows.CancelIoEx(h, nil)
		}
	})
	return nil
}

func (p *pipeConn) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.server {
		windows.FlushFileBuffers(p.h)
		windows.DisconnectNamedPipe(p.h)
	}
	return windows.CloseHandle(p.h)
}

// pipeListener keeps one unconnected instance of the pipe ready at all
// times, so a client never finds the name without an instance.
type pipeListener struct {
	name string
	sa   *windows.SecurityAttributes

	mu        sync.Mutex
	next      windows.Handle
	closed    bool
	accepting bool
}

// newInstance makes one instance of the pipe. The first is made with
// FILE_FLAG_FIRST_PIPE_INSTANCE, which fails if the name is already taken,
// so no other process can have squatted it. Every instance is byte mode,
// refuses remote clients and carries the user-only security descriptor.
func (l *pipeListener) newInstance(first bool) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	return windows.CreateNamedPipe(name, flags,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, MaxFrame+4, MaxFrame+4, 0, l.sa)
}

func (l *pipeListener) Accept() (conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, errListenerClosed
	}
	h := l.next
	l.accepting = true
	l.mu.Unlock()
	err := windows.ConnectNamedPipe(h, nil)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accepting = false
	if l.closed || (err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED)) {
		// Close left the idle instance to this call.
		windows.CloseHandle(h)
		return nil, errListenerClosed
	}
	nh, err := l.newInstance(false)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	l.next = nh
	return &pipeConn{h: h, server: true}, nil
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.accepting {
		// Cancel the Accept blocked in ConnectNamedPipe; it closes the
		// instance.
		return windows.CancelIoEx(l.next, nil)
	}
	return windows.CloseHandle(l.next)
}

var errListenerClosed = errors.New("the pipe listener is closed")

// listen makes the pipe. A name already taken is ErrAlreadyRunning when the
// holder answers as an agent for this user; otherwise the failure is
// reported as it is.
func listen(state string) (listener, error) {
	name, err := endpointFor(state, true)
	if err != nil {
		return nil, err
	}
	sid, err := userSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL(sid))
	if err != nil {
		return nil, errors.New("cannot build the key agent pipe's access control")
	}
	l := &pipeListener{name: name, sa: &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}}
	h, err := l.newInstance(true)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			if _, derr := (&Client{state: state, dial: dial}).Status(); derr == nil {
				return nil, ErrAlreadyRunning
			}
		}
		return nil, errors.New("cannot make the key agent pipe")
	}
	l.next = h
	return l, nil
}

// winDialer is pipeDialer for Windows.
type winDialer struct{ name string }

func (d winDialer) open() (conn, error) {
	name, err := windows.UTF16PtrFromString(d.name)
	if err != nil {
		return nil, err
	}
	for try := 0; try < 20; try++ {
		// SECURITY_ANONYMOUS: the server may not impersonate this client.
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_EXISTING, windows.SECURITY_SQOS_PRESENT|windows.SECURITY_ANONYMOUS, 0)
		switch {
		case err == nil:
			return &pipeConn{h: h}, nil
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
			return nil, ErrLocked
		case errors.Is(err, windows.ERROR_PIPE_BUSY):
			time.Sleep(50 * time.Millisecond)
		default:
			return nil, errors.New("cannot reach the key agent")
		}
	}
	return nil, errors.New("the key agent is busy")
}

func (d winDialer) serverOwner(c conn) (string, error) {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(c.(*pipeConn).h, &pid); err != nil {
		return "", err
	}
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(ph)
	var tok windows.Token
	if err = windows.OpenProcessToken(ph, windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

func (d winDialer) self() (string, error) { return userSID() }

// dial connects to the pipe and confirms its server is this user's process
// before the caller sends anything.
func dial(state string) (conn, error) {
	name, err := endpointFor(state, false)
	if err != nil {
		return nil, err
	}
	return connectPipe(winDialer{name: name})
}
