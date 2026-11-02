package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const maxIndexBytes = 4 << 10

// IndexSet records that the conversation belongs to the home (an absolute
// state directory). It is written at claim time and on aliasing, and by
// `agent add --home` up front.
func IndexSet(conv, home string) error {
	conv, err := normConv(conv)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") || len(home) > maxIndexBytes {
		return errors.New("the index needs an absolute home path")
	}
	dir, err := IndexDir()
	if err != nil {
		return err
	}
	if err := fsys.MkdirPrivate(dir); err != nil {
		return err
	}
	return fsys.WriteFile(filepath.Join(dir, conv), []byte(home))
}

// IndexGet is the home that owns the conversation. An entry that is missing,
// unreadable or not an absolute path is not found.
func IndexGet(conv string) (home string, ok bool) {
	conv, err := normConv(conv)
	if err != nil {
		return "", false
	}
	dir, err := IndexDir()
	if err != nil {
		return "", false
	}
	data, err := fsys.ReadFile(filepath.Join(dir, conv), maxIndexBytes)
	if err != nil {
		return "", false
	}
	home = string(data)
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") {
		return "", false
	}
	return home, true
}

// IndexDelete removes the entry; one that isn't there is not an error.
func IndexDelete(conv string) error {
	conv, err := normConv(conv)
	if err != nil {
		return err
	}
	dir, err := IndexDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, conv)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
