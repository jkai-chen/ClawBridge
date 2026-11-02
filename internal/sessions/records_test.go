package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaimNewAndInstance(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a := f.add(fakeBase+1, 10)
	r, err := Claim(dir, "web", convID(1), a, "inst-1", Hint{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "web" || r.Instance != "inst-1" || r.Owner == nil || *r.Owner != a ||
		len(r.Conversations) != 1 || r.Workspace != "ws" || r.Project != "proj" || r.Created.IsZero() {
		t.Fatalf("new record = %+v", r)
	}
	fi, err := os.Stat(filepath.Join(dir, "web.json"))
	if err != nil || runtimeMode(fi) != 0o600 {
		t.Errorf("record file: %v %v", fi, err)
	}
	// The same agent again is fine; the stored instance is never overwritten.
	r, err = Claim(dir, "web", convID(1), a, "inst-OTHER", Hint{})
	if err != nil || r.Instance != "inst-1" || len(r.Conversations) != 1 {
		t.Errorf("reclaim = %+v, %v", r, err)
	}
	// A record made with no instance takes one later, once.
	if _, err := Claim(dir, "api", "", a, "", Hint{}); err != nil {
		t.Fatal(err)
	}
	r, _ = Claim(dir, "api", "", a, "inst-2", Hint{})
	if r.Instance != "inst-2" {
		t.Errorf("empty instance not filled: %+v", r)
	}
	r, _ = Claim(dir, "api", "", a, "inst-3", Hint{})
	if r.Instance != "inst-2" {
		t.Errorf("instance replaced: %+v", r)
	}
}

func TestClaimOwnership(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	if _, err := Claim(dir, "web", convID(1), a, "i", Hint{}); err != nil {
		t.Fatal(err)
	}
	// A live other owner: refused.
	_, err := Claim(dir, "web", convID(1), b, "i", Hint{})
	var inUse ErrInUse
	if !errors.As(err, &inUse) || inUse.PID != a.PID {
		t.Fatalf("live owner: %v", err)
	}
	// The owner's PID reused by another process: the old owner is dead.
	f.restart(a.PID, 11)
	r, err := Claim(dir, "web", convID(2), b, "i", Hint{AgentStartName: "web"})
	if err != nil || *r.Owner != b || len(r.Conversations) != 2 {
		t.Fatalf("adopt after PID reuse: %+v %v", r, err)
	}
	// A dead owner is adopted too.
	f.kill(b.PID)
	c := f.add(fakeBase+3, 30)
	if r, err := Claim(dir, "web", convID(2), c, "i", Hint{}); err != nil || *r.Owner != c {
		t.Fatalf("adopt dead owner: %+v %v", r, err)
	}
}

func TestClaimConversationTaken(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	if _, err := Claim(dir, "web", convID(1), a, "i", Hint{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(dir, "other", convID(1), b, "i", Hint{}); !errors.Is(err, ErrConversationTaken) {
		t.Errorf("F8: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.json")); err == nil {
		t.Error("a refused claim must not create the record")
	}
	// Adopting a name with a conversation listed elsewhere is refused too.
	f.kill(a.PID)
	if _, err := Claim(dir, "other", convID(2), b, "i", Hint{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(dir, "other", convID(1), b, "i", Hint{}); !errors.Is(err, ErrConversationTaken) {
		t.Errorf("F8 on an existing record: %v", err)
	}
	if r, ok := FindByConversation(dir, convID(1)); !ok || r.Name != "web" {
		t.Errorf("FindByConversation = %+v %v", r, ok)
	}
	if _, ok := FindByConversation(dir, convID(9)); ok {
		t.Error("unknown conversation found")
	}
}

func TestAlias(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	Claim(dir, "api", convID(5), b, "i", Hint{})

	if err := Alias(dir, "web", convID(2), b); !errors.Is(err, ErrNotOwner) {
		t.Errorf("not the owner: %v", err)
	}
	if err := Alias(dir, "web", convID(5), a); !errors.Is(err, ErrConversationTaken) {
		t.Errorf("conversation of another record: %v", err)
	}
	if err := Alias(dir, "web", convID(2), a); err != nil {
		t.Fatal(err)
	}
	if err := Alias(dir, "web", convID(2), a); err != nil {
		t.Errorf("aliasing twice: %v", err)
	}
	r, _ := Get(dir, "web")
	if len(r.Conversations) != 2 || r.Conversations[1] != convID(2) {
		t.Errorf("conversations = %v", r.Conversations)
	}
	if err := Alias(dir, "nope", convID(3), a); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing record: %v", err)
	}
	// An owner that has died can no longer alias.
	f.kill(a.PID)
	if err := Alias(dir, "web", convID(3), a); !errors.Is(err, ErrNotOwner) {
		t.Errorf("dead owner: %v", err)
	}
}

func TestTakeoverAndRelease(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	if err := Release(dir, "web", b); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(dir, "web"); r.Owner == nil {
		t.Error("Release by a non-owner must keep the owner")
	}
	if err := Takeover(dir, "web", b); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(dir, "web"); r.Owner != nil || r.Instance != "i" {
		t.Errorf("Takeover left %+v", r)
	}
	if _, err := Claim(dir, "web", convID(1), b, "i", Hint{}); err != nil {
		t.Errorf("claim after takeover: %v", err)
	}
	if err := Release(dir, "web", b); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(dir, "web"); r.Owner != nil {
		t.Error("Release by the owner must clear it")
	}
	if err := Takeover(dir, "nope", b); !errors.Is(err, ErrNotFound) {
		t.Errorf("takeover of a missing record: %v", err)
	}
}

func TestClaimByNameNoLink(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)

	// New record, PID only.
	r, err := ClaimByNameNoLink(dir, "pidonly", a, Hint{})
	if err != nil || r.Owner == nil || len(r.Conversations) != 0 {
		t.Fatalf("new: %+v %v", r, err)
	}
	// A live owner is not displaced.
	var inUse ErrInUse
	if _, err := ClaimByNameNoLink(dir, "pidonly", b, Hint{}); !errors.As(err, &inUse) {
		t.Errorf("live owner: %v", err)
	}
	// Once the owner is dead a PID-only record is adoptable with no hint.
	f.kill(a.PID)
	if r, err := ClaimByNameNoLink(dir, "pidonly", b, Hint{}); err != nil || *r.Owner != b {
		t.Errorf("PID-only adopt: %+v %v", r, err)
	}

	// A record with conversations needs a matching hint.
	Claim(dir, "linked", convID(1), b, "i", Hint{})
	Release(dir, "linked", b)
	c := f.add(fakeBase+3, 30)
	if _, err := ClaimByNameNoLink(dir, "linked", c, Hint{}); !errors.Is(err, ErrBelongsToAnother) {
		t.Errorf("no hint: %v", err)
	}
	if _, err := ClaimByNameNoLink(dir, "linked", c, Hint{AgentStartName: "else", CodexThread: "t-1"}); !errors.Is(err, ErrBelongsToAnother) {
		t.Errorf("wrong hint: %v", err)
	}
	if r, err := ClaimByNameNoLink(dir, "linked", c, Hint{AgentStartName: "linked"}); err != nil || *r.Owner != c {
		t.Errorf("start-name hint: %+v %v", r, err)
	}
	// The Codex thread hint.
	Release(dir, "linked", c)
	if _, err := Update(dir, "linked", func(r *Record) { r.CodexThread = "thread-1" }); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimByNameNoLink(dir, "linked", c, Hint{CodexThread: "thread-2"}); !errors.Is(err, ErrBelongsToAnother) {
		t.Errorf("other thread: %v", err)
	}
	if r, err := ClaimByNameNoLink(dir, "linked", c, Hint{CodexThread: "thread-1"}); err != nil || *r.Owner != c {
		t.Errorf("thread hint: %+v %v", r, err)
	}
	// An empty thread on both sides is not a match.
	Update(dir, "linked", func(r *Record) { r.CodexThread = "" })
	Release(dir, "linked", c)
	if _, err := ClaimByNameNoLink(dir, "linked", c, Hint{}); !errors.Is(err, ErrBelongsToAnother) {
		t.Errorf("empty thread matched: %v", err)
	}
}

func TestUpdateGuards(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a := f.add(fakeBase+1, 10)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	Claim(dir, "api", convID(2), a, "j", Hint{})
	r, err := Update(dir, "web", func(r *Record) {
		r.CWD, r.Channel, r.Launch = "/work", true, []string{"claude", "--resume"}
		r.Name, r.Owner = "hijack", nil
	})
	if err != nil || r.Name != "web" || r.Owner == nil || r.CWD != "/work" || !r.Channel {
		t.Fatalf("Update = %+v %v", r, err)
	}
	if _, err := Update(dir, "web", func(r *Record) { r.Instance = "x" }); !errors.Is(err, ErrInstanceSet) {
		t.Errorf("instance replace: %v", err)
	}
	if _, err := Update(dir, "web", func(r *Record) { r.Conversations = append(r.Conversations, convID(2)) }); !errors.Is(err, ErrConversationTaken) {
		t.Errorf("F8 in Update: %v", err)
	}
	if _, err := Update(dir, "nope", func(*Record) {}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestListDelete(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a := f.add(fakeBase+1, 10)
	if recs, err := List(dir); err != nil || len(recs) != 0 {
		t.Errorf("empty List = %v %v", recs, err)
	}
	Claim(dir, "b", "", a, "i", Hint{})
	Claim(dir, "a", "", a, "i", Hint{})
	recs, err := List(dir)
	if err != nil || len(recs) != 2 || recs[0].Name != "a" {
		t.Fatalf("List = %v %v", recs, err)
	}
	if err := Delete(dir, "a"); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "a"); err != nil {
		t.Errorf("deleting again: %v", err)
	}
	if recs, _ := List(dir); len(recs) != 1 {
		t.Errorf("after delete: %v", recs)
	}
	if Delete(dir, "../x") == nil {
		t.Error("Delete with a path name must fail")
	}
}

func TestRecordStrictDecodeAndLimits(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a := f.add(fakeBase+1, 10)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	path := filepath.Join(dir, "web.json")
	good, _ := os.ReadFile(path)

	bad := map[string]string{
		"unknown field": strings.Replace(string(good), `"name"`, `"extra":1,"name"`, 1),
		"wrong version": strings.Replace(string(good), `"v":1`, `"v":2`, 1),
		"no version":    strings.Replace(string(good), `"v":1,`, ``, 1),
		"trailing data": string(good) + `{}`,
		"wrong name":    strings.Replace(string(good), `"name":"web"`, `"name":"zzz"`, 1),
		"bad conv":      strings.Replace(string(good), convID(1), `../../x`, 1),
	}
	for name, content := range bad {
		os.WriteFile(path, []byte(content), 0o600)
		if _, err := Get(dir, "web"); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Get = %v", name, err)
		}
		// F8 must not pass over a record it can't read.
		if _, err := Claim(dir, "other", convID(9), a, "i", Hint{}); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Claim beside it = %v", name, err)
		}
	}
	// Over the size cap, on read and on write.
	os.WriteFile(path, []byte(strings.Repeat(" ", maxRecordBytes+1)), 0o600)
	if _, err := Get(dir, "web"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("big file: %v", err)
	}
	os.WriteFile(path, good, 0o600)
	_, err := Update(dir, "web", func(r *Record) { r.Launch = []string{strings.Repeat("x", maxRecordBytes)} })
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("big record write: %v", err)
	}
	// A symlinked record is not followed.
	if runtimeMode(mustStat(t, dir)) == 0o700 && os.Symlink("web.json", filepath.Join(dir, "ln.json")) == nil {
		if _, err := Get(dir, "ln"); err == nil {
			t.Error("a symlinked record must be refused")
		}
		os.Remove(filepath.Join(dir, "ln.json"))
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestPathSegments(t *testing.T) {
	state := t.TempDir()
	for _, bad := range []string{"", "..", ".", "a/b", `a\b`, "a\x00b", ".hidden", "-x", strings.Repeat("a", 129), "a b", "é"} {
		if _, err := RecordsDir(state, bad, "p"); err == nil {
			t.Errorf("workspace %q accepted", bad)
		}
		if _, err := RecordsDir(state, "w", bad); err == nil {
			t.Errorf("project %q accepted", bad)
		}
		if _, err := Get(t.TempDir(), bad); err == nil {
			t.Errorf("session name %q accepted", bad)
		}
	}
	if _, err := RecordsDir("", "w", "p"); err == nil {
		t.Error("empty state dir accepted")
	}
	got, err := RecordsDir(state, "ws-1", "my.proj_2")
	if err != nil || got != filepath.Join(state, "sessions", "ws-1", "my.proj_2") {
		t.Errorf("RecordsDir = %q %v", got, err)
	}
}

func TestLockStale(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	os.MkdirAll(dir, 0o700)
	lock := filepath.Join(dir, ".lock")
	ran := func() bool {
		ok := false
		if err := WithProjectLock(dir, func() error { ok = true; return nil }); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// A holder that is gone.
	os.WriteFile(lock, []byte(`{"pid":`+itoa(fakeBase+50)+`,"start":5,"at":`+itoa(int(time.Now().UnixNano()))+`,"token":"x"}`), 0o600)
	if !ran() {
		t.Error("lock of a dead holder not taken")
	}
	// A live holder is never robbed, however old its lock.
	h := f.add(fakeBase+51, 6)
	oldWait := lockWait
	lockWait = 80 * time.Millisecond
	defer func() { lockWait = oldWait }()
	old := time.Now().Add(-time.Hour).UnixNano()
	os.WriteFile(lock, []byte(`{"pid":`+itoa(h.PID)+`,"start":6,"at":`+itoa(int(old))+`,"token":"x"}`), 0o600)
	if err := WithProjectLock(dir, func() error { t.Error("ran under an old live lock"); return nil }); !errors.Is(err, ErrLockTimeout) || !strings.Contains(err.Error(), itoa(h.PID)) {
		t.Errorf("old live lock: %v", err)
	}
	// A holder that can't be checked is not robbed either.
	f.mu.Lock()
	f.unknown[h.PID] = true
	f.mu.Unlock()
	if err := WithProjectLock(dir, func() error { t.Error("ran under an unknown-holder lock"); return nil }); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("unknown holder: %v", err)
	}
	f.mu.Lock()
	delete(f.unknown, h.PID)
	f.mu.Unlock()
	// The same lock is broken once its holder is marked gone, or has another
	// start time.
	f.kill(h.PID)
	if !ran() {
		t.Error("lock of a holder marked gone not taken")
	}
	h = f.add(fakeBase+51, 7)
	os.WriteFile(lock, []byte(`{"pid":`+itoa(h.PID)+`,"start":6,"at":`+itoa(int(old))+`,"token":"x"}`), 0o600)
	if !ran() {
		t.Error("lock of a PID reused with another start time not taken")
	}
	// A live, fresh holder is waited for, then times out.
	os.WriteFile(lock, []byte(`{"pid":`+itoa(h.PID)+`,"start":7,"at":`+itoa(int(time.Now().UnixNano()))+`,"token":"x"}`), 0o600)
	if err := WithProjectLock(dir, func() error { t.Error("ran under a live lock"); return nil }); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("live lock: %v", err)
	}
	// An unparseable lock file is stale by its age.
	os.WriteFile(lock, []byte(`garbage`), 0o600)
	if err := WithProjectLock(dir, func() error { return nil }); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("fresh garbage lock: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	os.Chtimes(lock, past, past)
	if !ran() {
		t.Error("old garbage lock not taken")
	}
	// The lock is gone after use, and a lock we lost isn't removed by us.
	if _, err := os.Stat(lock); err == nil {
		t.Error("lock left behind")
	}
	WithProjectLock(dir, func() error {
		os.WriteFile(lock, []byte(`{"pid":1,"start":1,"at":1,"token":"someone else"}`), 0o600)
		return nil
	})
	if _, err := os.Stat(lock); err != nil {
		t.Error("released a lock that was not ours")
	}
}

func itoa(n int) string { return formatInt(n) }

// Concurrent claims of one name: exactly one wins (C3).
func TestClaimConcurrentGoroutines(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	const n = 20
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		agent := f.add(fakeBase+100+i, int64(1000+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = Claim(dir, "web", convID(100+i), agent, "inst", Hint{})
		}()
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range results {
		var inUse ErrInUse
		switch {
		case err == nil:
			wins++
		case errors.As(err, &inUse):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d claims won, want exactly 1", wins)
	}
	r, err := Get(dir, "web")
	if err != nil || len(r.Conversations) != 1 || r.Owner == nil {
		t.Errorf("record = %+v %v", r, err)
	}
}

// A claim with no conversation can't take a record that lists conversations,
// even from a dead owner; ClaimByNameNoLink decides those.
func TestClaimEmptyConvRefusedOnLinkedRecord(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	f.kill(a.PID)
	if _, err := Claim(dir, "web", "", b, "i", Hint{}); !errors.Is(err, ErrBelongsToAnother) {
		t.Fatalf("Claim(conv empty) on a linked record = %v", err)
	}
	if r, _ := Get(dir, "web"); *r.Owner != a {
		t.Error("the refused claim changed the owner")
	}
	// With a conversation it still adopts.
	if _, err := Claim(dir, "web", convID(1), b, "i", Hint{}); err != nil {
		t.Errorf("claim with a conversation: %v", err)
	}
}

func TestConversationCaseNormalised(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a := f.add(fakeBase+1, 10)
	upper := "ABCDEF01-2345-6789-ABCD-EF0123456789"
	lower := strings.ToLower(upper)
	r, err := Claim(dir, "web", upper, a, "i", Hint{})
	if err != nil || len(r.Conversations) != 1 || r.Conversations[0] != lower {
		t.Fatalf("Claim = %+v %v", r, err)
	}
	for _, c := range []string{upper, lower} {
		if got, ok := FindByConversation(dir, c); !ok || got.Name != "web" {
			t.Errorf("FindByConversation(%s) = %v", c, ok)
		}
	}
	b := f.add(fakeBase+2, 20)
	if _, err := Claim(dir, "other", upper, b, "i", Hint{}); !errors.Is(err, ErrConversationTaken) {
		t.Errorf("F8 across case: %v", err)
	}
	if err := Alias(dir, "web", "ABCDEF01-0000-0000-0000-000000000001", a); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(dir, "web"); r.Conversations[1] != "abcdef01-0000-0000-0000-000000000001" {
		t.Errorf("alias not lowercased: %v", r.Conversations)
	}
	// Index and link.
	home := t.TempDir()
	if err := IndexSet(upper, home); err != nil {
		t.Fatal(err)
	}
	if h, ok := IndexGet(lower); !ok || h != home {
		t.Error("index: upper case set, lower case get")
	}
	if h, ok := IndexGet(upper); !ok || h != home {
		t.Error("index: upper case get")
	}
	if err := IndexDelete(upper); err != nil {
		t.Fatal(err)
	}
	if _, ok := IndexGet(lower); ok {
		t.Error("index entry not deleted")
	}
	if err := WriteLink(sampleLink(a, upper)); err != nil {
		t.Fatal(err)
	}
	if l, err := ReadLink(a); err != nil || l.ConversationID != lower {
		t.Errorf("link = %+v %v", l, err)
	}
}

func TestNameCaseCollision(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a := f.add(fakeBase+1, 10)
	if _, err := Claim(dir, "web", "", a, "i", Hint{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(dir, "Web", "", a, "i", Hint{}); !errors.Is(err, ErrNameCaseCollision) {
		t.Errorf("Claim Web: %v", err)
	}
	if _, err := ClaimByNameNoLink(dir, "WEB", a, Hint{}); !errors.Is(err, ErrNameCaseCollision) {
		t.Errorf("ClaimByNameNoLink WEB: %v", err)
	}
	// The exact name is still the existing record.
	if _, err := Claim(dir, "web", "", a, "i", Hint{}); err != nil {
		t.Errorf("exact name: %v", err)
	}
	if recs, _ := List(dir); len(recs) != 1 {
		t.Errorf("records = %d", len(recs))
	}
}

// A live holder in this process keeps its lock past lockStale: a second
// acquirer times out and does not break it.
func TestLockHeldPastStale(t *testing.T) {
	dir := testRecordsDir(t)
	oldStale, oldWait := lockStale, lockWait
	lockStale, lockWait = 50*time.Millisecond, 300*time.Millisecond
	defer func() { lockStale, lockWait = oldStale, oldWait }()
	err := WithProjectLock(dir, func() error {
		time.Sleep(120 * time.Millisecond) // past lockStale
		inner := WithProjectLock(dir, func() error { t.Error("second acquirer got a live holder's lock"); return nil })
		if !errors.Is(inner, ErrLockTimeout) {
			t.Errorf("second acquirer: %v", inner)
		}
		if _, err := os.Stat(filepath.Join(dir, ".lock")); err != nil {
			t.Error("the live holder's lock was removed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// R2-M1: a new conversation can't take another conversation's session by
// name, whatever the owner state.
func TestClaimNewConversationOnLinkedRecord(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	f.kill(a.PID)

	// (c) refused without the hint, and with another name's hint.
	if _, err := Claim(dir, "web", convID(2), b, "i", Hint{}); !errors.Is(err, ErrBelongsToAnother) {
		t.Fatalf("(c) no hint: %v", err)
	}
	if _, err := Claim(dir, "web", convID(2), b, "i", Hint{AgentStartName: "api"}); !errors.Is(err, ErrBelongsToAnother) {
		t.Errorf("(c) other name's hint: %v", err)
	}
	if r, _ := Get(dir, "web"); len(r.Conversations) != 1 || *r.Owner != a {
		t.Errorf("a refused claim changed the record: %+v", r)
	}
	// (c) accepted with agent start NAME.
	if r, err := Claim(dir, "web", convID(2), b, "i", Hint{AgentStartName: "web"}); err != nil || len(r.Conversations) != 2 {
		t.Errorf("(c) with hint: %+v %v", r, err)
	}
	// (a) a resume of a listed conversation, no hint.
	f.kill(b.PID)
	c := f.add(fakeBase+3, 30)
	if r, err := Claim(dir, "web", convID(1), c, "i", Hint{}); err != nil || *r.Owner != c {
		t.Errorf("(a) resume: %+v %v", r, err)
	}
	// (b) a record with no conversations is adoptable.
	f.kill(c.PID)
	d := f.add(fakeBase+4, 40)
	ClaimByNameNoLink(dir, "pidonly", c, Hint{})
	if r, err := Claim(dir, "pidonly", convID(3), d, "i", Hint{}); err != nil || *r.Owner != d || len(r.Conversations) != 1 {
		t.Errorf("(b) adopt: %+v %v", r, err)
	}
}

// R2-L1: an owner that can't be looked up is busy, not dead.
func TestUnknownOwnerIsBusy(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	Claim(dir, "web", convID(1), a, "i", Hint{})
	f.mu.Lock()
	f.unknown[a.PID] = true
	f.mu.Unlock()
	var inUse ErrInUse
	if _, err := Claim(dir, "web", convID(1), b, "i", Hint{}); !errors.As(err, &inUse) {
		t.Errorf("Claim: %v", err)
	}
	if _, err := ClaimByNameNoLink(dir, "web", b, Hint{AgentStartName: "web"}); !errors.As(err, &inUse) {
		t.Errorf("ClaimByNameNoLink: %v", err)
	}
	// The owner itself can still alias while its lookup fails.
	if err := Alias(dir, "web", convID(2), a); err != nil {
		t.Errorf("Alias by an owner that can't be checked: %v", err)
	}
}

func TestPreCheckMatchesClaimAndWritesNothing(t *testing.T) {
	f := newFakeProcs(t)
	dir := testRecordsDir(t)
	a, b := f.add(fakeBase+1, 10), f.add(fakeBase+2, 20)
	// Nothing there: it passes and creates nothing.
	if _, found, err := PreCheck(dir, "web", convID(1), a, Hint{}); err != nil || found {
		t.Fatalf("new name: %v %v", found, err)
	}
	if recs, _ := List(dir); len(recs) != 0 {
		t.Fatalf("PreCheck wrote: %+v", recs)
	}
	if _, err := Claim(dir, "web", convID(1), a, "i", Hint{}); err != nil {
		t.Fatal(err)
	}
	var inUse ErrInUse
	if _, _, err := PreCheck(dir, "web", convID(1), b, Hint{}); !errors.As(err, &inUse) || inUse.PID != a.PID {
		t.Fatalf("live owner: %v", err)
	}
	f.kill(a.PID)
	if r, found, err := PreCheck(dir, "web", convID(1), b, Hint{}); err != nil || !found || r.Instance != "i" {
		t.Fatalf("dead owner, known conversation: %+v %v %v", r, found, err)
	}
	if _, _, err := PreCheck(dir, "web", convID(2), b, Hint{}); !errors.Is(err, ErrBelongsToAnother) {
		t.Fatalf("other conversation: %v", err)
	}
	if _, _, err := PreCheck(dir, "web", convID(2), b, Hint{AgentStartName: "web"}); err != nil {
		t.Fatalf("with the start hint: %v", err)
	}
	if _, _, err := PreCheck(dir, "other", convID(1), b, Hint{}); !errors.Is(err, ErrConversationTaken) {
		t.Fatalf("F8: %v", err)
	}
	if _, _, err := PreCheck(dir, "WEB", convID(3), b, Hint{}); !errors.Is(err, ErrNameCaseCollision) {
		t.Fatalf("case: %v", err)
	}
	// No conversation: the no-link rules.
	if _, _, err := PreCheck(dir, "web", "", b, Hint{}); !errors.Is(err, ErrBelongsToAnother) {
		t.Fatalf("no link, record with conversations: %v", err)
	}
	if _, _, err := PreCheck(dir, "web", "", b, Hint{AgentStartName: "web"}); err != nil {
		t.Fatalf("no link with the hint: %v", err)
	}
}
