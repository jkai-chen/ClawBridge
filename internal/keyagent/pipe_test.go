package keyagent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const sidA = "S-1-5-21-1111111111-2222222222-3333333333-1001"
const sidB = "S-1-5-21-1111111111-2222222222-3333333333-1002"

func TestPipeNameAndACL(t *testing.T) {
	if got := pipeName(sidA, "0123456789abcdef"); got != `\\.\pipe\clawbridge-`+sidA+`-0123456789abcdef` {
		t.Fatal(got)
	}
	sddl := pipeSDDL(sidA)
	if sddl != "D:P(A;;GA;;;"+sidA+")" {
		t.Fatal(sddl)
	}
	// Protected, and one entry only: no Everyone (WD), Administrators (BA),
	// SYSTEM (SY), Authenticated Users (AU) or anonymous.
	if strings.Count(sddl, "(") != 1 || !strings.HasPrefix(sddl, "D:P") {
		t.Fatal("the DACL must be protected with one entry")
	}
	for _, other := range []string{";WD)", ";BA)", ";SY)", ";AU)", ";AN)", ";IU)"} {
		if strings.Contains(sddl, other) {
			t.Fatalf("DACL grants %s", other)
		}
	}
}

type fakeConn struct {
	wrote  []byte
	closed bool
}

func (f *fakeConn) Read([]byte) (int, error)    { return 0, errors.New("eof") }
func (f *fakeConn) Write(b []byte) (int, error) { f.wrote = append(f.wrote, b...); return len(b), nil }
func (f *fakeConn) Close() error                { f.closed = true; return nil }
func (f *fakeConn) SetDeadline(time.Time) error { return nil }

type fakeDialer struct {
	c        *fakeConn
	owner    string
	ownerErr error
	me       string
	openErr  error
}

func (d *fakeDialer) open() (conn, error) {
	if d.openErr != nil {
		return nil, d.openErr
	}
	return d.c, nil
}
func (d *fakeDialer) serverOwner(conn) (string, error) { return d.owner, d.ownerErr }
func (d *fakeDialer) self() (string, error)            { return d.me, nil }

func TestPipeClientChecksTheServerOwnerBeforeSending(t *testing.T) {
	t.Run("same user", func(t *testing.T) {
		d := &fakeDialer{c: &fakeConn{}, owner: sidA, me: sidA}
		c, err := connectPipe(d)
		if err != nil || c == nil || d.c.closed {
			t.Fatalf("%v", err)
		}
	})
	for name, d := range map[string]*fakeDialer{
		"another user":         {c: &fakeConn{}, owner: sidB, me: sidA},
		"owner cannot be read": {c: &fakeConn{}, ownerErr: errors.New("denied"), me: sidA},
		"empty owner":          {c: &fakeConn{}, owner: "", me: sidA},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := connectPipe(d)
			if err == nil || c != nil {
				t.Fatal("a squatted pipe was accepted")
			}
			if !d.c.closed || len(d.c.wrote) != 0 {
				t.Fatal("the connection was kept or written to")
			}
		})
	}
	t.Run("pipe absent", func(t *testing.T) {
		_, err := connectPipe(&fakeDialer{openErr: ErrLocked})
		if !errors.Is(err, ErrLocked) {
			t.Fatal(err)
		}
	})
}
