package backend

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// PreambleWord starts the fixed line a backend writes before any framing.
const PreambleWord = "CLAWBRIDGE-GUI-BACKEND/1"

// maxDiscard bounds what the shell reads while looking for the preamble.
// A login shell's rc noise is a few lines; more than this is not a backend.
const maxDiscard = 1 << 20

// maxDiscardLine bounds one discarded line, so noise without a newline
// cannot grow memory. Longer lines are cut for the log; the rest of the
// line is skipped.
const maxDiscardLine = 4096

// ErrNoPreamble means the backend's output ended, or exceeded the cap,
// before the preamble line.
var ErrNoPreamble = errors.New("backend ended before its preamble")

// NewNonce returns a fresh 128-bit nonce as 32 hex characters.
func NewNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// SyncPreamble discards every line before the exact preamble line for this
// nonce and hands each discarded line to discard (for Help → Logs). A line
// with the right word and a wrong nonce is discarded like any other: it is
// not the answer to this start. It returns ErrNoPreamble on EOF or when the
// discarded bytes pass the cap.
func SyncPreamble(r *bufio.Reader, nonce string, discard func(string)) error {
	want := PreambleWord + " " + nonce
	total := 0
	for {
		line, n, err := readPreambleLine(r, maxDiscard-total)
		total += n
		line = strings.TrimRight(line, "\r\n")
		if line == want {
			return nil
		}
		if line != "" && discard != nil {
			discard(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w (output ended)", ErrNoPreamble)
			}
			return fmt.Errorf("%w: %v", ErrNoPreamble, err)
		}
		if total > maxDiscard {
			return fmt.Errorf("%w (over %d bytes of other output)", ErrNoPreamble, maxDiscard)
		}
	}
}

// readPreambleLine reads to the next newline, keeping at most
// maxDiscardLine bytes of it and counting every byte read. It stops with an
// error once budget bytes have been read without a newline.
func readPreambleLine(r *bufio.Reader, budget int) (string, int, error) {
	var kept []byte
	n := 0
	for {
		chunk, err := r.ReadSlice('\n')
		n += len(chunk)
		if room := maxDiscardLine - len(kept); room > 0 {
			if len(chunk) > room {
				kept = append(kept, chunk[:room]...)
			} else {
				kept = append(kept, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			if n > budget {
				return string(kept), n, errors.New("output cap reached")
			}
			continue
		}
		line := string(kept)
		if n > maxDiscardLine {
			line = trimPartialRune(line)
		}
		return line, n, err
	}
}
