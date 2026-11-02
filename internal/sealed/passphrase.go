package sealed

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"clawbridge/internal/fsprivate"
)

// The passphrase wrap. In mode "passphrase" data.key holds the data key
// sealed with XChaCha20-Poly1305 under a key-encryption key (KEK) that
// argon2id derives from a passphrase and a per-wrap salt. The passphrase is a
// []byte everywhere and no function here turns it into a string. The KEK is
// zeroed after use; the copy the cipher keeps inside itself cannot be reached
// from Go and is left to the garbage collector, so zeroing is best effort.

// kdfParams are the argon2id cost parameters written to a new header.
type kdfParams struct{ m, t, p uint32 }

// The parameters of a new wrap: 64 MiB, three passes, one lane. Tests lower
// them through newKDFParams; the parser's floor still applies to what they write.
var newKDFParams = func() kdfParams { return kdfParams{m: 65536, t: 3, p: 1} }

// kdf is argon2.IDKey; tests count its calls to show that a header outside
// the limits is refused before any derivation.
var kdf = argon2.IDKey

func wrapAAD(kid string) []byte { return []byte("clawbridge-datakey/v2|" + kid) }

// deriveKEK runs the KDF. The caller zeroes the result.
func deriveKEK(pass, salt []byte, m, t, p uint32) []byte {
	return kdf(pass, salt, t, m, uint8(p), keyLen)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// wrap is the passphrase-mode data.key for key under pass, with a fresh salt
// and nonce.
func wrap(kid string, key, pass []byte) ([]byte, error) {
	if len(pass) == 0 {
		return nil, fail(CodeInvalid, "", "a passphrase is required")
	}
	prm := newKDFParams()
	salt, nonce := make([]byte, saltLen), make([]byte, nonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fail(CodeIO, "", "cannot get random bytes")
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, fail(CodeIO, "", "cannot get random bytes")
	}
	kek := deriveKEK(pass, salt, prm.m, prm.t, prm.p)
	defer zero(kek)
	aead, err := chacha20poly1305.NewX(kek)
	if err != nil {
		return nil, fail(CodeIO, "", "cannot set up the cipher")
	}
	ct := aead.Seal(nil, nonce, key, wrapAAD(kid))
	b, err := json.Marshal(dataKeyFile{
		V: dataKeyVersion, Kid: kid, Mode: modePassphrase, KDF: kdfName,
		M: prm.m, T: prm.t, P: prm.p,
		Salt: b64.EncodeToString(salt), Nonce: b64.EncodeToString(nonce), Wrapped: b64.EncodeToString(ct),
	})
	if err != nil {
		return nil, fail(CodeIO, "", "cannot encode the data key")
	}
	return append(b, '\n'), nil
}

// Unwrap opens a passphrase-mode data.key and returns its key ID and data
// key. The header's limits are checked first, so a crafted header never
// reaches the KDF. A wrong passphrase, and a header changed since it was
// wrapped, are both ErrWrongPassphrase with one fixed message. The key is a
// fresh slice the caller owns and must zero when done. Unwrap changes
// nothing on disk.
func Unwrap(raw, passphrase []byte) (kid string, key []byte, err error) {
	h, err := parseHeader(raw, "")
	if err != nil {
		return "", nil, err
	}
	if h.Mode != modePassphrase {
		return "", nil, fail(CodeInvalid, "", "the data key is not wrapped with a passphrase")
	}
	salt, nonce, wrapped, _ := h.passphraseFields()
	kek := deriveKEK(passphrase, salt, h.M, h.T, h.P)
	defer zero(kek)
	aead, err := chacha20poly1305.NewX(kek)
	if err != nil {
		return "", nil, fail(CodeIO, "", "cannot set up the cipher")
	}
	key, err = aead.Open(nil, nonce, wrapped, wrapAAD(h.Kid))
	if err != nil {
		return "", nil, fail(CodeWrongPassphrase, "", "the passphrase is wrong")
	}
	return h.Kid, key, nil
}

// header is data.key's parsed header, which for a passphrase store is all
// that can be read without the passphrase.
func (s *Store) header() (*dataKeyFile, error) {
	p := filepath.Join(s.dir, dataKeyName)
	raw, err := readFile(p, maxKeyFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return nil, s.missingKey()
	}
	if err != nil {
		return nil, err
	}
	return parseHeader(raw, p)
}

func (s *Store) writeDataKey(b []byte) error {
	p := filepath.Join(s.dir, dataKeyName)
	if err := fsprivate.WriteAtomic(s.dir, "."+dataKeyName+"-", dataKeyName, b); err != nil {
		return ioFail(p, "cannot write the data key", err)
	}
	return nil
}

func (s *Store) rotationPending() bool {
	_, err := os.Lstat(filepath.Join(s.dir, nextKeyName))
	return err == nil
}

// Rewrap changes how data.key is held: newMode "passphrase" from file mode
// needs newPass; from passphrase mode it changes the passphrase and needs
// oldPass and newPass; newMode "file" needs oldPass. A passphrase that is not
// needed is ignored. The data key and its ID never change, so no record is
// touched. data.key is replaced atomically, with a fresh salt and nonce.
// While a rotation is under way it is refused. Rewrap does not apply
// CheckPassphrase; the caller does, as it also reads and confirms the
// passphrase. Moving a store to passphrase mode forgets its cached keys, so
// it then needs OpenUnlocked again.
func (s *Store) Rewrap(newMode string, oldPass, newPass []byte) error {
	if newMode != modeFile && newMode != modePassphrase {
		return fail(CodeInvalid, "", "the mode must be file or passphrase")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotationPending() {
		return fail(CodeIO, filepath.Join(s.dir, nextKeyName), "a key rotation is under way; finish it before changing how the data key is held")
	}
	p := filepath.Join(s.dir, dataKeyName)
	raw, err := readFile(p, maxKeyFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return s.missingKey()
	}
	if err != nil {
		return err
	}
	h, err := parseHeader(raw, p)
	if err != nil {
		return err
	}
	if h.Mode == modeFile && newMode == modeFile {
		return fail(CodeInvalid, p, "the data key is not wrapped with a passphrase")
	}
	if newMode == modePassphrase && len(newPass) == 0 {
		return fail(CodeInvalid, "", "a passphrase is required")
	}
	var key []byte
	if h.Mode == modeFile {
		if _, key, err = parseDataKey(raw, p); err != nil {
			return err
		}
	} else if _, key, err = Unwrap(raw, oldPass); err != nil {
		return err
	}
	defer zero(key)
	var out []byte
	if newMode == modePassphrase {
		out, err = wrap(h.Kid, key, newPass)
	} else {
		out = encodeDataKey(h.Kid, key)
	}
	if err != nil {
		return err
	}
	if err = s.writeDataKey(out); err != nil {
		return err
	}
	if h.Mode == modeFile && newMode == modePassphrase {
		s.dropKeys()
	}
	return nil
}

// dropKeys zeroes and forgets every cached data key.
func (s *Store) dropKeys() {
	for k, v := range s.keys {
		zero(v)
		delete(s.keys, k)
	}
}

// Lock zeroes and forgets the store's cached data keys. A store from
// OpenUnlocked then answers passphrase_mode until it is opened unlocked again.
func (s *Store) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropKeys()
}

// OpenUnlocked is a Store over an already unwrapped data key, which the key
// agent holds. The key is copied; the caller keeps and zeroes its own. The
// on-disk data.key must name kid, and in file mode must hold key; a
// passphrase-mode header can only be matched by kid, since the key is wrapped
// and the passphrase is not here. A store from OpenUnlocked never makes or
// deletes a data key. If a rotation under way holds a wrapped data.key.next,
// only RotateWrapped finishes it and Seal answers passphrase_mode until then.
func OpenUnlocked(stateDir, kid string, key []byte) (*Store, error) {
	if !kidPattern.MatchString(kid) || len(key) != keyLen {
		return nil, fail(CodeInvalid, "", "the key ID or the key is malformed")
	}
	s, err := openDir(stateDir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, dataKeyName)
	raw, err := readFile(p, maxKeyFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return nil, s.missingKey()
	}
	if err != nil {
		return nil, err
	}
	h, err := parseHeader(raw, p)
	if err != nil {
		return nil, err
	}
	if h.Kid != kid {
		return nil, fail(CodeMissingDataKey, p, "the data key on disk is not the one given (its key ID differs); unlock it again")
	}
	if h.Mode == modeFile {
		disk, _ := b64.DecodeString(h.Key)
		ok := subtle.ConstantTimeCompare(disk, key) == 1
		zero(disk)
		if !ok {
			return nil, fail(CodeMissingDataKey, p, "the data key on disk is not the one given; unlock it again")
		}
	}
	s.unlocked = true
	s.keys[kid] = append([]byte(nil), key...)
	if err = s.recoverRotation(); err != nil {
		s.dropKeys()
		return nil, err
	}
	return s, nil
}

// CreatePassphrase makes a store whose data key is wrapped with pass from
// the start. It refuses a state that has a data.key, a data.key.next or any
// sealed record, and never replaces one. It does not apply CheckPassphrase;
// the caller does.
func CreatePassphrase(stateDir string, pass []byte) error {
	if len(pass) == 0 {
		return fail(CodeInvalid, "", "a passphrase is required")
	}
	s, err := openDir(stateDir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dk := filepath.Join(s.dir, dataKeyName)
	for _, n := range []string{dataKeyName, nextKeyName} {
		if _, err = os.Lstat(filepath.Join(s.dir, n)); !errors.Is(err, os.ErrNotExist) {
			return fail(CodeInvalid, dk, "a data key already exists; use Rewrap to change how it is held")
		}
	}
	has, err := s.hasRecords()
	if err != nil {
		return err
	}
	if has {
		return fail(CodeInvalid, dk, "sealed records exist; their data key is not made again")
	}
	kid, key := randomKid(), make([]byte, keyLen)
	defer zero(key)
	if _, err = rand.Read(key); err != nil {
		return ioFail(dk, "cannot get random bytes", err)
	}
	b, err := wrap(kid, key, pass)
	if err != nil {
		return err
	}
	if err = fsprivate.CreateExclusive(dk, b); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(CodeInvalid, dk, "a data key already exists; use Rewrap to change how it is held")
		}
		return ioFail(dk, "cannot make the data key", err)
	}
	return nil
}

// RotateWrapped is Rotate for a passphrase store: it replaces the data key
// and writes data.key.next wrapped under the same passphrase. The store must
// come from OpenUnlocked. pass is checked against data.key first, so a wrong
// passphrase changes nothing. A data.key.next left by a crash is unwrapped
// with pass and finished first, as Rotate finishes one, and then a new key is
// made.
func (s *Store) RotateWrapped(pass []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unlocked {
		return fail(CodeInvalid, "", "open the store with OpenUnlocked to rotate a passphrase store")
	}
	dk, next := filepath.Join(s.dir, dataKeyName), filepath.Join(s.dir, nextKeyName)
	raw, err := readFile(dk, maxKeyFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return s.missingKey()
	}
	if err != nil {
		return err
	}
	curKid, cur, err := Unwrap(raw, pass)
	if err != nil {
		return err
	}
	zero(cur)
	if raw, err = readFile(next, maxKeyFileSize); err == nil {
		if err = s.finishPending(raw, pass, curKid); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	kid, key := randomKid(), make([]byte, keyLen)
	if _, err = rand.Read(key); err != nil {
		return ioFail(s.dir, "cannot get random bytes", err)
	}
	b, err := wrap(kid, key, pass)
	if err != nil {
		zero(key)
		return err
	}
	if err = fsprivate.CreateExclusive(next, b); err != nil {
		zero(key)
		if errors.Is(err, os.ErrExist) {
			return fail(CodeIO, next, "another rotation is under way ("+next+"); try again when it ends")
		}
		return ioFail(next, "cannot write the new data key", err)
	}
	s.keys[kid] = key
	return s.finishRotation(kid, key)
}

// finishPending completes a rotation a crash left, whose data.key.next is
// raw. A damaged one no record is under is dropped, as Open drops it.
func (s *Store) finishPending(raw, pass []byte, curKid string) error {
	kid, key, err := Unwrap(raw, pass)
	if CodeOf(err) == CodeRecordCorrupt {
		return s.dropDamagedNext(filepath.Join(s.dir, nextKeyName), curKid, err)
	}
	if err != nil {
		return err
	}
	s.keys[kid] = key
	return s.finishRotation(kid, key)
}
