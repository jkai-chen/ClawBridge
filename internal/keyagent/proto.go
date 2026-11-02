package keyagent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// Version is the protocol version carried in every request and response.
const Version = 1

// MaxFrame is the largest request or response body, in bytes.
const MaxFrame = 64 << 10

// readTimeout is how long a request may take to arrive.
const readTimeout = 5 * time.Second

// The operations.
const (
	OpStatus    = "status"
	OpDeviceNew = "device_new"
	OpSign      = "sign"
	OpUnseal    = "unseal"
	OpSeal      = "seal"
	OpRotate    = "rotate"
	OpLock      = "lock"
)

// The two purpose tags that unseal of a Linear API key requires.
const (
	PurposeLinearKey  = "linear-key"
	PurposeLinearView = "linear-view"
)

// ModePassphrase is the mode every agent reports: an agent exists only for a
// passphrase store.
const ModePassphrase = "passphrase"

// Fixed error codes.
const (
	CodeLocked           = "keys_locked"
	CodeBadRequest       = "bad_request"
	CodeBadVersion       = "bad_version"
	CodeUnknownOp        = "unknown_op"
	CodeTooLarge         = "too_large"
	CodeForbidden        = "forbidden"
	CodePurposeRequired  = "purpose_required"
	CodeInvalid          = "invalid_argument"
	CodeNotFound         = "not_found"
	CodeExists           = "exists"
	CodeWrongPass        = "wrong_passphrase"
	CodeInternal         = "internal"
	CodeTimestampRange   = "timestamp_out_of_range"
	CodeOriginNotAllowed = "origin_not_allowed"
)

// MsgLocked is the text of ErrLocked.
const MsgLocked = "ClawBridge is locked: ask your person to run `clawbridge keys unlock` in their own terminal."

var messages = map[string]string{
	CodeLocked:           MsgLocked,
	CodeBadRequest:       "the request is malformed",
	CodeBadVersion:       "the protocol version is not supported",
	CodeUnknownOp:        "the operation is not known",
	CodeTooLarge:         "the request is too large",
	CodeForbidden:        "this operation is not allowed for that kind of record",
	CodePurposeRequired:  "this record needs a purpose tag the request does not carry",
	CodeInvalid:          "an argument is not valid",
	CodeNotFound:         "there is no such record",
	CodeExists:           "a record already exists",
	CodeWrongPass:        "the passphrase is wrong",
	CodeInternal:         "the key agent could not do that",
	CodeTimestampRange:   "the timestamp is not near the current time",
	CodeOriginNotAllowed: "this agent does not sign for that origin",
}

// Error is a failure the agent reported, or ErrLocked. Its message is fixed
// for its code.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Is matches an *Error by code, so errors.Is(err, ErrLocked) holds for any
// keys_locked failure.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// ErrLocked is what a client gets when the agent is absent or locked.
var ErrLocked = &Error{Code: CodeLocked, Msg: MsgLocked}

// newError is the fixed Error for a code; an unknown code is internal.
func newError(code string) *Error {
	msg, ok := messages[code]
	if !ok {
		code, msg = CodeInternal, messages[CodeInternal]
	}
	return &Error{Code: code, Msg: msg}
}

// request is every request's shape. Which fields an op takes is in ops.
type request struct {
	V          int    `json:"v"`
	Op         string `json:"op"`
	Method     string `json:"method,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Path       string `json:"path,omitempty"`
	BodySHA256 string `json:"body_sha256,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	ID         string `json:"id,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	Plaintext  []byte `json:"plaintext,omitempty"`
	Passphrase []byte `json:"passphrase,omitempty"`
}

// response is every response's shape.
type response struct {
	V    int    `json:"v"`
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`

	Locked    *bool  `json:"locked,omitempty"`
	KidPrefix string `json:"kid_prefix,omitempty"`
	Mode      string `json:"mode,omitempty"`
	IdleLeftS *int64 `json:"idle_left_s,omitempty"`

	DeviceID  string `json:"device_id,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
	Signature string `json:"signature,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	Plaintext []byte `json:"plaintext,omitempty"`
}

// fields names the request fields an op takes; any other set field is
// refused.
const (
	fMethod = 1 << iota
	fOrigin
	fPath
	fBody
	fTimestamp
	fDeviceID
	fKind
	fID
	fPurpose
	fPlaintext
	fPassphrase
)

var ops = map[string]int{
	OpStatus:    0,
	OpDeviceNew: 0,
	OpSign:      fMethod | fOrigin | fPath | fBody | fTimestamp | fDeviceID,
	OpUnseal:    fKind | fID | fPurpose,
	OpSeal:      fKind | fID | fPlaintext,
	OpRotate:    fPassphrase,
	OpLock:      0,
}

func (r *request) set() int {
	n := 0
	for bit, v := range map[int]bool{
		fMethod: r.Method != "", fOrigin: r.Origin != "", fPath: r.Path != "", fBody: r.BodySHA256 != "",
		fTimestamp: r.Timestamp != "", fDeviceID: r.DeviceID != "", fKind: r.Kind != "", fID: r.ID != "",
		fPurpose: r.Purpose != "", fPlaintext: len(r.Plaintext) > 0, fPassphrase: len(r.Passphrase) > 0,
	} {
		if v {
			n |= bit
		}
	}
	return n
}

// wipe zeroes the request's secret fields.
func (r *request) wipe() {
	zero(r.Plaintext)
	zero(r.Passphrase)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// writeFrame writes one length-prefixed frame.
func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrame {
		return errors.New("frame too large")
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	zero(buf)
	return err
}

// readFrame reads one frame of at most MaxFrame bytes. A length over the
// limit is refused before any of the body is read.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, newError(CodeBadRequest)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, newError(CodeTooLarge)
	}
	if n == 0 {
		return nil, newError(CodeBadRequest)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, newError(CodeBadRequest)
	}
	return buf, nil
}

// readRequest reads and checks one request. It returns only fixed *Error
// values and never echoes the request. The frame buffer is zeroed.
func readRequest(r io.Reader) (*request, error) {
	buf, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	defer zero(buf)
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var req request
	if err := dec.Decode(&req); err != nil {
		req.wipe()
		return nil, newError(CodeBadRequest)
	}
	if _, err := dec.Token(); err != io.EOF {
		req.wipe()
		return nil, newError(CodeBadRequest)
	}
	if req.V != Version {
		req.wipe()
		return nil, newError(CodeBadVersion)
	}
	allowed, ok := ops[req.Op]
	if !ok {
		req.wipe()
		return nil, newError(CodeUnknownOp)
	}
	if req.set()&^allowed != 0 {
		req.wipe()
		return nil, newError(CodeBadRequest)
	}
	return &req, nil
}

// printable reports whether s is non-empty ASCII with no space or control
// character, at most max bytes: nothing that could add a line to a signed
// string.
func printable(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func isDigits(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return true
	}
	return false
}

// validOrigin accepts scheme://host[:port] and nothing else.
func validOrigin(o string) bool {
	if !printable(o, 255) {
		return false
	}
	rest, ok := strings.CutPrefix(o, "https://")
	if !ok {
		if rest, ok = strings.CutPrefix(o, "http://"); !ok {
			return false
		}
	}
	return rest != "" && !strings.ContainsAny(rest, "/?#@\\")
}
