//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func linearInputFlags(t *testing.T, input *os.File) int {
	t.Helper()
	raw, err := input.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var opErr error
	if err = raw.Control(func(fd uintptr) { flags, opErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil || opErr != nil {
		t.Fatal(err, opErr)
	}
	return flags
}
func TestLinearImportReaderBlockingInheritedPipeRestoresFlags(t *testing.T) {
	original, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Fd deliberately changes this fixture to blocking mode, then NewFile
	// reproduces inherited fd0: its Go wrapper has no poller registration.
	fd, err := unix.Dup(int(original.Fd()))
	original.Close()
	if err != nil {
		t.Fatal(err)
	}
	inherited := os.NewFile(uintptr(fd), "inherited-stdin")
	if inherited == nil {
		unix.Close(fd)
		t.Fatal("fixture NewFile failed")
	}
	defer inherited.Close()
	before := linearInputFlags(t, inherited)
	if before&unix.O_NONBLOCK != 0 {
		t.Fatal("fixture did not reproduce blocking inherited stdin")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, cleanup, err := newLinearImportReader(ctx, inherited)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if flags := linearInputFlags(t, inherited); flags&unix.O_NONBLOCK == 0 {
		t.Fatal("owned reader did not make shared description nonblocking")
	}
	owned := r.(*linearImportReader)
	flags, err := unix.FcntlInt(uintptr(owned.fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("owned duplicate missing CLOEXEC", err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(r); done <- err }()
	// Let the read reach its actual bounded poll before cancellation. The pipe
	// writer deliberately remains open throughout cancellation and cleanup.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking inherited stdin did not cancel")
	}
	cleanup()
	if after := linearInputFlags(t, inherited); after != before {
		t.Fatalf("input flags changed: before=%#x after=%#x", before, after)
	}
	if _, err = unix.FcntlInt(uintptr(owned.fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatal("owned duplicate leaked", err)
	}
}
