package linearconn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type importObservedReader struct {
	*io.PipeReader
	entered, exited chan struct{}
	once            sync.Once
}

func (r *importObservedReader) Read(b []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	n, err := r.PipeReader.Read(b)
	if err != nil {
		close(r.exited)
	}
	return n, err
}

type importForbiddenStore struct{ calls atomic.Int32 }

func (s *importForbiddenStore) Get(string, string) (string, error) {
	s.calls.Add(1)
	return "", errors.New("forbidden")
}
func (s *importForbiddenStore) Set(string, string, string) error {
	s.calls.Add(1)
	return errors.New("forbidden")
}
func (s *importForbiddenStore) Delete(string, string) error {
	s.calls.Add(1)
	return errors.New("forbidden")
}
func TestLinearImportCancellationClosesOwnedPipeWithoutCommit(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	observed := &importObservedReader{PipeReader: reader, entered: make(chan struct{}), exited: make(chan struct{})}
	store := new(importForbiddenStore)
	dir := filepath.Join(t.TempDir(), "profiles")
	var verified atomic.Int32
	manager := New(Config{Directory: dir, Store: store, Verify: func(context.Context, *http.Client, Access) (Identity, error) {
		verified.Add(1)
		return Identity{}, errors.New("forbidden")
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := manager.Import(ctx, "cancelled", Read, observed); done <- err }()
	select {
	case <-observed.entered:
	case <-time.After(time.Second):
		t.Fatal("import read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation not returned", err)
		}
	case <-time.After(time.Second):
		reader.Close()
		<-done
		t.Fatal("owned pipe outlived cancellation")
	}
	select {
	case <-observed.exited:
	default:
		t.Fatal("owned read retained after return")
	}
	if store.calls.Load() != 0 || verified.Load() != 0 {
		t.Fatal("cancelled import reached credentials/provider")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("cancelled import initialized profiles", err)
	}
}
