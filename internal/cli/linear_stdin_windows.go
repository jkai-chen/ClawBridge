package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var linearPeekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

// Windows redirected pipes are polled for available bytes on an owned duplicate.
// Console reads are rejected: a synchronous inherited console handle cannot
// promise cancellation without changing console settings or an unowned reader.
func newLinearImportReader(ctx context.Context, input *os.File) (io.ReadCloser, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if input == nil {
		return nil, nil, errors.New("token input is unavailable")
	}
	raw, err := input.SyscallConn()
	if err != nil {
		return nil, nil, errors.New("cannot own token input handle")
	}
	var owned windows.Handle
	var kind uint32
	var opErr error
	err = raw.Control(func(fd uintptr) {
		kind, opErr = windows.GetFileType(windows.Handle(fd))
		if opErr != nil {
			return
		}
		if kind != windows.FILE_TYPE_PIPE && kind != windows.FILE_TYPE_DISK {
			opErr = errors.New("unsupported token input")
			return
		}
		process := windows.CurrentProcess()
		opErr = windows.DuplicateHandle(process, windows.Handle(fd), process, &owned, 0, false, windows.DUPLICATE_SAME_ACCESS)
	})
	if err != nil || opErr != nil {
		return nil, nil, errors.New("token import requires a redirected pipe or regular file on Windows; console cancellation is unsupported")
	}
	r := &linearImportReader{ctx: ctx, handle: owned, pipe: kind == windows.FILE_TYPE_PIPE, done: make(chan struct{})}
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
	handle    windows.Handle
	pipe      bool
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
		want := len(p)
		if r.pipe {
			var available uint32
			ok, _, peekErr := linearPeekNamedPipe.Call(uintptr(r.handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
			if ok == 0 {
				r.mu.Unlock()
				if errors.Is(peekErr, windows.ERROR_BROKEN_PIPE) || errors.Is(peekErr, windows.ERROR_PIPE_NOT_CONNECTED) {
					return 0, io.EOF
				}
				return 0, errors.New("cannot inspect redirected token pipe")
			}
			if available == 0 {
				r.mu.Unlock()
				timer := time.NewTimer(20 * time.Millisecond)
				select {
				case <-r.ctx.Done():
					timer.Stop()
					return 0, r.ctx.Err()
				case <-r.done:
					timer.Stop()
					return 0, os.ErrClosed
				case <-timer.C:
				}
				continue
			}
			if uint64(want) > uint64(available) {
				want = int(available)
			}
		}
		var read uint32
		err := windows.ReadFile(r.handle, p[:want], &read, nil)
		r.mu.Unlock()
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_HANDLE_EOF) {
			return int(read), io.EOF
		}
		if err != nil {
			return int(read), errors.New("token input read failed")
		}
		if read == 0 {
			return 0, io.EOF
		}
		return int(read), nil
	}
}
func (r *linearImportReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.done)
		r.mu.Lock()
		defer r.mu.Unlock()
		if windows.CloseHandle(r.handle) != nil {
			r.closeErr = errors.New("token input handle cleanup failed")
		}
	})
	return r.closeErr
}
