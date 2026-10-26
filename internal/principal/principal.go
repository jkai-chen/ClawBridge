// Package principal holds the one rule for a hosted email principal, shared by
// the token verifier, the server and the authority, so a principal one layer
// accepts is never rejected by another.
package principal

import (
	"net/mail"
	"regexp"
	"strings"
)

// Valid reports whether p is a plain email address principal: at most 320
// bytes, exactly one @ with text on both sides, no whitespace or NUL, and
// exactly what net/mail parses back, so no display name or angle brackets.
func Valid(p string) bool {
	if len(p) > 320 || strings.HasPrefix(p, CIPrefix) || strings.Count(p, "@") != 1 || strings.ContainsAny(p, " \t\r\n\x00") || strings.HasPrefix(p, "@") || strings.HasSuffix(p, "@") {
		return false
	}
	address, err := mail.ParseAddress(p)
	return err == nil && address.Address == p
}

// Normal is the stored form of a principal: trimmed and lowercased, matching
// what the token verifier produces.
func Normal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// NormalDomain is the one form of a Google Workspace domain (a token's hd
// claim or an allow-list entry): trimmed, lowercased and without a trailing
// dot. ok is false unless the result is a dotted DNS name of at most 253 bytes.
func NormalDomain(d string) (normal string, ok bool) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if len(d) > 253 || !domainPattern.MatchString(d) {
		return "", false
	}
	return d, true
}

// ServiceAccount reports whether p is a Google service account principal:
// user-managed (….iam.gserviceaccount.com), default compute
// (…@developer.gserviceaccount.com) or App Engine (…@appspot.gserviceaccount.com).
// Such accounts never carry an hd claim.
func ServiceAccount(p string) bool { return strings.HasSuffix(p, ".gserviceaccount.com") }

// CIPrefix starts a CI device's principal.
const CIPrefix = "ci:"

var ciPattern = regexp.MustCompile(`^ci:[a-z0-9][a-z0-9-]{0,39}@[a-z0-9][a-z0-9._-]{0,127}$`)

// CI reports whether p is a CI device's principal, ci:NAME@WORKSPACE. It is
// never an email (Valid refuses it), so no invite, role grant or Google
// account can take it; only creating a CI device admits it.
func CI(p string) bool { return ciPattern.MatchString(p) }

// CIPrincipal is the principal of the CI device name in workspace, or ""
// when name is not 1-40 lowercase letters, digits or hyphens.
func CIPrincipal(name, workspace string) string {
	p := CIPrefix + name + "@" + Normal(workspace)
	if !CI(p) {
		return ""
	}
	return p
}
