package fde

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeOut struct {
	out string
	err error
}

// fakeRun installs a runner answering from a map keyed by the command line.
// An unlisted command fails, so a test sees any command it did not expect.
func fakeRun(t *testing.T, m map[string]fakeOut) *[]string {
	t.Helper()
	var calls []string
	old := run
	run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, key)
		if o, ok := m[key]; ok {
			return []byte(o.out), o.err
		}
		return nil, errors.New("unexpected command: " + key)
	}
	t.Cleanup(func() { run = old })
	return &calls
}

func ok(s string) fakeOut { return fakeOut{out: s} }

func fakeWSL(t *testing.T, distro string, interop bool) {
	t.Helper()
	oldEnv, oldStat := getenv, pathStat
	getenv = func(k string) string {
		if k == "WSL_DISTRO_NAME" {
			return distro
		}
		return ""
	}
	pathStat = func(p string) (os.FileInfo, error) {
		if interop && p == wslInterop {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { getenv, pathStat = oldEnv, oldStat })
}

// fakeLook makes lookPath resolve every tool to /usr/bin/<name>, except the
// names in over.
func fakeLook(t *testing.T, over map[string]string) {
	t.Helper()
	old := lookPath
	lookPath = func(name string) (string, error) {
		if p, ok := over[name]; ok {
			return p, nil
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { lookPath = old })
}

func want(t *testing.T, r Result, state, method string) {
	t.Helper()
	if r.State != state || (method != "" && r.Method != method) {
		t.Fatalf("got %s/%s (%s), want %s/%s", r.State, r.Method, r.Detail, state, method)
	}
	if r.At.IsZero() {
		t.Fatal("At unset")
	}
}

const dfBoot = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk3s5 488245288 1 2 3% /System/Volumes/Data\n"
const dfExt = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/disk5s1 100 1 2 3% /Volumes/My Disk\n"

func plist(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>` + body + `</dict></plist>`
}

func TestDarwin(t *testing.T) {
	ctx := context.Background()
	d := "/Users/example/.clawbridge"
	cases := []struct {
		name  string
		m     map[string]fakeOut
		state string
	}{
		{"on", map[string]fakeOut{"/bin/df -P " + d: ok(dfBoot), "/usr/bin/fdesetup status": ok("FileVault is On.\n")}, StateDetected},
		{"off", map[string]fakeOut{"/bin/df -P " + d: ok(dfBoot), "/usr/bin/fdesetup status": ok("FileVault is Off.\n")}, StateNotDetected},
		{"garbage", map[string]fakeOut{"/bin/df -P " + d: ok(dfBoot), "/usr/bin/fdesetup status": ok("FileVault is On. (partially)")}, StateUnknown},
		{"on-prefix-only", map[string]fakeOut{"/bin/df -P " + d: ok(dfBoot), "/usr/bin/fdesetup status": ok("FileVault is On")}, StateUnknown},
		{"fdesetup error", map[string]fakeOut{"/bin/df -P " + d: ok(dfBoot), "/usr/bin/fdesetup status": {err: errors.New("x")}}, StateUnknown},
		{"df error", map[string]fakeOut{"/bin/df -P " + d: {err: errors.New("x")}}, StateUnknown},
		{"df garbage", map[string]fakeOut{"/bin/df -P " + d: ok("nonsense")}, StateUnknown},
		{"ext encrypted only", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Encrypted</key><true/><key>FileVault</key><false/>`))}, StateUnknown},
		{"ext encrypted no filevault key", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Encrypted</key><true/>`))}, StateUnknown},
		{"ext both", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Encrypted</key><true/><key>FileVault</key><true/>`))}, StateDetected},
		{"ext filevault", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>FileVault</key><true/>`))}, StateDetected},
		{"ext plain", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Encrypted</key><false/><key>FileVault</key><false/>`))}, StateNotDetected},
		{"ext missing keys", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Other</key><true/>`))}, StateUnknown},
		{"ext one false", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Encrypted</key><false/>`))}, StateUnknown},
		{"ext nested key ignored", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Sub</key><dict><key>Encrypted</key><true/></dict>`))}, StateUnknown},
		{"ext bad xml", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(`<plist><dict><key>Encrypted</key><true/>`)}, StateUnknown},
		{"ext error", map[string]fakeOut{"/bin/df -P " + d: ok(dfExt), "/usr/sbin/diskutil info -plist /Volumes/My Disk": {err: errors.New("x")}}, StateUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeRun(t, c.m)
			want(t, detectDarwin(ctx, d), c.state, "")
		})
	}
}

func TestDarwinEncryptedWithoutFileVaultDetail(t *testing.T) {
	fakeRun(t, map[string]fakeOut{
		"/bin/df -P /x": ok(dfExt),
		"/usr/sbin/diskutil info -plist /Volumes/My Disk": ok(plist(`<key>Encrypted</key><true/><key>FileVault</key><false/>`)),
	})
	r := detectDarwin(context.Background(), "/x")
	want(t, r, StateUnknown, "diskutil")
	if r.Detail != "volume encrypted without FileVault" {
		t.Fatalf("detail = %q", r.Detail)
	}
}

func TestDarwinFdesetupIsAWholeLineMatch(t *testing.T) {
	fakeRun(t, map[string]fakeOut{
		"/bin/df -P /x":            ok(dfBoot),
		"/usr/bin/fdesetup status": ok("FileVault is On.\nEncryption in progress: Percent completed = 12"),
	})
	want(t, detectDarwin(context.Background(), "/x"), StateUnknown, "")
}

func TestDarwinDetailHasNoHomePath(t *testing.T) {
	fakeRun(t, map[string]fakeOut{
		"/bin/df -P /x": ok("F 1 1 1 1% /Users/example/mnt\n"),
		"/usr/sbin/diskutil info -plist /Users/example/mnt": ok(plist(`<key>Encrypted</key><true/>`)),
	})
	r := detectDarwin(context.Background(), "/x")
	if strings.Contains(r.Detail, "example") || strings.Contains(r.Detail, "/Users") {
		t.Fatalf("detail leaks path: %q", r.Detail)
	}
}

const lsblkLUKS = "crypt luks-0000\npart sda3\ndisk sda\n"

func fm(src, typ string) fakeOut { return ok(src + " " + typ + "\n") }

func TestLinux(t *testing.T) {
	d := "/home/example/.clawbridge"
	find := "/usr/bin/findmnt -n -o SOURCE,FSTYPE --target " + d
	fscryptNo := "/usr/bin/fscrypt status " + d
	cases := []struct {
		name  string
		m     map[string]fakeOut
		state string
		meth  string
	}{
		{"luks", map[string]fakeOut{find: fm("/dev/mapper/luks-0000", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/mapper/luks-0000": ok(lsblkLUKS)}, StateDetected, methodLUKS},
		{"lvm on luks", map[string]fakeOut{find: fm("/dev/mapper/vg-root", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/mapper/vg-root": ok("lvm vg-root\ncrypt luks-0000\npart sda3\ndisk sda\n")}, StateDetected, methodLUKS},
		{"btrfs subvol on luks", map[string]fakeOut{find: fm("/dev/mapper/luks-0000[/@home]", "btrfs"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/mapper/luks-0000": ok(lsblkLUKS)}, StateDetected, methodLUKS},
		{"plain partition", map[string]fakeOut{find: fm("/dev/sda2", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/sda2": ok("part sda2\ndisk sda\n"), fscryptNo: {err: errors.New("not found")}}, StateNotDetected, methodLUKS},
		{"plain lvm", map[string]fakeOut{find: fm("/dev/mapper/vg-root", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/mapper/vg-root": ok("lvm vg-root\npart sda3\ndisk sda\n")}, StateNotDetected, methodLUKS},
		{"loop device", map[string]fakeOut{find: fm("/dev/loop3", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/loop3": ok("loop loop3\n")}, StateUnknown, ""},
		{"lsblk empty", map[string]fakeOut{find: fm("/dev/sda2", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/sda2": ok("")}, StateUnknown, ""},
		{"lsblk error", map[string]fakeOut{find: fm("/dev/sda2", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/sda2": {err: errors.New("x")}}, StateUnknown, ""},
		{"zfs encrypted", map[string]fakeOut{find: fm("rpool/home", "zfs"), "/usr/bin/zfs get -H -o value encryption rpool/home": ok("aes-256-gcm\n")}, StateDetected, methodZFS},
		{"zfs off", map[string]fakeOut{find: fm("rpool/home", "zfs"), "/usr/bin/zfs get -H -o value encryption rpool/home": ok("off\n")}, StateNotDetected, methodZFS},
		{"zfs dash", map[string]fakeOut{find: fm("rpool/home", "zfs"), "/usr/bin/zfs get -H -o value encryption rpool/home": ok("-\n")}, StateUnknown, methodZFS},
		{"zfs error", map[string]fakeOut{find: fm("rpool/home", "zfs"), "/usr/bin/zfs get -H -o value encryption rpool/home": {err: errors.New("x")}}, StateUnknown, methodZFS},
		{"ecryptfs", map[string]fakeOut{find: fm("/home/example/.Private", "ecryptfs")}, StateDetected, methodEcrypt},
		{"tmpfs", map[string]fakeOut{find: fm("tmpfs", "tmpfs"), fscryptNo: {err: errors.New("x")}}, StateUnknown, ""},
		{"overlay", map[string]fakeOut{find: fm("overlay", "overlay")}, StateUnknown, ""},
		{"nfs4", map[string]fakeOut{find: fm("srv:/x", "nfs4")}, StateUnknown, ""},
		{"cifs", map[string]fakeOut{find: fm("//srv/x", "cifs")}, StateUnknown, ""},
		{"fuse", map[string]fakeOut{find: fm("x", "fuse.sshfs")}, StateUnknown, ""},
		{"findmnt error", map[string]fakeOut{find: {err: errors.New("x")}}, StateUnknown, ""},
		{"findmnt garbage", map[string]fakeOut{find: ok("one\n")}, StateUnknown, ""},
		{"non-dev source", map[string]fakeOut{find: fm("server", "ext4")}, StateUnknown, ""},
		{"fscrypt on plain", map[string]fakeOut{find: fm("/dev/sda2", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/sda2": ok("part sda2\ndisk sda\n"), fscryptNo: ok("\"" + d + "\" is encrypted with fscrypt.\n")}, StateDetected, methodFscrypt},
		{"fscrypt on tmpfs", map[string]fakeOut{find: fm("tmpfs", "tmpfs"), fscryptNo: ok("\"" + d + "\" is encrypted with fscrypt.\n")}, StateDetected, methodFscrypt},
		{"fscrypt says not", map[string]fakeOut{find: fm("/dev/sda2", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/sda2": ok("part sda2\ndisk sda\n"), fscryptNo: ok("\"" + d + "\" is not encrypted with fscrypt.\n")}, StateNotDetected, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeWSL(t, "", false)
			fakeLook(t, nil)
			fakeRun(t, c.m)
			r := detectLinuxOrWSL(context.Background(), d)
			want(t, r, c.state, c.meth)
			if strings.Contains(r.Detail, "example") || strings.Contains(r.Detail, "rpool") || strings.Contains(r.Detail, "home") {
				t.Fatalf("detail leaks path: %q", r.Detail)
			}
		})
	}
}

func TestLinuxHostileFscryptDirName(t *testing.T) {
	d := `/home/example/is encrypted with fscrypt`
	find := "/usr/bin/findmnt -n -o SOURCE,FSTYPE --target " + d
	for _, out := range []string{
		"\"" + d + "\" is not encrypted with fscrypt.\n",
		"\"" + d + "\" is not encrypted with fscrypt. \"x\" is encrypted with fscrypt.\n",
		"\"" + d + "\" is encrypted with fscrypt. extra\n",
		"x \"" + d + "\" is encrypted with fscrypt.\n",
		"\"/other\" is encrypted with fscrypt.\n",
	} {
		fakeWSL(t, "", false)
		fakeLook(t, nil)
		fakeRun(t, map[string]fakeOut{find: fm("tmpfs", "tmpfs"), "/usr/bin/fscrypt status " + d: ok(out)})
		want(t, detectLinux(context.Background(), d), StateUnknown, "")
	}
	// The exact line, with trailing whitespace, still counts.
	fakeWSL(t, "", false)
	fakeLook(t, nil)
	fakeRun(t, map[string]fakeOut{find: fm("tmpfs", "tmpfs"), "/usr/bin/fscrypt status " + d: ok("\"" + d + "\" is encrypted with fscrypt. \r\n")})
	want(t, detectLinux(context.Background(), d), StateDetected, methodFscrypt)
}

func TestLinuxToolOutsideSystemDirs(t *testing.T) {
	d := "/home/example/x"
	for _, p := range []string{"/opt/bin/findmnt", "/home/example/bin/findmnt", "/usr/local/bin/findmnt", "/usr/bin/sub/findmnt", "findmnt"} {
		fakeWSL(t, "", false)
		fakeLook(t, map[string]string{"findmnt": p})
		calls := fakeRun(t, map[string]fakeOut{})
		want(t, detectLinux(context.Background(), d), StateUnknown, "")
		for _, c := range *calls {
			if strings.HasPrefix(c, p) {
				t.Fatalf("ran an untrusted tool: %s", c)
			}
		}
	}
	// A lookup that fails is unknown too.
	fakeWSL(t, "", false)
	old := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = old })
	fakeRun(t, nil)
	want(t, detectLinux(context.Background(), d), StateUnknown, "")
	// /sbin, /bin and /usr/sbin are accepted.
	for _, p := range []string{"/sbin/findmnt", "/bin/findmnt", "/usr/sbin/findmnt"} {
		fakeLook(t, map[string]string{"findmnt": p})
		fakeRun(t, map[string]fakeOut{p + " -n -o SOURCE,FSTYPE --target " + d: fm("/dev/sda2", "ext4"), "/usr/bin/lsblk -s -n -o TYPE,NAME /dev/sda2": ok("part sda2\ndisk sda\n"), "/usr/bin/fscrypt status " + d: {err: errors.New("x")}})
		want(t, detectLinux(context.Background(), d), StateNotDetected, methodLUKS)
	}
}

func TestWindowsIsPolicy(t *testing.T) {
	fakeRun(t, nil)
	r := windowsPolicy(context.Background())
	want(t, r, StateUnknown, "windows-policy")
	if r.Detail != "Windows and WSL use a passphrase by default" {
		t.Fatalf("detail = %q", r.Detail)
	}
}

func TestWSLIsPolicy(t *testing.T) {
	d := "/home/example/.clawbridge"
	cases := []struct {
		name    string
		distro  string
		interop bool
	}{
		{"distro name", "Ubuntu", false},
		{"interop file", "", true},
		{"both", "Ubuntu", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeWSL(t, c.distro, c.interop)
			fakeLook(t, nil)
			// The Linux tools answer as if the volume sat on LUKS. WSL must
			// not read them, or the VHDX's ext4 would be misjudged.
			calls := fakeRun(t, map[string]fakeOut{
				"/usr/bin/findmnt -n -o SOURCE,FSTYPE --target " + d:      fm("/dev/mapper/luks-0000", "ext4"),
				"/usr/bin/lsblk -s -n -o TYPE,NAME /dev/mapper/luks-0000": ok(lsblkLUKS),
			})
			r := detectLinuxOrWSL(context.Background(), d)
			want(t, r, StateUnknown, "windows-policy")
			if r.Detail != windowsPolicyDetail {
				t.Fatalf("detail = %q", r.Detail)
			}
			if len(*calls) != 0 {
				t.Fatalf("WSL ran commands: %v", *calls)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	old := run
	run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		select {} // ignores ctx on purpose
	}
	t.Cleanup(func() { run = old })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := detectDarwin(ctx, "/x")
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not return at the deadline")
	}
	want(t, r, StateUnknown, "")
	fakeWSL(t, "", false)
	fakeLook(t, nil)
	want(t, detectLinux(ctx, "/x"), StateUnknown, "")
}

func TestCommandContextHasFiveSecondDeadline(t *testing.T) {
	old := run
	var dl time.Time
	run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		dl, _ = ctx.Deadline()
		return nil, nil
	}
	t.Cleanup(func() { run = old })
	runCmd(context.Background(), "x")
	if d := time.Until(dl); d <= 4*time.Second || d > 5*time.Second {
		t.Fatalf("deadline in %v", d)
	}
}

// fakeDetect replaces the detector with one that counts its calls and
// returns *state, so the cache tests do not depend on the host platform.
func fakeDetect(t *testing.T, n *int, state *string) {
	t.Helper()
	old := detect
	detect = func(ctx context.Context, dir string) Result {
		*n++
		return Result{State: *state, Method: "fake", Detail: "fresh", At: now()}
	}
	t.Cleanup(func() { detect = old })
}

func TestCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys", "fde.json")
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = oldNow })
	n := 0
	state := StateUnknown
	fakeDetect(t, &n, &state)
	ctx := context.Background()

	// First call detects (the fake gives unknown) and stores.
	r1 := Cached(ctx, dir, path, 24*time.Hour)
	if r1.State != StateUnknown || n == 0 {
		t.Fatalf("first: %+v n=%d", r1, n)
	}
	calls := n
	// Windows has no Unix permission bits: Stat reports 0666 there.
	fi, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}

	// Fresh hit.
	clock = clock.Add(30 * time.Minute)
	Cached(ctx, dir, path, 24*time.Hour)
	if n != calls {
		t.Fatal("fresh unknown result was recomputed")
	}
	// Unknown is capped at one hour even with a 24 h maxAge.
	clock = clock.Add(31 * time.Minute)
	Cached(ctx, dir, path, 24*time.Hour)
	if n == calls {
		t.Fatal("unknown result outlived one hour")
	}

	// A detected result honours maxAge.
	writeCache(path, Result{State: StateDetected, Method: "m", Detail: "d", At: clock})
	n = 0
	clock = clock.Add(23 * time.Hour)
	if r := Cached(ctx, dir, path, 24*time.Hour); r.State != StateDetected || n != 0 {
		t.Fatalf("detected not served from cache: %+v", r)
	}
	clock = clock.Add(2 * time.Hour)
	Cached(ctx, dir, path, 24*time.Hour)
	if n == 0 {
		t.Fatal("stale detected result not recomputed")
	}

	// Refresh bypasses a fresh entry.
	writeCache(path, Result{State: StateDetected, Method: "m", At: clock})
	n = 0
	if r := Refresh(ctx, dir, path); n == 0 || r.Method != "fake" {
		t.Fatalf("refresh served the cache: %+v", r)
	}
}

func TestCacheRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fde.json")
	n := 0
	state := StateDetected
	fakeDetect(t, &n, &state)
	for _, body := range []string{"not json", `{"state":"bogus","at":"2099-01-01T00:00:00Z"}`, `{"state":"detected"}`} {
		os.WriteFile(path, []byte(body), 0o600)
		n = 0
		Cached(context.Background(), dir, path, time.Hour)
		if n == 0 {
			t.Fatalf("accepted %q", body)
		}
	}
	// A future timestamp is not fresh.
	b, _ := json.Marshal(Result{State: StateDetected, At: time.Now().Add(48 * time.Hour)})
	os.WriteFile(path, b, 0o600)
	n = 0
	Cached(context.Background(), dir, path, time.Hour)
	if n == 0 {
		t.Fatal("accepted a future timestamp")
	}
}
