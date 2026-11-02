package sealed

import "errors"

// Code is a fixed, machine-readable reason a sealed-store call failed.
type Code string

// The codes. A message names a path, never a key, plaintext or ciphertext.
const (
	CodeMissingDataKey Code = "missing_data_key"
	CodeRecordCorrupt  Code = "record_corrupt"
	CodeRecordMismatch Code = "record_mismatch"
	CodeNotPrivate     Code = "not_private"
	CodeWindowsDrive   Code = "windows_drive"
	CodePassphraseMode Code = "passphrase_mode"
	CodeNotFound       Code = "not_found"
	CodeIO             Code = "io_error"
	CodeInvalid        Code = "invalid_argument"
)

// Error is a sealed-store failure. errors.Is matches it against the Err*
// values by Code, and CodeOf reads the code.
type Error struct {
	Code Code
	Msg  string
	// Path is the file or directory the failure is about, when there is one.
	Path string
	err  error
}

func (e *Error) Error() string { return "sealed store: " + string(e.Code) + ": " + e.Msg }
func (e *Error) Unwrap() error { return e.err }

// Is reports whether target is an *Error with the same code and no other
// detail, as the Err* values are.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Msg == "" && t.Code == e.Code
}

// The error values to test with errors.Is.
var (
	ErrMissingDataKey = &Error{Code: CodeMissingDataKey}
	ErrRecordCorrupt  = &Error{Code: CodeRecordCorrupt}
	ErrRecordMismatch = &Error{Code: CodeRecordMismatch}
	ErrNotPrivate     = &Error{Code: CodeNotPrivate}
	ErrWindowsDrive   = &Error{Code: CodeWindowsDrive}
	ErrPassphraseMode = &Error{Code: CodePassphraseMode}
	ErrNotFound       = &Error{Code: CodeNotFound}
)

// CodeOf is err's code, or "" when err is not a sealed-store error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func fail(code Code, path, msg string) error { return &Error{Code: code, Path: path, Msg: msg} }

// ioFail wraps an operating system error. The cause is kept for Unwrap but
// not repeated in the message.
func ioFail(path, what string, cause error) error {
	return &Error{Code: CodeIO, Path: path, Msg: what + " (" + path + ")", err: cause}
}
