package sealed

import (
	"bytes"
	_ "embed"
	"unicode/utf8"
)

// commonPass is the embedded list of common password patterns, lowercase,
// one per line, '#' lines being comments. It is text generated from
// hand-written patterns; no third-party list is copied.
//
//go:embed commonpass.txt
var commonPass []byte

var commonList = parseCommon(commonPass)

// parseCommon reads the list. A Windows checkout can turn the file's line
// ends into CRLF, so a trailing CR is trimmed from each line.
func parseCommon(raw []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		l = bytes.TrimRight(l, "\r")
		if len(l) > 0 && l[0] != '#' {
			out = append(out, l)
		}
	}
	return out
}

const minPassphrase = 12

// CheckPassphrase refuses a passphrase that is too short (under 12 bytes or
// under 12 characters, or not UTF-8), a single character repeated, a short
// pattern repeated, a run such as 123456789012 or abcdefghijkl, or a common
// password with or without a trailing run of digits and symbols. The
// messages are fixed and repeat nothing of the passphrase, which is never
// copied into a string. Rewrap and CreatePassphrase leave this check to the
// caller.
func CheckPassphrase(p []byte) error {
	if len(p) < minPassphrase || !utf8.Valid(p) || utf8.RuneCount(p) < minPassphrase {
		return fail(CodeWeakPassphrase, "", "the passphrase must be at least 12 characters")
	}
	runes := bytes.Runes(p)
	defer func() {
		for i := range runes {
			runes[i] = 0
		}
	}()
	if repeated(runes) || sequence(runes) {
		return fail(CodeWeakPassphrase, "", "the passphrase is too simple: it is a repeated pattern or a run of characters")
	}
	low := bytes.ToLower(p)
	defer zero(low)
	stem := bytes.TrimRight(low, "0123456789!@#$%^&*()-_=+.,?")
	if common(low) || (len(stem) > 0 && common(stem)) {
		return fail(CodeWeakPassphrase, "", "the passphrase is too common")
	}
	return nil
}

func common(b []byte) bool {
	for _, w := range commonList {
		if bytes.Equal(w, b) {
			return true
		}
	}
	return false
}

// repeated reports a passphrase made of one pattern of one to four
// characters repeated.
func repeated(r []rune) bool {
	for period := 1; period <= 4; period++ {
		ok := true
		for i := period; i < len(r); i++ {
			if r[i] != r[i-period] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// sequence reports a run going up or down by one character, 9 and 0, and z
// and a, being neighbours.
func sequence(r []rune) bool {
	for _, d := range []rune{1, -1} {
		ok := true
		for i := 1; i < len(r); i++ {
			if !step(r[i-1], r[i], d) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func step(a, b, d rune) bool {
	if b-a == d {
		return true
	}
	switch {
	case d == 1 && a == '9' && b == '0', d == -1 && a == '0' && b == '9':
		return true
	case d == 1 && a == 'z' && b == 'a', d == -1 && a == 'a' && b == 'z':
		return true
	}
	return false
}
