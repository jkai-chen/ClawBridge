// Package webui is the local web UI's HTTP server: a loopback listener, the
// launch-token and session-secret checks, the embedded bundle, and the idle
// exit (live/GUI-DESIGN.md §12). It serves internal/guibackend's handlers
// and is the only place they are mounted.
package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/guibackend"
)

//go:embed dist
var embedded embed.FS

// Defaults for Config.
const (
	DefaultLaunchTTL         = 60 * time.Second
	DefaultIdleGrace         = 60 * time.Second
	DefaultStartGrace        = 2 * time.Minute
	DefaultEventWriteTimeout = 30 * time.Second
	DefaultBodyReadTimeout   = 30 * time.Second
)

// Serve's results when the server stops by itself.
var (
	// ErrIdle: no tab has been connected to /events for IdleGrace.
	ErrIdle = errors.New("no tab has been connected for a while")
	// ErrNeverConnected: no tab connected to /events within StartGrace.
	ErrNeverConnected = errors.New("no tab connected after the start")
)

// AlreadyRunning is New's error when another UI holds the lock.
type AlreadyRunning struct{}

func (*AlreadyRunning) Error() string { return "another clawbridge ui is running for this profile" }

// Config configures a Server. Zero values take the defaults.
type Config struct {
	// Backend is mounted at /rpc and /events. Its fields may be set after
	// New and before Serve.
	Backend *guibackend.Server
	// Profile is the resolved path of the client config this UI serves. One
	// UI runs per profile: the lock, socket and info file live in
	// StateDir/<hash of Profile>.
	Profile string
	// StateDir is the base of the per-profile state directories. Each holds the lock, the control socket and the launch files. It
	// is made a 0700 directory.
	StateDir string
	// Host must be empty or 127.0.0.1: nothing else is ever bound (C1).
	Host string
	// Port 0 picks a random port.
	Port int
	// Platform is guibackend.Platform()'s answer; it decides how a launch
	// reaches the browser (C2).
	Platform string
	Opener   Opener
	// Log gets one line per request: method, path, status (C12).
	Log io.Writer
	// Bundle holds index.html and the assets; the embedded placeholder by
	// default.
	Bundle fs.FS

	LaunchTTL         time.Duration
	IdleGrace         time.Duration
	StartGrace        time.Duration
	EventWriteTimeout time.Duration
	// BodyReadTimeout bounds reading a request body (any non-GET).
	BodyReadTimeout time.Duration
	// Poll is how often the idle watch looks.
	Poll time.Duration
}

// Server is the local web UI.
type Server struct {
	cfg      Config
	dir      string
	platform string
	opener   Opener
	port     string
	ln       net.Listener
	ctl      net.Listener
	unlock   func()
	tokens   *tokens
	log      *logger
	bundle   fs.FS
	index    []byte

	started time.Time
	http    *http.Server
	once    sync.Once
}

// New takes the profile's lock, binds 127.0.0.1 and the control socket. It
// returns *AlreadyRunning if another UI holds the lock. Nothing is served
// until Serve.
func New(cfg Config) (*Server, error) {
	if cfg.Host != "" && cfg.Host != "127.0.0.1" {
		return nil, fmt.Errorf("the UI listens on 127.0.0.1 only, not %q", cfg.Host)
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, errors.New("port out of range")
	}
	if cfg.Backend == nil {
		return nil, errors.New("webui: no backend")
	}
	if cfg.Platform == "" {
		cfg.Platform = guibackend.Platform()
	}
	if cfg.Opener == nil {
		cfg.Opener = SystemOpener(cfg.Platform)
	}
	if cfg.LaunchTTL <= 0 {
		cfg.LaunchTTL = DefaultLaunchTTL
	}
	if cfg.IdleGrace <= 0 {
		cfg.IdleGrace = DefaultIdleGrace
	}
	if cfg.StartGrace <= 0 {
		cfg.StartGrace = DefaultStartGrace
	}
	if cfg.EventWriteTimeout <= 0 {
		cfg.EventWriteTimeout = DefaultEventWriteTimeout
	}
	if cfg.BodyReadTimeout <= 0 {
		cfg.BodyReadTimeout = DefaultBodyReadTimeout
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	bundle := cfg.Bundle
	if bundle == nil {
		sub, err := fs.Sub(embedded, "dist")
		if err != nil {
			return nil, err
		}
		bundle = sub
	}
	index, err := fs.ReadFile(bundle, "index.html")
	if err != nil {
		return nil, errors.New("the UI bundle has no index.html")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("webui: no state directory")
	}
	base, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	dir := ProfileDir(base, cfg.Profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, d := range []string{base, dir} {
		if err := checkPrivateDir(d); err != nil {
			return nil, err
		}
	}
	unlock, err := lockFile(filepath.Join(dir, lockName))
	if errors.Is(err, errLocked) {
		return nil, &AlreadyRunning{}
	}
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, dir: dir, platform: cfg.Platform, opener: cfg.Opener, unlock: unlock,
		tokens: newTokens(cfg.LaunchTTL), log: &logger{w: cfg.Log}, bundle: bundle, index: index}
	// The lock is ours: launch files left by a crash are stale.
	if old, _ := filepath.Glob(filepath.Join(dir, "launch-*.html")); old != nil {
		for _, f := range old {
			os.Remove(f)
		}
	}
	s.ln, err = net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		s.Close()
		return nil, err
	}
	s.port = strconv.Itoa(s.ln.Addr().(*net.TCPAddr).Port)
	if err := s.listenControl(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.writeInfo(); err != nil {
		s.Close()
		return nil, err
	}
	if cfg.Backend.Log == nil {
		cfg.Backend.Log = cfg.Log
	}
	return s, nil
}

// ProfileDir is the state directory of one profile: base/<12 hex of the
// sha256 of the cleaned absolute profile path>.
func ProfileDir(base, profile string) string {
	if abs, err := filepath.Abs(profile); err == nil {
		profile = abs
	}
	sum := sha256.Sum256([]byte(filepath.Clean(profile)))
	return filepath.Join(base, hex.EncodeToString(sum[:])[:12])
}

// URL is where the server runs, without a token.
func (s *Server) URL() string { return "http://127.0.0.1:" + s.port }

// Addr is the listener's address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// StateDir is the state directory in use.
func (s *Server) StateDir() string { return s.dir }

// Close stops the server and removes what it made. It is safe to call more
// than once.
func (s *Server) Close() {
	s.once.Do(func() {
		if s.http != nil {
			s.http.Close()
		}
		if s.ln != nil {
			s.ln.Close()
		}
		if s.ctl != nil {
			s.ctl.Close()
		}
		s.tokens.closeAll()
		os.Remove(filepath.Join(s.dir, socketName))
		os.Remove(filepath.Join(s.dir, infoName))
		s.unlock()
	})
}

// Serve serves until ctx ends (nil), the idle watch stops it (ErrIdle or
// ErrNeverConnected) or the listener fails. C11: it stops IdleGrace after
// the last /events connection closes, and StartGrace after Serve starts if
// none ever connected. It closes the server before it returns.
func (s *Server) Serve(ctx context.Context) error {
	defer s.Close()
	s.started = time.Now()
	s.http = &http.Server{
		Handler:           s.secure(http.HandlerFunc(s.route)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	failed := make(chan error, 1)
	go func() { failed <- s.http.Serve(s.ln) }()
	if s.ctl != nil {
		go s.serveControl()
	}
	tick := time.NewTicker(s.cfg.Poll)
	defer tick.Stop()
	var firstSeen time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-tick.C:
		}
		now := time.Now()
		b := s.cfg.Backend
		if b.Connected() > 0 {
			if firstSeen.IsZero() {
				firstSeen = now
			}
			continue
		}
		// LastDisconnect is zero before the first connection, so the start
		// is the reference for the never-connected case, not it.
		last := b.LastDisconnect()
		if last.IsZero() && firstSeen.IsZero() {
			if now.Sub(s.started) >= s.cfg.StartGrace {
				return ErrNeverConnected
			}
			continue
		}
		if last.Before(firstSeen) {
			last = firstSeen
		}
		if now.Sub(last) >= s.cfg.IdleGrace {
			return ErrIdle
		}
	}
}

// route serves the paths behind the security middleware.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/rpc":
		s.cfg.Backend.Handler().ServeHTTP(w, r)
	case "/events":
		s.cfg.Backend.Events().ServeHTTP(w, r)
	case "/":
		s.serveShell(w, r)
	default:
		s.serveAsset(w, r)
	}
}

// serveShell is C3: the shell, with a fresh session secret in a one-time
// meta only for a valid launch token. A token that is present but unknown,
// used or expired gets the plain shell with a 403, never a secret.
func (s *Server) serveShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		refuse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	status, body := http.StatusOK, s.index
	if tok := r.URL.Query().Get("t"); r.URL.Query().Has("t") {
		if secret, ok := s.tokens.redeem(tok); ok {
			body = withSessionMeta(s.index, secret)
		} else {
			status = http.StatusForbidden
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// withSessionMeta inserts the one-time meta after <head>. The secret is hex;
// it is escaped anyway.
func withSessionMeta(index []byte, secret string) []byte {
	meta := []byte(`<meta name="cb-session" content="` + html.EscapeString(secret) + `">`)
	at := bytes.Index(bytes.ToLower(index), []byte("<head>"))
	if at < 0 {
		return append(meta, index...)
	}
	at += len("<head>")
	out := make([]byte, 0, len(index)+len(meta))
	out = append(out, index[:at]...)
	out = append(out, meta...)
	return append(out, index[at:]...)
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		refuse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "index.html" {
		refuse(w, http.StatusNotFound, "not found")
		return
	}
	fi, err := fs.Stat(s.bundle, name)
	if err != nil || !fi.Mode().IsRegular() {
		refuse(w, http.StatusNotFound, "not found")
		return
	}
	b, err := fs.ReadFile(s.bundle, name)
	if err != nil {
		refuse(w, http.StatusNotFound, "not found")
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}
