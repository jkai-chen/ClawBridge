package sealed

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/fsprivate"
)

var (
	devA  = devicekey.ID(ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey))
	devB  = devicekey.ID(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32)).Public().(ed25519.PublicKey))
	linA  = "lin_" + strings.Repeat("0", 32)
	linB  = "lin_" + strings.Repeat("1", 32)
	seed  = []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") // a device seed, base64url
	blob  = []byte("{\"version\": 1,  \"token\":\"synthetic-access-token\",\"name\":\"caf\\u00e9\", \"expires\":\"2030-01-01T00:00:00Z\"}\n")
	apiPT = []byte("lin_api_" + strings.Repeat("a", 40))
)

func newState(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := fsprivate.MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	state := newState(t)
	s, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	return s, state
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	if err == nil || CodeOf(err) != code {
		t.Fatalf("got %v (code %q), want code %q", err, CodeOf(err), code)
	}
}

func recFile(state, kind, id string) string {
	return filepath.Join(state, "keys", kind, id+suffix)
}

func seedAll(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []struct {
		kind, id string
		pt       []byte
	}{{KindDevice, devA, seed}, {KindLinear, linA, blob}, {KindLinearAPI, linA, apiPT}} {
		if err := s.Seal(c.kind, c.id, c.pt); err != nil {
			t.Fatal(err)
		}
	}
}

func readRec(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeRec(t *testing.T, path string, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTripPerKindIsByteExact(t *testing.T) {
	s, _ := open(t)
	cases := []struct {
		kind, id string
		pt       []byte
	}{
		{KindDevice, devA, seed},
		{KindLinear, linA, blob},
		{KindLinearAPI, linA, apiPT},
		{KindLinear, linB, []byte{}},
		{KindLinear, linB, []byte{0, 255, '\n', 0, 13, 10}},
	}
	for _, c := range cases {
		if err := s.Seal(c.kind, c.id, c.pt); err != nil {
			t.Fatal(err)
		}
		got, err := s.Unseal(c.kind, c.id)
		if err != nil || !bytes.Equal(got, c.pt) {
			t.Fatalf("%s %s: %q %v", c.kind, c.id, got, err)
		}
	}
	// The same plaintext twice gives different files: the nonce is fresh.
	// (Asserted by reading two seals of one record.)
}

func TestSealTwiceUsesAFreshNonceAndReplaces(t *testing.T) {
	s, state := open(t)
	p := recFile(state, KindDevice, devA)
	s.Seal(KindDevice, devA, seed)
	first, _ := os.ReadFile(p)
	s.Seal(KindDevice, devA, seed)
	second, _ := os.ReadFile(p)
	if bytes.Equal(first, second) {
		t.Fatal("two seals wrote the same bytes")
	}
	s.Seal(KindDevice, devA, []byte("other"))
	if got, _ := s.Unseal(KindDevice, devA); string(got) != "other" {
		t.Fatal(string(got))
	}
}

func TestRejectsBadKindsIDsAndHugeSecrets(t *testing.T) {
	s, _ := open(t)
	for _, c := range [][2]string{{"nope", linA}, {KindLinear, "../x"}, {KindLinear, "lin_short"}, {KindDevice, linA}, {KindLinear, devA}, {KindLinear, ""}, {KindDevice, "dev_" + strings.Repeat("l", 26)}} {
		wantCode(t, s.Seal(c[0], c[1], []byte("x")), CodeInvalid)
		_, err := s.Unseal(c[0], c[1])
		wantCode(t, err, CodeInvalid)
		wantCode(t, s.Delete(c[0], c[1]), CodeInvalid)
	}
	_, err := s.List("nope")
	wantCode(t, err, CodeInvalid)
	wantCode(t, s.Seal(KindLinear, linA, make([]byte, maxPlaintext+1)), CodeInvalid)
	if err := s.Seal(KindLinear, linA, make([]byte, maxPlaintext)); err != nil {
		t.Fatal("the largest secret was refused:", err)
	}
}

func TestUnsealMissingIsNotFoundAndDeleteIsIdempotent(t *testing.T) {
	s, _ := open(t)
	_, err := s.Unseal(KindLinear, linA)
	wantCode(t, err, CodeNotFound)
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("errors.Is does not match ErrNotFound")
	}
	if err = s.Delete(KindLinear, linA); err != nil {
		t.Fatal(err)
	}
	s.Seal(KindLinear, linA, blob)
	if err = s.Delete(KindLinear, linA); err != nil {
		t.Fatal(err)
	}
	_, err = s.Unseal(KindLinear, linA)
	wantCode(t, err, CodeNotFound)
}

func TestListIsSortedPerKindAndIgnoresStrays(t *testing.T) {
	s, state := open(t)
	s.Seal(KindLinear, linB, blob)
	s.Seal(KindLinear, linA, blob)
	s.Seal(KindDevice, devA, seed)
	os.WriteFile(filepath.Join(state, "keys", KindLinear, "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(state, "keys", KindLinear, "bad name.sealed"), []byte("x"), 0o600)
	got, err := s.List(KindLinear)
	if err != nil || fmt.Sprint(got) != fmt.Sprint([]string{linA, linB}) {
		t.Fatal(got, err)
	}
	if got, _ = s.List(KindLinearAPI); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestTamperedRecordsFailWithFixedCodes(t *testing.T) {
	flip := func(field string) func(map[string]any) {
		return func(m map[string]any) {
			b := []byte(m[field].(string))
			b[3] ^= 1
			if b[3] == '=' || b[3] == 0 {
				b[3] = 'A'
			}
			m[field] = string(b)
		}
	}
	other := "0123456789abcdef"
	cases := map[string]struct {
		edit func(map[string]any)
		want Code
	}{
		"flipped ciphertext":  {flip("ct"), CodeRecordCorrupt},
		"flipped nonce":       {flip("nonce"), CodeRecordCorrupt},
		"other kid":           {func(m map[string]any) { m["kid"] = other }, CodeRecordCorrupt},
		"short nonce":         {func(m map[string]any) { m["nonce"] = "AAAA" }, CodeRecordCorrupt},
		"bad version":         {func(m map[string]any) { m["v"] = 9 }, CodeRecordCorrupt},
		"unknown field":       {func(m map[string]any) { m["extra"] = 1 }, CodeRecordCorrupt},
		"id swapped in place": {func(m map[string]any) { m["id"] = linB }, CodeRecordMismatch},
		"kind swapped":        {func(m map[string]any) { m["kind"] = KindLinearAPI }, CodeRecordMismatch},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, state := open(t)
			s.Seal(KindLinear, linA, blob)
			p := recFile(state, KindLinear, linA)
			m := readRec(t, p)
			c.edit(m)
			writeRec(t, p, m)
			_, err := s.Unseal(KindLinear, linA)
			wantCode(t, err, c.want)
			if strings.Contains(err.Error(), "synthetic-access-token") {
				t.Fatal("the error repeats content")
			}
		})
	}
}

func TestRecordMovedToAnotherNameIsAMismatch(t *testing.T) {
	s, state := open(t)
	s.Seal(KindLinear, linA, blob)
	raw, _ := os.ReadFile(recFile(state, KindLinear, linA))
	os.WriteFile(recFile(state, KindLinear, linB), raw, 0o600)
	_, err := s.Unseal(KindLinear, linB)
	wantCode(t, err, CodeRecordMismatch)
	// A record in the wrong kind's directory is a mismatch too.
	os.MkdirAll(filepath.Join(state, "keys", KindLinearAPI), 0o700)
	fsprivate.MakePrivate(filepath.Join(state, "keys", KindLinearAPI))
	os.WriteFile(recFile(state, KindLinearAPI, linA), raw, 0o600)
	_, err = s.Unseal(KindLinearAPI, linA)
	wantCode(t, err, CodeRecordMismatch)
}

// A record whose own fields were rewritten to match a new file name still
// does not open: the ciphertext is bound to the original kind and ID.
func TestRewrittenRecordFieldsDoNotOpenBecauseOfTheBoundAAD(t *testing.T) {
	s, state := open(t)
	s.Seal(KindLinear, linA, blob)
	m := readRec(t, recFile(state, KindLinear, linA))
	m["id"] = linB
	writeRec(t, recFile(state, KindLinear, linB), m)
	_, err := s.Unseal(KindLinear, linB)
	wantCode(t, err, CodeRecordCorrupt)
	m["id"], m["kind"] = linA, KindLinearAPI
	os.MkdirAll(filepath.Join(state, "keys", KindLinearAPI), 0o700)
	fsprivate.MakePrivate(filepath.Join(state, "keys", KindLinearAPI))
	writeRec(t, recFile(state, KindLinearAPI, linA), m)
	_, err = s.Unseal(KindLinearAPI, linA)
	wantCode(t, err, CodeRecordCorrupt)
}

func TestTruncatedEmptyAndOversizedRecordsAreCorruptNotPanics(t *testing.T) {
	s, state := open(t)
	s.Seal(KindLinear, linA, blob)
	p := recFile(state, KindLinear, linA)
	raw, _ := os.ReadFile(p)
	for _, b := range [][]byte{raw[:len(raw)/2], raw[:1], {}, bytes.Repeat([]byte("x"), maxRecordSize+1), []byte("null"), []byte("[]"), append(append([]byte{}, raw...), raw...)} {
		os.WriteFile(p, b, 0o600)
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeRecordCorrupt)
	}
}

func TestFreshStateMakesTheKeyDirectoryAndDataKey(t *testing.T) {
	state := filepath.Join(newState(t), "nested", "state")
	s, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(state, "keys", "data.key"))
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		V    int    `json:"v"`
		Kid  string `json:"kid"`
		Mode string `json:"mode"`
		Key  string `json:"key"`
	}
	if err = json.Unmarshal(raw, &h); err != nil || h.V != 2 || h.Mode != "file" || !kidPattern.MatchString(h.Kid) {
		t.Fatal(string(raw), err)
	}
	if k, err := b64.DecodeString(h.Key); err != nil || len(k) != 32 {
		t.Fatal("the key is not 32 bytes")
	}
	if err = fsprivate.CheckPrivate(filepath.Join(state, "keys"), true); err != nil {
		t.Fatal(err)
	}
	if err = fsprivate.CheckPrivate(filepath.Join(state, "keys", "data.key"), false); err != nil {
		t.Fatal(err)
	}
	if s.Seal(KindDevice, devA, seed) != nil {
		t.Fatal("cannot seal")
	}
	if err = fsprivate.CheckPrivate(filepath.Join(state, "keys", KindDevice), true); err != nil {
		t.Fatal(err)
	}
	if err = fsprivate.CheckPrivate(recFile(state, KindDevice, devA), false); err != nil {
		t.Fatal(err)
	}
}

func TestMissingDataKeyWithRecordsIsRefusedAndNeverRegenerated(t *testing.T) {
	s, state := open(t)
	seedAll(t, s)
	dk := filepath.Join(state, "keys", "data.key")
	if err := os.Remove(dk); err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]func() (*Store, error){
		"the open store": func() (*Store, error) { return s, nil },
		"a reopened one": func() (*Store, error) { return Open(state) },
	} {
		t.Run(name, func(t *testing.T) {
			s2, err := st()
			if err != nil {
				t.Fatal(err)
			}
			wantCode(t, s2.Seal(KindLinear, linB, blob), CodeMissingDataKey)
			wantCode(t, s2.Rotate(), CodeMissingDataKey)
			if _, err = os.Lstat(dk); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("data.key was made again")
			}
		})
	}
	s3, _ := Open(state)
	_, err := s3.Unseal(KindLinear, linA)
	wantCode(t, err, CodeMissingDataKey)
	if !strings.Contains(err.Error(), dk) {
		t.Fatal("the message does not name the path")
	}
	rep := s3.Check()
	if rep.DataKey.Present || rep.DataKey.Err != CodeMissingDataKey || rep.OK() {
		t.Fatalf("%+v", rep)
	}
	// With the records gone too, a store may make a key again.
	for _, r := range []struct{ k, i string }{{KindDevice, devA}, {KindLinear, linA}, {KindLinearAPI, linA}} {
		s3.Delete(r.k, r.i)
	}
	if _, err = Open(state); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(dk); err != nil {
		t.Fatal("no key was made for an empty store:", err)
	}
}

func TestConcurrentOpensAgreeOnOneKey(t *testing.T) {
	state := newState(t)
	const n = 12
	stores := make([]*Store, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stores[i], errs[i] = Open(state)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(i, err)
		}
	}
	if err := stores[0].Seal(KindLinear, linA, blob); err != nil {
		t.Fatal(err)
	}
	for i, s := range stores {
		if got, err := s.Unseal(KindLinear, linA); err != nil || !bytes.Equal(got, blob) {
			t.Fatal(i, err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(state, "keys"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatal("left a temporary file:", e.Name())
		}
	}
}

func TestPassphraseHeaderIsLockedNotCorrupt(t *testing.T) {
	state := newState(t)
	keys := filepath.Join(state, "keys")
	os.Mkdir(keys, 0o700)
	fsprivate.MakePrivate(keys)
	hdr := `{"v":2,"kid":"0123456789abcdef","mode":"passphrase","kdf":"argon2id","m":65536,"t":3,"p":1,"salt":"AAAA","nonce":"AAAA","wrapped":"AAAA"}`
	os.WriteFile(filepath.Join(keys, "data.key"), []byte(hdr), 0o600)
	// A record sealed elsewhere stands in for one sealed before the switch.
	donor, donorState := open(t)
	donor.Seal(KindLinear, linA, blob)
	raw, _ := os.ReadFile(recFile(donorState, KindLinear, linA))
	os.Mkdir(filepath.Join(keys, KindLinear), 0o700)
	fsprivate.MakePrivate(filepath.Join(keys, KindLinear))
	os.WriteFile(recFile(state, KindLinear, linA), raw, 0o600)
	s, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.Seal(KindLinear, linA, blob), CodePassphraseMode)
	_, err = s.Unseal(KindLinear, linA)
	wantCode(t, err, CodePassphraseMode)
	if !errors.Is(err, ErrPassphraseMode) {
		t.Fatal("errors.Is does not match")
	}
	wantCode(t, s.Rotate(), CodePassphraseMode)
	rep := s.Check()
	if rep.DataKey.Mode != "passphrase" || !rep.DataKey.Parses || rep.DataKey.KidPrefix != "01234567" || rep.DataKey.Err != CodePassphraseMode {
		t.Fatalf("%+v", rep.DataKey)
	}
	// The key file is untouched.
	if b, _ := os.ReadFile(filepath.Join(keys, "data.key")); string(b) != hdr {
		t.Fatal("data.key was rewritten")
	}
}

var badDataKeys = map[string]string{
	"empty": "", "text": "hello", "partial": `{"v":2,"kid":"0123456789abcdef","mo`,
	"old version": `{"v":1,"kid":"0123456789abcdef","mode":"file","key":"AAAA"}`,
	"short key":   `{"v":2,"kid":"0123456789abcdef","mode":"file","key":"AAAA"}`,
	"bad kid":     `{"v":2,"kid":"zz","mode":"file","key":"` + strings.Repeat("A", 43) + `="}`,
	"odd mode":    `{"v":2,"kid":"0123456789abcdef","mode":"tpm"}`,
	"huge":        strings.Repeat(" ", maxKeyFileSize+1),
}

// With a record present, a damaged data.key is never replaced: that would
// orphan the record.
func TestDamagedDataKeyWithRecordsIsCorruptAndNeverReplaced(t *testing.T) {
	for name, content := range badDataKeys {
		t.Run(name, func(t *testing.T) {
			s, state := open(t)
			s.Seal(KindLinear, linA, blob)
			dk := filepath.Join(state, "keys", "data.key")
			os.WriteFile(dk, []byte(content), 0o600)
			s2, err := Open(state)
			if err == nil {
				err = s2.Seal(KindLinear, linB, blob)
			}
			wantCode(t, err, CodeRecordCorrupt)
			if b, _ := os.ReadFile(dk); string(b) != content {
				t.Fatal("data.key was replaced")
			}
		})
	}
}

// With no record anywhere, a damaged data.key orphans nothing and is made
// again.
func TestDamagedDataKeyWithoutRecordsIsReplaced(t *testing.T) {
	for name, content := range badDataKeys {
		t.Run(name, func(t *testing.T) {
			state := newState(t)
			keys := filepath.Join(state, "keys")
			os.Mkdir(keys, 0o700)
			fsprivate.MakePrivate(keys)
			dk := filepath.Join(keys, "data.key")
			os.WriteFile(dk, []byte(content), 0o600)
			s, err := Open(state)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Seal(KindLinear, linA, blob); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Unseal(KindLinear, linA); err != nil || !bytes.Equal(got, blob) {
				t.Fatal(err)
			}
			if rep := s.Check(); !rep.OK() || rep.DataKey.Mode != "file" {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

func damagedNextSetup(t *testing.T, next string) (state, keys string, logs *[]string) {
	t.Helper()
	s, state := open(t)
	seedAll(t, s)
	keys = filepath.Join(state, "keys")
	if err := os.WriteFile(filepath.Join(keys, "data.key.next"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	logs = &[]string{}
	old := logf
	logf = func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) }
	t.Cleanup(func() { logf = old })
	return state, keys, logs
}

func TestDamagedNextNoRecordUsesIsDeletedAndLogged(t *testing.T) {
	for name, next := range map[string]string{
		"unreadable kid":              "garbage",
		"empty":                       "",
		"readable kid, truncated key": `{"v":2,"kid":"fedcba9876543210","mode":"file","key":"AA"}`,
		"readable kid, no key":        `{"v":2,"kid":"fedcba9876543210","mode":"file"}`,
	} {
		t.Run(name, func(t *testing.T) {
			state, keys, logs := damagedNextSetup(t, next)
			s, err := Open(state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Lstat(filepath.Join(keys, "data.key.next")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the damaged file is still there")
			}
			if len(*logs) != 1 || !strings.Contains((*logs)[0], "data.key.next") {
				t.Fatal(*logs)
			}
			if got, err := s.Unseal(KindLinear, linA); err != nil || !bytes.Equal(got, blob) {
				t.Fatal(err)
			}
			if err = s.Rotate(); err != nil {
				t.Fatal("a rotation after the cleanup:", err)
			}
		})
	}
}

func TestDamagedNextWhoseKidARecordCarriesIsKept(t *testing.T) {
	next := `{"v":2,"kid":"fedcba9876543210","mode":"file","key":"AA"}`
	state, keys, logs := damagedNextSetup(t, next)
	p := recFile(state, KindLinear, linA)
	m := readRec(t, p)
	m["kid"] = "fedcba9876543210"
	writeRec(t, p, m)
	_, err := Open(state)
	wantCode(t, err, CodeRecordCorrupt)
	if b, _ := os.ReadFile(filepath.Join(keys, "data.key.next")); string(b) != next || len(*logs) != 0 {
		t.Fatal("the file was removed")
	}
}

func TestDamagedNextWithAnyOtherKidInTheRecordsIsKept(t *testing.T) {
	state, keys, logs := damagedNextSetup(t, "garbage")
	p := recFile(state, KindLinear, linA)
	m := readRec(t, p)
	m["kid"] = "fedcba9876543210"
	writeRec(t, p, m)
	_, err := Open(state)
	wantCode(t, err, CodeRecordCorrupt)
	if b, _ := os.ReadFile(filepath.Join(keys, "data.key.next")); string(b) != "garbage" || len(*logs) != 0 {
		t.Fatal("the file was removed")
	}
	// An unparseable record might carry any kid: also kept.
	m = readRec(t, p)
	m["kid"] = readRec(t, filepath.Join(keys, "data.key"))["kid"]
	writeRec(t, p, m)
	os.WriteFile(recFile(state, KindDevice, devA), []byte("junk"), 0o600)
	_, err = Open(state)
	wantCode(t, err, CodeRecordCorrupt)
	if _, e := os.Lstat(filepath.Join(keys, "data.key.next")); e != nil {
		t.Fatal("the file was removed")
	}
}

func TestWindowsDrivesAreRefusedFollowingSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this tests the WSL /mnt guard, which does not apply to native Windows")
	}
	old := windowsDrives
	t.Cleanup(func() { windowsDrives = old })
	base := t.TempDir()
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	windowsDrives = resolved + string(filepath.Separator)
	_, err = Open(filepath.Join(base, "state"))
	wantCode(t, err, CodeWindowsDrive)
	if !strings.Contains(err.Error(), "Linux home") {
		t.Fatal(err)
	}
	if _, e := os.Lstat(filepath.Join(base, "state")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a directory was made on a Windows drive")
	}
	// Through a symlink from outside.
	elsewhere := t.TempDir()
	link := filepath.Join(elsewhere, "link")
	if os.Symlink(base, link) != nil {
		t.Skip("no symlinks")
	}
	_, err = Open(filepath.Join(link, "state"))
	wantCode(t, err, CodeWindowsDrive)
}

func TestCheckReportsEveryRecordAndRepeatsNoSecret(t *testing.T) {
	s, state := open(t)
	seedAll(t, s)
	// One record is damaged.
	p := recFile(state, KindLinear, linA)
	m := readRec(t, p)
	m["ct"] = "AAAA"
	writeRec(t, p, m)
	rep := s.Check()
	if rep.OK() || !rep.DataKey.Present || !rep.DataKey.Private || !rep.DataKey.Parses || rep.DataKey.Mode != "file" || len(rep.DataKey.KidPrefix) != 8 {
		t.Fatalf("%+v", rep)
	}
	bad, good := 0, 0
	for _, r := range rep.Records {
		if r.OK {
			good++
		} else if r.Err == CodeRecordCorrupt {
			bad++
		}
	}
	if bad != 1 || good != 2 || len(rep.Records) != 3 {
		t.Fatalf("%+v", rep.Records)
	}
	out := fmt.Sprintf("%+v %#v", rep, rep)
	raw, _ := os.ReadFile(filepath.Join(state, "keys", "data.key"))
	var h dataKeyFile
	json.Unmarshal(raw, &h)
	for _, secret := range []string{h.Key, h.Kid, string(seed), string(blob), string(apiPT), "synthetic-access-token"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the report repeats %.6q...", secret)
		}
	}
	for _, rec := range []string{KindDevice, KindLinearAPI} {
		id := devA
		if rec == KindLinearAPI {
			id = linA
		}
		rm := readRec(t, recFile(state, rec, id))
		for _, f := range []string{"ct", "nonce"} {
			if strings.Contains(out, rm[f].(string)) {
				t.Fatalf("the report repeats a record's %s", f)
			}
		}
	}
	if !strings.HasPrefix(h.Kid, rep.DataKey.KidPrefix) {
		t.Fatal("the prefix is not the key ID's")
	}
}

// A store that holds the old key still reads and writes after another
// store (another process) rotates (R1 K1).
func TestAStoreSurvivesARotationByAnother(t *testing.T) {
	a, state := open(t)
	seedAll(t, a)
	if got, err := a.Unseal(KindLinear, linA); err != nil || !bytes.Equal(got, blob) {
		t.Fatal(err)
	}
	oldKids := len(a.keys)
	b, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Rotate(); err != nil {
		t.Fatal(err)
	}
	if oldKids != 1 {
		t.Fatal("setup: expected one cached key, got", oldKids)
	}
	for _, c := range []struct {
		kind, id string
		pt       []byte
	}{{KindDevice, devA, seed}, {KindLinear, linA, blob}, {KindLinearAPI, linA, apiPT}} {
		got, err := a.Unseal(c.kind, c.id)
		if err != nil || !bytes.Equal(got, c.pt) {
			t.Fatalf("%s: %v", c.kind, err)
		}
	}
	// A's next write goes under the new key, so B reads it.
	if err = a.Seal(KindLinear, linB, blob); err != nil {
		t.Fatal(err)
	}
	cur := readRec(t, filepath.Join(state, "keys", "data.key"))
	if readRec(t, recFile(state, KindLinear, linB))["kid"] != cur["kid"] {
		t.Fatal("a stale store sealed under a retired key")
	}
	if got, err := b.Unseal(KindLinear, linB); err != nil || !bytes.Equal(got, blob) {
		t.Fatal(err)
	}
}

func TestRotateReplacesTheKeyAndKeepsEveryRecord(t *testing.T) {
	s, state := open(t)
	seedAll(t, s)
	before := readRec(t, filepath.Join(state, "keys", "data.key"))["kid"]
	if err := s.Rotate(); err != nil {
		t.Fatal(err)
	}
	after := readRec(t, filepath.Join(state, "keys", "data.key"))["kid"]
	if before == after {
		t.Fatal("the key did not change")
	}
	if _, err := os.Lstat(filepath.Join(state, "keys", "data.key.next")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data.key.next is left")
	}
	for _, r := range []struct{ k, i string }{{KindDevice, devA}, {KindLinear, linA}, {KindLinearAPI, linA}} {
		if readRec(t, recFile(state, r.k, r.i))["kid"] != after {
			t.Fatalf("%s is on the old key", r.k)
		}
	}
	fresh, _ := Open(state)
	if got, err := fresh.Unseal(KindLinear, linA); err != nil || !bytes.Equal(got, blob) {
		t.Fatal(err)
	}
	if rep := fresh.Check(); !rep.OK() {
		t.Fatalf("%+v", rep)
	}
}

func TestRotationCrashIsFinishedByTheNextOpen(t *testing.T) {
	s, state := open(t)
	seedAll(t, s)
	s.Seal(KindLinear, linB, blob)
	keys := filepath.Join(state, "keys")
	oldKid := readRec(t, filepath.Join(keys, "data.key"))["kid"]
	s.afterReseal = func(done int) error {
		if done == 2 {
			return errors.New("simulated crash")
		}
		return nil
	}
	if err := s.Rotate(); err == nil {
		t.Fatal("the crash was not reported")
	}
	if _, err := os.Lstat(filepath.Join(keys, "data.key.next")); err != nil {
		t.Fatal("no rotation is under way:", err)
	}
	kids := map[any]int{}
	for _, r := range []struct{ k, i string }{{KindDevice, devA}, {KindLinear, linA}, {KindLinear, linB}, {KindLinearAPI, linA}} {
		kids[readRec(t, recFile(state, r.k, r.i))["kid"]]++
	}
	if len(kids) != 2 {
		t.Fatalf("the crash left records on %d keys, want a mix of 2", len(kids))
	}
	// Meanwhile a store that knows only the old key reads every record.
	stale, _ := openWithoutRecovery(t, state)
	for _, r := range []struct {
		k, i string
		pt   []byte
	}{{KindDevice, devA, seed}, {KindLinear, linA, blob}, {KindLinear, linB, blob}, {KindLinearAPI, linA, apiPT}} {
		if got, err := stale.Unseal(r.k, r.i); err != nil || !bytes.Equal(got, r.pt) {
			t.Fatalf("%s during the rotation: %v", r.k, err)
		}
	}
	// The next Open finishes it.
	s2, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(keys, "data.key.next")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data.key.next is left after recovery")
	}
	newKid := readRec(t, filepath.Join(keys, "data.key"))["kid"]
	if newKid == oldKid {
		t.Fatal("recovery did not install the new key")
	}
	for _, r := range []struct{ k, i string }{{KindDevice, devA}, {KindLinear, linA}, {KindLinear, linB}, {KindLinearAPI, linA}} {
		if readRec(t, recFile(state, r.k, r.i))["kid"] != newKid {
			t.Fatalf("%s %s was not resealed", r.k, r.i)
		}
	}
	if got, err := s2.Unseal(KindLinearAPI, linA); err != nil || !bytes.Equal(got, apiPT) {
		t.Fatal(err)
	}
}

// openWithoutRecovery opens a store as a process that started before the
// rotation began would hold it: with the old key cached and no recovery.
func openWithoutRecovery(t *testing.T, state string) (*Store, error) {
	t.Helper()
	s := &Store{dir: filepath.Join(state, "keys"), keys: map[string][]byte{}}
	if _, _, err := s.readKeyFile(filepath.Join(s.dir, dataKeyName)); err != nil {
		t.Fatal(err)
	}
	return s, nil
}

func TestRecordsSealedDuringARotationOpenAfterIt(t *testing.T) {
	s, state := open(t)
	seedAll(t, s)
	s.afterReseal = func(done int) error { return errors.New("simulated crash") }
	s.Rotate()
	s.afterReseal = nil
	// A write while data.key.next exists goes under the new key.
	if err := s.Seal(KindLinear, linB, blob); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Unseal(KindLinear, linB); err != nil || !bytes.Equal(got, blob) {
		t.Fatal(err)
	}
}

func FuzzRecordDecode(f *testing.F) {
	good := `{"v":1,"kid":"0123456789abcdef","kind":"linear","id":"lin_` + strings.Repeat("0", 32) + `","nonce":"` + b64.EncodeToString(make([]byte, 24)) + `","ct":"` + b64.EncodeToString(make([]byte, 20)) + `"}`
	f.Add([]byte(good))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"v":1,"kid":"x"`))
	f.Add([]byte(good + good))
	f.Add(bytes.Repeat([]byte("["), 1000))
	f.Fuzz(func(t *testing.T, raw []byte) {
		rec, nonce, ct, err := decodeRecord(raw)
		if err != nil {
			if CodeOf(err) != CodeRecordCorrupt {
				t.Fatal("not a fixed code:", err)
			}
			return
		}
		if len(raw) > maxRecordSize || len(nonce) != nonceLen || len(ct) < tagLen || len(ct) > maxPlaintext+tagLen || rec.V != 1 || validate(rec.Kind, rec.ID) != nil {
			t.Fatal("decodeRecord accepted a malformed record")
		}
	})
}

func TestDecodeRecordRefusesAnOversizedInputBeforeParsing(t *testing.T) {
	_, _, _, err := decodeRecord(bytes.Repeat([]byte(" "), maxRecordSize+1))
	wantCode(t, err, CodeRecordCorrupt)
}
