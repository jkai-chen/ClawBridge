package backend

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func frameReader(s string) *bufio.Reader { return newReader(strings.NewReader(s)) }

func TestReadFrame(t *testing.T) {
	good := "Content-Length: 5\r\n\r\nhello"
	body, err := ReadFrame(frameReader(good))
	if err != nil || string(body) != "hello" {
		t.Fatalf("good frame: %q %v", body, err)
	}
	// Header names are case-insensitive and other headers are ignored.
	body, err = ReadFrame(frameReader("content-type: x\r\ncontent-length: 2\r\n\r\nhi"))
	if err != nil || string(body) != "hi" {
		t.Fatalf("lowercase headers: %q %v", body, err)
	}
	// Two frames back to back, then a clean EOF.
	r := frameReader(good + good)
	for i := 0; i < 2; i++ {
		if _, err := ReadFrame(r); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if _, err := ReadFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("clean end: %v", err)
	}
}

func TestReadFrameFailures(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"eof inside the headers", "Content-Length: 5\r\n", io.ErrUnexpectedEOF},
		{"eof inside a header line", "Content-Len", io.ErrUnexpectedEOF},
		{"partial body", "Content-Length: 10\r\n\r\nabc", io.ErrUnexpectedEOF},
		{"empty body at eof", "Content-Length: 4\r\n\r\n", io.ErrUnexpectedEOF},
		{"oversized", fmt.Sprintf("Content-Length: %d\r\n\r\n", MaxFrame+1), ErrFrameTooLarge},
		{"no length", "X-Other: 1\r\n\r\n", ErrBadFrame},
		{"negative length", "Content-Length: -3\r\n\r\n", ErrBadFrame},
		{"not a number", "Content-Length: many\r\n\r\n", ErrBadFrame},
		{"conflicting lengths", "Content-Length: 1\r\nContent-Length: 2\r\n\r\nab", ErrBadFrame},
		{"junk header", "not a header\r\n\r\n", ErrBadFrame},
		{"endless header", strings.Repeat("a", 3*maxHeaderLine), ErrBadFrame},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadFrame(frameReader(tc.in)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadFrameAtTheCap(t *testing.T) {
	body := bytes.Repeat([]byte("x"), MaxFrame)
	in := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", MaxFrame, body)
	got, err := ReadFrame(frameReader(in))
	if err != nil || len(got) != MaxFrame {
		t.Fatalf("a frame at the cap must be read: %d %v", len(got), err)
	}
}
