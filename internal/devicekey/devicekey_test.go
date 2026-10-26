package devicekey

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

// A code is 80 bits in Crockford base32, and reads back whatever its case,
// spacing, dashes or prefix, with Crockford's look-alikes.
func TestCodeRoundTrip(t *testing.T) {
	code := NewCode()
	if len(code) != len("CBJ-XXXX-XXXX-XXXX-XXXX") || !strings.HasPrefix(code, CodePrefix) {
		t.Fatal(code)
	}
	want, ok := NormalCode(code)
	if !ok || len(want) != 16 {
		t.Fatal(want, ok)
	}
	for _, typed := range []string{strings.ToLower(code), strings.ReplaceAll(code, "-", " "), strings.TrimPrefix(code, CodePrefix), " " + code + " "} {
		if got, ok := NormalCode(typed); !ok || got != want {
			t.Fatalf("%q read as %q %v", typed, got, ok)
		}
	}
	if got, _ := NormalCode("CBJ-1O1L-0000-0000-000I"); got != "1011000000000001" {
		t.Fatal("look-alikes", got)
	}
	// A bare code that itself begins CBJ keeps those characters.
	if got, ok := NormalCode("CBJ0-0000-0000-0000"); !ok || got != "CBJ0000000000000" {
		t.Fatal("bare CBJ code", got, ok)
	}
	for _, bad := range []string{"", "CBJ-", "CBJ-AAAA-AAAA-AAAA-AAA", "CBJ-AAAA-AAAA-AAAA-AAAAA", "CBJ-AAAA-AAAA-AAAA-AAAU", "CBJ-AAAA-AAAA-AAAA-AAA!"} {
		if _, ok := NormalCode(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// The hash is salted by the invite's ID and never the code itself.
func TestCodeHash(t *testing.T) {
	h := CodeHash("inv_a", "0000000000000000")
	if h == CodeHash("inv_b", "0000000000000000") || strings.Contains(h, "0000000000000000") || !CodeMatches(h, "inv_a", "0000000000000000") || CodeMatches(h, "inv_a", "0000000000000001") {
		t.Fatal("hash", h)
	}
}

// The device ID depends on the key alone.
func TestDeviceID(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	if ID(pub) != ID(pub) || ID(pub) == ID(other) || len(ID(pub)) != len("dev_")+26 || ID(pub) != strings.ToLower(ID(pub)) {
		t.Fatal(ID(pub))
	}
	back, err := ParsePublicKey(EncodePublicKey(pub))
	if err != nil || !back.Equal(pub) {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "AAAA", EncodePublicKey(pub) + "AA", EncodePublicKey(pub) + "="} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
