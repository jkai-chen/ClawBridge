package ctxwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Binding is a session's local record of its transcript (§4.1), at
// <state home>/context/<session>.json. It is private to the user and never
// sent anywhere. Step 2's binding commands write it; the reader only reads.
type Binding struct {
	Version    int    `json:"version"`
	Agent      string `json:"agent"`
	Transcript string `json:"transcript"`
	BoundAt    string `json:"bound_at"`
	// Levels and HandoverHint are cached from the server's answer to a
	// context report (§4.4, CW-L5), so nothing local makes a network call.
	Levels       *Levels `json:"levels,omitempty"`
	HandoverHint string  `json:"handover_hint,omitempty"`
}

// Levels are a session's effective thresholds, in tokens.
type Levels struct {
	WarnTokens     int64 `json:"warn_tokens"`
	HandoverTokens int64 `json:"handover_tokens"`
}

// Level names where tokens stand against levels: ok, warn or handover.
func (l Levels) Level(tokens int64) string {
	switch {
	case l.HandoverTokens > 0 && tokens >= l.HandoverTokens:
		return "handover"
	case l.WarnTokens > 0 && tokens >= l.WarnTokens:
		return "warn"
	}
	return "ok"
}

// BindingPath is where session's binding record lives under dir.
func BindingPath(dir, session string) (string, error) {
	if session == "" || session != filepath.Base(session) || strings.ContainsAny(session, `/\:`) || session == "." || session == ".." {
		return "", fmt.Errorf("%q is not a session name", session)
	}
	return filepath.Join(dir, session+".json"), nil
}

// LoadBinding reads session's binding record from dir. A missing record is
// not an error: the session is simply not bound.
func LoadBinding(dir, session string) (Binding, bool, error) {
	path, err := BindingPath(dir, session)
	if err != nil {
		return Binding{}, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return Binding{}, false, fmt.Errorf("the binding record %s is not a regular file", path)
	}
	if err := privateToUser(info); err != nil {
		return Binding{}, false, fmt.Errorf("the binding record %s %s", path, err)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return Binding{}, false, fmt.Errorf("the binding record %s is not readable", path)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return Binding{}, false, fmt.Errorf("the binding record %s is not readable", path)
	}
	var b Binding
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil || b.Version != 1 || (b.Agent != ClaudeCode && b.Agent != Codex) || !filepath.IsAbs(b.Transcript) {
		return Binding{}, false, fmt.Errorf("the binding record %s is not valid; bind the session again", path)
	}
	return b, true, nil
}
