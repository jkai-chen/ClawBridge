// Package skills carries the agent skills inside the binary, so
// `clawbridge skill install` installs exactly the skills of its own version.
package skills

import "embed"

// ClawBridge holds the skills/clawbridge folder.
//
//go:embed clawbridge
var ClawBridge embed.FS

// ProvisionGCP holds the Google Cloud provisioning playbook.
//
//go:embed clawbridge-provision-gcp
var ProvisionGCP embed.FS
