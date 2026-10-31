package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RollbackResult is what Rollback did.
type RollbackResult struct {
	// From is the version replaced, To the kept version put back.
	From        string `json:"from"`
	To          string `json:"to"`
	Destination string `json:"destination"`
	SHA256      string `json:"sha256"`
	// Aside and Swept are as in Result, for Windows.
	Aside string `json:"aside,omitempty"`
	Swept int    `json:"swept,omitempty"`
	// Previous is From, now kept in the slot, so a second rollback undoes
	// this one; "" when it couldn't be kept (PreviousWarning says why) and
	// the slot is empty.
	Previous        string `json:"previous,omitempty"`
	PreviousWarning string `json:"previous_warning,omitempty"`
	// HoldVersion is the version automatic updates won't reinstall, as the
	// command recorded it in auto-update.json.
	HoldVersion string `json:"hold_version,omitempty"`
}

// Rollback puts the binary kept in o.PreviousDir back in place of the
// running one (o.Current): it stages a copy of the kept file, checks the
// copy's SHA-256 against previous.json, smoke-runs it (it must report the
// kept version), and swaps it in as an update does, keeping the replaced
// binary in the slot in turn. Any failure before the swap changes nothing.
func Rollback(ctx context.Context, o Options) (RollbackResult, error) {
	return rollback(ctx, o.withDefaults())
}

func rollback(ctx context.Context, o Options) (RollbackResult, error) {
	if o.PreviousDir == "" {
		return RollbackResult{}, errors.New("nothing was changed; no rollback slot is configured")
	}
	prev, err := ReadPrevious(o.PreviousDir)
	if errors.Is(err, ErrNoPrevious) {
		return RollbackResult{}, fmt.Errorf("nothing was changed; no previous binary is kept in %s (an update keeps the binary it replaces)", o.PreviousDir)
	}
	if err != nil {
		return RollbackResult{}, fmt.Errorf("nothing was changed; %w", err)
	}
	if prev.Version == o.Current {
		return RollbackResult{}, fmt.Errorf("nothing was changed; the kept binary is %s, which is already running", prev.Version)
	}

	dest, err := resolveExecutable(o.Executable)
	if err != nil {
		return RollbackResult{}, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return RollbackResult{}, err
	}
	if !info.Mode().IsRegular() {
		return RollbackResult{}, fmt.Errorf("nothing was changed; %s is not a regular file", dest)
	}
	pattern := stagePattern
	if hostOS == "windows" {
		pattern += ".exe"
	}
	stage, err := os.CreateTemp(filepath.Dir(dest), pattern)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("nothing was changed; %s is not writable (%w)", dest, err)
	}
	staged := stage.Name()
	installed := false
	defer func() {
		if !installed {
			_ = stage.Close()
			_ = os.Remove(staged)
		}
	}()
	if err := stage.Chmod(0o700); err != nil {
		return RollbackResult{}, err
	}

	// The staged copy is what is checked and run, so a slot changed after
	// the check is never installed.
	sum, err := copyHashed(stage, filepath.Join(o.PreviousDir, prev.File))
	if cerr := stage.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return RollbackResult{}, fmt.Errorf("nothing was changed; read the kept %s: %w", prev.Version, err)
	}
	if sum != prev.SHA256 {
		return RollbackResult{}, fmt.Errorf("nothing was changed; the kept %s doesn't match the sha256 in %s", prev.Version, previousManifest)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return RollbackResult{}, err
	}
	if err := smoke(ctx, o, staged, prev.Version, "the kept binary"); err != nil {
		return RollbackResult{}, err
	}

	sw, err := swapIn(o, staged, dest, o.Current)
	if err != nil {
		return RollbackResult{}, err
	}
	installed = true
	return RollbackResult{
		From: o.Current, To: prev.Version, Destination: dest, SHA256: sum,
		Aside: sw.aside, Swept: sw.swept, Previous: sw.kept, PreviousWarning: sw.warning,
	}, nil
}

// copyHashed copies the file at src into w and returns its hex SHA-256.
func copyHashed(w io.Writer, src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
