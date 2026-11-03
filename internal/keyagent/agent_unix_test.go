//go:build !windows

package keyagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/fsprivate"
	"clawbridge/internal/sealed"
)

const (
	linA  = "lin_0123456789abcdef0123456789abcdef"
	linB  = "lin_fedcba9876543210fedcba9876543210"
	tpass = "correct horse battery staple 42"
)

var (
	fixOnce sync.Once
	fixRaw  []byte // data.key of a passphrase store
	fixKid  string
	fixKey  []byte
)

// fixture makes one passphrase-mode data key for every test, since the
// KDF is slow.
func fixture(t *testing.T) {
	t.Helper()
	fixOnce.Do(func() {
		d, err := os.MkdirTemp("", "kaf")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(d)
		if err = sealed.CreatePassphrase(d, []byte(tpass)); err != nil {
			t.Fatal(err)
		}
		fixRaw, _ = os.ReadFile(filepath.Join(d, "keys", "data.key"))
		fixKid, fixKey, err = sealed.Unwrap(fixRaw, []byte(tpass))
		if err != nil {
			t.Fatal(err)
		}
	})
}

// shortTemp is a short temporary directory, since a socket path must fit
// sockaddr_un.
func shortTemp(t *testing.T) string {
	d, err := os.MkdirTemp("", "ka")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// seams points the endpoint at a short per-test root, as Linux's /tmp
// fallback, and makes the timers fast.
func seams(t *testing.T) {
	t.Helper()
	root := shortTemp(t)
	oGoos, oGet, oFall, oHarden := goos, getenv, fallbackRoot, harden
	oState, oIdle, oAfter := stateCheckEvery, idleCheckEvery, afterOpen
	goos, getenv, fallbackRoot, harden = "linux", func(string) string { return "" }, root, nil
	stateCheckEvery, idleCheckEvery = 15*time.Millisecond, 15*time.Millisecond
	t.Cleanup(func() {
		goos, getenv, fallbackRoot, harden = oGoos, oGet, oFall, oHarden
		stateCheckEvery, idleCheckEvery, afterOpen = oState, oIdle, oAfter
	})
}

type agentT struct {
	state  string
	key    []byte // the buffer handed to Serve
	store  *sealed.Store
	once   sync.Once // only the first Serve's store is kept
	cancel context.CancelFunc
	done   chan error
}

func newState(t *testing.T) string {
	fixture(t)
	state := shortTemp(t)
	if err := os.MkdirAll(filepath.Join(state, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "keys", "data.key"), fixRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

// start runs an agent on a fresh state directory and waits for its socket.
func start(t *testing.T, opts Options) *agentT {
	t.Helper()
	seams(t)
	return startOn(t, newState(t), opts)
}

func startOn(t *testing.T, state string, opts Options) *agentT {
	t.Helper()
	if opts.ServerOrigin == "" {
		opts.ServerOrigin = testOrigin
	}
	a := &agentT{state: state, key: append([]byte(nil), fixKey...), done: make(chan error, 1)}
	afterOpen = func(s *sealed.Store) { a.once.Do(func() { a.store = s }) }
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go func() { a.done <- Serve(ctx, state, fixKid, a.key, opts) }()
	canon, _ := canonicalState(state)
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-a.done:
			a.done <- err
			return a
		default:
		}
		if p, err := endpointFor(canon, false); err == nil {
			if c, err := net.Dial("unix", p); err == nil {
				c.Close()
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
		}
	})
	return a
}

func (a *agentT) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-a.done:
		a.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not exit")
		return nil
	}
}

func dialT(t *testing.T, a *agentT) *Client {
	t.Helper()
	c, err := Dial(a.state)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// raw sends bytes as they are and returns the response frame, or nil when
// the server closed without answering.
func raw(t *testing.T, state string, in []byte) *response {
	t.Helper()
	canon, _ := canonicalState(state)
	c, err := dial(canon)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(in)
	b, err := readFrame(c)
	if err != nil {
		return nil
	}
	var r response
	if err = json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestRoundTrip(t *testing.T) {
	a := start(t, Options{})
	c := dialT(t, a)

	st, err := c.Status()
	if err != nil || st.Locked || st.Mode != "passphrase" || st.KidPrefix != fixKid[:8] || st.IdleLeft >= 0 {
		t.Fatalf("%+v %v", st, err)
	}

	id, pub, err := c.DeviceNew()
	if err != nil || devicekey.ID(pub) != id {
		t.Fatalf("%v", err)
	}
	// The record is sealed in the store, and its seed derives the key; the
	// reply carried only the public half.
	seed, err := a.store.Unseal(sealed.KindDevice, id)
	if err != nil || len(seed) != 32 || !bytes.Equal(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), pub) {
		t.Fatalf("%v", err)
	}

	body := []byte(`{"a":1}`)
	sum := sha256.Sum256(body)
	req := SignRequest{DeviceID: id, Method: "POST", Origin: "https://hub.example.test", Path: "/v1/things?x=1",
		BodySHA256: hex.EncodeToString(sum[:]), Timestamp: nowTS(0)}
	nowTSval := req.Timestamp
	sg, err := c.Sign(req)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sg.Signature)
	if err != nil || sg.Nonce == "" {
		t.Fatal("bad signature or nonce")
	}
	// The canonical string is the one the server and devicesign build.
	want := devicekey.RequestBase("POST", "https://hub.example.test", "/v1/things?x=1", nowTSval, sg.Nonce, body)
	if !ed25519.Verify(pub, want, sig) {
		t.Fatal("the signature does not verify against devicekey.RequestBase")
	}
	sg2, _ := c.Sign(req)
	if sg2.Nonce == sg.Nonce {
		t.Fatal("a nonce was reused")
	}

	// linear and linear-api records.
	if err = c.Seal(sealed.KindLinear, linA, []byte("tok-1")); err != nil {
		t.Fatal(err)
	}
	pt, err := c.Unseal(sealed.KindLinear, linA, "")
	if err != nil || string(pt) != "tok-1" {
		t.Fatalf("%q %v", pt, err)
	}
	if err = c.Seal(sealed.KindLinearAPI, linB, []byte("key-1")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{PurposeLinearKey, PurposeLinearView} {
		if pt, err = c.Unseal(sealed.KindLinearAPI, linB, p); err != nil || string(pt) != "key-1" {
			t.Fatalf("%q %v", pt, err)
		}
	}
	_, err = c.Unseal(sealed.KindLinearAPI, linB, "")
	wantCode(t, err, CodePurposeRequired)
	_, err = c.Unseal(sealed.KindLinearAPI, linB, "other")
	wantCode(t, err, CodePurposeRequired)
	_, err = c.Unseal(sealed.KindLinear, linA, PurposeLinearKey)
	wantCode(t, err, CodeBadRequest)
	_, err = c.Unseal(sealed.KindLinear, linB, "")
	wantCode(t, err, CodeNotFound)

	// The device seed can't be read back, or replaced, through the agent.
	_, err = c.Unseal(sealed.KindDevice, id, "")
	wantCode(t, err, CodeForbidden)
	_, err = c.Unseal(sealed.KindDevice, id, PurposeLinearKey)
	wantCode(t, err, CodeForbidden)
	wantCode(t, c.Seal(sealed.KindDevice, id, bytes.Repeat([]byte{1}, 32)), CodeForbidden)
	wantCode(t, func() error { _, e := c.Unseal("bogus", linA, ""); return e }(), CodeForbidden)

	if err = c.Lock(); err != nil {
		t.Fatal(err)
	}
	if err = a.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err = Dial(a.state); !errors.Is(err, ErrLocked) {
		t.Fatalf("after lock: %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "keys_locked" || e.Msg != "ClawBridge is locked: ask your person to run `clawbridge keys unlock` in their own terminal." {
		t.Fatalf("%v", err)
	}
	// The key is gone from the store and from the buffer given to Serve.
	if _, err = a.store.Unseal(sealed.KindLinear, linA); sealed.CodeOf(err) != sealed.CodePassphraseMode {
		t.Fatalf("the store still holds the key: %v", err)
	}
	if !bytes.Equal(a.key, make([]byte, len(a.key))) {
		t.Fatal("the key buffer was not zeroed")
	}
}

func TestAbsentAgentIsLocked(t *testing.T) {
	seams(t)
	_, err := Dial(newState(t))
	if !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
}

func TestSignRefusesBadFieldsAndUnknownDevice(t *testing.T) {
	a := start(t, Options{})
	c := dialT(t, a)
	id, _, _ := c.DeviceNew()
	sum := hex.EncodeToString(make([]byte, 32))
	good := SignRequest{DeviceID: id, Method: "GET", Origin: "https://hub.example.test", Path: "/x", BodySHA256: sum, Timestamp: nowTS(0)}
	if _, err := c.Sign(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*SignRequest){
		"newline in path":   func(r *SignRequest) { r.Path = "/x\nPOST" },
		"newline in origin": func(r *SignRequest) { r.Origin = "https://a\nb" },
		"relative path":     func(r *SignRequest) { r.Path = "x" },
		"lower-case method": func(r *SignRequest) { r.Method = "get" },
		"short digest":      func(r *SignRequest) { r.BodySHA256 = "ab" },
		"upper digest":      func(r *SignRequest) { r.BodySHA256 = "AB" + sum[2:] },
		"timestamp text":    func(r *SignRequest) { r.Timestamp = "1\n2" },
		"empty field":       func(r *SignRequest) { r.Method = "" },
		"bad device":        func(r *SignRequest) { r.DeviceID = "dev_x" },
	} {
		r := good
		mut(&r)
		_, err := c.Sign(r)
		wantCode(t, err, CodeInvalid)
		_ = name
	}
	r := good
	r.DeviceID = "dev_" + "0123456789abcdefghjkmnpqrs"[:26]
	_, err := c.Sign(r)
	wantCode(t, err, CodeNotFound)
}

func TestSignNeverTakesACallerDigestOrBytes(t *testing.T) {
	a := start(t, Options{})
	for _, f := range []string{"digest", "message", "payload", "canonical", "nonce", "data", "bytes", "sign"} {
		in, _ := json.Marshal(map[string]any{"v": 1, "op": "sign", f: "AAAA"})
		r := raw(t, a.state, frame(in))
		if r == nil || r.OK || r.Code != CodeBadRequest {
			t.Fatalf("%s: %+v", f, r)
		}
	}
}

func TestProtocolRefusalsOverTheSocket(t *testing.T) {
	a := start(t, Options{})
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, MaxFrame+1)
	for name, c := range map[string]struct {
		in   []byte
		code string
	}{
		"oversize":    {big, CodeTooLarge},
		"bad version": {frame([]byte(`{"v":9,"op":"status"}`)), CodeBadVersion},
		"unknown op":  {frame([]byte(`{"v":1,"op":"rewrap"}`)), CodeUnknownOp},
		"garbage":     {frame([]byte("\x00\x01\x02")), CodeBadRequest},
	} {
		r := raw(t, a.state, c.in)
		if r == nil || r.OK || r.Code != c.code || r.Msg != messages[c.code] {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// The agent is still up and still answers.
	if _, err := dialT(t, a).Status(); err != nil {
		t.Fatal(err)
	}
}

func TestTruncatedFrameIsRefusedWithoutAPanic(t *testing.T) {
	a := start(t, Options{})
	canon, _ := canonicalState(a.state)
	c, err := dial(canon)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte{0, 0, 1, 0, '{'})
	c.(*net.UnixConn).CloseWrite()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	b, err := readFrame(c)
	c.Close()
	var r response
	if err != nil || json.Unmarshal(b, &r) != nil || r.Code != CodeBadRequest {
		t.Fatalf("%v %s", err, b)
	}
	if _, err := dialT(t, a).Status(); err != nil {
		t.Fatal(err)
	}
}

func TestPeerUIDCheck(t *testing.T) {
	a := start(t, Options{})
	canon, _ := canonicalState(a.state)
	t.Cleanup(func() { peerUIDSeam.Store(nil) })
	path, err := endpointFor(canon, false)
	if err != nil {
		t.Fatal(err)
	}
	// The server's side of a connection is the one whose local address is
	// the socket path; there the peer looks like another uid. The client's
	// own check of the server is left true. A client's unnamed local address
	// prints differently per OS, so it is not used to tell the sides apart.
	seam := func(c *net.UnixConn) (uint32, error) {
		if a, ok := c.LocalAddr().(*net.UnixAddr); ok && a != nil && a.Name == path {
			return uint32(os.Getuid()) + 1, nil
		}
		return peerUIDOf(c)
	}
	peerUIDSeam.Store(&seam)
	c, err := dial(canon)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	writeFrame(c, []byte(`{"v":1,"op":"status"}`))
	if b, err := readFrame(c); err == nil {
		t.Fatalf("a peer of another uid was answered: %s", b)
	}
	peerUIDSeam.Store(nil)
	if _, err := dialT(t, a).Status(); err != nil {
		t.Fatal(err)
	}
}

func TestClientRefusesAServerOfAnotherUser(t *testing.T) {
	a := start(t, Options{})
	t.Cleanup(func() { peerUIDSeam.Store(nil) })
	foreign := func(*net.UnixConn) (uint32, error) { return uint32(os.Getuid()) + 1, nil }
	peerUIDSeam.Store(&foreign)
	canon, _ := canonicalState(a.state)
	if c, err := dial(canon); err == nil {
		c.Close()
		t.Fatal("the client accepted a server of another uid")
	}
}

func TestSocketAndDirectoryPermissions(t *testing.T) {
	a := start(t, Options{})
	canon, _ := canonicalState(a.state)
	p, err := endpointFor(canon, false)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	di, _ := os.Lstat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("%v", di.Mode())
	}
}

func TestFallbackDirectoryChecks(t *testing.T) {
	seams(t)
	state := newState(t)
	canon, _ := canonicalState(state)
	dir := filepath.Join(fallbackRoot, "clawbridge-"+strconv.Itoa(os.Getuid()))
	mk := func() { os.RemoveAll(dir); os.Mkdir(dir, 0o700) }

	mk()
	if _, err := endpointFor(canon, true); err != nil {
		t.Fatal(err)
	}
	t.Run("wrong mode", func(t *testing.T) {
		os.Chmod(dir, 0o755)
		defer os.Chmod(dir, 0o700)
		if _, err := endpointFor(canon, true); err == nil {
			t.Fatal("a 0755 directory was used")
		}
		if _, err := endpointFor(canon, false); err == nil {
			t.Fatal("a 0755 directory was used by a client")
		}
	})
	t.Run("another owner", func(t *testing.T) {
		old := fsprivate.CurrentUID
		fsprivate.CurrentUID = func() int { return os.Getuid() + 1 }
		defer func() { fsprivate.CurrentUID = old }()
		// The path is by the other uid too; make that directory ours.
		other := filepath.Join(fallbackRoot, "clawbridge-"+strconv.Itoa(os.Getuid()+1))
		os.Mkdir(other, 0o700)
		if _, err := endpointFor(canon, true); err == nil {
			t.Fatal("a directory of another owner was used")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		os.RemoveAll(dir)
		target := shortTemp(t)
		os.Symlink(target, dir)
		if _, err := endpointFor(canon, true); err == nil {
			t.Fatal("a symlinked directory was used")
		}
		if err := listenErr(canon); err == nil {
			t.Fatal("listened in a symlinked directory")
		}
	})
	t.Run("XDG", func(t *testing.T) {
		x := shortTemp(t)
		getenv = func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return x
			}
			return ""
		}
		p, err := endpointFor(canon, true)
		if err != nil || filepath.Dir(p) != filepath.Join(x, "clawbridge") {
			t.Fatalf("%s %v", p, err)
		}
		os.Chmod(filepath.Join(x, "clawbridge"), 0o770)
		if _, err = endpointFor(canon, true); err == nil {
			t.Fatal("a wide XDG directory was used")
		}
	})
	t.Run("macOS", func(t *testing.T) {
		goos = "darwin"
		x := shortTemp(t)
		getenv = func(k string) string {
			if k == "TMPDIR" {
				return x
			}
			return ""
		}
		p, err := endpointFor(canon, true)
		if err != nil || filepath.Base(p) != "agent.sock" || filepath.Dir(filepath.Dir(p)) != x {
			t.Fatalf("%s %v", p, err)
		}
	})
}

func listenErr(state string) error {
	l, err := listen(state)
	if err == nil {
		l.Close()
	}
	return err
}

func TestEndpointIsPerStateDirAndFollowsSymlinks(t *testing.T) {
	seams(t)
	a, b := shortTemp(t), shortTemp(t)
	pa, err := Endpoint(a)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := Endpoint(b)
	if pa == pb {
		t.Fatal("two state directories share an agent")
	}
	link := filepath.Join(shortTemp(t), "l")
	os.Symlink(a, link)
	pl, _ := Endpoint(link)
	if pl != pa {
		t.Fatal("a symlink to a state directory has its own agent")
	}
	canon, _ := canonicalState(a)
	if len(stateHash(canon)) != 16 {
		t.Fatal("hash length")
	}
}

func TestStaleSocketIsRebound(t *testing.T) {
	seams(t)
	state := newState(t)
	canon, _ := canonicalState(state)
	p, _ := endpointFor(canon, true)
	// A dead agent's socket: bind and abandon without unlinking.
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("no stale socket to test with")
	}
	a := startOn(t, state, Options{})
	if _, err := dialT(t, a).Status(); err != nil {
		t.Fatal(err)
	}
}

func TestSocketNotOursIsNeverRemoved(t *testing.T) {
	seams(t)
	state := newState(t)
	canon, _ := canonicalState(state)
	p, _ := endpointFor(canon, true)
	os.WriteFile(p, []byte("not a socket"), 0o600)
	if err := listenErr(canon); err == nil {
		t.Fatal("bound over a file that is not a socket")
	}
	if b, _ := os.ReadFile(p); string(b) != "not a socket" {
		t.Fatal("the file was removed")
	}
	// A stale socket that is not provably ours is not removed either.
	os.Remove(p)
	l, _ := net.Listen("unix", p)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	old := isOurSocket
	isOurSocket = func(string) (bool, error) { return false, nil }
	defer func() { isOurSocket = old }()
	if err := listenErr(canon); err == nil {
		t.Fatal("bound over a socket not proved to be ours")
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatal("the socket was removed")
	}
	// ownedSocket itself: a socket of ours is ours, a file is not.
	if ok, _ := old(p); !ok {
		t.Fatal("our own socket was not recognised")
	}
	if ok, _ := old(filepath.Join(filepath.Dir(p), "agent.sock.lock")); ok {
		t.Fatal("a regular file was called a socket")
	}
}

func TestSecondServeFailsWhileAnAgentLives(t *testing.T) {
	a := start(t, Options{})
	key := append([]byte(nil), fixKey...)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := Serve(ctx, a.state, fixKid, key, Options{ServerOrigin: testOrigin})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("%v", err)
	}
	// The first agent was not disturbed, and the failed Serve zeroed its key.
	if _, err = dialT(t, a).Status(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, make([]byte, 32)) {
		t.Fatal("a refused Serve kept the key")
	}
}

func TestServeRefusesAWrongKeyOrKid(t *testing.T) {
	seams(t)
	state := newState(t)
	bad := bytes.Repeat([]byte{7}, 32)
	if err := Serve(context.Background(), state, "0000000000000000", bad, Options{ServerOrigin: testOrigin}); err == nil {
		t.Fatal("served with a kid that is not data.key's")
	}
	if err := Serve(context.Background(), state, "short", bad, Options{ServerOrigin: testOrigin}); err == nil {
		t.Fatal("served with a malformed kid")
	}
}

func TestIdleTimeoutEndsTheAgent(t *testing.T) {
	a := start(t, Options{IdleTimeout: 400 * time.Millisecond})
	c := dialT(t, a)
	if err := c.Seal(sealed.KindLinear, linA, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	st, _ := c.Status()
	if st.IdleLeft < 0 || st.IdleLeft > time.Second {
		t.Fatalf("idle left %v", st.IdleLeft)
	}
	if err := a.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(a.state); !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
	if !bytes.Equal(a.key, make([]byte, 32)) {
		t.Fatal("key not zeroed")
	}
	if _, err := a.store.Unseal(sealed.KindLinear, linA); sealed.CodeOf(err) != sealed.CodePassphraseMode {
		t.Fatalf("store still unlocked: %v", err)
	}
}

func TestRequestsRestartTheIdleClockButStatusDoesNot(t *testing.T) {
	seams(t)
	var mu sync.Mutex
	now := time.Now()
	oldClock := clock
	clock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	t.Cleanup(func() { clock = oldClock })
	adv := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	a := startOn(t, newState(t), Options{IdleTimeout: 10 * time.Second})
	c := dialT(t, a)
	adv(6 * time.Second)
	st, _ := c.Status()
	if st.IdleLeft != 4*time.Second {
		t.Fatalf("status counted as use or the clock is off: %v", st.IdleLeft)
	}
	if _, err := c.Unseal(sealed.KindLinear, linA, ""); err == nil {
		t.Fatal("want not found")
	}
	st, _ = c.Status()
	if st.IdleLeft != 10*time.Second {
		t.Fatalf("a request did not restart the clock: %v", st.IdleLeft)
	}
	adv(11 * time.Second)
	if err := a.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestStateDirRemovedEndsTheAgent(t *testing.T) {
	a := start(t, Options{})
	dialT(t, a)
	os.RemoveAll(a.state)
	if err := a.wait(t); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.key, make([]byte, 32)) {
		t.Fatal("key not zeroed")
	}
}

func TestContextCancelEndsTheAgentAndZeroesTheKey(t *testing.T) {
	a := start(t, Options{})
	if err := dialT(t, a).Seal(sealed.KindLinear, linA, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	a.cancel()
	if err := a.wait(t); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.key, make([]byte, 32)) {
		t.Fatal("the key buffer was not zeroed")
	}
	if _, err := a.store.Unseal(sealed.KindLinear, linA); sealed.CodeOf(err) != sealed.CodePassphraseMode {
		t.Fatalf("store still unlocked: %v", err)
	}
	if _, err := Dial(a.state); !errors.Is(err, ErrLocked) {
		t.Fatal(err)
	}
}

func TestRotate(t *testing.T) {
	a := start(t, Options{})
	oldDelay := wrongPassDelay
	wrongPassDelay = 0
	t.Cleanup(func() { wrongPassDelay = oldDelay })
	c := dialT(t, a)
	if err := c.Seal(sealed.KindLinear, linA, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c.Rotate([]byte("not the passphrase at all")), CodeWrongPass)
	st, _ := c.Status()
	if st.KidPrefix != fixKid[:8] {
		t.Fatal("a failed rotation changed the key")
	}
	wantCode(t, c.Rotate(nil), CodeInvalid)
	if err := c.Rotate([]byte(tpass)); err != nil {
		t.Fatal(err)
	}
	st, _ = c.Status()
	if st.KidPrefix == fixKid[:8] || len(st.KidPrefix) != 8 {
		t.Fatalf("kid prefix %q after rotation", st.KidPrefix)
	}
	pt, err := c.Unseal(sealed.KindLinear, linA, "")
	if err != nil || string(pt) != "tok" {
		t.Fatalf("%q %v", pt, err)
	}
}

func TestHardenTurnsCoreDumpsOff(t *testing.T) {
	Harden()
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &rl); err != nil || rl.Cur != 0 {
		t.Fatalf("%+v %v", rl, err)
	}
}

func TestServeAppliesTheHardening(t *testing.T) {
	seams(t)
	called := false
	harden = func() []string { called = true; return []string{"one step failed"} }
	var logged []string
	a := startOn(t, newState(t), Options{Logf: func(f string, args ...any) { logged = append(logged, f) }})
	dialT(t, a)
	if !called || len(logged) != 1 {
		t.Fatalf("%v %v", called, logged)
	}
}

func TestStatusNeverCarriesKeyMaterial(t *testing.T) {
	a := start(t, Options{})
	r := raw(t, a.state, frame([]byte(`{"v":1,"op":"status"}`)))
	b, _ := json.Marshal(r)
	for _, secret := range [][]byte{fixKey, []byte(base64.StdEncoding.EncodeToString(fixKey)), []byte(hex.EncodeToString(fixKey)), []byte(fixKid)} {
		if bytes.Contains(b, secret) {
			t.Fatalf("status carries %q", secret)
		}
	}
}

func nowTS(d time.Duration) string { return strconv.FormatInt(time.Now().Add(d).Unix(), 10) }

func signAt(c *Client, id string, d time.Duration) error {
	_, err := c.Sign(SignRequest{DeviceID: id, Method: "GET", Origin: "https://hub.example.test", Path: "/x",
		BodySHA256: hex.EncodeToString(make([]byte, 32)), Timestamp: nowTS(d)})
	return err
}

func TestSignRefusesATimestampOutsideTheWindow(t *testing.T) {
	a := start(t, Options{})
	c := dialT(t, a)
	id, _, _ := c.DeviceNew()
	for _, d := range []time.Duration{0, 14 * time.Minute, -14 * time.Minute} {
		if err := signAt(c, id, d); err != nil {
			t.Fatalf("%v: %v", d, err)
		}
	}
	for _, d := range []time.Duration{16 * time.Minute, -16 * time.Minute, 24 * time.Hour} {
		wantCode(t, signAt(c, id, d), CodeTimestampRange)
	}
}

func TestSignWindowFollowsTheClockOffset(t *testing.T) {
	seams(t)
	a := startOn(t, newState(t), Options{ClockOffset: 10 * time.Minute})
	c := dialT(t, a)
	id, _, _ := c.DeviceNew()
	// Server time is local plus 10 minutes, so the window is centred there.
	for _, d := range []time.Duration{10 * time.Minute, 24 * time.Minute, -4 * time.Minute} {
		if err := signAt(c, id, d); err != nil {
			t.Fatalf("%v: %v", d, err)
		}
	}
	for _, d := range []time.Duration{26 * time.Minute, -6 * time.Minute} {
		wantCode(t, signAt(c, id, d), CodeTimestampRange)
	}
}

func TestSignOnlyServesTheProfilesOrigin(t *testing.T) {
	seams(t)
	a := startOn(t, newState(t), Options{ServerOrigin: "https://Hub.Example.test"})
	c := dialT(t, a)
	id, pub, _ := c.DeviceNew()
	if err := signAt(c, id, 0); err != nil {
		t.Fatal(err)
	}
	// Case is canonicalised, and the lower-case form is what is signed.
	ts := nowTS(0)
	sum := hex.EncodeToString(make([]byte, 32))
	sg, err := c.Sign(SignRequest{DeviceID: id, Method: "GET", Origin: "HTTPS://HUB.example.test", Path: "/x", BodySHA256: sum, Timestamp: ts})
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(sg.Signature)
	if !ed25519.Verify(pub, devicekey.RequestBaseSum("GET", "https://hub.example.test", "/x", ts, sg.Nonce, [32]byte{}), sig) {
		t.Fatal("not signed over the lower-case origin")
	}
	for _, o := range []string{"https://other.example.test", "http://hub.example.test", "https://hub.example.test:8443"} {
		_, err := c.Sign(SignRequest{DeviceID: id, Method: "GET", Origin: o, Path: "/x", BodySHA256: sum, Timestamp: ts})
		wantCode(t, err, CodeOriginNotAllowed)
	}
}

func TestWrongRotationPauseDoesNotBlockOtherRequests(t *testing.T) {
	a := start(t, Options{})
	c := dialT(t, a)
	id, _, _ := c.DeviceNew()
	old := wrongPassDelay
	wrongPassDelay = 4 * time.Second
	t.Cleanup(func() { wrongPassDelay = old })
	paused := make(chan struct{}, 1)
	beforePause = func() {
		select {
		case paused <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { beforePause = nil })
	done := make(chan error, 1)
	go func() { done <- c.Rotate([]byte("not the passphrase at all")) }()
	// The KDF runs under the mutex and can take long under -race, so wait
	// for the signal that the request has reached its pause.
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		t.Fatal("the rotation never reached its pause")
	}
	start := time.Now()
	if _, err := c.Status(); err != nil {
		t.Fatal(err)
	}
	if err := signAt(c, id, 0); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("status and sign waited %v behind the pause", time.Since(start))
	}
	select {
	case <-done:
		t.Fatal("the rotation was not still pausing")
	default:
	}
	wantCode(t, <-done, CodeWrongPass)
}
