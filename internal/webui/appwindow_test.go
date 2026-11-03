package webui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// fakeProbe is a machine with only the listed files, PATH entries and env.
func fakeProbe(platform string, files, path []string, env map[string]string) (Probe, *[][]string) {
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	onPath := map[string]string{}
	for _, f := range path {
		onPath[filepath.Base(f)] = f
	}
	var calls [][]string
	return Probe{
		Platform: platform,
		Exists:   func(p string) bool { return have[p] },
		LookPath: func(n string) (string, error) {
			if p, ok := onPath[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		EvalSymlinks: func(p string) (string, error) { return p, nil },
		Getenv:       func(k string) string { return env[k] },
		MkdirAll:     os.MkdirAll,
		Output: func(name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			switch {
			case name == "cmd.exe" && args[2] == "%ProgramFiles%":
				return "C:\\Program Files\r\n", nil
			case name == "cmd.exe" && args[2] == "%ProgramFiles(x86)%":
				return "C:\\Program Files (x86)\r\n", nil
			case name == "cmd.exe" && args[2] == "%LOCALAPPDATA%":
				return "C:\\Profiles\\sam\\AppData\\Local\r\n", nil
			case name == "wslpath":
				w := strings.ReplaceAll(strings.TrimPrefix(args[1], "C:"), `\`, "/")
				return "/mnt/c" + w + "\n", nil
			}
			return "", errors.New("unexpected")
		},
	}, &calls
}

func winFolders(p *Probe) {
	p.WinFolder = func(n string) string {
		return map[string]string{"ProgramFiles": `C:\PF`, "ProgramFilesX86": `C:\PF86`, "LocalAppData": `C:\LA`}[n]
	}
	p.WinAppPath = func(exe string) string {
		if exe == "msedge.exe" {
			return `D:\Edge\msedge.exe`
		}
		return ""
	}
}

func TestFindBrowserOrder(t *testing.T) {
	home := "/opt/sam"
	for _, tc := range []struct {
		name     string
		platform string
		files    []string
		path     []string
		want     string // Browser.Path, "" for none
	}{
		{"mac chrome first", "darwin", []string{"/Applications/Brave Browser.app", "/Applications/Google Chrome.app", "/Applications/Microsoft Edge.app"}, nil, "/Applications/Google Chrome.app"},
		{"mac edge", "darwin", []string{"/Applications/Microsoft Edge.app", "/Applications/Chromium.app"}, nil, "/Applications/Microsoft Edge.app"},
		{"mac user apps", "darwin", []string{home + "/Applications/Brave Browser.app"}, nil, home + "/Applications/Brave Browser.app"},
		{"mac /Applications before ~/Applications", "darwin", []string{"/Applications/Chromium.app", home + "/Applications/Chromium.app"}, nil, "/Applications/Chromium.app"},
		{"mac none", "darwin", []string{"/Applications/Safari.app"}, nil, ""},
		{"linux order", "linux", nil, []string{"/usr/bin/brave-browser", "/usr/bin/chromium", "/usr/bin/google-chrome-stable"}, "/usr/bin/google-chrome-stable"},
		{"linux chromium before edge", "linux", nil, []string{"/usr/bin/microsoft-edge", "/usr/bin/chromium-browser"}, "/usr/bin/chromium-browser"},
		{"linux none", "linux", nil, []string{"/usr/bin/firefox"}, ""},
		{"windows edge x86 first", "windows", []string{`C:\PF\Google\Chrome\Application\chrome.exe`, `C:\PF86\Microsoft\Edge\Application\msedge.exe`}, nil, `C:\PF86\Microsoft\Edge\Application\msedge.exe`},
		{"windows chrome", "windows", []string{`C:\PF\Google\Chrome\Application\chrome.exe`}, nil, `C:\PF\Google\Chrome\Application\chrome.exe`},
		{"windows chrome per user", "windows", []string{`C:\LA\Google\Chrome\Application\chrome.exe`}, nil, `C:\LA\Google\Chrome\Application\chrome.exe`},
		{"windows registry", "windows", []string{`D:\Edge\msedge.exe`}, nil, `D:\Edge\msedge.exe`},
		{"windows none", "windows", nil, nil, ""},
		{"wsl edge", "wsl", []string{"/mnt/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe", "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"}, nil, "/mnt/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"},
		{"wsl chrome", "wsl", []string{"/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"}, nil, "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"},
		{"wsl none", "wsl", nil, nil, ""},
	} {
		p, _ := fakeProbe(tc.platform, tc.files, tc.path, map[string]string{"HOME": home})
		winFolders(&p)
		b, ok := FindBrowser(p)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: found %v", tc.name, b)
			}
			continue
		}
		if !ok || b.Path != tc.want {
			t.Errorf("%s: got %v %v, want %s", tc.name, b, ok, tc.want)
		}
	}
}

func TestPlanNoBrowser(t *testing.T) {
	p, _ := fakeProbe("linux", nil, nil, nil)
	if _, err := PlanAppWindow(p, "/state", "/p/a.toml"); !errors.Is(err, ErrNoBrowser) {
		t.Fatal(err)
	}
}

var tokenAlphabet = regexp.MustCompile(`^[0-9a-f]{64}$`)

// A3: exact argv per platform, and never a token or a shell in it.
func TestAppArgvExact(t *testing.T) {
	hash := profileHash("/p/a.toml")
	state := "/state/ui"
	fileTarget := "file:///state/ui/" + hash + "/launch-ab.html"
	const urlTarget = "http://127.0.0.1:5000/?t=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tail := func(dir, target string) []string {
		return []string{"--user-data-dir=" + dir, "--app=" + target, "--no-first-run", "--no-default-browser-check"}
	}
	cat := func(a []string, b []string) []string { return append(append([]string{}, a...), b...) }
	for _, tc := range []struct {
		name     string
		platform string
		files    []string
		path     []string
		target   string
		want     []string
	}{
		{"darwin", "darwin", []string{"/Applications/Google Chrome.app"}, nil, fileTarget,
			cat([]string{"/usr/bin/open", "-na", "/Applications/Google Chrome.app", "--args"}, tail(state+"/"+hash+"/browser", fileTarget))},
		{"linux", "linux", nil, []string{"/usr/bin/google-chrome"}, fileTarget,
			cat([]string{"/usr/bin/google-chrome"}, tail(state+"/"+hash+"/browser", fileTarget))},
		{"windows", "windows", []string{`C:\PF86\Microsoft\Edge\Application\msedge.exe`}, nil, urlTarget,
			cat([]string{`C:\PF86\Microsoft\Edge\Application\msedge.exe`}, tail(`C:\LA\ClawBridge\ui\`+hash+`\browser`, urlTarget))},
		{"wsl", "wsl", []string{"/mnt/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"}, nil, urlTarget,
			cat([]string{"/mnt/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"}, tail(`C:\Profiles\sam\AppData\Local\ClawBridge\ui\`+hash+`\browser`, urlTarget))},
	} {
		p, calls := fakeProbe(tc.platform, tc.files, tc.path, map[string]string{"HOME": "/opt/sam"})
		winFolders(&p)
		a, err := PlanAppWindow(p, state, "/p/a.toml")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := a.Argv(tc.target)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s argv:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		joined := strings.Join(got, " ")
		if (tc.platform == "darwin" || tc.platform == "linux") && (strings.Contains(joined, "?t=") || strings.Contains(joined, "http")) {
			t.Errorf("%s: token URL in argv: %q", tc.name, got)
		}
		if tc.platform == "wsl" {
			for _, c := range *calls {
				if strings.Contains(strings.Join(c, " "), "?t=") || c[0] != "cmd.exe" && c[0] != "wslpath" {
					t.Errorf("wsl helper call %q", c)
				}
			}
			// The browser is exec'd directly, never through cmd.exe.
			if strings.Contains(strings.ToLower(got[0]), "cmd.exe") {
				t.Errorf("argv goes through cmd.exe: %q", got)
			}
		}
	}
}

// A1: the real token is lowercase hex, so no shell metacharacter can be in it.
func TestLaunchTokenIsHex(t *testing.T) {
	r := start(t, func(c *Config) { c.Platform = "wsl" })
	u, err := r.srv.NoOpenURL()
	if err != nil {
		t.Fatal(err)
	}
	if !tokenAlphabet.MatchString(u[strings.Index(u, "?t=")+3:]) {
		t.Errorf("token alphabet: %q", u)
	}
}

// WSL does not assume /mnt/c, and cmd.exe only ever echoes a fixed variable.
func TestWSLResolvesWithoutAssumingMnt(t *testing.T) {
	p, calls := fakeProbe("wsl", []string{"/mnt/d/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"}, nil, nil)
	p.Output = func(name string, args ...string) (string, error) {
		*calls = append(*calls, append([]string{name}, args...))
		switch {
		case name == "cmd.exe" && args[2] == "%ProgramFiles(x86)%":
			return "D:\\Program Files (x86)\r\n", nil
		case name == "cmd.exe" && args[2] == "%LOCALAPPDATA%":
			return "D:\\Profiles\\sam\\AppData\\Local\r\n", nil
		case name == "wslpath":
			return "/mnt/d" + strings.ReplaceAll(strings.TrimPrefix(args[1], "D:"), `\`, "/") + "\n", nil
		}
		return "", errors.New("no")
	}
	a, err := PlanAppWindow(p, "/state", "/p/a.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Profile, `D:\Profiles\sam\AppData\Local\ClawBridge\ui\`) || !strings.HasPrefix(a.mkdir, "/mnt/d/Profiles/sam/AppData/Local/ClawBridge/ui/") {
		t.Errorf("profile %q mkdir %q", a.Profile, a.mkdir)
	}
	for _, c := range *calls {
		if c[0] == "cmd.exe" && (len(c) != 4 || c[1] != "/c" || c[2] != "echo") {
			t.Errorf("cmd.exe call %q", c)
		}
	}
}

func TestSnapSelection(t *testing.T) {
	home := "/opt/sam"
	hash := profileHash("/p/a.toml")
	for _, tc := range []struct {
		name  string
		files []string
		path  string
		eval  string
		snap  string
	}{
		{"snap bin", nil, "/snap/bin/chromium", "/usr/bin/snap", "chromium"},
		{"resolved under /snap", nil, "/usr/local/bin/chromium", "/snap/brave/42/opt/brave", "brave"},
		{"ubuntu wrapper", []string{"/snap/bin/chromium"}, "/usr/bin/chromium-browser", "/usr/bin/chromium-browser", "chromium"},
		{"wrapper without snap", nil, "/usr/bin/chromium-browser", "/usr/bin/chromium-browser", ""},
		{"deb chrome", []string{"/snap/bin/chromium"}, "/usr/bin/google-chrome", "/opt/google/chrome/chrome", ""},
	} {
		p, _ := fakeProbe("linux", tc.files, []string{tc.path}, map[string]string{"HOME": home})
		p.EvalSymlinks = func(string) (string, error) { return tc.eval, nil }
		a, err := PlanAppWindow(p, "/state/ui", "/p/a.toml")
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if a.Browser.Snap != tc.snap {
			t.Errorf("%s: snap %q, want %q", tc.name, a.Browser.Snap, tc.snap)
		}
		if tc.snap == "" {
			if a.LaunchDir != "" || a.Profile != "/state/ui/"+hash+"/browser" {
				t.Errorf("%s: %q %q", tc.name, a.LaunchDir, a.Profile)
			}
			continue
		}
		dir := home + "/snap/" + tc.snap + "/common/clawbridge-ui/" + hash
		if a.LaunchDir != dir || a.Profile != dir+"/browser" {
			t.Errorf("%s: launch %q profile %q", tc.name, a.LaunchDir, a.Profile)
		}
	}
}

func TestProfileDirIs0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	old := syscallUmask(0)
	defer syscallUmask(old)
	state := filepath.Join(t.TempDir(), "ui")
	p, _ := fakeProbe("linux", nil, []string{"/usr/bin/chromium"}, nil)
	a, err := PlanAppWindow(p, state, "/p/a.toml")
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	a.start = func(_ context.Context, v []string) (bool, error) { argv = v; return true, nil }
	if ok, err := a.Opener()(context.Background(), "file:///x/launch-1.html"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	fi, err := os.Stat(a.Profile)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	if len(argv) != 5 {
		t.Errorf("argv %q", argv)
	}
}

// A snap's launch directory is accepted in exactly its shape, kept 0700, and
// holds the launch file; anything else is refused.
func TestSnapLaunchDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("snaps are Linux-only, and Windows has no mode bits")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := start(t, nil)
	good := filepath.Join(home, "snap", "chromium", "common", "clawbridge-ui", filepath.Base(r.srv.dir))
	target, err := r.srv.openTarget(good)
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(target, "file://")
	if filepath.Dir(path) != good {
		t.Errorf("launch file at %s", path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file %v %v", fi, err)
	}
	if fi, _ := os.Stat(good); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o", fi.Mode().Perm())
	}
	for _, bad := range []string{
		filepath.Join(home, "snap", "chromium", "common", "clawbridge-ui", "other"),
		filepath.Join(home, "snap", "chromium", "common", "clawbridge-ui", filepath.Base(r.srv.dir), ".."),
		filepath.Join(home, "snap", "chromium", "common"),
		filepath.Join(home, "elsewhere", "common", "clawbridge-ui", filepath.Base(r.srv.dir)),
		"/tmp",
	} {
		if _, err := r.srv.openTarget(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// At start the server sweeps stale launch files from every snap directory of
// the accepted shape, not only the one its config names, and only regular
// launch-*.html files in a private dir.
func TestStartSweepsStaleSnapLaunchFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(shortDir(t), "ui")
	hash := profileHash("/profiles/a.toml")
	mk := func(name string, mode os.FileMode) string {
		d := filepath.Join(home, "snap", name, "common", "clawbridge-ui", hash)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		os.Chmod(d, mode)
		return d
	}
	chromium, brave, loose := mk("chromium", 0o700), mk("brave", 0o700), mk("loose", 0o755)
	otherHash := filepath.Join(home, "snap", "chromium", "common", "clawbridge-ui", "000000000000")
	os.MkdirAll(otherHash, 0o700)
	write := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{write(chromium, "launch-aa.html"), write(brave, "launch-bb.html")}
	kept := []string{write(chromium, "keep.txt"), write(loose, "launch-cc.html"), write(otherHash, "launch-dd.html")}
	r := start(t, func(c *Config) { c.StateDir = state })
	_ = r
	for _, f := range gone {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("stale %s not removed", f)
		}
	}
	for _, f := range kept {
		if _, err := os.Lstat(f); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
}
