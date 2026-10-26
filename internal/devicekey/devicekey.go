// Package devicekey holds the forms ClawBridge's own sign-in shares between
// the server and the client: a device's ID derived from its Ed25519 public
// key, the invite code a person types, and the code's stored hash
// (live/AUTH-INVITE-DESIGN.md §3, §4).
package devicekey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// crockford is Crockford's base32 alphabet: no I, L, O or U, so a code read
// aloud has no ambiguous letters.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var encoding = base32.NewEncoding(crockford).WithPadding(base32.NoPadding)

// CodePrefix starts every invite code.
const CodePrefix = "CBJ-"

// codeChars is the code's length after its prefix: 16 characters, 80 bits.
const codeChars = 16

// NewCode makes an invite code: 80 random bits as CBJ-XXXX-XXXX-XXXX-XXXX.
func NewCode() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	s := encoding.EncodeToString(b[:])
	return CodePrefix + s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}

// NormalCode reads a typed code: any case, spaces or dashes, with or
// without its prefix, and Crockford's look-alikes (I and L as 1, O as 0).
// It returns the 16 characters, or false for anything else.
func NormalCode(in string) (string, bool) {
	s := strings.NewReplacer("-", "", " ", "").Replace(strings.ToUpper(in))
	// The prefix is told by length, since a bare code may itself begin CBJ.
	if len(s) == len("CBJ")+codeChars && strings.HasPrefix(s, "CBJ") {
		s = s[len("CBJ"):]
	}
	if len(s) != codeChars {
		return "", false
	}
	out := []byte(s)
	for i, c := range out {
		switch c {
		case 'I', 'L':
			out[i] = '1'
		case 'O':
			out[i] = '0'
		}
		if !strings.ContainsRune(crockford, rune(out[i])) {
			return "", false
		}
	}
	return string(out), true
}

// CodeHash is an invite code's stored form, salted by its invite's ID. code
// is the normal form (NormalCode).
func CodeHash(inviteID, code string) string {
	sum := sha256.Sum256([]byte("clawbridge-invite-v1" + inviteID + code))
	return hex.EncodeToString(sum[:])
}

// CodeMatches compares a stored hash with a code in constant time.
func CodeMatches(stored, inviteID, code string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(CodeHash(inviteID, code))) == 1
}

// NewInviteID names an invite: inv_ and 16 random characters.
func NewInviteID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "inv_" + strings.ToLower(encoding.EncodeToString(b[:]))
}

// ID derives a device's ID from its public key, so it can't be chosen:
// dev_ and the first 26 characters of the key's SHA-256 in base32.
func ID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "dev_" + strings.ToLower(encoding.EncodeToString(sum[:]))[:26]
}

// EncodePublicKey is a public key's wire and stored form: unpadded
// base64url.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub)
}

// ParsePublicKey reads EncodePublicKey's form, refusing any other length.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public key must be a 32-byte Ed25519 key in unpadded base64url")
	}
	return ed25519.PublicKey(b), nil
}

// RequestBase is the bytes a device signs for a request (§5): the method,
// the server's public origin, the request target exactly as sent (path and
// query), the timestamp and nonce as written in the header, and the body's
// SHA-256.
func RequestBase(method, origin, target, ts, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte("clawbridge-request-v1\n" + method + "\n" + origin + "\n" + target + "\n" + ts + "\n" + nonce + "\n" + hex.EncodeToString(sum[:]))
}

// JoinProofBase is the bytes a joining device signs to prove it holds the
// key it binds (§4). code is the normal form (NormalCode), so any typed
// spelling of a code gives one proof.
func JoinProofBase(origin, email, code, ts string) []byte {
	sum := sha256.Sum256([]byte(code))
	return []byte("clawbridge-join-v1\n" + origin + "\n" + email + "\n" + hex.EncodeToString(sum[:]) + "\n" + ts)
}
