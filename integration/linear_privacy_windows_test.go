package integration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"unsafe"

	"clawbridge/internal/linearconn"
	"golang.org/x/sys/windows"
)

// Native Windows fixture only. This inspects actual temp-file ACLs with a memory
// credential store; compiling it elsewhere is not ACL/keyring runtime evidence.
func l1WindowsPrivateACL(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(user.User.Sid) {
		return errors.New("owner differs from process user")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("ACL inheritance not protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return errors.New("missing private ACL grants")
	}
	for n := uint32(0); n < uint32(dacl.AceCount); n++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, n, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user.User.Sid) {
			return errors.New("ACL permits another identity")
		}
	}
	return nil
}

func TestL1ProfileWindowsExplicitACLCreateReloadAndRejectBroadFile(t *testing.T) {
	f := newL1OAuth(t, linearconn.Read, "")
	var lockPrivacy error
	f.store.afterSet = func() { lockPrivacy = l1WindowsPrivateACL(filepath.Join(f.dir, ".credentials.lock")) }
	p, err := f.manager.Connect(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if lockPrivacy != nil {
		t.Fatalf("actual profile lock ACL: %v", lockPrivacy)
	}
	index := filepath.Join(f.dir, "profiles.json")
	for _, path := range []string{f.dir, index} {
		if err := l1WindowsPrivateACL(path); err != nil {
			t.Fatalf("actual profile metadata ACL %s: %v", filepath.Base(path), err)
		}
	}
	m := linearconn.New(linearconn.Config{Directory: f.dir, Store: f.store})
	if profiles, err := m.Profiles(); err != nil || len(profiles) != 1 || profiles[0].ID != p.ID {
		t.Fatal("fresh native Windows profile reload failed", err)
	}
	if _, err := m.Status(context.Background(), p.ID); err != nil {
		t.Fatal("explicit-memory credential lifecycle failed on Windows", err)
	}
	gets := f.store.gets
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FR;;;BU)", user.User.Sid.String(), user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(index, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Profiles(); err == nil {
		t.Fatal("profile file with Builtin Users grant accepted")
	}
	if _, err := m.Status(context.Background(), p.ID); err == nil {
		t.Fatal("broad native ACL reached credential store")
	}
	if f.store.gets != gets {
		t.Fatal("broad ACL touched native-profile credential selection")
	}
}
