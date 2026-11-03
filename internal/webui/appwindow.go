package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"clawbridge/internal/auth"
)

// The app window (GUI-DESIGN §12.8): a Chromium-family browser started as
// its own app, with a profile directory of ClawBridge's own. Only fixed
// names and well-known locations are tried; nothing here comes from config,
// and nothing is ever run through a shell.

// ErrNoBrowser says no Chrome, Edge, Chromium or Brave was found.
var ErrNoBrowser = errors.New("no Chromium-family browser found")

// Probe is everything the browser search reads from the machine, so tests
// can fake it.
type Probe struct {
	// Platform is "darwin", "linux", "windows" or "wsl".
	Platform string
	// Exists says a file or directory is there.
	Exists       func(path string) bool
	LookPath     func(name string) (string, error)
	EvalSymlinks func(path string) (string, error)
	Getenv       func(key string) string
	// Output runs a fixed command and returns its stdout. It is used on WSL
	// only, to resolve folders (cmd.exe /c echo %VAR%, wslpath); it is never
	// given a URL or a token.
	Output   func(name string, args ...string) (string, error)
	MkdirAll func(path string, perm os.FileMode) error
	// WinFolder (native Windows) answers a known folder: "ProgramFiles",
	// "ProgramFilesX86" or "LocalAppData".
	WinFolder func(name string) string
	// WinAppPath (native Windows) answers the App Paths registry entry of an
	// executable such as "msedge.exe".
	WinAppPath func(exe string) string
}

// SystemProbe is the real machine.
func SystemProbe(platform string) Probe {
	p := Probe{
		Platform:     platform,
		Exists:       func(path string) bool { _, err := os.Stat(path); return err == nil },
		LookPath:     exec.LookPath,
		EvalSymlinks: filepath.EvalSymlinks,
		Getenv:       os.Getenv,
		MkdirAll:     os.MkdirAll,
		Output: func(name string, args ...string) (string, error) {
			b, err := exec.Command(name, args...).Output()
			return string(b), err
		},
	}
	systemWindows(&p)
	return p
}

// Browser is the browser picked.
type Browser struct {
	// Name is the product ("Google Chrome", "Microsoft Edge", ...).
	Name string
	// Path is the .app bundle on macOS, otherwise the executable. On WSL it
	// is the Linux-side path of the Windows .exe.
	Path string
	// Snap is the snap's name for a Linux snap browser, otherwise empty.
	Snap string
}

var (
	macApps   = [][2]string{{"Google Chrome", "Google Chrome.app"}, {"Microsoft Edge", "Microsoft Edge.app"}, {"Chromium", "Chromium.app"}, {"Brave", "Brave Browser.app"}}
	linuxBins = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "brave-browser"}
	// snapNames are the wrappers whose browser is a snap when
	// /snap/bin/<name> exists.
	snapNames = map[string]string{"chromium": "chromium", "chromium-browser": "chromium", "brave-browser": "brave"}
)

// winRel are the browsers' install paths below a Program Files folder.
const (
	edgeRel   = `Microsoft\Edge\Application\msedge.exe`
	chromeRel = `Google\Chrome\Application\chrome.exe`
)

// FindBrowser picks the first browser found, in GUI-DESIGN §12.8's order.
func FindBrowser(p Probe) (Browser, bool) {
	switch p.Platform {
	case "darwin":
		home := p.Getenv("HOME")
		for _, a := range macApps {
			dirs := []string{"/Applications"}
			if home != "" {
				dirs = append(dirs, path.Join(home, "Applications"))
			}
			for _, d := range dirs {
				if app := path.Join(d, a[1]); p.Exists(app) {
					return Browser{Name: a[0], Path: app}, true
				}
			}
		}
	case "linux":
		for _, name := range linuxBins {
			path, err := p.LookPath(name)
			if err != nil || path == "" {
				continue
			}
			return Browser{Name: name, Path: path, Snap: snapOf(p, name, path)}, true
		}
	case "windows":
		for _, c := range []struct{ name, rel, exe string }{{"Microsoft Edge", edgeRel, "msedge.exe"}, {"Google Chrome", chromeRel, "chrome.exe"}} {
			var cands []string
			if p.WinFolder != nil {
				for _, f := range []string{"ProgramFilesX86", "ProgramFiles"} {
					if dir := p.WinFolder(f); dir != "" {
						cands = append(cands, dir+`\`+c.rel)
					}
				}
				if c.exe == "chrome.exe" {
					if dir := p.WinFolder("LocalAppData"); dir != "" {
						cands = append(cands, dir+`\`+c.rel)
					}
				}
			}
			if p.WinAppPath != nil {
				if a := p.WinAppPath(c.exe); a != "" {
					cands = append(cands, a)
				}
			}
			for _, path := range cands {
				if p.Exists(path) {
					return Browser{Name: c.name, Path: path}, true
				}
			}
		}
	case "wsl":
		for _, c := range []struct{ name, rel string }{{"Microsoft Edge", edgeRel}, {"Google Chrome", chromeRel}} {
			rel := strings.ReplaceAll(c.rel, `\`, "/")
			for _, f := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
				if dir, _, ok := wslFolder(p, f); ok {
					if path := dir + "/" + rel; p.Exists(path) {
						return Browser{Name: c.name, Path: path}, true
					}
				}
			}
			if c.name == "Google Chrome" {
				if dir, _, ok := wslFolder(p, "LOCALAPPDATA"); ok {
					if path := dir + "/" + rel; p.Exists(path) {
						return Browser{Name: c.name, Path: path}, true
					}
				}
			}
		}
	}
	return Browser{}, false
}

// snapOf is the snap's name when the browser found at path is a snap: its
// resolved path is under /snap/, or it is /snap/bin/<name>, or it is a known
// wrapper (Ubuntu's /usr/bin/chromium-browser) and /snap/bin/<name> exists.
func snapOf(p Probe, name, browserPath string) string {
	if strings.HasPrefix(browserPath, "/snap/bin/") {
		return path.Base(browserPath)
	}
	if p.EvalSymlinks != nil {
		if r, err := p.EvalSymlinks(browserPath); err == nil && strings.HasPrefix(r, "/snap/") {
			if parts := strings.Split(strings.TrimPrefix(r, "/snap/"), "/"); len(parts) > 1 && parts[0] != "" && parts[0] != "bin" {
				return parts[0]
			}
		}
	}
	if sn, ok := snapNames[name]; ok && p.Exists("/snap/bin/"+sn) {
		return sn
	}
	return ""
}

var winAbs = regexp.MustCompile(`^[A-Za-z]:\\[^%"<>|*?\r\n]*$`)

// wslFolder resolves a Windows folder variable such as ProgramFiles: the
// Windows form and the Linux form. The only command run is cmd.exe /c echo
// %VAR% for a fixed VAR, then wslpath -u; no URL or token is ever involved.
func wslFolder(p Probe, v string) (unix, win string, ok bool) {
	if p.Output == nil {
		return "", "", false
	}
	out, err := p.Output("cmd.exe", "/c", "echo", "%"+v+"%")
	win = strings.TrimSpace(out)
	if err != nil || !winAbs.MatchString(win) {
		return "", "", false
	}
	out, err = p.Output("wslpath", "-u", win)
	unix = strings.TrimSpace(out)
	if err != nil || !strings.HasPrefix(unix, "/") {
		return "", "", false
	}
	return unix, win, true
}

// AppWindow opens the UI as a browser app window.
type AppWindow struct {
	Browser Browser
	// Profile is the --user-data-dir value as the browser reads it (a
	// Windows path for a Windows browser).
	Profile string
	// LaunchDir is a snap browser's launch directory, else empty (see
	// Config.LaunchDir).
	LaunchDir string

	platform string
	mkdir    string // where Profile is created, as this process sees it
	chmod    bool
	probe    Probe
	// start runs the argv; tests replace it.
	start func(ctx context.Context, argv []string) (bool, error)
}

// PlanAppWindow finds a browser and works out its profile directory, without
// creating anything. It returns ErrNoBrowser when none is found.
func PlanAppWindow(p Probe, stateDir, profile string) (*AppWindow, error) {
	b, ok := FindBrowser(p)
	if !ok {
		return nil, ErrNoBrowser
	}
	hash := profileHash(profile)
	a := &AppWindow{Browser: b, platform: p.Platform, probe: p, start: startDetached}
	switch {
	case b.Snap != "":
		home := p.Getenv("HOME")
		if home == "" {
			return nil, errors.New("no home directory for the snap browser's profile")
		}
		// These are the platform's paths, whatever OS runs this code.
		a.LaunchDir = path.Join(home, "snap", b.Snap, "common", "clawbridge-ui", hash)
		a.Profile = path.Join(a.LaunchDir, "browser")
		a.mkdir, a.chmod = a.Profile, true
	case p.Platform == "darwin" || p.Platform == "linux":
		a.Profile = path.Join(stateDir, hash, "browser")
		a.mkdir, a.chmod = a.Profile, true
	case p.Platform == "windows":
		if p.WinFolder == nil || p.WinFolder("LocalAppData") == "" {
			return nil, errors.New("no LocalAppData folder")
		}
		a.Profile = p.WinFolder("LocalAppData") + `\ClawBridge\ui\` + hash + `\browser`
		a.mkdir = a.Profile
	case p.Platform == "wsl":
		unix, win, ok := wslFolder(p, "LOCALAPPDATA")
		if !ok {
			return nil, errors.New("cannot resolve the Windows LOCALAPPDATA folder")
		}
		a.Profile = win + `\ClawBridge\ui\` + hash + `\browser`
		a.mkdir = unix + "/ClawBridge/ui/" + hash + "/browser"
	default:
		return nil, ErrNoBrowser
	}
	return a, nil
}

// Argv is the whole command, an exact list (no shell).
func (a *AppWindow) Argv(target string) []string {
	flags := []string{"--user-data-dir=" + a.Profile, "--app=" + target, "--no-first-run", "--no-default-browser-check"}
	if a.platform == "darwin" {
		return append([]string{"/usr/bin/open", "-na", a.Browser.Path, "--args"}, flags...)
	}
	return append([]string{a.Browser.Path}, flags...)
}

// Opener opens target as an app window. The profile directory is created
// (0700 where modes exist) when it first opens.
func (a *AppWindow) Opener() Opener {
	return func(ctx context.Context, target string) (bool, error) {
		mk := a.probe.MkdirAll
		if mk == nil {
			mk = os.MkdirAll
		}
		if err := mk(a.mkdir, 0o700); err != nil {
			return false, fmt.Errorf("cannot create the browser profile directory %s: %w", a.mkdir, err)
		}
		if a.chmod {
			// Tighten what a umask or an earlier run left; the parents are
			// ClawBridge's own too.
			if err := os.Chmod(a.mkdir, 0o700); err != nil {
				return false, fmt.Errorf("cannot make the browser profile directory %s private: %w", a.mkdir, err)
			}
		}
		return a.start(ctx, a.Argv(target))
	}
}

func startDetached(ctx context.Context, argv []string) (bool, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	detach(cmd)
	return auth.LaunchCommand(ctx, cmd, openerWait)
}
