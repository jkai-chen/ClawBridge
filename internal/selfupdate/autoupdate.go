package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// AutoUpdateFile is the automatic-update state of FR24 §1, in the client's
// state home beside config.toml. A rollback writes hold_version here; the
// automatic updates add their own fields to AutoUpdateState.
const AutoUpdateFile = "auto-update.json"

// AutoUpdateState is auto-update.json.
type AutoUpdateState struct {
	// HoldVersion is a release the person rolled back from: automatic
	// updates don't install it again.
	HoldVersion string `json:"hold_version,omitempty"`
}

// ReadAutoUpdateState reads path; a missing file is the zero state.
func ReadAutoUpdateState(path string) (AutoUpdateState, error) {
	var st AutoUpdateState
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s is malformed: %w", path, err)
	}
	return st, nil
}

// SetHoldVersion sets hold_version in path and keeps every other field,
// including those a newer client wrote. A malformed file is left as it is.
func SetHoldVersion(path, version string) error {
	if !validVersion(version) {
		return fmt.Errorf("invalid canonical release version %q", version)
	}
	fields := map[string]json.RawMessage{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &fields); err != nil || fields == nil {
			return fmt.Errorf("%s is malformed; fix or remove it", path)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	fields["hold_version"], _ = json.Marshal(version)
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'))
}
