package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The control socket (unix) lets a second `clawbridge ui` ask the running
// server for a fresh launch. It is a 0600 socket in the 0700 state dir. On
// Windows there is no control channel for now: a second instance reads the
// info file (ui.json, no secrets) and prints where the first one runs.
const (
	socketName = "control.sock"
	infoName   = "ui.json"
	lockName   = "ui.lock"
	// maxSocketPath keeps sun_path within macOS's 104 bytes.
	maxSocketPath = 100
)

type controlRequest struct {
	// Want is "open" (what the opener is given) or "url" (a token URL for
	// the person's terminal).
	Want string `json:"want"`
}

type controlReply struct {
	Profile string `json:"profile,omitempty"`
	Target  string `json:"target,omitempty"`
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
}

type info struct {
	PID     int    `json:"pid"`
	Profile string `json:"profile"`
	URL     string `json:"url"`
}

func controlAvailable(dir string) bool {
	return runtime.GOOS != "windows" && len(filepath.Join(dir, socketName)) <= maxSocketPath
}

func (s *Server) listenControl() error {
	if !controlAvailable(s.dir) {
		return nil
	}
	path := filepath.Join(s.dir, socketName)
	// The lock is held, so a socket file here is stale.
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.ctl = ln
	return nil
}

func (s *Server) serveControl() {
	for {
		c, err := s.ctl.Accept()
		if err != nil {
			return
		}
		go s.handleControl(c)
	}
}

func (s *Server) handleControl(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReaderSize(c, 1024).ReadBytes('\n')
	if err != nil {
		return
	}
	var req controlRequest
	var rep controlReply
	switch {
	case json.Unmarshal(line, &req) != nil:
		rep.Error = "bad request"
	case req.Want == "url":
		rep.Target, err = s.NoOpenURL()
	case req.Want == "open":
		rep.Target, err = s.OpenTarget()
	default:
		rep.Error = "bad request"
	}
	if err != nil {
		rep.Target, rep.Error = "", "could not make a launch"
	}
	rep.URL = s.URL()
	rep.Profile = s.profile()
	json.NewEncoder(c).Encode(rep)
}

func (s *Server) writeInfo() error {
	b, _ := json.Marshal(info{PID: os.Getpid(), Profile: s.profile(), URL: s.URL()})
	return os.WriteFile(filepath.Join(s.dir, infoName), b, 0o600)
}

// Handoff is a fresh launch the running server made.
type Handoff struct {
	// Target is what to give the opener (want "open") or print (want "url").
	Target string
	// URL is where the server runs, without a token.
	URL string
}

// NoControlError says a UI runs but cannot be asked for a launch.
type NoControlError struct {
	// URL is where it runs, if known.
	URL string
}

func (e *NoControlError) Error() string {
	if e.URL != "" {
		return "ClawBridge UI is already running on " + e.URL + " but cannot be asked for a new tab from here"
	}
	return "ClawBridge UI is already running but cannot be asked for a new tab from here"
}

// RequestHandoff asks the running server for a fresh launch. want is "open"
// or "url". With no control channel it returns a *NoControlError.
func RequestHandoff(ctx context.Context, stateDir, profile, want string) (Handoff, error) {
	return requestHandoffAt(ctx, ProfileDir(stateDir, profile), profile, want)
}

func requestHandoffAt(ctx context.Context, stateDir, profile, want string) (Handoff, error) {
	var running info
	if b, err := os.ReadFile(filepath.Join(stateDir, infoName)); err == nil {
		json.Unmarshal(b, &running)
	}
	if running.Profile != "" && running.Profile != cleanProfile(profile) {
		return Handoff{}, &ProfileMismatchError{}
	}
	if !controlAvailable(stateDir) {
		return Handoff{}, &NoControlError{URL: running.URL}
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "unix", filepath.Join(stateDir, socketName))
	if err != nil {
		return Handoff{}, &NoControlError{URL: running.URL}
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(controlRequest{Want: want}); err != nil {
		return Handoff{}, err
	}
	var rep controlReply
	if err := json.NewDecoder(c).Decode(&rep); err != nil {
		return Handoff{}, err
	}
	if rep.Error != "" || rep.Target == "" {
		return Handoff{}, errors.New("the running ClawBridge UI refused: " + rep.Error)
	}
	if rep.Profile != cleanProfile(profile) {
		return Handoff{}, &ProfileMismatchError{}
	}
	return Handoff{Target: rep.Target, URL: rep.URL}, nil
}

// ProfileMismatchError says the UI that answered serves another profile.
type ProfileMismatchError struct{}

func (*ProfileMismatchError) Error() string {
	return "the running ClawBridge UI serves a different profile; it was not used"
}

func cleanProfile(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

func (s *Server) profile() string { return cleanProfile(s.cfg.Profile) }
