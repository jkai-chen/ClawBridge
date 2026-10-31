// Package backend is the GUI shell's side of the ClawBridge GUI backend
// protocol (GUI-DESIGN §4.2, §6.2): it starts `clawbridge gui-backend
// --stdio`, syncs on its preamble, speaks content-length JSON-RPC to it and
// gates every call the webview makes. It has no Wails dependency, so it is
// tested without a window.
package backend

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxFrame is the largest body the shell accepts from the backend (8 MiB).
// A longer frame is refused and the backend counts as stopped: the stream
// cannot be resynchronised after it.
const MaxFrame = 8 << 20

// maxHeaderLine bounds one header line, so a backend that never sends a
// newline cannot grow memory.
const maxHeaderLine = 1024

// readerSize is the bufio buffer; a single line longer than it is "too long".
const readerSize = 64 << 10

var (
	// ErrFrameTooLarge means a Content-Length above MaxFrame.
	ErrFrameTooLarge = errors.New("backend frame larger than the 8 MiB cap")
	// ErrBadFrame means a malformed header block.
	ErrBadFrame = errors.New("malformed backend frame")
)

func newReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, readerSize) }

// readLine returns one line without its CR LF. A line over max bytes is an
// error. io.EOF is returned only when no byte of a line was read.
func readLine(r *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > max+2 {
			return "", fmt.Errorf("%w: header line over %d bytes", ErrBadFrame, max)
		}
		switch {
		case err == nil:
			return strings.TrimRight(string(buf), "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return "", io.EOF
			}
			return "", io.ErrUnexpectedEOF
		default:
			return "", err
		}
	}
}

// ReadFrame reads one content-length frame (LSP style) and returns its
// body. io.EOF means the stream ended between frames; io.ErrUnexpectedEOF
// means it ended inside one.
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	length := int64(-1)
	first := true
	for {
		line, err := readLine(r, maxHeaderLine)
		if err != nil {
			if errors.Is(err, io.EOF) && !first {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		first = false
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%w: header without a colon", ErrBadFrame)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%w: bad Content-Length", ErrBadFrame)
			}
			if length >= 0 && length != n {
				return nil, fmt.Errorf("%w: conflicting Content-Length", ErrBadFrame)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("%w: no Content-Length", ErrBadFrame)
	}
	if length > MaxFrame {
		return nil, ErrFrameTooLarge
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// WriteFrame writes one frame. The caller serialises writers.
func WriteFrame(w io.Writer, body []byte) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.Write(body)
	_, err := w.Write(b.Bytes())
	return err
}
