//go:build windows

package ctxwatch

import "os"

// On Windows the transcript lives in the user's profile; the regular-file,
// no-link and same-file checks still apply. Context records are checked by
// ACL (private_windows.go).
func ownedByUser(os.FileInfo) error { return nil }

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
