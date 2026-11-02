//go:build windows

package keyagent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/sealed"
)

const (
	winLin  = "lin_0123456789abcdef0123456789abcdef"
	winPass = "correct horse battery staple 42"
	winOrig = "https://hub.example.test"
)

var (
	winOnce  sync.Once
	winState string
	winKid   string
	winKey   []byte
)

// winFixture makes one passphrase store for every test, since the KDF is
// slow. Each test runs its own agent on it and stops that agent before it
// ends, so the pipe name is free for the next.
func winFixture(t *testing.T) string {
	t.Helper()
	winOnce.Do(func() {
		d, err := os.MkdirTemp("", "kaw")
		if err != nil {
			t.Fatal(err)
		}
		if err = sealed.CreatePassphrase(d, []byte(winPass)); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(d + `\keys\data.key`)
		if err != nil {
			t.Fatal(err)
		}
		winKid, winKey, err = sealed.Unwrap(raw, []byte(winPass))
		if err != nil {
			t.Fatal(err)
		}
		winState = d
	})
	if winState == "" {
		t.Fatal("no fixture")
	}
	return winState
}

type winAgent struct {
	cancel context.CancelFunc
	done   chan error
}

// winServe runs an agent on the fixture state and waits until the pipe
// answers or Serve returns.
func winServe(t *testing.T, opts Options) *winAgent {
	t.Helper()
	state := winFixture(t)
	oS, oI := stateCheckEvery, idleCheckEvery
	stateCheckEvery, idleCheckEvery = 20*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	a := &winAgent{cancel: cancel, done: make(chan error, 1)}
	key := append([]byte(nil), winKey...)
	go func() { a.done <- Serve(ctx, state, winKid, key, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-a.done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not stop")
		}
		stateCheckEvery, idleCheckEvery = oS, oI
	})
	canon, _ := canonicalState(state)
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-a.done:
			a.done <- err
			return a
		default:
		}
		if c, err := (&Client{state: canon, dial: dial}).Status(); err == nil && !c.Locked {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (a *winAgent) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-a.done:
		a.done <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit")
		return nil
	}
}

// requireFree fails unless a new agent can take the pipe name.
func requireFree(t *testing.T) {
	t.Helper()
	b := winServe(t, Options{})
	select {
	case err := <-b.done:
		t.Fatalf("a new agent could not take the pipe: %v", err)
	default:
	}
	b.cancel()
	b.wait(t)
}

func TestWinPipeRoundTrip(t *testing.T) {
	winServe(t, Options{ServerOrigin: winOrig})
	c, err := Dial(winState)
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status()
	if err != nil || st.Locked || st.Mode != "passphrase" || st.KidPrefix != winKid[:8] {
		t.Fatalf("%+v %v", st, err)
	}
	id, pub, err := c.DeviceNew()
	if err != nil || devicekey.ID(pub) != id {
		t.Fatalf("%v", err)
	}
	sum := sha256.Sum256([]byte(`{"a":1}`))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sg, err := c.Sign(SignRequest{DeviceID: id, Method: "POST", Origin: winOrig, Path: "/v1/things?x=1",
		BodySHA256: hex.EncodeToString(sum[:]), Timestamp: ts})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sg.Signature)
	if err != nil || sg.Nonce == "" {
		t.Fatal("bad signature or nonce")
	}
	if !ed25519.Verify(pub, devicekey.RequestBaseSum("POST", winOrig, "/v1/things?x=1", ts, sg.Nonce, sum), sig) {
		t.Fatal("the signature does not verify")
	}
	// An origin other than the profile's is refused.
	if _, err = c.Sign(SignRequest{DeviceID: id, Method: "GET", Origin: "https://other.example.test", Path: "/x",
		BodySHA256: hex.EncodeToString(sum[:]), Timestamp: ts}); err == nil {
		t.Fatal("a foreign origin was signed")
	}
	if err = c.Seal(sealed.KindLinear, winLin, []byte("tok-1")); err != nil {
		t.Fatal(err)
	}
	pt, err := c.Unseal(sealed.KindLinear, winLin, "")
	if err != nil || string(pt) != "tok-1" {
		t.Fatalf("%q %v", pt, err)
	}
	if err = c.Lock(); err != nil {
		t.Fatal(err)
	}
}

func TestWinPipeLockEndsTheAgent(t *testing.T) {
	a := winServe(t, Options{})
	c, err := Dial(winState)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Lock(); err != nil {
		t.Fatal(err)
	}
	if err = a.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err = Dial(winState); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", err)
	}
	requireFree(t)
}

func TestWinPipeSecondInstance(t *testing.T) {
	winServe(t, Options{})
	// A second agent for the same state is refused as already running.
	key := append([]byte(nil), winKey...)
	if err := Serve(context.Background(), winState, winKid, key, Options{}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("got %v, want ErrAlreadyRunning", err)
	}
	// The name cannot be squatted while the agent holds it.
	canon, _ := canonicalState(winState)
	name, err := endpointFor(canon, false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, windows.PIPE_UNLIMITED_INSTANCES, 4096, 4096, 0, nil)
	if err == nil {
		windows.CloseHandle(h)
		t.Fatal("the pipe name was squatted while the agent held it")
	}
	// The agent still answers.
	if _, err = Dial(winState); err != nil {
		t.Fatal(err)
	}
}

// squatPipe serves a raw pipe the test controls and records what the client
// sends. The connection is accepted only once.
type squatPipe struct {
	name  string
	read  chan int
	ready chan struct{}
}

func newSquat(t *testing.T, tag string) *squatPipe {
	t.Helper()
	sid, err := userSID()
	if err != nil {
		t.Fatal(err)
	}
	s := &squatPipe{name: pipeName(sid, "test"+tag+strconv.Itoa(os.Getpid())), read: make(chan int, 1), ready: make(chan struct{})}
	l := &pipeListener{name: s.name}
	h, err := l.newInstance(true)
	if err != nil {
		t.Fatal(err)
	}
	l.next = h
	t.Cleanup(func() { l.Close() })
	go func() {
		close(s.ready)
		c, err := l.Accept()
		if err != nil {
			s.read <- -1
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		s.read <- n
		// Never answer; hold until the client leaves or the deadline.
		c.Read(buf)
	}()
	<-s.ready
	return s
}

type otherOwner struct{ winDialer }

func (otherOwner) serverOwner(conn) (string, error) { return "S-1-5-21-1-2-3-1999", nil }

func TestWinPipeRefusesAForeignOwnerBeforeAnyByte(t *testing.T) {
	// Control: with the real owner check the same raw server does see bytes,
	// so a zero below means nothing was sent.
	ctl := newSquat(t, "ctl")
	cc := &Client{state: "x", dial: func(string) (conn, error) { return connectPipe(winDialer{name: ctl.name}) }}
	oT := callTimeout
	callTimeout = 500 * time.Millisecond
	t.Cleanup(func() { callTimeout = oT })
	cc.Status()
	if n := <-ctl.read; n <= 0 {
		t.Fatalf("control: the raw server read %d bytes", n)
	}

	s := newSquat(t, "sq")
	c := &Client{state: "x", dial: func(string) (conn, error) { return connectPipe(otherOwner{winDialer{name: s.name}}) }}
	_, err := c.Status()
	if !errors.Is(err, errPipeServer) {
		t.Fatalf("got %v, want the owner error", err)
	}
	if err = c.Lock(); !errors.Is(err, errPipeServer) {
		t.Fatalf("got %v, want the owner error", err)
	}
	select {
	case n := <-s.read:
		if n != 0 {
			t.Fatalf("the squatter read %d bytes", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the squatter saw no disconnect")
	}
}

func TestWinPipeCallDeadline(t *testing.T) {
	s := newSquat(t, "dl")
	oT := callTimeout
	callTimeout = 400 * time.Millisecond
	t.Cleanup(func() { callTimeout = oT })
	c := &Client{state: "x", dial: func(string) (conn, error) { return connectPipe(winDialer{name: s.name}) }}
	start := time.Now()
	_, err := c.Status()
	el := time.Since(start)
	if err == nil {
		t.Fatal("a silent server was answered")
	}
	if el < 300*time.Millisecond || el > 4*time.Second {
		t.Fatalf("returned after %v, want about the deadline", el)
	}
}

func TestWinPipeIdleAndCancelExitCleanly(t *testing.T) {
	a := winServe(t, Options{IdleTimeout: 600 * time.Millisecond})
	if err := a.wait(t); err != nil {
		t.Fatalf("idle exit: %v", err)
	}
	if _, err := Dial(winState); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", err)
	}
	b := winServe(t, Options{})
	if _, err := Dial(winState); err != nil {
		t.Fatal(err)
	}
	b.cancel()
	if err := b.wait(t); err != nil {
		t.Fatalf("cancel exit: %v", err)
	}
	requireFree(t)
}
