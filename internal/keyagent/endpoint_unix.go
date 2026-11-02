//go:build !windows

package keyagent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"clawbridge/internal/fsprivate"
)

// Seams; tests replace them.
var (
	goos   = runtime.GOOS
	getenv = os.Getenv
	// fallbackRoot is where the Linux fallback directory lives.
	fallbackRoot = "/tmp"
)

// maxSockPath is the longest socket path that fits sockaddr_un on macOS
// (104 bytes with the NUL); Linux allows 108.
const maxSockPath = 103

// endpointFor is the socket path for a canonical state directory. When
// create is set the directory is made 0700 if missing. The directory is
// checked on every use: a plain directory, 0700 or narrower, owned by this
// user; anything else is refused.
func endpointFor(state string, create bool) (string, error) {
	hash := stateHash(state)
	var dir, name string
	switch {
	case goos == "darwin":
		base := getenv("TMPDIR")
		if base == "" || !filepath.IsAbs(base) {
			base = fallbackRoot
		}
		dir, name = filepath.Join(base, "clawbridge-"+hash), "agent.sock"
	case getenv("XDG_RUNTIME_DIR") != "" && filepath.IsAbs(getenv("XDG_RUNTIME_DIR")):
		dir, name = filepath.Join(getenv("XDG_RUNTIME_DIR"), "clawbridge"), hash+".sock"
	default:
		dir, name = filepath.Join(fallbackRoot, "clawbridge-"+strconv.Itoa(fsprivate.CurrentUID())), hash+".sock"
	}
	if create {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			if err = os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", errors.New("cannot make the key agent's directory " + dir)
			}
			if err = fsprivate.MakePrivate(dir); err != nil {
				return "", errors.New("cannot make the key agent's directory " + dir + " private")
			}
		}
	}
	if _, lerr := os.Lstat(dir); create || !errors.Is(lerr, fs.ErrNotExist) {
		if err := fsprivate.CheckPrivate(dir, true); err != nil {
			return "", errors.New("the key agent's directory " + dir + " must be a plain directory, 0700, owned by this user; refusing to use it")
		}
	}
	path := filepath.Join(dir, name)
	if len(path) > maxSockPath {
		return "", errors.New("the key agent's socket path is too long: " + path)
	}
	return path, nil
}
