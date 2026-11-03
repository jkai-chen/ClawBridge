package selfupdate

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Release assets that change how a release is installed. MANUAL-UPDATE-REQUIRED
// is published by scripts/publish-release.sh when the release notes carry the
// marker; AUTO-UPDATE-HOLD is FR24's hold marker.
const (
	ManualAsset = "MANUAL-UPDATE-REQUIRED"
	HoldAsset   = "AUTO-UPDATE-HOLD"
	// maxMarker caps a marker asset's text.
	maxMarker = 16 << 10
)

// CheckResult is what `self-update --check` found, plus what the release
// says about being installed.
type CheckResult struct {
	Plan
	// ManualRequired: the release has the MANUAL-UPDATE-REQUIRED asset;
	// automatic updates never install it.
	ManualRequired bool `json:"manual_required"`
	// ManualText is the asset's text: the version and the notes' marker line.
	ManualText string `json:"manual_text,omitempty"`
	// Hold: the release has the AUTO-UPDATE-HOLD asset.
	Hold bool `json:"hold"`
}

// Check is PlanUpdate plus a look at the target release's marker assets. It
// downloads nothing but those small marker texts, and installs nothing.
func Check(ctx context.Context, o Options) (CheckResult, error) {
	plan, err := PlanUpdate(ctx, o)
	if err != nil {
		return CheckResult{}, err
	}
	out := CheckResult{Plan: plan}
	if plan.Action != Update {
		return out, nil
	}
	o = o.withDefaults()
	base := o.BaseURL + "/download/" + plan.Target + "/"
	text, found, err := marker(ctx, o, base+ManualAsset)
	if err != nil {
		return CheckResult{}, fmt.Errorf("read the release's %s marker: %w", ManualAsset, err)
	}
	out.ManualRequired, out.ManualText = found, text
	_, out.Hold, err = marker(ctx, o, base+HoldAsset)
	if err != nil {
		return CheckResult{}, fmt.Errorf("read the release's %s marker: %w", HoldAsset, err)
	}
	return out, nil
}

// marker reads a marker asset: its text, and whether it exists (a 404 is
// "no marker", anything else that fails is an error).
func marker(ctx context.Context, o Options, url string) (string, bool, error) {
	resp, err := get(ctx, o, url)
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxMarker))
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(b)), true, nil
}

// Newer reports whether canonical release a is newer than b; false when
// either is not a canonical vX.Y.Z (a development build).
func Newer(a, b string) bool {
	return validVersion(a) && validVersion(b) && compareVersions(a, b) > 0
}
