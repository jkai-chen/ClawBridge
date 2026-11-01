package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"sort"
	"strings"
)

// PlaceholderID is the bundle id of the hand-written "UI not built" page.
const PlaceholderID = "placeholder"

// builtDir is where web/ builds the real bundle inside dist (it is
// gitignored). dist/index.html itself is the committed placeholder, so a
// plain `go build` with no npm build embeds the placeholder, and a release
// that built web/ first embeds the bundle.
const builtDir = "app"

// manifestName is the file web/'s build writes beside the bundle: one
// "<sha256>  <path>" line per file, sorted by path, itself excluded.
const manifestName = "manifest.sha256"

// embeddedBundle picks what the binary serves: dist/app when it has an
// index.html, else the placeholder in dist.
func embeddedBundle() (fs.FS, string, error) {
	root, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, "", err
	}
	return pickBundle(root)
}

func pickBundle(root fs.FS) (fs.FS, string, error) {
	if fi, err := fs.Stat(root, builtDir+"/index.html"); err == nil && fi.Mode().IsRegular() {
		sub, err := fs.Sub(root, builtDir)
		if err != nil {
			return nil, "", err
		}
		return sub, BundleID(sub), nil
	}
	return root, PlaceholderID, nil
}

// BundleManifest is the canonical manifest text of a bundle, computed from
// its files: what web/'s build writes to manifest.sha256.
func BundleManifest(b fs.FS) string {
	var names []string
	fs.WalkDir(b, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && p != manifestName {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	var out strings.Builder
	for _, p := range names {
		data, err := fs.ReadFile(b, p)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out.WriteString(hex.EncodeToString(sum[:]) + "  " + p + "\n")
	}
	return out.String()
}

// BundleID is the sha256 of the bundle's manifest, in hex.
func BundleID(b fs.FS) string {
	sum := sha256.Sum256([]byte(BundleManifest(b)))
	return hex.EncodeToString(sum[:])
}

// EmbeddedBundleID is the id of the bundle this binary embeds.
func EmbeddedBundleID() (string, error) {
	_, id, err := embeddedBundle()
	return id, err
}
