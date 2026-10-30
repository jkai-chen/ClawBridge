package linearconn

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
func checkPrivate(path string, _ os.FileInfo) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(u.User.Sid) {
		return errors.New("Linear profile storage owner mismatch")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("Linear profile storage requires a protected ACL")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("Linear profile storage requires private ACL")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(u.User.Sid) {
			return errors.New("Linear profile storage ACL must allow only this user")
		}
	}
	return nil
}
func syncDirectory(string) error { return nil }

// checkFile refuses a connection file or directory that is a symlink, or
// whose ACL is not this user's alone: POSIX modes don't apply here.
func checkFile(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !dir {
			return errNoFile
		}
		return fmt.Errorf("cannot read the Linear connection's file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return fmt.Errorf("the Linear connection path %s is not a plain file or directory", path)
	}
	if err = checkPrivate(path, info); err != nil {
		return fmt.Errorf("the Linear connection path %s must be private to this user: %w", path, err)
	}
	return nil
}

// openNoFollow is unused on Windows: checkFile refuses a symlink, and the
// opened file is compared with the checked one.
const openNoFollow = 0
