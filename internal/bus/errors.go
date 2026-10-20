package bus

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
)

type serviceError struct {
	code  string
	cause error
}

func (e *serviceError) Error() string { return e.cause.Error() }
func (e *serviceError) Unwrap() error { return e.cause }
func (e *serviceError) Code() string  { return e.code }
func classify(err error) error {
	if err == nil {
		return nil
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return err
	}
	code := "invalid"
	var networkError net.Error
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "23505":
			code = "conflict"
		case "23503", "23514", "22001":
			code = "invalid"
		default:
			code = "unavailable"
		}
	} else {
		text := err.Error()
		switch {
		case errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(text, "authority unavailable") || strings.Contains(text, "authority commit failed"):
			code = "unavailable"
		case strings.Contains(text, "rate limit"):
			code = "rate_limited"
		case strings.Contains(text, "membership") || strings.Contains(text, "permission") || strings.Contains(text, "bootstrap owner") || strings.Contains(text, "ownership") || strings.Contains(text, "instance mismatch") || strings.Contains(text, "different OS user") || strings.Contains(text, "workspace owner required"):
			code = "forbidden"
		case strings.Contains(text, "already") || strings.Contains(text, "conflicts") || strings.Contains(text, "different payload"):
			code = "conflict"
		case strings.Contains(text, "not found"):
			code = "not_found"
		}
	}
	return &serviceError{code: code, cause: err}
}

var errNotFound = errors.New("scope or entity not found or ambiguous")

// Missing rows are policy failures; real query failures retain their database or
// transport cause so the server can distinguish rejection from unavailability.
func scopedError(err error, message, code string) error {
	if errors.Is(err, errNotFound) {
		return &serviceError{code: code, cause: errors.New(message)}
	}
	return err
}
