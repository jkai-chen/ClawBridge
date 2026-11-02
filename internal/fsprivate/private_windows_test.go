//go:build windows

package fsprivate

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRenameBlockedCodes(t *testing.T) {
	if !renameBlocked(windows.ERROR_ACCESS_DENIED) || !renameBlocked(windows.ERROR_SHARING_VIOLATION) {
		t.Fatal("a blocked rename is not retried")
	}
	if renameBlocked(windows.ERROR_FILE_NOT_FOUND) {
		t.Fatal("a missing file is retried")
	}
}

func TestOpenReadRefusesAReparsePoint(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "t")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "l")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlink privilege here:", err)
	}
	if f, err := OpenRead(link); err == nil || !IsSymlinkRefusal(err) {
		if f != nil {
			f.Close()
		}
		t.Fatal("opened a reparse point:", err)
	}
	if CheckPrivate(link, false) == nil {
		t.Fatal("a reparse point passed")
	}
}

func TestCheckPrivateDecisions(t *testing.T) {
	d := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(d, true); err != nil {
		t.Fatal("a private directory was refused:", err)
	}
	protected := filepath.Join(d, "protected")
	if err := os.WriteFile(protected, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(protected); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(protected, false); err != nil {
		t.Fatal("a protected private file was refused:", err)
	}
	inherited := filepath.Join(d, "inherited")
	if err := os.WriteFile(inherited, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(inherited, false); err != nil {
		t.Fatal("a file that inherited the owner-only entry was refused:", err)
	}
	f, err := OpenRead(inherited)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := CheckOpened(f, inherited); err != nil {
		t.Fatal("an open inherited file was refused:", err)
	}
}

func TestCheckPrivateRefusesAnotherTrustee(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "shared")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(p); err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(everyone)},
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(p, false); err == nil {
		t.Fatal("a file readable by Everyone passed")
	}
}

// addACE adds an entry for the well-known SID text to path's DACL, leaving
// the DACL in the protection state flag names.
func addACE(t *testing.T, path, sidText string, mode windows.ACCESS_MODE, flag windows.SECURITY_INFORMATION) {
	t.Helper()
	sid, err := windows.StringToSid(sidText)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        mode,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|flag, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPrivateIgnoresADenyEntry(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "denied")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(p); err != nil {
		t.Fatal(err)
	}
	addACE(t, p, "S-1-1-0", windows.DENY_ACCESS, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := CheckPrivate(p, false); err != nil {
		t.Fatal("a deny entry for Everyone made the file not private:", err)
	}
}

func TestCheckPrivateRefusesAnInheritedAdministratorsEntry(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(parent); err != nil {
		t.Fatal(err)
	}
	addACE(t, parent, "S-1-5-32-544", windows.GRANT_ACCESS, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := CheckPrivate(parent, true); err == nil {
		t.Fatal("a directory that allows Administrators passed")
	}
}
