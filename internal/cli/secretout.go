package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/x/term"
)

// stdoutIsTerminal reports whether w is a terminal. A one-time secret is
// printed only there (G9 F2): an agent or a pipe that captured it would
// keep it. A variable so tests can stand in for a terminal.
var stdoutIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// secretOut is where a command's one-time secret goes: this terminal, or
// a new file (0600, never over an existing one), as `operator
// launcher-token` does. It is chosen before the secret exists, so a
// refused run leaves nothing behind.
type secretOut struct {
	path string
	file *os.File
}

func openSecretOut(command, path string, stdout io.Writer) (*secretOut, error) {
	if path == "" {
		if !stdoutIsTerminal(stdout) {
			return nil, fmt.Errorf("%s shows a one-time secret, so it prints only on your own terminal, not to an agent or a pipe; use --out FILE to write it to a new file (0600) instead", command)
		}
		return &secretOut{}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// The path is already in the message: give the cause alone where
		// there is one (R122-L2).
		cause := errors.Unwrap(err)
		if cause == nil {
			cause = err
		}
		return nil, fmt.Errorf("cannot create %s (it must not exist yet): %w", path, cause)
	}
	return &secretOut{path: path, file: f}, nil
}

// toFile reports whether the secret goes to a file.
func (o *secretOut) toFile() bool { return o != nil && o.file != nil }

// discard removes a file no secret was written to.
func (o *secretOut) discard() {
	if o.toFile() {
		_ = o.file.Close()
		_ = os.Remove(o.path)
	}
}

// write puts text in the file. A failed write removes the file; the
// caller then revokes the secret, which reached no one.
func (o *secretOut) write(text string) error {
	_, err := o.file.WriteString(text)
	if err == nil {
		err = o.file.Sync()
	}
	if e := o.file.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = os.Remove(o.path)
		return fmt.Errorf("could not write %s", o.path)
	}
	return nil
}
