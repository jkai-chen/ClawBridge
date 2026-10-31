package devicekey

import (
	"bytes"
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

// The signed bases are fixed byte strings both sides build the same way.
func TestSignedBases(t *testing.T) {
	got := string(RequestBase("POST", "https://cb.example.com", "/v1/execute?a=%20", "1761498000", "bm9uY2U", []byte("{}")))
	want := "clawbridge-request-v1\nPOST\nhttps://cb.example.com\n/v1/execute?a=%20\n1761498000\nbm9uY2U\n44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if got != want {
		t.Fatalf("%q", got)
	}
	got = string(JoinProofBase("https://cb.example.com", "ada@example.com", "0000000000000000", "1761498000"))
	if !strings.HasPrefix(got, "clawbridge-join-v1\nhttps://cb.example.com\nada@example.com\n") || strings.Contains(got, "0000000000000000") || !strings.HasSuffix(got, "\n1761498000") {
		t.Fatalf("%q", got)
	}
}

// A caller-chosen code must be as strong as NewCode's: its exact shape, and
// not patterned. Every refusal leaves the code out of its text.
func TestStrongCode(t *testing.T) {
	for i := 0; i < 200; i++ {
		if c := NewCode(); !StrongCode(c) {
			t.Fatal("NewCode refused:", c)
		}
	}
	for name, code := range map[string]string{
		"too short":        "CBJ-ABCD-EFGH-JKMN",
		"too long":         "CBJ-ABCD-EFGH-JKMN-PQRS-TVWX",
		"wrong alphabet":   "CBJ-ABCD-EFGH-JKMN-PQRU",
		"look-alike":       "CBJ-ABCD-EFGH-JKMN-PQRO",
		"lower case":       "cbj-abcd-efgh-jkmn-pqrs",
		"wrong separators": "CBJ ABCD EFGH JKMN PQRS",
		"no separators":    "CBJ-ABCDEFGHJKMNPQRS",
		"no prefix":        "ABCD-EFGH-JKMN-PQRS",
		"one character":    "CBJ-AAAA-AAAA-AAAA-AAAA",
		"zeros":            "CBJ-0000-0000-0000-0000",
		"two characters":   "CBJ-ABAB-ABAB-ABAB-ABAB",
		"empty":            "",
	} {
		if StrongCode(code) {
			t.Errorf("%s: accepted", name)
		}
	}
}

// NewCode redraws a code StrongCode would refuse, so a generated code always
// passes: the first ten bytes here encode to sixteen zeros.
func TestNewCodeRedrawsWeakDraw(t *testing.T) {
	good := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x13, 0x57}
	r := bytes.NewReader(append(make([]byte, 10), good...))
	code := newCodeFrom(r)
	if !StrongCode(code) || r.Len() != 0 || strings.Contains(code, "0000") {
		t.Fatal("not redrawn:", code, r.Len())
	}
}
