package notify

import (
	"errors"
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
		return errors.New("notification state owner mismatch")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("notification state requires private ACL")
	}
	entries := make([]aclEntry, 0, dacl.AceCount)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil {
			return err
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
func syncDirectory(string) error { return nil }
