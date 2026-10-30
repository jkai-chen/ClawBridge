package transporterr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassify(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "x.example.test"}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	cases := []struct {
		name                       string
		err                        error
		reason, coarse             string
		noConn, dnsF, tlsF, alertF bool
	}{
		{"nil", nil, "unreachable", "unreachable", false, false, false, false},
		{"dial refused", &url.Error{Op: "Get", Err: refused}, "unreachable", "unreachable", true, false, false, false},
		{"read op error is not a dial", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, "unreachable", "unreachable", false, false, false, false},
		{"dns", &url.Error{Op: "Get", Err: dns}, "dns", "dns", true, true, false, false},
		{"dns inside dial", &net.OpError{Op: "dial", Err: dns}, "dns", "dns", true, true, false, false},
		{"deadline", &url.Error{Op: "Get", Err: context.DeadlineExceeded}, "timeout", "unreachable", true, false, false, false},
		{"net timeout", timeoutErr{}, "timeout", "unreachable", true, false, false, false},
		{"context canceled", context.Canceled, "unreachable", "unreachable", false, false, false, false},
		{"tls unknown authority", &url.Error{Op: "Get", Err: x509.UnknownAuthorityError{}}, "tls", "tls", false, false, true, false},
		{"tls hostname", x509.HostnameError{}, "tls", "tls", false, false, true, false},
		{"tls invalid cert", x509.CertificateInvalidError{}, "tls", "tls", false, false, true, false},
		{"tls verification", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "tls", "tls", false, false, true, false},
		{"tls record header", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "tls", "tls", false, false, true, false},
		{"tls alert", tls.AlertError(42), "tls", "unreachable", false, false, false, true},
		{"tls error from a dial", &net.OpError{Op: "dial", Err: x509.UnknownAuthorityError{}}, "tls", "tls", true, false, true, false},
		{"tls handshake timeout", timeoutErr{}, "timeout", "unreachable", true, false, false, false},
		{"eof", io.EOF, "unreachable", "unreachable", false, false, false, false},
		{"unexpected eof", io.ErrUnexpectedEOF, "unreachable", "unreachable", false, false, false, false},
		{"connection reset", syscall.ECONNRESET, "unreachable", "unreachable", false, false, false, false},
		{"plain message", errors.New("connection refused"), "unreachable", "unreachable", false, false, false, false},
		{"joined: a deadline behind a non-timeout net.Error", errors.Join(&net.OpError{Op: "read", Err: io.EOF}, context.DeadlineExceeded), "timeout", "unreachable", false, false, false, false},
		{"wrapped dns", fmt.Errorf("check: %w", &url.Error{Op: "Get", Err: dns}), "dns", "dns", true, true, false, false},
		{"wrapped tls", fmt.Errorf("a: %w", fmt.Errorf("b: %w", x509.UnknownAuthorityError{})), "tls", "tls", false, false, true, false},
		{"wrapped deadline", fmt.Errorf("a: %w", context.DeadlineExceeded), "timeout", "unreachable", true, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := Classify(c.err)
			if got := i.Reason(); got != c.reason {
				t.Errorf("Reason = %q, want %q", got, c.reason)
			}
			if got := i.CoarseReason(); got != c.coarse {
				t.Errorf("CoarseReason = %q, want %q", got, c.coarse)
			}
			if got := i.NoConnection(); got != c.noConn {
				t.Errorf("NoConnection = %v, want %v", got, c.noConn)
			}
			if i.DNS != c.dnsF || i.TLS != c.tlsF || i.TLSAlert != c.alertF {
				t.Errorf("facts = %+v", i)
			}
		})
	}
}
