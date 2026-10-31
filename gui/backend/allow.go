package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ReadMethods is the one list of methods the webview may call (GUI-DESIGN
// §4.2: an allow-list of typed calls, never a passthrough). P2 registers
// reads only; a write or confirm name is refused here before the backend
// sees it, and the backend refuses it again with -32601.
var ReadMethods = []string{
	"hello",
	"sessions.list",
	"doctor.checks",
}

// maxParams bounds the params a page may send.
const maxParams = 1 << 20

var (
	// ErrNotAllowed means the method is not in ReadMethods.
	ErrNotAllowed = errors.New("method not allowed from the page")
	// ErrNonce means the params carried a nonce field (GUI-DESIGN §4.2: the
	// shell rejects any webview call that carries one).
	ErrNonce = errors.New("a call from the page may not carry a nonce")
	// ErrBadParams means params that are not a JSON object.
	ErrBadParams = errors.New("params must be a JSON object")
)

// MethodAllowed reports whether the page may call method.
func MethodAllowed(method string) bool {
	for _, m := range ReadMethods {
		if m == method {
			return true
		}
	}
	return false
}

// CheckCall validates one call from the page and returns the params to
// send: the page's object, or {} when it sent none.
func CheckCall(method string, params json.RawMessage) (json.RawMessage, error) {
	if !MethodAllowed(method) {
		return nil, fmt.Errorf("%w: %q", ErrNotAllowed, method)
	}
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if len(trimmed) > maxParams {
		return nil, fmt.Errorf("%w: over %d bytes", ErrBadParams, maxParams)
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadParams, err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, ErrBadParams
	}
	if hasNonce(v) {
		return nil, ErrNonce
	}
	return json.RawMessage(trimmed), nil
}

// hasNonce finds a "nonce" key at any depth. The match ignores case,
// because Go's JSON decoding in the backend does too.
func hasNonce(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, "nonce") || hasNonce(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if hasNonce(child) {
				return true
			}
		}
	}
	return false
}
