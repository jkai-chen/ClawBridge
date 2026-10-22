package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/linearconn"
)

type l1ObservedImportPipe struct {
	*io.PipeReader
	entered, returned, closed        chan struct{}
	enterOnce, returnOnce, closeOnce sync.Once
}

func (p *l1ObservedImportPipe) Read(b []byte) (int, error) {
	p.enterOnce.Do(func() { close(p.entered) })
	n, err := p.PipeReader.Read(b)
	if err != nil {
		p.returnOnce.Do(func() { close(p.returned) })
	}
	return n, err
}
func (p *l1ObservedImportPipe) Close() error {
	err := p.PipeReader.Close()
	p.closeOnce.Do(func() { close(p.closed) })
	return err
}

func TestL1ImportOwnedPipeCancellationJoinsReadBeforeCommit(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-input", true: "partial-token-without-EOF"}[partial], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "native-personal-profiles")
			store := &l1MemoryCredentials{values: map[string]string{}}
			var verifications atomic.Int64
			m := linearconn.New(linearconn.Config{Directory: dir, Store: store, Verify: func(context.Context, *http.Client, linearconn.Access) (linearconn.Identity, error) {
				verifications.Add(1)
				return linearconn.Identity{}, nil
			}})
			r, w := io.Pipe()
			defer w.Close()
			reader := &l1ObservedImportPipe{PipeReader: r, entered: make(chan struct{}), returned: make(chan struct{}), closed: make(chan struct{})}
			defer reader.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				p, err := m.Import(ctx, "synthetic canceled import", linearconn.Read, reader)
				if p.ID != "" {
					done <- errors.New("canceled import published a profile")
					return
				}
				done <- err
			}()
			select {
			case <-reader.entered:
			case <-time.After(time.Second):
				t.Fatal("owned import reader did not enter")
			}
			if partial {
				if _, err := w.Write([]byte("synthetic-partial-import-token")); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("import cancellation not returned: %v", err)
				}
			case <-time.After(time.Second):
				reader.Close()
				<-done
				t.Fatal("owned blocked import ignored cancellation")
			}
			for name, ch := range map[string]<-chan struct{}{"Read returned": reader.returned, "Close callback completed": reader.closed} {
				select {
				case <-ch:
				case <-time.After(time.Second):
					t.Fatalf("owned-reader cleanup did not join: %s", name)
				}
			}
			if verifications.Load() != 0 || len(store.snapshot()) != 0 || store.sets != 0 || store.gets != 0 {
				t.Fatal("canceled blocked import reached provider/keyring transaction")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("canceled blocked import initialized profile storage")
			}
		})
	}
}

func TestL1ImportUnownedReaderCallerCleanupHasNoDetachedRead(t *testing.T) {
	store := &l1MemoryCredentials{values: map[string]string{}}
	m := linearconn.New(linearconn.Config{Directory: filepath.Join(t.TempDir(), "profiles"), Store: store})
	r, w := io.Pipe()
	defer w.Close()
	defer r.Close()
	observed := &l1ObservedImportPipe{PipeReader: r, entered: make(chan struct{}), returned: make(chan struct{}), closed: make(chan struct{})}
	// Deliberately hide Close: the caller, not Import, must unblock this reader.
	unowned := struct{ io.Reader }{observed}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Import(ctx, "unowned finite seam", linearconn.Read, unowned); done <- err }()
	select {
	case <-observed.entered:
	case <-time.After(time.Second):
		t.Fatal("unowned reader did not enter")
	}
	cancel()
	if err := observed.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "token") {
			t.Fatal("caller-unblocked cancellation was not clean", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cleanup did not settle import")
	}
	select {
	case <-observed.returned:
	default:
		t.Fatal("unowned blocking Read outlived joined Import")
	}
	if len(store.snapshot()) != 0 || store.sets != 0 || store.gets != 0 {
		t.Fatal("canceled unowned reader reached credential transaction")
	}
}
