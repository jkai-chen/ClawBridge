package sealed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"clawbridge/internal/devicekey"
)

// The kinds of secret the store holds, one directory each.
const (
	KindDevice    = "device"
	KindLinear    = "linear"
	KindLinearAPI = "linear-api"
)

// Size limits. A record or key file larger than its limit is refused
// before it is parsed, so a planted file can't make a reader allocate.
const (
	maxRecordSize  = 64 << 10
	maxKeyFileSize = 4 << 10
	maxPlaintext   = 32 << 10
)

const (
	recordVersion  = 1
	dataKeyVersion = 2
	modeFile       = "file"
	modePassphrase = "passphrase"
	kdfName        = "argon2id"
	keyLen         = 32
	nonceLen       = 24
	tagLen         = 16
)

var (
	kidPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// linearIDPattern is a Linear connection's ID. internal/linearconn owns
	// the same pattern; it is repeated here because that package will call
	// this one.
	linearIDPattern = regexp.MustCompile(`^lin_[0-9a-f]{32}$`)
	b64             = base64.StdEncoding.Strict()
)

// validate refuses a kind or ID the store does not hold, which also keeps
// a path separator or ".." out of every file name.
func validate(kind, id string) error {
	ok := false
	switch kind {
	case KindDevice:
		ok = devicekey.ValidID(id)
	case KindLinear, KindLinearAPI:
		ok = linearIDPattern.MatchString(id)
	default:
		return fail(CodeInvalid, "", "unknown kind of sealed record")
	}
	if !ok {
		return fail(CodeInvalid, "", "the ID is not valid for kind "+kind)
	}
	return nil
}

func validKind(kind string) bool {
	return kind == KindDevice || kind == KindLinear || kind == KindLinearAPI
}

// record is the on-disk form of one sealed secret.
type record struct {
	V     int    `json:"v"`
	Kid   string `json:"kid"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Nonce string `json:"nonce"`
	Ct    string `json:"ct"`
}

// aad binds a ciphertext to its kind, ID and key, so a record moved to
// another name or sealed under another key does not open.
func aad(kind, id, kid string) []byte {
	return []byte("clawbridge-sealed/v1|" + kind + "|" + id + "|" + kid)
}

// decodeRecord parses and checks a record's shape. It never decrypts, and
// its errors name no content. The returned nonce and ciphertext are copies.
func decodeRecord(raw []byte) (rec record, nonce, ct []byte, err error) {
	bad := func(msg string) (record, []byte, []byte, error) {
		return record{}, nil, nil, fail(CodeRecordCorrupt, "", msg)
	}
	if len(raw) > maxRecordSize {
		return bad("the record is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&rec) != nil || dec.More() {
		return bad("the record is not valid JSON of the expected shape")
	}
	if rec.V != recordVersion {
		return bad("the record has an unsupported version")
	}
	if !kidPattern.MatchString(rec.Kid) || !validKind(rec.Kind) || validate(rec.Kind, rec.ID) != nil {
		return bad("the record's key ID, kind or ID is malformed")
	}
	if nonce, err = b64.DecodeString(rec.Nonce); err != nil || len(nonce) != nonceLen {
		return bad("the record's nonce is malformed")
	}
	if ct, err = b64.DecodeString(rec.Ct); err != nil || len(ct) < tagLen || len(ct) > maxPlaintext+tagLen {
		return bad("the record's ciphertext is malformed")
	}
	return rec, nonce, ct, nil
}

// dataKeyFile is the header of data.key. File mode holds the key; passphrase
// mode holds the key wrapped under a key derived from a passphrase.
type dataKeyFile struct {
	V    int    `json:"v"`
	Kid  string `json:"kid"`
	Mode string `json:"mode"`
	Key  string `json:"key,omitempty"`

	KDF     string `json:"kdf,omitempty"`
	M       uint32 `json:"m,omitempty"`
	T       uint32 `json:"t,omitempty"`
	P       uint32 `json:"p,omitempty"`
	Salt    string `json:"salt,omitempty"`
	Nonce   string `json:"nonce,omitempty"`
	Wrapped string `json:"wrapped,omitempty"`
}

// KDF parameter limits, in KiB, passes and lanes. A header outside them is
// corrupt before any key derivation runs: the floor stops a crafted file
// from downgrading the KDF, the ceiling from making a reader burn memory.
const (
	minKDFMemory = 19456
	maxKDFMemory = 1048576
	maxKDFTime   = 16
	maxKDFLanes  = 8
	saltLen      = 16
	wrappedLen   = keyLen + tagLen
)

// passphraseFields checks a passphrase header's KDF parameters and field
// sizes and returns the decoded salt, nonce and wrapped key. It runs no KDF.
func (h *dataKeyFile) passphraseFields() (salt, nonce, wrapped []byte, err error) {
	bad := fail(CodeRecordCorrupt, "", "the passphrase header is malformed")
	if h.Key != "" || h.KDF != kdfName {
		return nil, nil, nil, bad
	}
	if h.M < minKDFMemory || h.M > maxKDFMemory || h.T < 1 || h.T > maxKDFTime || h.P < 1 || h.P > maxKDFLanes {
		return nil, nil, nil, bad
	}
	if salt, err = b64.DecodeString(h.Salt); err != nil || len(salt) != saltLen {
		return nil, nil, nil, bad
	}
	if nonce, err = b64.DecodeString(h.Nonce); err != nil || len(nonce) != nonceLen {
		return nil, nil, nil, bad
	}
	if wrapped, err = b64.DecodeString(h.Wrapped); err != nil || len(wrapped) != wrappedLen {
		return nil, nil, nil, bad
	}
	return salt, nonce, wrapped, nil
}

// parseHeader checks a data key file's shape without returning its key: the
// size, the version, the key ID, the mode and, in passphrase mode, the KDF
// limits.
func parseHeader(raw []byte, path string) (*dataKeyFile, error) {
	if len(raw) > maxKeyFileSize {
		return nil, fail(CodeRecordCorrupt, path, "the data key file "+path+" is too large")
	}
	var h dataKeyFile
	if json.Unmarshal(raw, &h) != nil || h.V != dataKeyVersion || !kidPattern.MatchString(h.Kid) {
		return nil, fail(CodeRecordCorrupt, path, "the data key file "+path+" is not a valid key file")
	}
	switch h.Mode {
	case modeFile:
		if key, err := b64.DecodeString(h.Key); err != nil || len(key) != keyLen || h.KDF != "" || h.Wrapped != "" {
			return nil, fail(CodeRecordCorrupt, path, "the data key file "+path+" holds a malformed key")
		}
	case modePassphrase:
		if _, _, _, err := h.passphraseFields(); err != nil {
			return nil, fail(CodeRecordCorrupt, path, "the data key file "+path+" has a malformed passphrase header")
		}
	default:
		return nil, fail(CodeRecordCorrupt, path, "the data key file "+path+" has an unknown mode")
	}
	return &h, nil
}

// parseDataKey returns a data key file's kid and key. Passphrase mode is
// ErrPassphraseMode, with the kid: the key is wrapped and nothing here can
// unwrap it. A passphrase header outside the KDF limits is corrupt.
func parseDataKey(raw []byte, path string) (kid string, key []byte, err error) {
	h, err := parseHeader(raw, path)
	if err != nil {
		return "", nil, err
	}
	if h.Mode == modePassphrase {
		return h.Kid, nil, fail(CodePassphraseMode, path, "the data key "+path+" is locked with a passphrase; unlock it first")
	}
	key, _ = b64.DecodeString(h.Key)
	return h.Kid, key, nil
}

// encodeDataKey is a new file-mode data key file.
func encodeDataKey(kid string, key []byte) []byte {
	b, _ := json.Marshal(dataKeyFile{V: dataKeyVersion, Kid: kid, Mode: modeFile, Key: b64.EncodeToString(key)})
	return append(b, '\n')
}

// safeName is name when it is short plain ASCII, else a placeholder: a
// report must not repeat whatever a stray file is called.
func safeName(name string) string {
	if len(name) > 64 || name == "" {
		return "(invalid name)"
	}
	for _, c := range name {
		if c < 0x20 || c > 0x7e || strings.ContainsRune(`/\`, c) {
			return "(invalid name)"
		}
	}
	return name
}
