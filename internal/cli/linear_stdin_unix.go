//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// newLinearImportReader borrows exclusive read access to input for its lifetime,
// owns only a CLOEXEC duplicate, and restores the shared open-description flags
// before closing that duplicate. No read goroutine owns the caller's stdin.
func newLinearImportReader(ctx context.Context, input *os.File) (io.ReadCloser, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if input == nil {
		return nil, nil, errors.New("token input is unavailable")
	}
	st, err := input.Stat()
	if err != nil {
		return nil, nil, errors.New("cannot inspect token input")
	}
	if !st.Mode().IsRegular() && st.Mode()&(os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) == 0 {
		return nil, nil, errors.New("token input requires a pipe, terminal, socket or regular file")
	}
	raw, err := input.SyscallConn()
	if err != nil {
		return nil, nil, errors.New("cannot own token input handle")
	}
	fd, flags := -1, 0
	var opErr error
	err = raw.Control(func(original uintptr) {
		flags, opErr = unix.FcntlInt(original, unix.F_GETFL, 0)
		if opErr != nil {
			return
		}
		fd, opErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
		if opErr != nil {
			return
		}
		_, opErr = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK)
		if opErr != nil {
			_ = unix.Close(fd)
			fd = -1
		}
	})
	if err != nil || opErr != nil || fd < 0 {
		return nil, nil, errors.New("cannot prepare interruptible token input")
	}
	r := &linearImportReader{ctx: ctx, fd: fd, flags: flags, done: make(chan struct{})}
	stop := context.AfterFunc(ctx, func() { _ = r.Close() })
	cleanup := func() { stop(); _ = r.Close() }
	if err = ctx.Err(); err != nil {
		cleanup()
		return nil, nil, err
	}
	return r, cleanup, nil
}

type linearImportReader struct {
	ctx       context.Context
	mu        sync.Mutex
	fd, flags int
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func (r *linearImportReader) stopped() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	select {
	case <-r.done:
		return os.ErrClosed
	default:
		return nil
	}
}
func (r *linearImportReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.stopped(); err != nil {
			return 0, err
		}
		r.mu.Lock()
		if err := r.stopped(); err != nil {
			r.mu.Unlock()
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, 20)
		if err != nil {
			r.mu.Unlock()
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, errors.New("token input polling failed")
		}
		if err = r.stopped(); err != nil {
			r.mu.Unlock()
			return 0, err
		}
		if ready == 0 {
			r.mu.Unlock()
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			r.mu.Unlock()
			return 0, errors.New("token input handle is invalid")
		}
		n, err := unix.Read(r.fd, p)
		r.mu.Unlock()
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return 0, errors.New("token input read failed")
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}
func (r *linearImportReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.done)
		r.mu.Lock()
		defer r.mu.Unlock()
		// Poll and nonblocking Read share this lock: restoring blocking flags
		// cannot strand a read or race with descriptor reuse.
		_, restoreErr := unix.FcntlInt(uintptr(r.fd), unix.F_SETFL, r.flags)
		closeErr := unix.Close(r.fd)
		if restoreErr != nil || closeErr != nil {
			r.closeErr = errors.New("token input handle cleanup failed")
		}
	})
	return r.closeErr
}
