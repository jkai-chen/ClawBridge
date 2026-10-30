//go:build windows

package ctxwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These tests assert real Windows ACL behaviour, so they run only on
// Windows (CI's windows runners); a cross-compile establishes none of it.

func currentUserSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// securityOf is path's owner, whether its DACL is protected, and its
// entries.
func securityOf(t *testing.T, path string) (string, bool, []aclEntry) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(path, err)
	}
	var entries []aclEntry
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, aclEntry{kind: ace.Header.AceType, flags: ace.Header.AceFlags, sid: (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()})
	}
	return owner.String(), control&windows.SE_DACL_PROTECTED != 0, entries
}

// assertPrivate: owned by this user, with a protected DACL of a single
// allow entry for this user.
func assertPrivate(t *testing.T, path string) {
	t.Helper()
	user := currentUserSID(t)
	owner, protected, entries := securityOf(t, path)
	if owner != user || !protected || len(entries) != 1 || entries[0].kind != aceAllowed || entries[0].sid != user {
		t.Fatalf("%s is not private: owner %s, protected %v, entries %+v", path, owner, protected, entries)
	}
}

// setDACL replaces path's DACL with a protected one from SDDL entries.
func setDACL(t *testing.T, path, entries string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:P" + entries)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func testQueueRecord() QueueRecord {
	return QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "i", Server: "https://x.example.test", MCPPID: 1, AgentPID: 2, UpdatedAt: Now()}
}

func testBinding() Binding {
	return Binding{Agent: ClaudeCode, Transcript: absPath("x", "t.jsonl"), BoundAt: "2026-01-01T10:00:00Z"}
}

// saveAll writes each kind of context record for session agent.
func saveAll(t *testing.T, dir string) []string {
	t.Helper()
	if err := SaveQueueRecord(dir, testQueueRecord()); err != nil {
		t.Fatal(err)
	}
	if err := SaveBinding(dir, "agent", testBinding()); err != nil {
		t.Fatal(err)
	}
	if err := SaveCompactionHold(dir, "agent", CompactionHold{Instance: "i", HeldAt: Now()}); err != nil {
		t.Fatal(err)
	}
	if err := SaveStopMark(dir, "agent", StopMark{Instance: "i", LatestSeq: 7}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, suffix := range []string{".queue.json", ".json", ".compaction-hold.json", ".stop.json"} {
		paths = append(paths, filepath.Join(dir, "agent"+suffix))
	}
	return paths
}

// Every context record, and the context directory ClawBridge creates, is
// owned by this user with a protected DACL that allows only this user.
func TestWindowsRecordsAreWrittenPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	paths := saveAll(t, dir)
	assertPrivate(t, dir)
	for _, p := range paths {
		assertPrivate(t, p)
	}
	if _, ok, err := LoadQueueRecord(dir, "agent"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, ok, err := LoadBinding(dir, "agent"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, ok := LoadCompactionHold(dir, "agent"); !ok {
		t.Fatal("the compaction hold was not read")
	}
	if m, ok := LoadStopMark(dir, "agent"); !ok || m.LatestSeq != 7 {
		t.Fatal("the stop mark was not read", m)
	}
}

// The writers make the temp file private and then rename it: a rename on
// the same volume keeps the security descriptor, rather than taking the
// target directory's inherited ACL as a new file there does.
func TestWindowsRenameKeepsTheDACL(t *testing.T) {
	from := filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(from, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makePrivate(from); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "inherits")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(target, "fresh")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, protected, _ := securityOf(t, fresh); protected {
		t.Fatal("a new file in the target directory has a protected DACL; the test shows nothing")
	}
	to := filepath.Join(target, "record")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, to)
}

// A record whose DACL also lets Everyone or Users in is refused on read,
// whichever kind it is; a writer replaces it, since this user owns it, and
// the new record is private.
func TestWindowsBroadRecordsAreRefusedOnReadAndReplacedOnWrite(t *testing.T) {
	user := currentUserSID(t)
	for _, other := range []string{"WD", "BU"} { // Everyone S-1-1-0, Users S-1-5-32-545
		dir := filepath.Join(t.TempDir(), "context")
		paths := saveAll(t, dir)
		for _, p := range paths {
			setDACL(t, p, "(A;;FA;;;"+user+")(A;;FR;;;"+other+")")
		}
		if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil || !strings.Contains(err.Error(), "the queue record "+paths[0]+" is not private: its ACL lets others") {
			t.Fatalf("%s: queue record: %v %v", other, ok, err)
		}
		if _, ok, err := LoadBinding(dir, "agent"); ok || err == nil || !strings.Contains(err.Error(), "the binding record "+paths[1]+" is not private: its ACL lets others") {
			t.Fatalf("%s: binding: %v %v", other, ok, err)
		}
		if _, ok := LoadCompactionHold(dir, "agent"); ok {
			t.Fatalf("%s: a broad compaction hold was read", other)
		}
		if _, ok := LoadStopMark(dir, "agent"); ok {
			t.Fatalf("%s: a broad stop mark was read", other)
		}
		// The writers replace this user's own records, privately.
		saveAll(t, dir)
		for _, p := range paths {
			assertPrivate(t, p)
		}
		if _, ok, err := LoadQueueRecord(dir, "agent"); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if _, ok, err := LoadBinding(dir, "agent"); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
}

// A broad record renamed in between the path's check and the open is
// refused: the opened file is checked through its handle.
func TestWindowsBroadRecordSwappedInAfterTheStatIsRefused(t *testing.T) {
	user := currentUserSID(t)
	dir := filepath.Join(t.TempDir(), "context")
	if err := SaveQueueRecord(dir, testQueueRecord()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { afterStat = func() {} })
	afterStat = func() {
		afterStat = func() {}
		other := filepath.Join(t.TempDir(), "context")
		if err := SaveQueueRecord(other, testQueueRecord()); err != nil {
			t.Fatal(err)
		}
		swap := filepath.Join(other, "agent.queue.json")
		setDACL(t, swap, "(A;;FA;;;"+user+")(A;;FR;;;WD)")
		if err := os.Rename(swap, filepath.Join(dir, "agent.queue.json")); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil || !strings.Contains(err.Error(), "is not private") {
		t.Fatal("a broad record swapped in after the stat was read", ok, err)
	}
}

// A record created with its directory's default inherited ACL, as before
// records were private by ACL: inside the user profile that ACL allows only
// SYSTEM, Administrators and the user, so the check accepts it and existing
// records keep working. CI runs these tests with TEMP inside the profile.
func TestWindowsDefaultProfileRecordIsAccepted(t *testing.T) {
	dir := t.TempDir()
	profile := os.Getenv("USERPROFILE")
	if profile == "" || !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(profile)+`\`) {
		t.Skipf("the temp directory %s is not inside the user profile %s", dir, profile)
	}
	r := testQueueRecord()
	r.Version = 1
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.queue.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, protected, entries := securityOf(t, path); protected {
		t.Fatalf("the record's ACL is not the inherited default: %+v", entries)
	}
	if _, ok, err := LoadQueueRecord(dir, "agent"); !ok || err != nil {
		t.Fatal("a record with the profile's default ACL was refused", ok, err)
	}
}

// A context directory this user owns with a broader ACL, from before
// records were private by ACL, is made private by the next write.
func TestWindowsOwnBroadContextDirIsMadePrivate(t *testing.T) {
	user := currentUserSID(t)
	dir := filepath.Join(t.TempDir(), "context")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, dir, "(A;OICI;FA;;;"+user+")(A;OICI;FA;;;WD)")
	if err := SaveStopMark(dir, "agent", StopMark{Instance: "i", LatestSeq: 1}); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, dir)
	assertPrivate(t, filepath.Join(dir, "agent.stop.json"))
}
