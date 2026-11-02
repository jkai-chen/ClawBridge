package keyagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/sealed"
)

// ErrAlreadyRunning is Serve's answer when an agent already serves the
// state directory.
var ErrAlreadyRunning = errors.New("a key agent already serves this state directory")

// Options configure Serve.
type Options struct {
	// IdleTimeout ends the agent after this long without a request; 0 is
	// off. Status requests do not count.
	IdleTimeout time.Duration
	// ClockOffset is the profile's stored server-time correction (server
	// time minus this machine's time, as devicesign.Signer keeps it). sign
	// accepts a timestamp within MaxTimestampSkew of now plus this offset.
	ClockOffset time.Duration
	// ServerOrigin, when set, is the only origin sign serves (compared in
	// lower case); the profile's server origin.
	ServerOrigin string
	// Logf, when set, receives one line for each hardening step that did
	// not apply. It is never given key material.
	Logf func(format string, args ...any)
}

// Timing and platform seams; tests replace them.
var (
	// stateCheckEvery is how often the state directory is checked.
	stateCheckEvery = 30 * time.Second
	// idleCheckEvery is how often the idle timeout is checked.
	idleCheckEvery = time.Second
	// harden applies the process hardening before the key is touched.
	harden = Harden
	// afterOpen is a test hook given the store the agent serves.
	afterOpen func(*sealed.Store)
	clock     = time.Now
)

// stateHash is the first 16 hex digits of the SHA-256 of the canonical
// state directory path.
func stateHash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// canonicalState resolves symlinks and makes the path absolute.
func canonicalState(stateDir string) (string, error) {
	if stateDir == "" {
		return "", errors.New("no state directory")
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Serve is the key agent for stateDir. kid and key are the unwrapped data
// key; Serve takes ownership of the key buffer and has zeroed it by the
// time it returns (it zeroes it as soon as the store has its copy). It runs
// until ctx ends, a lock request, the idle timeout, or the state directory
// disappearing, and returns nil for each of those. It returns
// ErrAlreadyRunning if an agent already answers for the same state
// directory, and any setup failure.
func Serve(ctx context.Context, stateDir, kid string, key []byte, opts Options) error {
	defer zero(key)
	state, err := canonicalState(stateDir)
	if err != nil {
		return err
	}
	if harden != nil {
		for _, msg := range harden() {
			if opts.Logf != nil {
				opts.Logf("key agent hardening: %s", msg)
			}
		}
	}
	store, err := sealed.OpenUnlocked(state, kid, key)
	// The store has its own copy; this buffer is no longer needed.
	zero(key)
	if err != nil {
		return err
	}
	defer store.Lock()
	if afterOpen != nil {
		afterOpen(store)
	}
	ln, err := listen(state)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := &handler{store: store, kid: kid[:8], idle: opts.IdleTimeout, offset: opts.ClockOffset, origin: strings.ToLower(opts.ServerOrigin), now: clock, stop: cancel}
	h.lastReq = h.now()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.serveConn(c)
			}()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.watch(ctx, state, cancel)
	}()

	<-ctx.Done()
	ln.Close()
	wg.Wait()
	return nil
}

// watch ends the agent when the idle timeout passes or the state directory
// is gone.
func (h *handler) watch(ctx context.Context, state string, stop func()) {
	idle := time.NewTicker(idleCheckEvery)
	defer idle.Stop()
	dir := time.NewTicker(stateCheckEvery)
	defer dir.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			if h.idleExpired() {
				stop()
				return
			}
		case <-dir.C:
			if _, err := os.Stat(state); errors.Is(err, os.ErrNotExist) {
				stop()
				return
			}
		}
	}
}

// serveConn answers one request on c and closes it.
func (h *handler) serveConn(c conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(readTimeout))
	req, err := readRequest(c)
	var resp *response
	if err != nil {
		resp = failure(err)
	} else {
		resp = h.do(req)
		req.wipe()
	}
	b, err := json.Marshal(resp)
	zero(resp.Plaintext)
	if err != nil {
		return
	}
	if len(b) > MaxFrame {
		zero(b)
		b, _ = json.Marshal(failure(newError(CodeInternal)))
	}
	c.SetDeadline(time.Now().Add(readTimeout))
	writeFrame(c, b)
	zero(b)
}

// conn is one connection, as both ends use it.
type conn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// listener accepts connections from this user only.
type listener interface {
	Accept() (conn, error)
	Close() error
}
