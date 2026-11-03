//go:build !windows

package keyagent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"clawbridge/internal/sealed"
)

var errFakeClosed = errors.New("the fake listener is closed")

// fakeListener hands out what its accept function returns; Close wakes a
// blocked Accept.
type fakeListener struct {
	accept func() (conn, error)
	done   chan struct{}
	once   sync.Once
}

func newFakeListener(accept func() (conn, error)) *fakeListener {
	return &fakeListener{accept: accept, done: make(chan struct{})}
}

func (f *fakeListener) Accept() (conn, error) { return f.accept() }

func (f *fakeListener) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}

// block waits until the listener is closed, as Accept does with no client.
func (f *fakeListener) block() (conn, error) {
	<-f.done
	return nil, errFakeClosed
}

// serveFake runs Serve on a fresh state with ln as its listener and returns
// the store it serves and the channel its result arrives on.
func serveFake(t *testing.T, ln listener) (*sealed.Store, context.CancelFunc, chan error) {
	t.Helper()
	seams(t)
	oListen, oMin := listenFor, acceptBackoffMin
	listenFor, acceptBackoffMin = func(string) (listener, error) { return ln, nil }, time.Millisecond
	var store *sealed.Store
	ready := make(chan struct{})
	afterOpen = func(s *sealed.Store) {
		s.Seal(sealed.KindLinear, linA, []byte("tok"))
		store = s
		close(ready)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	state := newState(t)
	key := append([]byte(nil), fixKey...)
	go func() {
		done <- Serve(ctx, state, fixKid, key, Options{ServerOrigin: testOrigin})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		listenFor, acceptBackoffMin = oListen, oMin
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve returned before opening the store: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not open the store")
	}
	return store, cancel, done
}

const testOrigin = "https://hub.example.test"

func requireLocked(t *testing.T, s *sealed.Store) {
	t.Helper()
	if _, err := s.Unseal(sealed.KindLinear, linA); sealed.CodeOf(err) != sealed.CodePassphraseMode {
		t.Fatalf("the store is not locked: %v", err)
	}
}

func TestServeFailsClosedOnAFatalAcceptError(t *testing.T) {
	ln := newFakeListener(func() (conn, error) { return nil, errors.New("accept failed for good") })
	store, _, done := serveFake(t, ln)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running after a fatal accept error")
	}
	requireLocked(t, store)
}

func TestAcceptRetriesATemporaryError(t *testing.T) {
	cl, sv := net.Pipe()
	var calls atomic.Int32
	var ln *fakeListener
	ln = newFakeListener(func() (conn, error) {
		switch calls.Add(1) {
		case 1:
			return nil, &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}
		case 2:
			return nil, &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.ECONNABORTED)}
		case 3:
			return sv, nil
		}
		return ln.block()
	})
	store, cancel, done := serveFake(t, ln)
	c := &Client{state: "x", dial: func(string) (conn, error) { return cl, nil }}
	st, err := c.Status()
	if err != nil || st.Locked {
		t.Fatalf("%+v %v", st, err)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned after temporary errors: %v", err)
	default:
	}
	if _, err := store.Unseal(sealed.KindLinear, linA); sealed.CodeOf(err) == sealed.CodePassphraseMode {
		t.Fatal("the store was locked by a temporary error")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestTemporaryAcceptAndBackoff(t *testing.T) {
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ECONNABORTED, syscall.EINTR,
		&net.OpError{Err: os.NewSyscallError("accept", syscall.EMFILE)}, os.ErrDeadlineExceeded} {
		if !temporaryAccept(e) {
			t.Errorf("%v is not temporary", e)
		}
	}
	for _, e := range []error{errors.New("boom"), net.ErrClosed, errFakeClosed, syscall.EBADF} {
		if temporaryAccept(e) {
			t.Errorf("%v is temporary", e)
		}
	}
	var p time.Duration
	var got []time.Duration
	for i := 0; i < 12; i++ {
		p = nextPause(p)
		got = append(got, p)
	}
	if got[0] != acceptBackoffMin || got[1] != 2*acceptBackoffMin || got[11] != acceptBackoffMax {
		t.Fatalf("backoff %v", got)
	}
}

func TestConnectionCapIsHonoured(t *testing.T) {
	type pair struct{ cl, sv net.Conn }
	pairs := make(chan pair, maxConns+1)
	var clients []net.Conn
	for i := 0; i <= maxConns; i++ {
		cl, sv := net.Pipe()
		clients = append(clients, cl)
		pairs <- pair{cl, sv}
	}
	var calls atomic.Int32
	reached := make(chan int32, maxConns+2)
	var ln *fakeListener
	ln = newFakeListener(func() (conn, error) {
		n := calls.Add(1)
		reached <- n
		select {
		case p := <-pairs:
			return p.sv, nil
		default:
			return ln.block()
		}
	})
	_, cancel, done := serveFake(t, ln)
	// The first maxConns clients are silent, so each holds its slot.
	for i := 1; i <= maxConns; i++ {
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatalf("accept %d never came", i)
		}
	}
	select {
	case n := <-reached:
		t.Fatalf("accept %d ran with every slot busy", n)
	case <-time.After(150 * time.Millisecond):
	}
	// One client leaving frees a slot, and the loop accepts again.
	clients[0].Close()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("a freed slot was not used")
	}
	// With the clients gone, Serve ends with nothing left behind.
	for _, c := range clients {
		c.Close()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestServeRefusesAnEmptyServerOrigin(t *testing.T) {
	seams(t)
	key := append([]byte(nil), fixKey...)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := Serve(ctx, newState(t), fixKid, key, Options{})
	if !errors.Is(err, errNoOrigin) {
		t.Fatalf("got %v", err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("a refused Serve kept the key")
	}
}

// lockHandler is a handler over a fresh unlocked store holding one secret.
func lockHandler(t *testing.T) (*handler, *int) {
	t.Helper()
	fixture(t)
	state := newState(t)
	key := append([]byte(nil), fixKey...)
	store, err := sealed.OpenUnlocked(state, fixKid, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Seal(sealed.KindLinear, linA, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	stops := new(int)
	h := &handler{store: store, kid: fixKid[:8], origin: testOrigin, now: time.Now}
	h.stop = func() {
		// The key must already be gone when the agent is asked to stop. This
		// runs under h.mu, so it reports with Errorf and never ends the test
		// goroutine.
		if _, err := store.Unseal(sealed.KindLinear, linA); sealed.CodeOf(err) != sealed.CodePassphraseMode {
			t.Errorf("stop ran with the store unlocked: %v", err)
		}
		*stops++
	}
	h.lastReq = h.now()
	t.Cleanup(store.Lock)
	return h, stops
}

func unsealReq() *request {
	return &request{V: Version, Op: OpUnseal, Kind: sealed.KindLinear, ID: linA}
}

func wantLockedResp(t *testing.T, r *response) {
	t.Helper()
	if r.OK || r.Code != CodeLocked || len(r.Plaintext) != 0 {
		t.Fatalf("got %+v, want keys_locked", r)
	}
}

func TestLockDropsTheKeyBeforeTheReply(t *testing.T) {
	h, stops := lockHandler(t)
	if r := h.do(unsealReq()); !r.OK {
		t.Fatalf("%+v", r)
	}
	r := h.do(&request{V: Version, Op: OpLock})
	// The reply is only now being written, and the key is already gone.
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	requireLocked(t, h.store)
	if *stops != 1 {
		t.Fatalf("stop ran %d times", *stops)
	}
	for _, op := range []string{OpUnseal, OpSign, OpStatus, OpDeviceNew, OpLock} {
		wantLockedResp(t, h.do(&request{V: Version, Op: op, Kind: sealed.KindLinear, ID: linA}))
	}
}

func TestRequestQueuedBehindALockGetsKeysLocked(t *testing.T) {
	h, _ := lockHandler(t)
	queued := make(chan struct{})
	beforeDo = func(*request) { close(queued) }
	t.Cleanup(func() { beforeDo = nil })
	// The test holds the handler's mutex, as the request being served when
	// the lock arrives would, so the unseal waits behind it.
	h.mu.Lock()
	res := make(chan *response, 1)
	go func() { res <- h.do(unsealReq()) }()
	<-queued
	h.lockLocked()
	h.mu.Unlock()
	select {
	case r := <-res:
		wantLockedResp(t, r)
	case <-time.After(5 * time.Second):
		t.Fatal("the queued request never ran")
	}
}

func TestIdleExpiryLocksBeforeAnyLaterRequest(t *testing.T) {
	h, stops := lockHandler(t)
	var mu sync.Mutex
	now := time.Now()
	h.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h.idle = 10 * time.Second
	h.lastReq = h.now()
	if h.lockIfIdle() || h.locked {
		t.Fatal("locked before the idle timeout")
	}
	mu.Lock()
	now = now.Add(11 * time.Second)
	mu.Unlock()
	if !h.lockIfIdle() {
		t.Fatal("did not lock after the idle timeout")
	}
	requireLocked(t, h.store)
	if *stops != 1 {
		t.Fatalf("stop ran %d times", *stops)
	}
	wantLockedResp(t, h.do(unsealReq()))
}

func TestWatchLocksWhenTheStateDirIsGone(t *testing.T) {
	h, stops := lockHandler(t)
	oldState := stateCheckEvery
	stateCheckEvery = 5 * time.Millisecond
	t.Cleanup(func() { stateCheckEvery = oldState })
	gone := t.TempDir() + "/missing"
	done := make(chan struct{})
	go func() { h.watch(context.Background(), gone); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not end")
	}
	if *stops != 1 || !h.locked {
		t.Fatalf("stops %d locked %v", *stops, h.locked)
	}
	wantLockedResp(t, h.do(unsealReq()))
}

func TestAgentDirectoryRaceIsOnlyChecked(t *testing.T) {
	seams(t)
	canon, _ := canonicalState(shortTemp(t))
	oldMkdir := mkdir
	t.Cleanup(func() { mkdir = oldMkdir })
	// Another process wins the race to make the directory, and leaves it
	// open to others. MakePrivate would fix that and let it pass.
	var made string
	mkdir = func(dir string, _ fs.FileMode) error {
		made = dir
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		os.Chmod(dir, 0o755)
		return fs.ErrExist
	}
	if _, err := endpointFor(canon, true); err == nil {
		t.Fatal("used a directory another process made")
	}
	if fi, err := os.Stat(made); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the directory was changed: %v %v", fi, err)
	}
}
