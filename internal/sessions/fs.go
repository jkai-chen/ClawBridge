package sessions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// privateFS is the few file operations this package needs, all private to
// the user: 0700 directories, 0600 files, atomic replacement, no symlink
// following. localFS is the implementation here.
//
// TODO: switch to internal/fsprivate once key storage (PR #433) merges, and
// delete localFS.
type privateFS interface {
	// MkdirPrivate creates dir (and parents) 0700 and tightens an existing
	// dir that is looser. A symlink is refused.
	MkdirPrivate(dir string) error
	// WriteFile replaces path atomically with data, 0600.
	WriteFile(path string, data []byte) error
	// CreateExclusive creates path 0600 holding data, failing with
	// os.ErrExist when it is there.
	CreateExclusive(path string, data []byte) error
	// ReadFile reads a regular file of at most max bytes without following a
	// symlink; a larger one is ErrTooLarge.
	ReadFile(path string, max int64) ([]byte, error)
}

// ErrTooLarge is a file over its size cap.
var ErrTooLarge = errors.New("file is over the size limit")

// fsys is the file layer; tests could replace it.
var fsys privateFS = localFS{}

type localFS struct{}

func (localFS) MkdirPrivate(dir string) error {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return tightenDir(dir)
}

func (localFS) WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := renameRetry(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

func (localFS) CreateExclusive(path string, data []byte) error {
	f, err := openExclusive(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
	}
	return werr
}

func (localFS) ReadFile(path string, max int64) ([]byte, error) {
	f, err := openRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > max {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Bounds of the retry for a transient Windows file error.
var (
	retryTries = 10
	retryEvery = 100 * time.Millisecond
)

// retry runs op again, up to about a second, while it fails with a transient
// error (see transient). Any other result is final.
func retry(op func() error) error {
	err := op()
	for i := 1; i < retryTries && err != nil && transient(err); i++ {
		time.Sleep(retryEvery)
		err = op()
	}
	return err
}

func renameRetry(from, to string) error {
	return retry(func() error { return os.Rename(from, to) })
}

// removeRetry removes path; a file that is already gone is not an error.
func removeRetry(path string) error {
	err := retry(func() error { return os.Remove(path) })
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
