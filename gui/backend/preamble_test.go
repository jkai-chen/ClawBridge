package backend

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const testNonce = "00112233445566778899aabbccddeeff"

func TestSyncPreambleSkipsJunk(t *testing.T) {
	in := "motd: welcome to the box\r\n" +
		"nvm: using node v22\n" +
		"\n" +
		PreambleWord + " " + testNonce + "\r\n" +
		"Content-Length: 2\r\n\r\n{}"
	r := newReader(strings.NewReader(in))
	var junk []string
	if err := SyncPreamble(r, testNonce, func(l string) { junk = append(junk, l) }); err != nil {
		t.Fatal(err)
	}
	if len(junk) != 2 || junk[0] != "motd: welcome to the box" || junk[1] != "nvm: using node v22" {
		t.Fatalf("discarded lines: %q", junk)
	}
	// The reader is positioned exactly at the first frame.
	body, err := ReadFrame(r)
	if err != nil || string(body) != "{}" {
		t.Fatalf("frame after the preamble: %q %v", body, err)
	}
}

func TestSyncPreambleWrongNonceIsJunk(t *testing.T) {
	stale := PreambleWord + " ffffffffffffffffffffffffffffffff"
	in := stale + "\n" + PreambleWord + " " + testNonce + " trailing\n" + PreambleWord + "\n" + PreambleWord + " " + testNonce + "\n"
	var junk []string
	if err := SyncPreamble(newReader(strings.NewReader(in)), testNonce, func(l string) { junk = append(junk, l) }); err != nil {
		t.Fatal(err)
	}
	if len(junk) != 3 {
		t.Fatalf("a wrong nonce, trailing text and a bare word are all discarded: %q", junk)
	}
}

func TestSyncPreambleEOFBeforeThePreamble(t *testing.T) {
	for name, in := range map[string]string{
		"empty":               "",
		"only junk":           "bash: clawbridge: command not found\n",
		"junk without \\n":    "partial line with no newline",
		"wrong nonce only":    PreambleWord + " " + strings.Repeat("0", 32) + "\n",
		"preamble unfinished": PreambleWord + " " + testNonce, // no newline: EOF mid-line is not a preamble
	} {
		t.Run(name, func(t *testing.T) {
			var junk []string
			err := SyncPreamble(newReader(strings.NewReader(in)), testNonce, func(l string) { junk = append(junk, l) })
			if name == "preamble unfinished" {
				// The line equals the preamble once trimmed; a backend that
				// wrote it and died is still a preamble. The read loop then
				// sees EOF at once and reports "stopped".
				if err != nil {
					t.Fatalf("a complete preamble line without a newline is accepted: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrNoPreamble) {
				t.Fatalf("got %v, want ErrNoPreamble", err)
			}
		})
	}
}

func TestSyncPreambleReadError(t *testing.T) {
	r := newReader(iotest.ErrReader(errors.New("pipe broke")))
	if err := SyncPreamble(r, testNonce, nil); !errors.Is(err, ErrNoPreamble) {
		t.Fatalf("got %v", err)
	}
}

func TestSyncPreambleCapsEndlessOutput(t *testing.T) {
	// A shell that prints without ever ending a line must not grow memory
	// or hang the shell: the discard cap ends the wait.
	endless := io.LimitReader(zeroReader{}, 4*maxDiscard)
	err := SyncPreamble(newReader(endless), testNonce, nil)
	if !errors.Is(err, ErrNoPreamble) || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("got %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestNewNonce(t *testing.T) {
	a, err := NewNonce()
	if err != nil || len(a) != 32 {
		t.Fatalf("nonce %q %v", a, err)
	}
	if b, _ := NewNonce(); a == b {
		t.Fatal("two nonces were equal")
	}
}
