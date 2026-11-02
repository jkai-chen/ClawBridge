package sealed

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPass  = "tangerine lantern quarry bishop"
	testPass2 = "marble vessel orchid tundra"
)

// lowKDF makes new wraps cheap (the parser's floor, one pass) for the test.
func lowKDF(t *testing.T) {
	t.Helper()
	old := newKDFParams
	newKDFParams = func() kdfParams { return kdfParams{m: minKDFMemory, t: 1, p: 1} }
	t.Cleanup(func() { newKDFParams = old })
}

// countKDF counts the real derivations the test makes.
func countKDF(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	old := kdf
	kdf = func(pw, salt []byte, tm, m uint32, p uint8, l uint32) []byte {
		n.Add(1)
		return old(pw, salt, tm, m, p, l)
	}
	t.Cleanup(func() { kdf = old })
	return &n
}

func fixedKey() []byte { return bytes.Repeat([]byte{7}, keyLen) }

const kidA = "0123456789abcdef"

func mustWrap(t *testing.T, kid string, key []byte, pass string) []byte {
	t.Helper()
	b, err := wrap(kid, key, []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hdrOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func hdrBytes(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keyFilePath(state string) string { return filepath.Join(state, "keys", "data.key") }

func readKeyHdr(t *testing.T, state string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(keyFilePath(state))
	if err != nil {
		t.Fatal(err)
	}
	return hdrOf(t, raw)
}

func TestWrapUnwrapRoundTrip(t *testing.T) {
	lowKDF(t)
	key := fixedKey()
	raw := mustWrap(t, kidA, key, testPass)
	h := hdrOf(t, raw)
	if h["v"] != float64(2) || h["mode"] != "passphrase" || h["kdf"] != "argon2id" || h["kid"] != kidA {
		t.Fatalf("%v", h)
	}
	kid, got, err := Unwrap(raw, []byte(testPass))
	if err != nil || kid != kidA || !bytes.Equal(got, key) {
		t.Fatalf("%v %q", err, kid)
	}
	// A fresh salt and nonce on every wrap.
	h2 := hdrOf(t, mustWrap(t, kidA, key, testPass))
	if h["salt"] == h2["salt"] || h["nonce"] == h2["nonce"] {
		t.Fatal("a wrap reused its salt or nonce")
	}
	// The result is the caller's: zeroing it changes nothing else.
	for i := range got {
		got[i] = 0
	}
	if _, again, err := Unwrap(raw, []byte(testPass)); err != nil || !bytes.Equal(again, key) {
		t.Fatal("the key was shared")
	}
	if _, _, err := Unwrap(encodeDataKey(kidA, key), []byte(testPass)); CodeOf(err) != CodeInvalid {
		t.Fatalf("a file-mode header unwrapped: %v", err)
	}
	if _, err := wrap(kidA, key, nil); CodeOf(err) != CodeInvalid {
		t.Fatal("wrapped under an empty passphrase")
	}
}

func TestWrongPassphraseIsOneFixedError(t *testing.T) {
	lowKDF(t)
	raw := mustWrap(t, kidA, fixedKey(), testPass)
	_, key, err := Unwrap(raw, []byte(testPass2))
	wantCode(t, err, CodeWrongPassphrase)
	if key != nil || !strings.Contains(err.Error(), "the passphrase is wrong") || strings.Contains(err.Error(), testPass2) {
		t.Fatalf("%v %v", key, err)
	}
	_, _, err = Unwrap(raw, nil)
	wantCode(t, err, CodeWrongPassphrase)
}

func TestTamperedWrapFailsWithoutPanic(t *testing.T) {
	lowKDF(t)
	raw := mustWrap(t, kidA, fixedKey(), testPass)
	flip := func(field string) func(map[string]any) {
		return func(m map[string]any) {
			b, _ := b64.DecodeString(m[field].(string))
			b[0] ^= 1
			m[field] = b64.EncodeToString(b)
		}
	}
	cases := map[string]func(map[string]any){
		"salt":    flip("salt"),
		"nonce":   flip("nonce"),
		"wrapped": flip("wrapped"),
		"tag": func(m map[string]any) {
			b, _ := b64.DecodeString(m["wrapped"].(string))
			b[47] ^= 1
			m["wrapped"] = b64.EncodeToString(b)
		},
		// The kid is in the AAD: the same key under another kid must not open.
		"kid": func(m map[string]any) { m["kid"] = "fedcba9876543210" },
		"m":   func(m map[string]any) { m["m"] = float64(minKDFMemory + 1) },
		"t":   func(m map[string]any) { m["t"] = float64(2) },
		"p":   func(m map[string]any) { m["p"] = float64(2) },
	}
	for name, mutate := range cases {
		m := hdrOf(t, raw)
		mutate(m)
		_, _, err := Unwrap(hdrBytes(t, m), []byte(testPass))
		if c := CodeOf(err); c != CodeWrongPassphrase && c != CodeRecordCorrupt {
			t.Errorf("%s: got %v", name, err)
		}
		if err == nil {
			t.Errorf("%s: a tampered header unwrapped", name)
		}
	}
}

func TestHeaderLimitsAreRefusedBeforeAnyKDF(t *testing.T) {
	lowKDF(t)
	calls := countKDF(t)
	raw := mustWrap(t, kidA, fixedKey(), testPass)
	calls.Store(0)
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	short := func(k string, n int) func(map[string]any) {
		return func(m map[string]any) {
			b, _ := b64.DecodeString(m[k].(string))
			m[k] = b64.EncodeToString(b[:n])
		}
	}
	long := func(k string) func(map[string]any) {
		return func(m map[string]any) {
			b, _ := b64.DecodeString(m[k].(string))
			m[k] = b64.EncodeToString(append(b, 0))
		}
	}
	cases := map[string]func(map[string]any){
		"m below the floor": set("m", float64(minKDFMemory-1)), "m one": set("m", float64(1)), "m zero": set("m", float64(0)),
		"m above the ceiling": set("m", float64(maxKDFMemory+1)), "m huge": set("m", float64(4294967295)),
		"t zero": set("t", float64(0)), "t above": set("t", float64(maxKDFTime+1)), "t huge": set("t", float64(4294967295)),
		"p zero": set("p", float64(0)), "p above": set("p", float64(maxKDFLanes+1)),
		"kdf": set("kdf", "argon2i"), "no kdf": set("kdf", ""), "a key too": set("key", fixedB64()),
		"salt short": short("salt", 15), "salt long": long("salt"), "nonce short": short("nonce", 23), "nonce long": long("nonce"),
		"wrapped short": short("wrapped", 47), "wrapped long": long("wrapped"),
		"salt not base64": set("salt", "!!!"), "nonce empty": set("nonce", ""), "wrapped empty": set("wrapped", ""),
	}
	for name, mutate := range cases {
		m := hdrOf(t, raw)
		mutate(m)
		b := hdrBytes(t, m)
		if _, _, err := Unwrap(b, []byte(testPass)); CodeOf(err) != CodeRecordCorrupt {
			t.Errorf("%s: Unwrap gave %v", name, err)
		}
		if _, _, err := parseDataKey(b, "p"); CodeOf(err) != CodeRecordCorrupt {
			t.Errorf("%s: parseDataKey gave %v", name, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the KDF ran %d times for a header outside the limits", n)
	}
	// The limits are inclusive.
	for _, m := range []map[string]any{
		{"m": float64(minKDFMemory), "t": float64(1), "p": float64(1)},
		{"m": float64(maxKDFMemory), "t": float64(maxKDFTime), "p": float64(maxKDFLanes)},
	} {
		h := hdrOf(t, raw)
		for k, v := range m {
			h[k] = v
		}
		if _, err := parseHeader(hdrBytes(t, h), "p"); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
}

func fixedB64() string { return b64.EncodeToString(fixedKey()) }

func hashRecords(t *testing.T, state string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, kind := range []string{KindDevice, KindLinear, KindLinearAPI} {
		ents, _ := os.ReadDir(filepath.Join(state, "keys", kind))
		for _, e := range ents {
			b, err := os.ReadFile(filepath.Join(state, "keys", kind, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[kind+"/"+e.Name()] = sha256.Sum256(b)
		}
	}
	return out
}

func sameHashes(t *testing.T, a, b map[string][32]byte) {
	t.Helper()
	if len(a) != 3 || len(a) != len(b) {
		t.Fatalf("%d records before, %d after", len(a), len(b))
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("record %s changed", k)
		}
	}
}

func unsealAll(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []struct {
		kind, id string
		pt       []byte
	}{{KindDevice, devA, seed}, {KindLinear, linA, blob}, {KindLinearAPI, linA, apiPT}} {
		got, err := s.Unseal(c.kind, c.id)
		if err != nil || !bytes.Equal(got, c.pt) {
			t.Fatalf("%s: %v", c.kind, err)
		}
	}
}

func TestRewrapCyclesLeaveTheRecordsAlone(t *testing.T) {
	lowKDF(t)
	s, state := open(t)
	seedAll(t, s)
	before := hashRecords(t, state)
	orig := readKeyHdr(t, state)
	kid, _ := orig["kid"].(string)
	origKey, _ := b64.DecodeString(orig["key"].(string))

	// file -> passphrase.
	wantCode(t, s.Rewrap("passphrase", nil, nil), CodeInvalid)
	if err := s.Rewrap("passphrase", nil, []byte(testPass)); err != nil {
		t.Fatal(err)
	}
	h1 := readKeyHdr(t, state)
	if h1["mode"] != "passphrase" || h1["kid"] != kid || h1["key"] != nil {
		t.Fatalf("%v", h1)
	}
	if len(s.keys) != 0 {
		t.Fatal("the key cache survived the switch")
	}
	wantCode(t, s.Seal(KindLinear, linA, blob), CodePassphraseMode)
	plain, err := Open(filepath.Dir(filepath.Dir(keyFilePath(state))))
	if err != nil {
		t.Fatal(err)
	}
	_, err = plain.Unseal(KindLinear, linA)
	wantCode(t, err, CodePassphraseMode)
	if rep := plain.Check(); rep.DataKey.Mode != "passphrase" || rep.DataKey.KidPrefix != kid[:8] {
		t.Fatalf("%+v", rep.DataKey)
	}
	raw, _ := os.ReadFile(keyFilePath(state))
	k1, key, err := Unwrap(raw, []byte(testPass))
	if err != nil || k1 != kid || !bytes.Equal(key, origKey) {
		t.Fatal("the key or its ID changed", err)
	}
	u, err := OpenUnlocked(state, kid, key)
	if err != nil {
		t.Fatal(err)
	}
	unsealAll(t, u)
	sameHashes(t, before, hashRecords(t, state))

	// passphrase -> passphrase: the old passphrase must be right and stops working.
	wantCode(t, plain.Rewrap("passphrase", []byte(testPass2), []byte(testPass2)), CodeWrongPassphrase)
	if after, _ := os.ReadFile(keyFilePath(state)); !bytes.Equal(after, raw) {
		t.Fatal("a refused rewrap changed data.key")
	}
	if err = plain.Rewrap("passphrase", []byte(testPass), []byte(testPass2)); err != nil {
		t.Fatal(err)
	}
	h2 := readKeyHdr(t, state)
	if h2["salt"] == h1["salt"] || h2["nonce"] == h1["nonce"] || h2["kid"] != kid {
		t.Fatalf("%v", h2)
	}
	raw2, _ := os.ReadFile(keyFilePath(state))
	if _, _, err = Unwrap(raw2, []byte(testPass)); CodeOf(err) != CodeWrongPassphrase {
		t.Fatal("the old passphrase still opens it")
	}
	// The same passphrase again still gets a fresh salt.
	if err = plain.Rewrap("passphrase", []byte(testPass2), []byte(testPass2)); err != nil {
		t.Fatal(err)
	}
	h3 := readKeyHdr(t, state)
	if h3["salt"] == h2["salt"] || h3["nonce"] == h2["nonce"] {
		t.Fatal("a rewrap reused its salt or nonce")
	}

	// passphrase -> file.
	wantCode(t, plain.Rewrap("file", nil, nil), CodeWrongPassphrase)
	if err = plain.Rewrap("file", []byte(testPass2), nil); err != nil {
		t.Fatal(err)
	}
	h4 := readKeyHdr(t, state)
	if h4["mode"] != "file" || h4["kid"] != kid || h4["key"] != orig["key"] {
		t.Fatalf("%v", h4)
	}
	wantCode(t, plain.Rewrap("file", nil, nil), CodeInvalid)
	wantCode(t, plain.Rewrap("tpm", nil, nil), CodeInvalid)
	back, err := Open(filepath.Dir(filepath.Dir(keyFilePath(state))))
	if err != nil {
		t.Fatal(err)
	}
	unsealAll(t, back)
	sameHashes(t, before, hashRecords(t, state))
}

func TestRewrapIsRefusedWhileARotationIsUnderWay(t *testing.T) {
	lowKDF(t)
	s, state := open(t)
	seedAll(t, s)
	if err := os.WriteFile(filepath.Join(state, "keys", nextKeyName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.Rewrap("passphrase", nil, []byte(testPass)), CodeIO)
	if readKeyHdr(t, state)["mode"] != "file" {
		t.Fatal("data.key changed")
	}
}

func TestOpenUnlockedChecksTheKeyOnDisk(t *testing.T) {
	lowKDF(t)
	state := newState(t)
	if err := CreatePassphrase(state, []byte(testPass)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(keyFilePath(state))
	kid, key, err := Unwrap(raw, []byte(testPass))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenUnlocked(state, "fedcba9876543210", key); CodeOf(err) != CodeMissingDataKey {
		t.Fatalf("a mismatched kid was accepted: %v", err)
	}
	if _, err = OpenUnlocked(state, "xyz", key); CodeOf(err) != CodeInvalid {
		t.Fatal(err)
	}
	if _, err = OpenUnlocked(state, kid, key[:16]); CodeOf(err) != CodeInvalid {
		t.Fatal(err)
	}
	s, err := OpenUnlocked(state, kid, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Seal(KindLinear, linA, blob); err != nil {
		t.Fatal(err)
	}
	// The store holds a copy: the caller may zero its own.
	for i := range key {
		key[i] = 0
	}
	if got, err := s.Unseal(KindLinear, linA); err != nil || !bytes.Equal(got, blob) {
		t.Fatal(err)
	}
	// Lock forgets the key, zeroing what it held.
	held := s.keys[kid]
	s.Lock()
	for _, b := range held {
		if b != 0 {
			t.Fatal("Lock left key bytes")
		}
	}
	_, err = s.Unseal(KindLinear, linA)
	wantCode(t, err, CodePassphraseMode)

	// A file-mode store: the key must be the one on disk.
	fs, fstate := open(t)
	seedAll(t, fs)
	h := readKeyHdr(t, fstate)
	fkey, _ := b64.DecodeString(h["key"].(string))
	if _, err = OpenUnlocked(fstate, h["kid"].(string), fkey); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenUnlocked(fstate, h["kid"].(string), bytes.Repeat([]byte{9}, keyLen)); CodeOf(err) != CodeMissingDataKey {
		t.Fatalf("a wrong file-mode key was accepted: %v", err)
	}
	// A store with no data.key is not made one.
	empty := newState(t)
	if _, err = OpenUnlocked(empty, kid, fkey); CodeOf(err) != CodeMissingDataKey {
		t.Fatal(err)
	}
	if _, err = os.Stat(keyFilePath(empty)); err == nil {
		t.Fatal("OpenUnlocked made a data key")
	}
}

func TestCreatePassphraseNeverReplaces(t *testing.T) {
	lowKDF(t)
	state := newState(t)
	wantCode(t, CreatePassphrase(state, nil), CodeInvalid)
	if err := CreatePassphrase(state, []byte(testPass)); err != nil {
		t.Fatal(err)
	}
	if h := readKeyHdr(t, state); h["mode"] != "passphrase" {
		t.Fatalf("%v", h)
	}
	before, _ := os.ReadFile(keyFilePath(state))
	wantCode(t, CreatePassphrase(state, []byte(testPass2)), CodeInvalid)
	if after, _ := os.ReadFile(keyFilePath(state)); !bytes.Equal(before, after) {
		t.Fatal("an existing data.key was replaced")
	}

	// Records without a data.key: the key is gone, and none is made.
	s, state2 := open(t)
	seedAll(t, s)
	if err := os.Remove(keyFilePath(state2)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, CreatePassphrase(state2, []byte(testPass)), CodeInvalid)
	if _, err := os.Stat(keyFilePath(state2)); err == nil {
		t.Fatal("a data key was made beside orphaned records")
	}

	// A rotation file alone.
	state3 := newState(t)
	if _, err := openDir(state3); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state3, "keys", nextKeyName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantCode(t, CreatePassphrase(state3, []byte(testPass)), CodeInvalid)
}

func openUnlockedHeld(t *testing.T, state, pass string) (*Store, string, []byte) {
	t.Helper()
	raw, err := os.ReadFile(keyFilePath(state))
	if err != nil {
		t.Fatal(err)
	}
	kid, key, err := Unwrap(raw, []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenUnlocked(state, kid, key)
	if err != nil {
		t.Fatal(err)
	}
	return s, kid, key
}

func TestRotateWrappedKeepsTheWrapAndReseals(t *testing.T) {
	lowKDF(t)
	state := newState(t)
	if err := CreatePassphrase(state, []byte(testPass)); err != nil {
		t.Fatal(err)
	}
	s, oldKid, _ := openUnlockedHeld(t, state, testPass)
	seedAll(t, s)
	wantCode(t, s.Rotate(), CodePassphraseMode)
	wantCode(t, s.RotateWrapped([]byte(testPass2)), CodeWrongPassphrase)
	if readKeyHdr(t, state)["kid"] != oldKid {
		t.Fatal("a wrong passphrase rotated")
	}
	if err := s.RotateWrapped([]byte(testPass)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "keys", nextKeyName)); err == nil {
		t.Fatal("data.key.next is left")
	}
	h := readKeyHdr(t, state)
	newKid := h["kid"].(string)
	if h["mode"] != "passphrase" || newKid == oldKid {
		t.Fatalf("%v", h)
	}
	for _, c := range []struct{ kind, id string }{{KindDevice, devA}, {KindLinear, linA}, {KindLinearAPI, linA}} {
		if readRec(t, recFile(state, c.kind, c.id))["kid"] != newKid {
			t.Fatalf("%s was not resealed", c.kind)
		}
	}
	unsealAll(t, s)
	again, _, _ := openUnlockedHeld(t, state, testPass)
	unsealAll(t, again)

	// A store that was not opened unlocked cannot rotate.
	plain, _ := Open(state)
	wantCode(t, plain.RotateWrapped([]byte(testPass)), CodeInvalid)
}

func TestRotateWrappedFinishesACrashedRotation(t *testing.T) {
	lowKDF(t)
	state := newState(t)
	if err := CreatePassphrase(state, []byte(testPass)); err != nil {
		t.Fatal(err)
	}
	s, oldKid, _ := openUnlockedHeld(t, state, testPass)
	seedAll(t, s)
	s.afterReseal = func(done int) error {
		if done == 1 {
			return os.ErrClosed
		}
		return nil
	}
	if err := s.RotateWrapped([]byte(testPass)); err == nil {
		t.Fatal("the crash did not stop the rotation")
	}
	nextRaw, err := os.ReadFile(filepath.Join(state, "keys", nextKeyName))
	if err != nil {
		t.Fatal(err)
	}
	if h := hdrOf(t, nextRaw); h["mode"] != "passphrase" || h["kid"] == oldKid {
		t.Fatalf("data.key.next is not a wrapped new key: %v", h)
	}
	// A new process opens (the wrapped .next waits for the passphrase) and finishes it.
	s2, _, _ := openUnlockedHeld(t, state, testPass)
	if err := s2.RotateWrapped([]byte(testPass)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "keys", nextKeyName)); err == nil {
		t.Fatal("data.key.next is left")
	}
	s3, _, _ := openUnlockedHeld(t, state, testPass)
	unsealAll(t, s3)
}

func TestCheckPassphrase(t *testing.T) {
	bad := []string{
		"", "short", "elevenchars", "ééééééééééé", // 11 runes, 22 bytes
		"aaaaaaaaaaaa", "            ", "123456789012", "abcdefghijkl", "210987654321", "zyxwvutsrqpo", "ababababababab", "xyzabcdefghi",
		"Password1234", "PASSWORD123456", "qwertyuiopas", "letmein12345!", "iloveyou1234", "passwordpassword", "Qwerty123456",
		"\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8\xf7\xf6\xf5\xf4",
	}
	for _, p := range bad {
		wantCode(t, CheckPassphrase([]byte(p)), CodeWeakPassphrase)
	}
	for _, p := range []string{testPass, testPass2, "twelve chars", "aaaaaaaaaaab", "correct horse battery staple", "日本語のパスフレーズです長い"} {
		if err := CheckPassphrase([]byte(p)); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	// The message repeats nothing of the passphrase.
	err := CheckPassphrase([]byte("Password1234"))
	if strings.Contains(strings.ToLower(err.Error()), "password1234") {
		t.Fatal(err)
	}
	if len(commonList) < 100 || len(commonList) > 1000 {
		t.Fatalf("the list has %d entries", len(commonList))
	}
	for _, w := range commonList {
		if !bytes.Equal(w, bytes.ToLower(w)) {
			t.Fatalf("%q is not lowercase", w)
		}
	}
}

// R1 A3: the passphrase is never a string.
func TestNoPassphraseStringConversionInTheSource(t *testing.T) {
	re := regexp.MustCompile(`string\((old|new)?[pP]ass`)
	files, _ := filepath.Glob("*.go")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		n++
		b, _ := os.ReadFile(f)
		if loc := re.Find(b); loc != nil {
			t.Errorf("%s converts a passphrase to a string: %s", f, loc)
		}
	}
	if n < 5 {
		t.Fatal("found too few source files")
	}
}

// One wrap at the real parameters (m=64 MiB, t=3, p=1), to prove they work.
func TestRealKDFParameters(t *testing.T) {
	if testing.Short() {
		t.Skip("real KDF parameters")
	}
	key := fixedKey()
	t0 := time.Now()
	raw := mustWrap(t, kidA, key, testPass)
	wrapTime := time.Since(t0)
	h := hdrOf(t, raw)
	if h["m"] != float64(65536) || h["t"] != float64(3) || h["p"] != float64(1) {
		t.Fatalf("%v", h)
	}
	t0 = time.Now()
	_, got, err := Unwrap(raw, []byte(testPass))
	unwrapTime := time.Since(t0)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal(err)
	}
	t.Logf("real parameters: wrap %v, unwrap %v", wrapTime, unwrapTime)
	if unwrapTime > 5*time.Second {
		t.Fatalf("unwrap took %v", unwrapTime)
	}
}

func FuzzDataKeyHeader(f *testing.F) {
	var calls atomic.Int64
	old := kdf
	kdf = func(pw, salt []byte, tm, m uint32, p uint8, l uint32) []byte {
		calls.Add(1)
		return make([]byte, l)
	}
	defer func() { kdf = old }()
	oldP := newKDFParams
	newKDFParams = func() kdfParams { return kdfParams{m: minKDFMemory, t: 1, p: 1} }
	defer func() { newKDFParams = oldP }()
	wrapped, _ := wrap(kidA, fixedKey(), []byte(testPass))
	f.Add(wrapped)
	f.Add(encodeDataKey(kidA, fixedKey()))
	f.Add([]byte(`{"v":2,"kid":"0123456789abcdef","mode":"passphrase","kdf":"argon2id","m":4294967295,"t":4294967295,"p":255,"salt":"","nonce":"","wrapped":""}`))
	f.Add([]byte(`{"v":2,"kid":"0123456789abcdef","mode":"passphrase"}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		before := calls.Load()
		_, key, err := Unwrap(raw, []byte("x"))
		ran := calls.Load() != before
		h, perr := parseHeader(raw, "p")
		if ran && (perr != nil || h.Mode != modePassphrase) {
			t.Fatal("the KDF ran for a header the parser refuses")
		}
		if !ran && err == nil {
			t.Fatal("unwrapped without a KDF")
		}
		if err == nil && len(key) != keyLen {
			t.Fatal("a key of the wrong size")
		}
		if ran && h.M > maxKDFMemory {
			t.Fatal("the KDF ran above the ceiling")
		}
		parseDataKey(raw, "p")
	})
}

// A Windows checkout can give the embedded list CRLF line ends; the parsed
// list must be the same, so a listed password is still refused.
func TestCommonListParsesCRLF(t *testing.T) {
	crlf := bytes.ReplaceAll(commonPass, []byte("\n"), []byte("\r\n"))
	got := parseCommon(crlf)
	if len(got) != len(commonList) {
		t.Fatalf("the CRLF list has %d entries, want %d", len(got), len(commonList))
	}
	for i, w := range got {
		if !bytes.Equal(w, commonList[i]) || bytes.ContainsRune(w, '\r') {
			t.Fatalf("entry %d differs: %q", i, w)
		}
	}
	saved := commonList
	t.Cleanup(func() { commonList = saved })
	commonList = got
	for _, w := range saved {
		if len(w) >= minPassphrase {
			if err := CheckPassphrase(w); CodeOf(err) != CodeWeakPassphrase {
				t.Fatalf("a listed password passed with the CRLF list: %v", err)
			}
			return
		}
	}
	t.Skip("no listed password is long enough")
}
