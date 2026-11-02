package sessions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"clawbridge/internal/bus"
)

// convPattern is the shape of a conversation ID: a UUID-ish string.
var convPattern = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)

// userConfigDir is the seam for the per-user config directory.
var userConfigDir = os.UserConfigDir

// ErrInvalidName is a name or ID that fails validation.
var ErrInvalidName = errors.New("invalid name")

func checkSegment(what, v string) error {
	if !bus.ValidName(v) {
		return fmt.Errorf("%w: %s must be 1-128 letters, digits, dot, underscore or hyphen, starting with a letter or digit", ErrInvalidName, what)
	}
	return nil
}

// normConv lowercases a conversation ID (a Codex session_id may be upper
// case) and validates it. Every entry point goes through it, so an ID and its
// lowercase form are the same conversation.
func normConv(conv string) (string, error) {
	conv = strings.ToLower(conv)
	if !convPattern.MatchString(conv) {
		return "", fmt.Errorf("%w: a conversation id is 8-64 hex digits and hyphens", ErrInvalidName)
	}
	return conv, nil
}

// checkConv validates an ID that is already normalised: a stored ID in
// upper case is refused.
func checkConv(conv string) error {
	if !convPattern.MatchString(conv) {
		return fmt.Errorf("%w: a stored conversation id must be lowercase hex and hyphens", ErrInvalidName)
	}
	return nil
}

// LinksDir is the per-user directory of agent links, independent of any
// profile: <user config dir>/ClawBridge/links.
func LinksDir() (string, error) {
	base, err := userConfigDir()
	if err != nil {
		return "", err
	}
	if base == "" {
		return "", errors.New("no user config directory")
	}
	return filepath.Join(base, "ClawBridge", "links"), nil
}

// IndexDir is the per-user conversation index, LinksDir()/index.
func IndexDir() (string, error) {
	l, err := LinksDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(l, "index"), nil
}

// RecordsDir is where one project's session records live in a home's state
// directory: <state>/sessions/<workspace>/<project>. Both names are
// validated, so neither can climb out of the directory.
func RecordsDir(stateDir, workspace, project string) (string, error) {
	if stateDir == "" || strings.ContainsRune(stateDir, 0) {
		return "", errors.New("state directory is empty or invalid")
	}
	if err := checkSegment("workspace", workspace); err != nil {
		return "", err
	}
	if err := checkSegment("project", project); err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "sessions", workspace, project), nil
}
