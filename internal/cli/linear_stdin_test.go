package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLinearImportReaderOwnedPipeCancellation(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, cleanup, err := newLinearImportReader(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(reader); done <- err }()
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled read required writer EOF")
	}
	cleanup()
	if _, err = r.Stat(); err != nil {
		t.Fatal("borrowed original input closed", err)
	}
	if _, err = w.Write([]byte("original-remains-usable")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len("original-remains-usable"))
	if _, err = io.ReadFull(r, b); err != nil || string(b) != "original-remains-usable" {
		t.Fatal("original pipe damaged", err)
	}
}

func TestLinearImportReaderConcurrentManualCloseAndCleanup(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	reader, cleanup, err := newLinearImportReader(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(reader); done <- err }()
	closed := make(chan error, 1)
	go func() { closed <- reader.Close() }()
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("manual Close blocked with pipe writer open")
	}
	select {
	case err = <-done:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not join after manual Close")
	}
	cleanup()
	cleanup()
}

func TestLinearImportReaderBoundedFiniteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("synthetic-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, cleanup, err := newLinearImportReader(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	b, err := io.ReadAll(io.LimitReader(r, 16385))
	if err != nil || string(b) != "synthetic-token\n" {
		t.Fatal(string(b), err)
	}
	cleanup()
	if _, err = f.Stat(); err != nil {
		t.Fatal("borrowed file closed", err)
	}
}
