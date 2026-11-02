//go:build windows

package fsprivate

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MakePrivate makes path owned by this user, with a protected DACL that
// allows only this user (inherited by what a directory then holds).
func MakePrivate(path string) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + u.User.Sid.String() + "D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, u.User.Sid, nil, dacl, nil)
}

// CheckPrivate refuses path unless it is a plain file or directory (no
// symlink, junction or other reparse point) owned by this user whose DACL
// allows only this user.
func CheckPrivate(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if reparsePoint(info) || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) {
		return fmt.Errorf("%s is not a plain file or directory", path)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return privateSecurity(sd, u.User.Sid, path)
}

// privateSecurity accepts a descriptor owned by sid whose DACL, inherited
// or explicit, allows only sid (deny entries are ignored). The protected flag is not required: a file
// restored from a backup into a private directory inherits the owner-only
// entry and is just as private.
func privateSecurity(sd *windows.SECURITY_DESCRIPTOR, sid *windows.SID, path string) error {
	owner, _, err := sd.Owner()
	if err != nil || !(owner.Equals(sid) || ownedByTokenOwner(owner)) {
		return errors.New("the owner of " + path + " is not this user")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New(path + " needs a private ACL")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		// A deny entry only removes access, so it is harmless. Any other
		// kind of entry (an allow for another trustee, or one this check
		// does not understand) makes the path not private.
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
				continue
			}
		}
		return errors.New("the ACL of " + path + " must allow only this user")
	}
	return nil
}

// ownedByTokenOwner reports whether owner is this process's default owner,
// which is what a file it creates is owned by. For an elevated
// administrator that is the Administrators group, not the user's own SID, so
// a file this user just wrote or restored is owned that way. The DACL, which
// is checked as well, still has to allow only the user.
func ownedByTokenOwner(owner *windows.SID) bool {
	var n uint32
	t := windows.GetCurrentProcessToken()
	_ = windows.GetTokenInformation(t, windows.TokenOwner, nil, 0, &n)
	if n == 0 {
		return false
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(t, windows.TokenOwner, &buf[0], n, &n); err != nil {
		return false
	}
	return (*(**windows.SID)(unsafe.Pointer(&buf[0]))).Equals(owner)
}

// CheckOpened is CheckPrivate for a file already open: its owner and DACL
// are read through the handle.
func CheckOpened(f *os.File, path string) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return privateSecurity(sd, u.User.Sid, path)
}

// CheckOpenedLinks is CheckOpened: Windows has no link count to relax.
func CheckOpenedLinks(f *os.File, path string, _ int) error { return CheckOpened(f, path) }

// SyncDir does nothing: Windows has no directory sync, and the rename is
// journaled.
func SyncDir(string) error { return nil }

// errReparse is the refusal of a symlink, junction or other reparse point.
var errReparse = errors.New("is a reparse point (symlink or junction)")

// reparsePoint reports whether info is a reparse point of any kind.
func reparsePoint(info os.FileInfo) bool {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// open opens path with share mode READ|DELETE (so a writer can rename over
// it while it is open) and without following a reparse point, then refuses
// one by its attributes.
func open(path string, flags uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var fi windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &fi); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errReparse}
	}
	return os.NewFile(uintptr(h), path), nil
}

// OpenRead opens a file for reading and refuses a reparse point.
func OpenRead(path string) (*os.File, error) { return open(path, 0) }

// OpenDir opens a directory for listing and refuses a reparse point.
func OpenDir(path string) (*os.File, error) { return open(path, windows.FILE_FLAG_BACKUP_SEMANTICS) }

// IsSymlinkRefusal reports whether err is the open of a reparse point
// refused.
func IsSymlinkRefusal(err error) bool { return errors.Is(err, errReparse) }

// renameBlocked reports whether a rename failed because another process
// has the target open, which clears when it closes.
func renameBlocked(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// renameNoReplace moves oldpath to newpath only if newpath does not exist:
// MoveFileEx without MOVEFILE_REPLACE_EXISTING fails if it does.
func renameNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	err = renameRetry(func() error { return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH) }, renameBlocked, time.Sleep)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return os.ErrExist
	}
	return err
}
