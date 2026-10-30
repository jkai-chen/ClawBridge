// Package transporterr classifies an error from an HTTP request as a
// transport failure (the server was not reached or its TLS handshake failed)
// as opposed to an answer. Callers pick the view they need from one set of
// facts, so the classification rules live in one place.
package transporterr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
)

// Info holds the facts found in an error chain. The zero value (from a nil
// or unrelated error) has every fact false.
type Info struct {
	DNS      bool // a *net.DNSError
	TLS      bool // certificate verification, record header, or x509 trust/name/validity errors
	TLSAlert bool // a tls.AlertError (a peer's handshake alert); not part of TLS
	Dial     bool // a *net.OpError for the "dial" operation
	Timeout  bool // context.DeadlineExceeded anywhere in the chain, or NetTimeout
	// NetTimeout: the first net.Error in the chain times out. Narrower than
	// Timeout only for a chain that holds several errors (errors.Join).
	NetTimeout bool
}

// Classify reads the facts out of err, following wrapped errors.
func Classify(err error) Info {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var op *net.OpError
	var ne net.Error
	netTimeout := errors.As(err, &ne) && ne.Timeout()
	return Info{
		DNS: errors.As(err, &dns),
		TLS: errors.As(err, &cert) || errors.As(err, &record) ||
			errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid),
		TLSAlert:   errors.As(err, &alert),
		Dial:       errors.As(err, &op) && op.Op == "dial",
		Timeout:    errors.Is(err, context.DeadlineExceeded) || netTimeout,
		NetTimeout: netTimeout,
	}
}

// Reason names why a request got no answer: "dns", "tls" (alerts included),
// "timeout", or "unreachable" for anything else. The words are a fixed set so
// a log never echoes the raw error.
func (i Info) Reason() string {
	switch {
	case i.DNS:
		return "dns"
	case i.TLS || i.TLSAlert:
		return "tls"
	case i.Timeout:
		return "timeout"
	}
	return "unreachable"
}

// CoarseReason is Reason without the alert and timeout distinctions: an alert
// or a timeout is "unreachable".
func (i Info) CoarseReason() string {
	switch {
	case i.DNS:
		return "dns"
	case i.TLS:
		return "tls"
	}
	return "unreachable"
}

// NoConnection reports that the server was never reached: a DNS failure, a
// failed dial or a net timeout, whatever else the chain holds (a TLS error
// from a dial still counts).
func (i Info) NoConnection() bool {
	return i.DNS || i.Dial || i.NetTimeout
}
