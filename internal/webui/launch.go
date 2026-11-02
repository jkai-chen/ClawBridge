package webui

import (
	"context"
	"errors"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clawbridge/internal/auth"
)

// Opener opens target (a file:// URL or an http URL) in the person's
// browser. It reports whether it is known to have opened; false with no
// error means the outcome is unknown.
type Opener func(ctx context.Context, target string) (bool, error)

const openerWait = 5 * time.Second

// SystemOpener is the platform's opener. On WSL it is wslview if present,
// otherwise cmd.exe /c start; with neither it fails, and the caller says
// how to get a link.
func SystemOpener(platform string) Opener {
	if platform != "wsl" {
		return func(ctx context.Context, target string) (bool, error) {
			return auth.LaunchBrowser(ctx, target, openerWait)
		}
	}
	return func(ctx context.Context, target string) (bool, error) {
		if p, err := exec.LookPath("wslview"); err == nil {
			return auth.LaunchCommand(ctx, exec.Command(p, target), openerWait)
		}
		if p, err := exec.LookPath("cmd.exe"); err == nil {
			return auth.LaunchCommand(ctx, exec.Command(p, "/c", "start", "", target), openerWait)
		}
		return false, errors.New("no browser opener found (wslview or cmd.exe)")
	}
}

// fileRoute says whether the opener gets a launch file's file:// path (C2,
// R1-T2). Elsewhere (native Windows, and WSL through wslview or cmd.exe,
// which run Windows-side where other non-admin users cannot read the argv)
// it gets the URL with the token. That exception is written in GUI-DESIGN
// §12.3 C2.
// FileRoute says whether platform opens a launch file (see fileRoute).
func FileRoute(platform string) bool { return fileRoute(platform) }

func fileRoute(platform string) bool { return platform == "darwin" || platform == "linux" }

// tokenURL issues a launch token and the URL that redeems it.
func (s *Server) tokenURL() (tok, u string, err error) {
	tok, err = s.tokens.issue()
	if err != nil {
		return "", "", err
	}
	return tok, s.URL() + "/?t=" + tok, nil
}

// NoOpenURL is a fresh single-use URL for the person's own terminal.
func (s *Server) NoOpenURL() (string, error) {
	_, u, err := s.tokenURL()
	return u, err
}

// OpenTarget is what the opener is given: the launch file's file:// URL on
// darwin and linux, the token URL where argv is private.
func (s *Server) OpenTarget() (string, error) { return s.openTarget(s.cfg.LaunchDir) }

// launchDirFor is where a launch file goes: the state directory, or a snap
// browser's own directory when dir has exactly the shape
// ~/snap/<name>/common/clawbridge-ui/<this profile's hash>.
func (s *Server) launchDirFor(dir string) (string, error) {
	if dir == "" {
		return s.dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(filepath.Join(home, "snap"), dir)
	if err != nil {
		return "", errors.New("launch directory refused")
	}
	p := strings.Split(rel, string(filepath.Separator))
	if len(p) != 4 || p[0] == "" || p[0] == ".." || p[0] == "." || p[1] != "common" || p[2] != "clawbridge-ui" || p[3] != filepath.Base(s.dir) {
		return "", errors.New("launch directory refused")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := checkPrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Server) openTarget(launchDir string) (string, error) {
	dir, err := s.launchDirFor(launchDir)
	if err != nil {
		return "", err
	}
	tok, u, err := s.tokenURL()
	if err != nil {
		return "", err
	}
	if !fileRoute(s.platform) {
		return u, nil
	}
	path, err := s.writeLaunchFile(u, dir)
	if err != nil {
		return "", err
	}
	s.tokens.attachFile(tok, path)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String(), nil
}

// writeLaunchFile writes the 0600 page that sends the browser to u. It has
// no script: a zero-delay meta refresh replaces the file's history entry.
func (s *Server) writeLaunchFile(u, dir string) (string, error) {
	name, err := randomHex(8)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "launch-"+name+".html")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	page := `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=` + html.EscapeString(u) + `"><title>ClawBridge</title></head><body>Opening ClawBridge UI</body></html>` + "\n"
	_, werr := f.WriteString(page)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return "", werr
	}
	return path, nil
}

// Open opens a new tab on this server with the configured opener.
func (s *Server) Open(ctx context.Context) (bool, error) {
	target, err := s.OpenTarget()
	if err != nil {
		return false, err
	}
	return s.opener(ctx, target)
}

// sweepSnapLaunchFiles removes stale launch files a hand-off left in any
// ~/snap/<name>/common/clawbridge-ui/<hash> directory, not only the one this
// server's config names. Only regular launch-*.html files in a private
// directory of ours are removed.
func sweepSnapLaunchFiles(hash string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	files, _ := filepath.Glob(filepath.Join(home, "snap", "*", "common", "clawbridge-ui", hash, "launch-*.html"))
	for _, f := range files {
		// A dir that is not already private is left alone, not tightened.
		if di, err := os.Lstat(filepath.Dir(f)); err != nil || !di.IsDir() || di.Mode().Perm()&0o077 != 0 || checkPrivateDir(filepath.Dir(f)) != nil {
			continue
		}
		if fi, err := os.Lstat(f); err == nil && fi.Mode().IsRegular() {
			os.Remove(f)
		}
	}
}
