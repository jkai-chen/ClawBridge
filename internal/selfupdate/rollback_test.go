package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// withSlot gives e a rollback slot in a fresh directory and returns it.
func (e *env) withSlot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state", PreviousDirName)
	e.opts.PreviousDir = dir
	return dir
}

// slotFiles lists the slot's entries.
func slotFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// assertSlot checks the slot holds exactly version's file with content,
// and previous.json records it.
func assertSlot(t *testing.T, dir, version string, content []byte) {
	t.Helper()
	name := slotName(version)
	if got := slotFiles(t, dir); !reflect.DeepEqual(got, []string{name, previousManifest}) {
		t.Fatalf("slot = %q, want only %s and previous.json", got, name)
	}
	if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("kept %s = %q (%v), want %q", name, got, err, content)
	}
	p, err := CheckPrevious(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != version || p.File != name || p.SHA256 != sumOf(content) || p.SavedAt.IsZero() {
		t.Fatalf("previous.json = %+v", p)
	}
}

// update applies e's plan and returns the result.
func (e *env) update(t *testing.T) Result {
	t.Helper()
	res, err := Apply(context.Background(), e.plan(t), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// toRelease makes e run current and publishes next as the latest.
func (e *env) toRelease(current, next string) []byte {
	e.opts.Current = current
	e.gh.latest = "/releases/tag/" + next
	asset := e.gh.release(next)
	e.runner.asset, e.runner.version = asset, next
	return asset
}

func TestApplyKeepsTheReplacedBinary(t *testing.T) {
	e := newEnv(t)
	dir := e.withSlot(t)
	old := linkFile
	t.Cleanup(func() { linkFile = old })
	var links [][2]string
	linkFile = func(from, to string) error { links = append(links, [2]string{from, to}); return os.Link(from, to) }
	res := e.update(t)
	if res.Previous != "v1.2.3" || res.PreviousWarning != "" {
		t.Fatalf("result = %+v, want v1.2.3 kept", res)
	}
	if len(links) != 1 || links[0][0] != e.dest || filepath.Dir(links[0][1]) != dir {
		t.Errorf("hard links = %q, want the replaced binary linked into the slot", links)
	}
	assertSlot(t, dir, "v1.2.3", []byte("old binary"))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("slot dir mode = %v (%v), want 0700", info.Mode(), err)
		}
		info, err = os.Stat(filepath.Join(dir, "clawbridge-v1.2.3"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("kept binary mode = %v (%v), want the replaced binary's 0755", info.Mode(), err)
		}
	}

	// The next update replaces the slot: one binary is kept.
	asset := e.runner.asset
	e.toRelease("v1.3.0", "v1.4.0")
	if res := e.update(t); res.Previous != "v1.3.0" {
		t.Fatalf("result = %+v", res)
	}
	assertSlot(t, dir, "v1.3.0", asset)
}

func TestApplyCopiesWhenTheSlotCantHardLink(t *testing.T) {
	e := newEnv(t)
	dir := e.withSlot(t)
	old := linkFile
	t.Cleanup(func() { linkFile = old })
	links := 0
	linkFile = func(string, string) error { links++; return errors.New("cross-device link") }
	if err := os.Chmod(e.dest, 0o700); err != nil {
		t.Fatal(err)
	}
	e.update(t)
	if links != 1 {
		t.Errorf("hard links tried = %d, want 1", links)
	}
	assertSlot(t, dir, "v1.2.3", []byte("old binary"))
	if info, err := os.Stat(filepath.Join(dir, "clawbridge-v1.2.3")); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o700) {
		t.Errorf("copied binary mode = %v (%v), want the replaced binary's 0700", info.Mode(), err)
	}
}

func TestApplyWithoutASlotKeepsNothing(t *testing.T) {
	e := newEnv(t)
	if res := e.update(t); res.Previous != "" || res.PreviousWarning != "" {
		t.Fatalf("result = %+v", res)
	}
}

// Failing to keep the slot never blocks an update: it warns, and the slot
// is emptied, so it never offers anything but the binary just replaced.
func TestApplySlotFailureOnlyWarns(t *testing.T) {
	e := newEnv(t)
	dir := e.withSlot(t)
	e.update(t)
	// A development build isn't kept; the v1.2.3 slot from before goes.
	e.toRelease("dev", "v1.4.0")
	e.opts.Version = "v1.4.0"
	res := e.update(t)
	if res.Previous != "" || !strings.Contains(res.PreviousWarning, "rollback isn't available") || !strings.Contains(res.PreviousWarning, "development build (dev)") {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := os.ReadFile(e.dest); !bytes.Equal(got, e.runner.asset) {
		t.Fatalf("the update did not stand: dest = %q", got)
	}
	if got := slotFiles(t, dir); len(got) != 0 {
		t.Fatalf("slot = %q, want it emptied", got)
	}
	if _, err := ReadPrevious(dir); !errors.Is(err, ErrNoPrevious) {
		t.Fatal(err)
	}

	// A slot directory that can't be made.
	e = newEnv(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.opts.PreviousDir = filepath.Join(blocker, PreviousDirName)
	res = e.update(t)
	if res.Previous != "" || !strings.Contains(res.PreviousWarning, "rollback isn't available") {
		t.Fatalf("result = %+v", res)
	}
}

// A failed install leaves the slot as it was, with no temporary copy.
func TestApplyFailureLeavesTheSlot(t *testing.T) {
	e := newEnv(t)
	dir := e.withSlot(t)
	e.update(t)
	e.toRelease("v1.3.0", "v1.4.0")
	old := rename
	t.Cleanup(func() { rename = old })
	rename = func(string, string) error { return errors.New("injected rename failure") }
	if _, err := Apply(context.Background(), e.plan(t), e.opts); err == nil {
		t.Fatal("the install succeeded")
	}
	assertSlot(t, dir, "v1.2.3", []byte("old binary"))
}

func TestApplyWindowsKeepsTheSetAsideBinary(t *testing.T) {
	e := newEnv(t)
	dir := e.withSlot(t)
	actAsWindows(t, 0)
	res := e.update(t)
	if res.Previous != "v1.2.3" {
		t.Fatalf("result = %+v", res)
	}
	assertSlot(t, dir, "v1.2.3", []byte("old binary"))
	if !strings.HasSuffix(slotName("v1.2.3"), ".exe") {
		t.Fatal("a Windows slot is not an .exe")
	}
	// A copy, not the aside itself: that stays until running processes exit
	// and a later update sweeps it.
	if got, _ := os.ReadFile(res.Aside); string(got) != "old binary" {
		t.Errorf("aside holds %q", got)
	}
	asset := e.runner.asset
	e.toRelease("v1.3.0", "v1.4.0")
	asideID = func() string { return strings.Repeat("cd", 16) }
	if res := e.update(t); res.Swept != 1 {
		t.Fatalf("result = %+v, want the first aside swept", res)
	}
	assertSlot(t, dir, "v1.3.0", asset)
}

// rolledBack is e after an update from v1.2.3 to v1.3.0, running v1.3.0,
// with the runner expecting the kept v1.2.3.
func rolledBack(t *testing.T) (*env, string, []byte) {
	t.Helper()
	e := newEnv(t)
	dir := e.withSlot(t)
	e.update(t)
	newAsset := e.runner.asset
	e.opts.Current = "v1.3.0"
	e.runner.asset, e.runner.version, e.runner.calls = []byte("old binary"), "v1.2.3", nil
	return e, dir, newAsset
}

func TestRollbackRestoresTheKeptBinary(t *testing.T) {
	e, dir, newAsset := rolledBack(t)
	res, err := Rollback(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	want := RollbackResult{From: "v1.3.0", To: "v1.2.3", Destination: e.dest, SHA256: sumOf([]byte("old binary")), Previous: "v1.3.0"}
	if res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	e.assertUntouched(t)
	if info, err := os.Stat(e.dest); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o755) {
		t.Errorf("dest mode = %v (%v)", info.Mode(), err)
	}
	if len(e.runner.calls) != 1 || !reflect.DeepEqual(e.runner.calls[0][1:], []string{"version"}) {
		t.Errorf("runs = %q, want the kept binary's version", e.runner.calls)
	}
	// The version rolled back from is kept, so a second rollback undoes the
	// first.
	assertSlot(t, dir, "v1.3.0", newAsset)
	e.opts.Current = "v1.2.3"
	e.runner.asset, e.runner.version = newAsset, "v1.3.0"
	if _, err := Rollback(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(e.dest); !bytes.Equal(got, newAsset) {
		t.Fatalf("dest = %q after the second rollback", got)
	}
	assertSlot(t, dir, "v1.2.3", []byte("old binary"))
}

func TestRollbackRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, e *env, dir string)
		want  string
		smoke bool // the kept binary ran
	}{
		{"sha mismatch", func(t *testing.T, e *env, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "clawbridge-v1.2.3"), []byte("tampered"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "nothing was changed; the kept v1.2.3 doesn't match the sha256 in previous.json", false},
		{"version mismatch", func(t *testing.T, e *env, dir string) { e.runner.version = "v1.2.4" },
			"the kept binary doesn't report v1.2.3; nothing was changed", true},
		{"version fails", func(t *testing.T, e *env, dir string) { e.runner.smokeErr = errors.New("exec format error") },
			"the kept binary doesn't report v1.2.3; nothing was changed", true},
		{"no slot", func(t *testing.T, e *env, dir string) {
			if err := os.Remove(filepath.Join(dir, previousManifest)); err != nil {
				t.Fatal(err)
			}
		}, "nothing was changed; no previous binary is kept in ", false},
		{"kept file gone", func(t *testing.T, e *env, dir string) {
			if err := os.Remove(filepath.Join(dir, "clawbridge-v1.2.3")); err != nil {
				t.Fatal(err)
			}
		}, "nothing was changed; read the kept v1.2.3", false},
		{"malformed previous.json", func(t *testing.T, e *env, dir string) {
			if err := os.WriteFile(filepath.Join(dir, previousManifest), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "previous.json is malformed", false},
		{"a file outside the slot", func(t *testing.T, e *env, dir string) {
			p, _ := ReadPrevious(dir)
			p.File = "../clawbridge-v1.2.3"
			b, _ := json.Marshal(p)
			if err := os.WriteFile(filepath.Join(dir, previousManifest), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "previous.json is malformed", false},
		{"already running", func(t *testing.T, e *env, dir string) { e.opts.Current = "v1.2.3" },
			"the kept binary is v1.2.3, which is already running", false},
		{"no slot configured", func(t *testing.T, e *env, dir string) { e.opts.PreviousDir = "" },
			"no rollback slot is configured", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, dir, newAsset := rolledBack(t)
			c.setup(t, e, dir)
			before := map[string][]byte{}
			for _, name := range slotFiles(t, dir) {
				before[name], _ = os.ReadFile(filepath.Join(dir, name))
			}
			_, err := Rollback(context.Background(), e.opts)
			wantErr(t, err, c.want)
			if got, _ := os.ReadFile(e.dest); !bytes.Equal(got, newAsset) {
				t.Errorf("dest = %q, want the running v1.3.0 untouched", got)
			}
			e.assertNoStaged(t)
			after := map[string][]byte{}
			for _, name := range slotFiles(t, dir) {
				after[name], _ = os.ReadFile(filepath.Join(dir, name))
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("the slot changed: %q → %q", before, after)
			}
			if ran := len(e.runner.calls) > 0; ran != c.smoke {
				t.Errorf("runs = %q", e.runner.calls)
			}
		})
	}
}

func TestRollbackWindowsSetsTheRunningBinaryAside(t *testing.T) {
	e, dir, newAsset := rolledBack(t)
	calls := actAsWindows(t, 0)
	// The slot was written as a Unix one; rewrite it as Windows names it.
	if err := os.Rename(filepath.Join(dir, "clawbridge-v1.2.3"), filepath.Join(dir, "clawbridge-v1.2.3.exe")); err != nil {
		t.Fatal(err)
	}
	p := Previous{Version: "v1.2.3", SHA256: sumOf([]byte("old binary")), File: "clawbridge-v1.2.3.exe"}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(dir, previousManifest), b, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Rollback(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(e.tempDir, "clawbridge-previous-"+strings.Repeat("ab", 16)+".exe.old")
	if res.Aside != aside || len(*calls) != 2 || (*calls)[0] != [2]string{e.dest, aside} || (*calls)[1][1] != e.dest ||
		!strings.HasSuffix((*calls)[1][0], ".exe") {
		t.Fatalf("result = %+v, renames = %q", res, *calls)
	}
	if got, _ := os.ReadFile(aside); !bytes.Equal(got, newAsset) {
		t.Errorf("aside holds %q, want the replaced v1.3.0", got)
	}
	e.assertUntouched(t)
	assertSlot(t, dir, "v1.3.0", newAsset)
}

func TestSetHoldVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", AutoUpdateFile)
	if st, err := ReadAutoUpdateState(path); err != nil || st.HoldVersion != "" {
		t.Fatal(st, err)
	}
	if err := SetHoldVersion(path, "v1.3.0"); err != nil {
		t.Fatal(err)
	}
	if st, err := ReadAutoUpdateState(path); err != nil || st.HoldVersion != "v1.3.0" {
		t.Fatal(st, err)
	}
	// Other fields, a newer client's too, are kept.
	if err := os.WriteFile(path, []byte(`{"enabled":true,"future":{"x":1},"hold_version":"v1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetHoldVersion(path, "v1.3.1"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, _ := os.ReadFile(path)
	if json.Unmarshal(b, &got) != nil || got["enabled"] != true || got["hold_version"] != "v1.3.1" || got["future"] == nil {
		t.Fatalf("auto-update.json = %s", b)
	}
	if info, err := os.Stat(path); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o600) {
		t.Errorf("mode = %v (%v)", info.Mode(), err)
	}
	for _, bad := range []string{"{", "null", "[]"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := SetHoldVersion(path, "v1.3.1"); err == nil {
			t.Errorf("%q was overwritten", bad)
		}
		if b, _ := os.ReadFile(path); string(b) != bad {
			t.Errorf("%q became %s", bad, b)
		}
	}
	if err := SetHoldVersion(path, "dev"); err == nil {
		t.Error("a development version was held")
	}
}
