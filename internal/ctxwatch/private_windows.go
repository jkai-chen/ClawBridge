package ctxwatch

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no file modes: a context record is private by its owner and
// DACL. Every record is written private (makePrivate on the temp file, before
// the rename, which keeps the security descriptor on the same volume), and
// every read checks the owner and DACL (privateOwner, privateACL), so a
// record another local user planted in a state home outside the profile is
// never acted on. The user profile's own ACL passes, so records written
// before this check keep working.

// makePrivate makes path owned by this user, with a protected DACL that
// allows only this user (inherited by what a directory then holds).
func makePrivate(path string) error {
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

var unreadableSecurity = &notPrivateError{"its owner and ACL could not be read; delete it"}

// privateToUser checks path's owner and DACL.
func privateToUser(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return unreadableSecurity
	}
	return privateSecurity(sd, true)
}

// openedPrivate checks the opened file's owner and DACL through its handle
// (os.Open's GENERIC_READ includes READ_CONTROL), so a file renamed into
// path after the path's check is checked as the one actually read.
func openedPrivate(f *os.File, _ string, _ os.FileInfo) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return unreadableSecurity
	}
	return privateSecurity(sd, true)
}

// recordReplaceable and bindingReplaceable say whether a writer may replace
// the existing record at path. A record owned by this user (or, for an
// elevated administrator, by the Administrators group) is replaced whatever
// its ACL, and the new one is private: that is a record written before
// records were made private by ACL. A record owned by anyone else, or by the
// Administrators group when this process is not elevated, is refused, not
// replaced: it is not this user's to overwrite.
func recordReplaceable(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return unreadableSecurity
	}
	return privateSecurity(sd, false)
}

func bindingReplaceable(path string, info os.FileInfo) error { return recordReplaceable(path, info) }

// privateSecurity checks sd's owner and, when withACL, its DACL.
func privateSecurity(sd *windows.SECURITY_DESCRIPTOR, withACL bool) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return unreadableSecurity
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return unreadableSecurity
	}
	elevated := false
	if admins, e := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); e == nil && owner.Equals(admins) {
		// IsMember counts only an enabled group: an elevated administrator,
		// not a filtered (non-elevated) token where the group is deny-only.
		elevated, _ = windows.Token(0).IsMember(admins)
	}
	if err = privateOwner(owner.String(), u.User.Sid.String(), elevated); err != nil || !withACL {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		// No DACL at all grants everyone everything.
		return &notPrivateError{"it has no ACL, which lets everyone in; delete it"}
	}
	entries := make([]aclEntry, 0, dacl.AceCount)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil {
			return unreadableSecurity
		}
		entry := aclEntry{kind: ace.Header.AceType, flags: ace.Header.AceFlags}
		// Allow and deny entries share this layout; the SID of any other
		// entry type is never read, and privateACL refuses the entry.
		if entry.kind == aceAllowed || entry.kind == aceDenied {
			entry.sid = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		entries = append(entries, entry)
	}
	return privateACL(entries, u.User.Sid.String())
}

// MakeContextDir creates the context directory dir, private to the user. An existing one that is
// already private (the user profile's ACL passes) is left as it is; one this
// user owns with a broader ACL, from before records were private by ACL, is
// made private; one owned by anyone else is refused.
func MakeContextDir(dir string) error {
	_, err := os.Lstat(dir)
	created := errors.Is(err, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the context directory %s could not be created", dir)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("the context directory %s is not a directory", dir)
	}
	if !created {
		if privateToUser(dir, info) == nil {
			return nil
		}
		if err := recordReplaceable(dir, info); err != nil {
			return privacyError("context directory", dir, err, "%s %s")
		}
	}
	// Made private before any record is written in it, so every file created
	// there inherits only this user's access from the start.
	if err := makePrivate(dir); err != nil {
		return fmt.Errorf("the context directory %s could not be made private", dir)
	}
	return nil
}
