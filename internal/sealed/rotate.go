package sealed

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

	"clawbridge/internal/fsprivate"
)

// Rotate replaces the data key. It writes data.key.next, reseals every
// record under the new key (each one atomically), then renames data.key.next
// over data.key. A crash part way is finished by the next Open, and until
// then a record opens under either key.
func (s *Store) Rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverRotation(); err != nil {
		return err
	}
	_, _, err := s.currentKey()
	if err != nil {
		return err
	}
	if h, herr := s.header(); herr != nil {
		return herr
	} else if h.Mode == modePassphrase {
		return fail(CodePassphraseMode, filepath.Join(s.dir, dataKeyName), "the data key is wrapped with a passphrase; rotate it with RotateWrapped so the new key is wrapped too")
	}
	kid, key := randomKid(), make([]byte, keyLen)
	if _, err = rand.Read(key); err != nil {
		return ioFail(s.dir, "cannot get random bytes", err)
	}
	next := filepath.Join(s.dir, nextKeyName)
	if err = fsprivate.CreateExclusive(next, encodeDataKey(kid, key)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(CodeIO, next, "another rotation is under way ("+next+"); try again when it ends")
		}
		return ioFail(next, "cannot write the new data key", err)
	}
	s.keys[kid] = key
	return s.finishRotation(kid, key)
}

// recoverRotation completes a rotation a crash interrupted, if
// data.key.next exists.
func (s *Store) recoverRotation() error {
	next := filepath.Join(s.dir, nextKeyName)
	if _, err := os.Lstat(next); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	curKid, _, err := s.currentKey()
	if errors.Is(err, ErrPassphraseMode) {
		// Only RotateWrapped, which has the passphrase, can finish this.
		return nil
	}
	if err != nil {
		return err
	}
	kid, key, err := s.readKeyFile(next)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrPassphraseMode) {
		return nil
	}
	if CodeOf(err) == CodeRecordCorrupt {
		return s.dropDamagedNext(next, curKid, err)
	}
	if err != nil {
		return err
	}
	return s.finishRotation(kid, key)
}

// finishRotation reseals every record not yet under kid, then makes
// data.key.next the data key.
func (s *Store) finishRotation(kid string, key []byte) error {
	done := 0
	for _, kind := range []string{KindDevice, KindLinear, KindLinearAPI} {
		names, err := s.names(kind)
		if err != nil {
			return err
		}
		for _, n := range names {
			id := strings.TrimSuffix(n, suffix)
			if validate(kind, id) != nil {
				continue
			}
			if err = s.reseal(kind, id, kid, key); err != nil {
				return err
			}
			done++
			if s.afterReseal != nil {
				if err = s.afterReseal(done); err != nil {
					return err
				}
			}
		}
	}
	dataKey, next := filepath.Join(s.dir, dataKeyName), filepath.Join(s.dir, nextKeyName)
	if err := fsprivate.Rename(next, dataKey); err != nil {
		// A process that finished the same rotation first has already done it.
		if cur, _, e := s.currentKey(); e == nil && cur == kid {
			return nil
		}
		return ioFail(dataKey, "cannot make the new data key current", err)
	}
	if err := fsprivate.SyncDir(s.dir); err != nil {
		return ioFail(s.dir, "cannot sync the directory", err)
	}
	for k := range s.keys {
		if k != kid {
			delete(s.keys, k)
		}
	}
	return nil
}

// reseal moves one record to the key kid, unless it is there already.
func (s *Store) reseal(kind, id, kid string, key []byte) error {
	p := s.recordPath(kind, id)
	raw, err := readFile(p, maxRecordSize)
	if errors.Is(err, os.ErrNotExist) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	if rec, _, _, derr := decodeRecord(raw); derr == nil && rec.Kid == kid && rec.Kind == kind && rec.ID == id {
		return nil
	}
	pt, err := s.open(kind, id)
	if err != nil {
		return err
	}
	b, err := seal(kind, id, kid, key, pt)
	for i := range pt {
		pt[i] = 0
	}
	if err != nil {
		return err
	}
	return s.write(filepath.Join(s.dir, kind), kind, id, b)
}

// logf says what recovery did on its own; tests replace it.
var logf = log.Printf

// dropDamagedNext deletes a data.key.next that cannot be read, but only
// when no record could be under it: none carries its kid, or, when the kid
// is unreadable, none carries any kid but data.key's. Otherwise cause is
// returned and nothing is deleted.
func (s *Store) dropDamagedNext(next, curKid string, cause error) error {
	var hint struct {
		Kid string `json:"kid"`
	}
	kid := ""
	if raw, err := readFile(next, maxKeyFileSize); err == nil {
		if json.Unmarshal(raw, &hint) == nil && kidPattern.MatchString(hint.Kid) {
			kid = hint.Kid
		}
	} else if CodeOf(err) != CodeRecordCorrupt {
		return err
	}
	kids, ok := s.recordKids()
	if !ok {
		return cause
	}
	if kid != "" {
		if kids[kid] {
			return cause
		}
	} else {
		for k := range kids {
			if k != curKid {
				return cause
			}
		}
	}
	if err := fsprivate.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ioFail(next, "cannot remove the damaged file", err)
	}
	if err := fsprivate.SyncDir(s.dir); err != nil {
		return ioFail(s.dir, "cannot sync the directory", err)
	}
	logf("sealed store: removed the damaged %s; no sealed record was under it", next)
	return nil
}

// recordKids is the set of key IDs the records carry. It is not ok when a
// record cannot be read or parsed, since that record might carry any.
func (s *Store) recordKids() (map[string]bool, bool) {
	kids := map[string]bool{}
	for _, kind := range []string{KindDevice, KindLinear, KindLinearAPI} {
		names, err := s.names(kind)
		if err != nil {
			return nil, false
		}
		for _, n := range names {
			raw, err := readFile(filepath.Join(s.dir, kind, n), maxRecordSize)
			if err != nil {
				return nil, false
			}
			rec, _, _, err := decodeRecord(raw)
			if err != nil {
				return nil, false
			}
			kids[rec.Kid] = true
		}
	}
	return kids, true
}
