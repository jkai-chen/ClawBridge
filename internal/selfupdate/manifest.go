package selfupdate

import (
	"errors"
	"regexp"
	"strings"
)

var errManifest = errors.New("manifest must contain exactly one valid selected digest and no malformed/duplicate entries")

var (
	manifestHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
	manifestName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// selectDigest parses SHA256SUMS exactly as install.sh's awk: every line is
// 64 lower-hex, two spaces and a name in [A-Za-z0-9_.-]+; no malformed line,
// no duplicate name, and exactly one line for asset, whose digest it returns.
func selectDigest(manifest []byte, asset string) (string, error) {
	text := string(manifest)
	if text == "" {
		return "", errManifest
	}
	// awk's records: a final newline ends the last record, it doesn't start
	// another.
	text = strings.TrimSuffix(text, "\n")
	seen := map[string]bool{}
	count, selected := 0, ""
	for _, line := range strings.Split(text, "\n") {
		if len(line) < 67 || line[64:66] != "  " {
			return "", errManifest
		}
		hash, name := line[:64], line[66:]
		if !manifestHash.MatchString(hash) || !manifestName.MatchString(name) || seen[name] {
			return "", errManifest
		}
		seen[name] = true
		if name == asset {
			count++
			selected = hash
		}
	}
	if count != 1 {
		return "", errManifest
	}
	return selected, nil
}
