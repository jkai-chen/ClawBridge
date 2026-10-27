// Package secretenv keeps ClawBridge's own secrets out of the environment
// of the programs ClawBridge starts (G9 F3).
package secretenv

import "strings"

// Secret reports whether the environment variable name holds one of
// ClawBridge's own secrets: the device key, the database URL, and any
// CLAWBRIDGE_ name that is a secret, token, seal key or DSN. Settings that
// aren't secrets, such as CLAWBRIDGE_KEY_STORE or CLAWBRIDGE_PROJECT, are
// not. Names are compared upper-cased: Windows' are case-insensitive.
func Secret(name string) bool {
	rest, ok := strings.CutPrefix(strings.ToUpper(name), "CLAWBRIDGE_")
	if !ok {
		return false
	}
	switch {
	case rest == "DEVICE_KEY", rest == "DATABASE_URL":
		return true
	case strings.Contains(rest, "SECRET"), strings.Contains(rest, "TOKEN"), strings.Contains(rest, "SEAL_KEY"), strings.HasSuffix(rest, "_DSN"):
		return true
	}
	return false
}

// Scrub is env (NAME=value entries, as os.Environ gives) without
// ClawBridge's secrets. A program started from it, such as an agent CLI,
// keeps everything else it needs: the user's path, home, cloud and
// provider settings. A list of what to keep would break those, so this is
// a list of what to drop.
func Scrub(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !Secret(name) {
			out = append(out, kv)
		}
	}
	return out
}
