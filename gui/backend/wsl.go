package backend

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// WSLLaunchScript is GUI-DESIGN §6.2's fixed login-shell string. It holds no
// user input: only the distro name, a separate argv element, varies.
// $SHELL is not reliably set under `wsl.exe -- sh -c`, so the login shell
// comes from the password database; its rc files may print to stdout, which
// the preamble handshake skips.
const WSLLaunchScript = `L=$(getent passwd "$(id -un)" | cut -d: -f7); exec "${L:-/bin/sh}" -lc "exec clawbridge gui-backend --stdio"`

// WSLArgv is the exact argv for starting the backend in a WSL distro.
func WSLArgv(distro string) []string {
	return []string{"wsl.exe", "-d", distro, "--cd", "~", "--", "sh", "-c", WSLLaunchScript}
}

// DecodeWSLList decodes the output of `wsl.exe -l -q`, which is UTF-16LE
// (with or without a byte-order mark) on stock Windows and plain UTF-8 when
// WSL_UTF8=1 is set. It returns the distro names in order, without blanks.
func DecodeWSLList(raw []byte) []string {
	var text string
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		text = decodeUTF16LE(raw[2:])
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		text = decodeUTF16BE(raw[2:])
	case looksUTF16LE(raw):
		text = decodeUTF16LE(raw)
	default:
		text = string(raw)
	}
	var names []string
	for _, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(strings.Trim(line, bomAndNul))
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// bomAndNul are trimmed from every decoded line.
var bomAndNul = string(rune(0xFEFF)) + "\x00"

// looksUTF16LE is true when the odd bytes of the first characters are zero,
// which is what ASCII distro names look like in UTF-16LE.
func looksUTF16LE(raw []byte) bool {
	if len(raw) < 4 || len(raw)%2 != 0 {
		return false
	}
	n := len(raw)
	if n > 16 {
		n = 16
	}
	zeros := 0
	for i := 1; i < n; i += 2 {
		if raw[i] == 0 {
			zeros++
		}
	}
	return zeros >= n/4
}

func decodeUTF16LE(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

func decodeUTF16BE(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

// ListWSLDistros runs `wsl.exe -l -q` and decodes it.
func ListWSLDistros() ([]string, error) {
	cmd := exec.Command("wsl.exe", "-l", "-q")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("wsl.exe -l -q: %w", err)
	}
	return DecodeWSLList(out), nil
}

// ErrUnknownDistro means a distro name that is not in `wsl.exe -l -q`.
var ErrUnknownDistro = errors.New("not a WSL distro on this machine")

// CheckDistro returns name only if it is in the list exactly (GUI-DESIGN
// §6.2, R2 L1). An option-like name is refused as well.
func CheckDistro(name string, list []string) (string, error) {
	if name == "" || strings.HasPrefix(name, "-") {
		return "", ErrUnknownDistro
	}
	for _, d := range list {
		if d == name {
			return d, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownDistro, name)
}
