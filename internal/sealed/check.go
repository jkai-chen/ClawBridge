package sealed

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Report is what Check found, for a doctor to print. It holds codes, names
// and an 8-character key ID prefix; never a key, plaintext or ciphertext.
type Report struct {
	// Dir is the keys directory; DirErr is why it failed its checks, if so.
	Dir    string
	DirErr Code
	// DataKey describes data.key.
	DataKey KeyReport
	// RotationPending is set while data.key.next exists.
	RotationPending bool
	// Records has one entry per sealed file, by kind then ID.
	Records []RecordReport
}

// KeyReport is the state of data.key.
type KeyReport struct {
	Present bool
	Private bool
	Parses  bool
	// Mode is "file" or "passphrase" when the header parses, else "".
	Mode string
	// KidPrefix is the first 8 hex digits of the key ID.
	KidPrefix string
	// Err is the code of the first problem, or "".
	Err Code
}

// RecordReport is the state of one sealed record.
type RecordReport struct {
	Kind string
	ID   string
	OK   bool
	// Err is why the record is not OK: a fixed code.
	Err Code
}

// OK reports whether every check passed.
func (r Report) OK() bool {
	if r.DirErr != "" || r.DataKey.Err != "" {
		return false
	}
	for _, rec := range r.Records {
		if !rec.OK {
			return false
		}
	}
	return true
}

// Check inspects the store without changing it: whether data.key is there,
// private and well formed, and whether each record opens.
func (s *Store) Check() Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Report{Dir: s.dir}
	if err := s.checkDir(s.dir); err != nil {
		r.DirErr = CodeOf(err)
		return r
	}
	_, nerr := os.Lstat(filepath.Join(s.dir, nextKeyName))
	r.RotationPending = nerr == nil

	p := filepath.Join(s.dir, dataKeyName)
	raw, err := readFile(p, maxKeyFileSize)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.DataKey.Err = CodeMissingDataKey
	case err != nil:
		r.DataKey.Present = true
		r.DataKey.Private = CodeOf(err) != CodeNotPrivate
		r.DataKey.Err = CodeOf(err)
	default:
		r.DataKey.Present, r.DataKey.Private = true, true
		kid, _, perr := parseDataKey(raw, p)
		switch {
		case perr == nil:
			r.DataKey.Parses, r.DataKey.Mode = true, "file"
		case CodeOf(perr) == CodePassphraseMode:
			r.DataKey.Parses, r.DataKey.Mode = true, "passphrase"
			r.DataKey.Err = CodePassphraseMode
		default:
			r.DataKey.Err = CodeRecordCorrupt
		}
		if len(kid) >= 8 {
			r.DataKey.KidPrefix = kid[:8]
		}
	}

	for _, kind := range []string{KindDevice, KindLinear, KindLinearAPI} {
		names, err := s.names(kind)
		if err != nil {
			r.Records = append(r.Records, RecordReport{Kind: kind, ID: "(directory)", Err: CodeOf(err)})
			continue
		}
		for _, n := range names {
			id := strings.TrimSuffix(n, suffix)
			if validate(kind, id) != nil {
				r.Records = append(r.Records, RecordReport{Kind: kind, ID: safeName(id), Err: CodeRecordCorrupt})
				continue
			}
			pt, err := s.open(kind, id)
			for i := range pt {
				pt[i] = 0
			}
			r.Records = append(r.Records, RecordReport{Kind: kind, ID: id, OK: err == nil, Err: CodeOf(err)})
		}
	}
	return r
}
