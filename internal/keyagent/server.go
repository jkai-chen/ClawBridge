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
	"syscall"
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
	// ServerOrigin is the only origin sign serves (compared in lower case);
	// the profile's server origin. Serve refuses to start without it.
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
	// listenFor makes the agent's listener; tests replace it with a fake.
	listenFor = listen
	// acceptBackoffMin and acceptBackoffMax bound the pause after a
	// temporary accept error. The pause doubles on each failure in a row.
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// maxConns is how many connections the agent serves at once. When all are
// busy the accept loop waits for one to finish, and further clients wait in
// the socket's backlog, so a same-user program cannot make the agent run
// out of descriptors or memory.
const maxConns = 16

// errNoOrigin is Serve's refusal to run without the profile's server
// origin, since sign would then accept any origin.
var errNoOrigin = errors.New("the key agent needs the profile's server origin")

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
	if opts.ServerOrigin == "" {
		return errNoOrigin
	}
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
	ln, err := listenFor(state)
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
		acceptLoop(ctx, ln, h, &wg, cancel)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.watch(ctx, state)
	}()

	<-ctx.Done()
	ln.Close()
	wg.Wait()
	return nil
}

// acceptLoop serves connections until ctx ends. A temporary accept error is
// retried with a growing pause. Any other error ends the agent through
// cancel, so it never sits unreachable with the key unlocked.
func acceptLoop(ctx context.Context, ln listener, h *handler, wg *sync.WaitGroup, cancel func()) {
	slots := make(chan struct{}, maxConns)
	var pause time.Duration
	for {
		// A slot is taken before accepting, so a full agent leaves further
		// clients in the backlog instead of holding their descriptors.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		c, err := ln.Accept()
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return
			}
			if !temporaryAccept(err) {
				cancel()
				return
			}
			pause = nextPause(pause)
			t := time.NewTimer(pause)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return
			}
			continue
		}
		pause = 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			h.serveConn(c)
		}()
	}
}

// nextPause is the pause after a temporary accept error, given the last.
func nextPause(last time.Duration) time.Duration {
	if last <= 0 {
		return acceptBackoffMin
	}
	if last *= 2; last > acceptBackoffMax {
		last = acceptBackoffMax
	}
	return last
}

// temporaryAccept reports whether an accept error may pass: running out of
// descriptors, an aborted connection, an interrupted call or a timeout.
func temporaryAccept(err error) bool {
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ECONNABORTED, syscall.EINTR} {
		if errors.Is(err, e) {
			return true
		}
	}
	var ne interface {
		Timeout() bool
		Temporary() bool
	}
	return errors.As(err, &ne) && (ne.Timeout() || ne.Temporary())
}

// watch locks the agent and ends it when the idle timeout passes or the
// state directory is gone.
func (h *handler) watch(ctx context.Context, state string) {
	idle := time.NewTicker(idleCheckEvery)
	defer idle.Stop()
	dir := time.NewTicker(stateCheckEvery)
	defer dir.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			if h.lockIfIdle() {
				return
			}
		case <-dir.C:
			if _, err := os.Stat(state); errors.Is(err, os.ErrNotExist) {
				h.lock()
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
