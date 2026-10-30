package selfupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	smokeTimeout  = 10 * time.Second
	attestTimeout = 2 * time.Minute
	stagePattern  = ".clawbridge-update-*"
)

// The file-system seams: tests fail a rename, act as Windows on any OS, or
// put the temp directory on another volume.
var (
	rename     = os.Rename
	hostOS     = runtime.GOOS
	volumeName = filepath.VolumeName
	asideID    = randomHex
)

func randomHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

func apply(ctx context.Context, p Plan, o Options) (Result, error) {
	if p.Action != Update {
		return Result{}, fmt.Errorf("nothing to apply: the plan's action is %s", p.Action)
	}
	if !supportedPlatform(o.GOOS, o.GOARCH) {
		return Result{}, errNoReleaseBinary
	}
	if !validVersion(p.Target) {
		return Result{}, fmt.Errorf("invalid canonical release version %q", p.Target)
	}
	if want := assetName(p.Target, o.GOOS, o.GOARCH); p.Asset != want {
		return Result{}, fmt.Errorf("the plan's asset %q is not %s", p.Asset, want)
	}
	if p.Destination == "" {
		return Result{}, errors.New("the plan names no destination")
	}

	// 1. The destination: a regular file in a directory we can write.
	dest, err := resolveExecutable(p.Destination)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("nothing was changed; %s is not a regular file", dest)
	}
	pattern := stagePattern
	if hostOS == "windows" {
		pattern += ".exe" // Windows runs only a file with an executable extension
	}
	stage, err := os.CreateTemp(filepath.Dir(dest), pattern)
	if err != nil {
		return Result{}, fmt.Errorf("nothing was changed; %s is not writable: reinstall with the install script, or run it where you can write it (%w)", dest, err)
	}
	staged := stage.Name()
	installed := false
	defer func() {
		if !installed {
			_ = stage.Close()
			_ = os.Remove(staged)
		}
	}()
	if err := stage.Chmod(0o700); err != nil {
		return Result{}, err
	}

	// 2. The manifest.
	base := o.BaseURL + "/download/" + p.Target + "/"
	digest, err := fetchDigest(ctx, o, base+"SHA256SUMS", p.Asset)
	if err != nil {
		return Result{}, err
	}

	// 3. The asset, hashed as it streams.
	sum, n, err := download(ctx, o, base+p.Asset, stage)
	if cerr := stage.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Result{}, fmt.Errorf("binary download failed; nothing was changed: %w", err)
	}
	if n == 0 || sum != digest {
		return Result{}, errors.New("binary checksum mismatch; nothing was changed")
	}

	// 4. The build provenance, when asked.
	res := Result{From: p.Current, To: p.Target, Destination: dest, SHA256: sum, Attestation: "not_checked"}
	if o.Attest || o.RequireBundle {
		method, err := provenance(ctx, o, base, staged, p.Asset, sum, p.Target)
		if err != nil {
			return Result{}, err
		}
		res.Attestation, res.AttestationMethod = "verified", method
	}

	// 5. The smoke test: the staged binary says it is the target.
	if err := os.Chmod(staged, 0o755); err != nil {
		return Result{}, err
	}
	if err := smoke(ctx, o, staged, p.Target); err != nil {
		return Result{}, err
	}

	// 6. Replace in place.
	if hostOS == "windows" {
		aside := asidePath(o.TempDir, dest)
		if err := replaceWindows(staged, dest, aside); err != nil {
			return Result{}, err
		}
		installed = true
		res.Aside = aside
		// 7. Sweep the old binaries of earlier updates.
		res.Swept = sweep(o.TempDir, filepath.Dir(dest), aside)
		return res, nil
	}
	if err := rename(staged, dest); err != nil {
		return Result{}, fmt.Errorf("nothing was changed; install %s: %w", dest, err)
	}
	installed = true
	return res, nil
}

func fetchDigest(ctx context.Context, o Options, url, asset string) (string, error) {
	resp, err := get(ctx, o, url)
	if err != nil {
		return "", fmt.Errorf("manifest download failed: %w", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := copyLimited(&buf, resp.Body, maxManifest); err != nil {
		return "", fmt.Errorf("manifest download failed: %w", err)
	}
	return selectDigest(buf.Bytes(), asset)
}

// download streams url into w, at most maxAsset bytes, and returns the hex
// SHA-256 and the size.
func download(ctx context.Context, o Options, url string, w io.Writer) (string, int64, error) {
	resp, err := get(ctx, o, url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxAsset {
		return "", 0, errTooLarge
	}
	h := sha256.New()
	n, err := copyLimited(io.MultiWriter(w, h), resp.Body, maxAsset)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// verifyBundle checks a provenance bundle (a test replaces it, since its
// fake assets have no real bundle).
var verifyBundle = verifyProvenance

// provenance verifies the staged asset's build provenance and returns the
// method: "bundle" when the release has PROVENANCE.sigstore.json, checked
// in-process; "gh" for a release without one (before v0.3.13), unless
// RequireBundle. Any other failure to fetch the bundle installs nothing.
func provenance(ctx context.Context, o Options, base, staged, asset, sum, version string) (string, error) {
	fail := fmt.Sprintf("the build provenance of %s could not be verified (%s); nothing was installed", version, provenanceAsset)
	bundleJSON, err := fetchBundle(ctx, o, base+provenanceAsset)
	if isNotFound(err) {
		if o.RequireBundle {
			return "", fmt.Errorf("%s: the release has no provenance bundle", fail)
		}
		return "gh", attest(ctx, o, staged, version)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", fail, err)
	}
	tm, err := trustedMaterial(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: %w", fail, err)
	}
	if err := verifyBundle(bundleJSON, asset, sum, version, tm); err != nil {
		return "", fmt.Errorf("%s: %w", fail, err)
	}
	return "bundle", nil
}

// fetchBundle downloads the provenance bundle, at most maxBundle bytes.
func fetchBundle(ctx context.Context, o Options, url string) ([]byte, error) {
	resp, err := get(ctx, o, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := copyLimited(&buf, resp.Body, maxBundle); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func attest(ctx context.Context, o Options, staged, version string) error {
	fail := fmt.Sprintf("the build provenance of %s could not be verified (gh attestation verify); nothing was installed", version)
	gh, err := o.LookPath("gh")
	if err != nil {
		return fmt.Errorf("%s: the GitHub CLI (gh) was not found", fail)
	}
	ctx, cancel := context.WithTimeout(ctx, attestTimeout)
	defer cancel()
	if _, err := o.Run(ctx, gh, "attestation", "verify", staged,
		"--repo", "jkai-chen/ClawBridge",
		"--signer-workflow", "jkai-chen/ClawBridge/.github/workflows/release.yml",
		"--source-ref", "refs/tags/"+version); err != nil {
		return fmt.Errorf("%s: %w", fail, err)
	}
	return nil
}

func smoke(ctx context.Context, o Options, staged, version string) error {
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	out, err := o.Run(ctx, staged, "version")
	if err != nil || !strings.Contains(string(out), `"version":"`+version+`"`) {
		return fmt.Errorf("the downloaded binary doesn't report %s; nothing was changed", version)
	}
	return nil
}

// asidePath is where Windows sets the old binary, install.ps1's
// Get-ClawBridgeBackupPath: in tempDir when it is on dest's volume, else a
// dot-file beside dest.
func asidePath(tempDir, dest string) string {
	name := "clawbridge-previous-" + asideID() + ".exe.old"
	if strings.EqualFold(volumeName(tempDir), volumeName(dest)) {
		return filepath.Join(tempDir, name)
	}
	return filepath.Join(filepath.Dir(dest), "."+name)
}

// replaceWindows moves the running dest aside (Windows lets a running .exe
// be renamed, not overwritten), then staged into dest; if that fails, the
// old binary goes back.
func replaceWindows(staged, dest, aside string) error {
	if err := rename(dest, aside); err != nil {
		return fmt.Errorf("nothing was changed; move %s aside: %w", dest, err)
	}
	if err := rename(staged, dest); err != nil {
		if rerr := rename(aside, dest); rerr != nil {
			return fmt.Errorf("could not install %s or restore the previous binary, which is now at %s; move it back to %s (%v; %v)", dest, aside, dest, err, rerr)
		}
		return fmt.Errorf("nothing was changed; install %s: %w", dest, err)
	}
	return nil
}

// sweep removes the old binaries of earlier updates, best effort, except
// keep, and counts what it removed.
func sweep(tempDir, destDir, keep string) int {
	var found []string
	for _, pattern := range []string{
		filepath.Join(tempDir, "clawbridge-previous-*.exe.old"),
		filepath.Join(destDir, ".clawbridge-previous-*.exe.old"),
	} {
		matches, _ := filepath.Glob(pattern)
		found = append(found, matches...)
	}
	removed := 0
	for _, path := range found {
		if path == keep {
			continue
		}
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed
}
