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

// ProvisionAWS holds the AWS provisioning playbook.
//
//go:embed clawbridge-provision-aws
var ProvisionAWS embed.FS

// ProvisionLocal holds the playbook for a server on this machine or the LAN.
//
//go:embed clawbridge-provision-local
var ProvisionLocal embed.FS

// ProvisionVM holds the playbook for a server the user already has,
// reached over SSH.
//
//go:embed clawbridge-provision-vm
var ProvisionVM embed.FS
