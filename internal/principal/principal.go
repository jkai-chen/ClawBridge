// Package principal holds the one rule for a hosted email principal, shared by
// the token verifier, the server and the authority, so a principal one layer
// accepts is never rejected by another.
package principal

import (
	"net/mail"
	"strings"
)

// Valid reports whether p is a plain email address principal: at most 320
// bytes, exactly one @ with text on both sides, no whitespace or NUL, and
// exactly what net/mail parses back, so no display name or angle brackets.
func Valid(p string) bool {
	if len(p) > 320 || strings.Count(p, "@") != 1 || strings.ContainsAny(p, " \t\r\n\x00") || strings.HasPrefix(p, "@") || strings.HasSuffix(p, "@") {
		return false
	}
	address, err := mail.ParseAddress(p)
	return err == nil && address.Address == p
}

// Normal is the stored form of a principal: trimmed and lowercased, matching
// what the token verifier produces.
func Normal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }
