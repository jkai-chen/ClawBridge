package linearconn

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// This test asserts actual Windows ACL behavior only when run on Windows. A
// cross-compile never establishes these runtime guarantees.
func TestLinearWindowsExistingUnsafeACLIsRefusedWithoutRepair(t *testing.T) {
	d := filepath.Join(t.TempDir(), "linear")
	if err := os.Mkdir(d, 0700); err != nil {
		t.Fatal(err)
	}
	if err := makePrivate(d); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(d, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Directory: d})
	if _, err = m.privateDir(); err == nil {
		t.Fatal("unsafe existing Windows ACL accepted")
	}
	if _, err = m.load(); err == nil {
		t.Fatal("unsafe existing Windows ACL read")
	}
	if err = privatePath(d, true); err == nil {
		t.Fatal("unsafe existing Windows ACL silently repaired")
	}
}
