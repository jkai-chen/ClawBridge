package webui

import (
	"context"
	"errors"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
func (s *Server) OpenTarget() (string, error) {
	tok, u, err := s.tokenURL()
	if err != nil {
		return "", err
	}
	if !fileRoute(s.platform) {
		return u, nil
	}
	path, err := s.writeLaunchFile(u)
	if err != nil {
		return "", err
	}
	s.tokens.attachFile(tok, path)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String(), nil
}

// writeLaunchFile writes the 0600 page that sends the browser to u. It has
// no script: a zero-delay meta refresh replaces the file's history entry.
func (s *Server) writeLaunchFile(u string) (string, error) {
	name, err := randomHex(8)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, "launch-"+name+".html")
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
