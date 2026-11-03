package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func TestPickBundlePrefersTheBuiltAppAndFallsBackToThePlaceholder(t *testing.T) {
	placeholder := fstest.MapFS{"index.html": file("placeholder")}
	b, id, err := pickBundle(placeholder)
	if err != nil || id != PlaceholderID {
		t.Fatalf("placeholder: id %q err %v", id, err)
	}
	if data, _ := readAll(b, "index.html"); data != "placeholder" {
		t.Fatal("did not serve the placeholder")
	}
	built := fstest.MapFS{"index.html": file("placeholder"), "app/index.html": file("real"), "app/assets/a.js": file("js")}
	b, id, err = pickBundle(built)
	if err != nil || id == PlaceholderID || len(id) != 64 {
		t.Fatalf("built: id %q err %v", id, err)
	}
	data, err := readAll(b, "index.html")
	if err != nil || data != "real" {
		t.Fatalf("served %q, %v", data, err)
	}
	if _, err := readAll(b, "app/index.html"); err == nil {
		t.Fatal("the placeholder tree is still reachable under the bundle")
	}
}

func TestBundleIDIsTheManifestsHashAndFollowsTheFiles(t *testing.T) {
	a := fstest.MapFS{"index.html": file("x"), "assets/b.js": file("b"), "manifest.sha256": file("ignored")}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	wantManifest := sum("b") + "  assets/b.js\n" + sum("x") + "  index.html\n"
	if got := BundleManifest(a); got != wantManifest {
		t.Fatalf("manifest %q, want %q", got, wantManifest)
	}
	if BundleID(a) != sum(wantManifest) {
		t.Fatal("id is not the manifest's sha256")
	}
	a["assets/b.js"] = file("changed")
	if BundleID(a) == sum(wantManifest) {
		t.Fatal("id did not change with a file")
	}
}

func TestServerReportsItsBundleID(t *testing.T) {
	// A custom bundle is hashed; the embedded placeholder is "placeholder"
	// unless web/ was built into this tree.
	s := &Server{bundleID: "abc"}
	if s.BundleID() != "abc" {
		t.Fatal("BundleID")
	}
}

// TestEmbeddedBundle is the release's assertion (R1-T8): with
// CLAWBRIDGE_REQUIRE_UI_BUNDLE=1 the embedded bundle must be the real one,
// and web/'s manifest must agree with the files. Otherwise it only checks
// the id is well formed.
func TestEmbeddedBundle(t *testing.T) {
	id, err := EmbeddedBundleID()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CLAWBRIDGE_REQUIRE_UI_BUNDLE") == "1" {
		if id == PlaceholderID {
			t.Fatal("this build embeds the UI-not-built placeholder; run npm ci --ignore-scripts && npm run build in web/ first")
		}
		b, _, _ := embeddedBundle()
		written, err := readAll(b, manifestName)
		if err != nil {
			t.Fatalf("the bundle has no %s: %v", manifestName, err)
		}
		if written != BundleManifest(b) {
			t.Fatalf("%s does not match the bundle's files", manifestName)
		}
	}
	if id != PlaceholderID && len(id) != 64 {
		t.Fatalf("bundle id %q", id)
	}
	if strings.ContainsAny(id, " \n") {
		t.Fatal("bundle id has whitespace")
	}
}

func readAll(b fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(b, name)
	return string(data), err
}

// A bundler names shared chunks like _commonjsHelpers-HASH.js. A plain
// `//go:embed dist` skips names starting with "_" or "."; the server must
// embed with all: so such a chunk is not silently dropped from the binary.
//
//go:embed all:testdata/underscore
var underscoreFixture embed.FS

func TestEmbedKeepsFilesStartingWithAnUnderscore(t *testing.T) {
	if _, err := fs.Stat(underscoreFixture, "testdata/underscore/_commonjsHelpers-abc.js"); err != nil {
		t.Fatalf("all: embed dropped the file: %v", err)
	}
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "//go:embed all:dist\n") {
		t.Fatal("server.go must embed dist with the all: prefix")
	}
}
