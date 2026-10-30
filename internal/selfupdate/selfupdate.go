// Package selfupdate replaces the running clawbridge binary with a release
// from GitHub, with the install scripts' checks (FR12, live/FR12-PLAN.md):
// the latest release from the /releases/latest redirect (a canonical tag
// only), the asset's digest from the release's SHA256SUMS (exactly one valid
// line), the build provenance when asked (the release's Sigstore bundle,
// in-process, or gh for releases without one), and a smoke run of the new
// binary before it replaces anything. Downloaded bytes are data: nothing
// runs before its digest matched.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// DefaultBaseURL is the releases page of the repository.
const DefaultBaseURL = "https://github.com/jkai-chen/ClawBridge/releases"

// DefaultAllowedHosts are the hosts a request or a redirect may reach:
// GitHub and its release-asset hosts, never anything else.
var DefaultAllowedHosts = []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}

// Options is one self-update. Zero values take the defaults noted.
type Options struct {
	// Current is the running version (buildinfo.Version).
	Current string
	// Version is the release asked for ("" is the latest). Naming one is
	// the only way to go to an older release.
	Version string
	// GOOS and GOARCH pick the asset (default runtime's).
	GOOS, GOARCH string
	// Attest checks the build provenance (--verify-attestation, or
	// CLAWBRIDGE_VERIFY=required): in-process against the release's
	// PROVENANCE.sigstore.json, or with gh for a release without one. Any
	// failure installs nothing.
	Attest bool
	// RequireBundle checks the provenance as Attest does, but a release
	// without PROVENANCE.sigstore.json is a failure, not a fallback to gh
	// (automatic updates).
	RequireBundle bool
	// BaseURL is the releases page (default DefaultBaseURL).
	BaseURL string
	// AllowedHosts are the hosts a request or a redirect may reach (default
	// DefaultAllowedHosts); every URL must be https.
	AllowedHosts []string
	// Client makes the requests (default: timeouts, no redirect off
	// AllowedHosts or off https, at most 5).
	Client *http.Client
	// Executable is the binary to replace (default os.Executable with
	// symlinks resolved); it must be a regular file.
	Executable string
	// TempDir is where Windows sets the old binary aside when it is on the
	// same volume (default os.TempDir()).
	TempDir string
	// Run runs a program and returns its stdout: the staged binary's
	// version, and gh (default os/exec, with a timeout).
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath finds gh (default exec.LookPath).
	LookPath func(name string) (string, error)
}

// Action is what a plan would do.
type Action string

const (
	Update           Action = "update"
	UpToDate         Action = "up_to_date"
	DowngradeRefused Action = "downgrade_refused"
	DevBuild         Action = "dev_build"
)

// Plan is what PlanUpdate found: the running and target versions, the
// action, and for an update the asset and the file it replaces.
type Plan struct {
	Current     string `json:"current"`
	Target      string `json:"target"`
	Action      Action `json:"action"`
	Downgrade   bool   `json:"downgrade,omitempty"`
	Asset       string `json:"asset,omitempty"`
	Destination string `json:"destination,omitempty"`
}

// Result is what Apply did.
type Result struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Destination string `json:"destination"`
	SHA256      string `json:"sha256"`
	// Attestation is "verified", or "not_checked" without Options.Attest.
	Attestation string `json:"attestation"`
	// AttestationMethod is how it was verified: "bundle" (the release's
	// Sigstore bundle, in-process) or "gh"; "" when not checked.
	AttestationMethod string `json:"attestation_method,omitempty"`
	// Aside is where Windows set the old binary (it stays while a running
	// clawbridge mcp maps it); "" elsewhere.
	Aside string `json:"aside,omitempty"`
	// Swept counts the old binaries of earlier updates removed.
	Swept int `json:"swept,omitempty"`
}

// ErrNotImplemented marks the API before T1 fills it in.
var ErrNotImplemented = errors.New("selfupdate: not implemented")

// PlanUpdate finds the target release and decides the action. It reads only
// the latest-release redirect (none when Version is given) and downloads
// nothing. A downgrade without Version is DowngradeRefused; a development
// build without Version is DevBuild; the same version is UpToDate.
func PlanUpdate(ctx context.Context, o Options) (Plan, error) {
	o = o.withDefaults()
	if !supportedPlatform(o.GOOS, o.GOARCH) {
		return Plan{}, errNoReleaseBinary
	}
	target := o.Version
	if target != "" {
		if !validVersion(target) {
			return Plan{}, fmt.Errorf("invalid canonical release version %q", target)
		}
	} else {
		latest, err := latestVersion(ctx, o)
		if err != nil {
			return Plan{}, err
		}
		target = latest
	}
	p := Plan{Current: o.Current, Target: target}
	switch {
	case !validVersion(o.Current):
		if o.Version == "" {
			p.Action = DevBuild
			return p, nil
		}
		p.Action = Update
	case compareVersions(target, o.Current) == 0:
		p.Action = UpToDate
		return p, nil
	case compareVersions(target, o.Current) < 0:
		if o.Version == "" {
			p.Action = DowngradeRefused
			return p, nil
		}
		p.Action, p.Downgrade = Update, true
	default:
		p.Action = Update
	}
	dest, err := resolveExecutable(o.Executable)
	if err != nil {
		return Plan{}, err
	}
	p.Asset, p.Destination = assetName(target, o.GOOS, o.GOARCH), dest
	return p, nil
}

// Apply carries out a plan whose Action is Update: download the manifest
// and asset, check the digest (and the provenance when asked), smoke-run the
// staged binary, replace the destination in place, and sweep old binaries of
// earlier updates. Any failure leaves the destination as it was.
func Apply(ctx context.Context, p Plan, o Options) (Result, error) {
	return apply(ctx, p, o.withDefaults())
}
