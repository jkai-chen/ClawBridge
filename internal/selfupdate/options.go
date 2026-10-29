package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var errNoReleaseBinary = errors.New("this platform has no release binary; build from source")

// withDefaults fills the zero fields of o. A given Client still gets the
// redirect policy (https only, AllowedHosts only, at most 5).
func (o Options) withDefaults() Options {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.AllowedHosts == nil {
		o.AllowedHosts = DefaultAllowedHosts
	}
	if o.Client == nil {
		o.Client = defaultClient(o.AllowedHosts)
	} else {
		o.Client = withPolicy(o.Client, o.AllowedHosts)
	}
	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}
	if o.Run == nil {
		o.Run = runProgram
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	return o
}

// runProgram runs name and returns its stdout. The caller's context carries
// the timeout. CLAWBRIDGE_OUTPUT=json keeps `clawbridge version` in JSON
// whatever the person's environment asks for.
func runProgram(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "CLAWBRIDGE_OUTPUT=json")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
	}
	return out, err
}

func supportedPlatform(goos, goarch string) bool {
	switch goos {
	case "darwin", "linux":
		return goarch == "amd64" || goarch == "arm64"
	case "windows":
		return goarch == "amd64"
	}
	return false
}

// assetName is the release binary for a platform, as the install scripts
// name it.
func assetName(version, goos, goarch string) string {
	name := "clawbridge_" + version + "_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

var canonicalVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// validVersion is install.sh's valid_version: vMAJOR.MINOR.PATCH, no
// leading zeros, nothing else.
func validVersion(v string) bool { return canonicalVersion.MatchString(v) }

// compareVersions orders two canonical versions numerically: -1, 0 or 1.
// The parts have no leading zeros, so a longer part is the larger number
// and equal lengths compare as strings, with no overflow.
func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		x, y := pa[i], pb[i]
		if len(x) != len(y) {
			if len(x) < len(y) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// resolveExecutable is the file to replace: path (or the running binary)
// with its symlinks resolved.
func resolveExecutable(path string) (string, error) {
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("find the running binary: %w", err)
		}
		path = exe
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve the binary %s: %w", path, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve the binary %s: %w", path, err)
	}
	return abs, nil
}
