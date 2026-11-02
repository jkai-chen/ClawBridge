package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndex(t *testing.T) {
	testConfig(t)
	home := filepath.Join(t.TempDir(), "home-a")
	if _, ok := IndexGet(convID(1)); ok {
		t.Fatal("empty index")
	}
	if err := IndexSet(convID(1), home); err != nil {
		t.Fatal(err)
	}
	if got, ok := IndexGet(convID(1)); !ok || got != home {
		t.Errorf("IndexGet = %q %v", got, ok)
	}
	dir, _ := IndexDir()
	if fi, err := os.Stat(filepath.Join(dir, convID(1))); err != nil || runtimeMode(fi) != 0o600 {
		t.Errorf("index file: %v %v", fi, err)
	}
	if err := IndexDelete(convID(1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := IndexGet(convID(1)); ok {
		t.Error("deleted entry still found")
	}
	if err := IndexDelete(convID(1)); err != nil {
		t.Errorf("deleting a missing entry: %v", err)
	}
}

func TestIndexValidation(t *testing.T) {
	testConfig(t)
	abs := t.TempDir()
	for _, conv := range []string{"", "short", "../../etc/passwd", "ABCDEFGH-1234", "abc/defgh12", "abcdefgh\x00", string(make([]byte, 65))} {
		if IndexSet(conv, abs) == nil {
			t.Errorf("IndexSet(%q) should fail", conv)
		}
		if _, ok := IndexGet(conv); ok {
			t.Errorf("IndexGet(%q) should not find", conv)
		}
		if IndexDelete(conv) == nil {
			t.Errorf("IndexDelete(%q) should fail", conv)
		}
	}
	for _, home := range []string{"", "relative/dir", "./x", "a\nb", abs + "\x00"} {
		if IndexSet(convID(1), home) == nil {
			t.Errorf("IndexSet home %q should fail", home)
		}
	}
	// A hand-made entry with a relative path is not trusted.
	dir, _ := IndexDir()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, convID(2)), []byte("relative"), 0o600)
	if _, ok := IndexGet(convID(2)); ok {
		t.Error("a relative path in the index must be refused")
	}
}

// C1 at library level: two homes, one folder, two conversations. Each
// conversation finds its own home and neither record is touched by the other.
func TestIndexTwoHomesOneFolder(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	homeA, homeB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	dirA, _ := RecordsDir(homeA, "ws", "proj")
	dirB, _ := RecordsDir(homeB, "ws", "proj")
	pa, pb := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	convA, convB := convID(1), convID(2)

	if _, err := Claim(dirA, "web", convA, pa, "inst-a", Hint{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(dirB, "web", convB, pb, "inst-b", Hint{}); err != nil {
		t.Fatal(err)
	}
	IndexSet(convA, homeA)
	IndexSet(convB, homeB)

	// After a relaunch with no environment: conversation -> home -> record.
	for conv, want := range map[string]struct{ home, inst string }{convA: {homeA, "inst-a"}, convB: {homeB, "inst-b"}} {
		home, ok := IndexGet(conv)
		if !ok || home != want.home {
			t.Fatalf("index for %s = %q", conv, home)
		}
		dir, _ := RecordsDir(home, "ws", "proj")
		r, ok := FindByConversation(dir, conv)
		if !ok || r.Instance != want.inst {
			t.Errorf("record for %s = %+v", conv, r)
		}
	}
	if _, ok := FindByConversation(dirA, convB); ok {
		t.Error("home A must not know conversation B")
	}
	if _, ok := FindByConversation(dirB, convA); ok {
		t.Error("home B must not know conversation A")
	}
}
