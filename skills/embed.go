// Package skills carries the agent skill inside the binary, so
// `clawbridge skill install` installs exactly the skill of its own version.
package skills

import "embed"

// ClawBridge holds the skills/clawbridge folder.
//
//go:embed clawbridge
var ClawBridge embed.FS
