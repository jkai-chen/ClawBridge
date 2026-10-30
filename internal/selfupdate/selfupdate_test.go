package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

const testHost = "127.0.0.1"

// fakeGitHub stands in for the releases pages: /releases/latest redirects
// to latest's tag, files serves the downloads, and /hop, /to-http and
// /to-other exercise the redirect policy.
type fakeGitHub struct {
	srv      *httptest.Server
	latest   string // the Location of /releases/latest, relative to the server
	files    map[string][]byte
	handlers map[string]http.HandlerFunc
	requests atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{files: map[string][]byte{}, handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) base() string { return f.srv.URL + "/releases" }

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if h, ok := f.handlers[r.URL.Path]; ok {
		h(w, r)
		return
	}
	switch {
	case r.URL.Path == "/releases/latest":
		w.Header().Set("Location", f.srv.URL+f.latest)
		w.WriteHeader(http.StatusFound)
		return
	case strings.HasPrefix(r.URL.Path, "/hop/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if n == 0 {
			_, _ = w.Write([]byte("arrived"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
		return
	case r.URL.Path == "/to-http":
		http.Redirect(w, r, "http://"+strings.TrimPrefix(f.srv.URL, "https://")+"/hop/0", http.StatusFound)
		return
	case r.URL.Path == "/to-other":
		http.Redirect(w, r, strings.Replace(f.srv.URL, testHost, "localhost", 1)+"/hop/0", http.StatusFound)
		return
	}
	if body, ok := f.files[r.URL.Path]; ok {
		_, _ = w.Write(body)
		return
	}
	http.NotFound(w, r)
}

// release publishes version's asset for linux/amd64 with a valid manifest
// and returns the asset's bytes.
func (f *fakeGitHub) release(version string) []byte {
	asset := []byte("#!/bin/false\nfake clawbridge " + version + "\n")
	name := assetName(version, "linux", "amd64")
	sum := sha256.Sum256(asset)
	f.files["/releases/download/"+version+"/"+name] = asset
	f.files["/releases/download/"+version+"/SHA256SUMS"] = []byte(
		strings.Repeat("0", 64) + "  clawbridge_" + version + "_darwin_arm64\n" +
			hex.EncodeToString(sum[:]) + "  " + name + "\n")
	return asset
}

// fakeRunner fakes the staged binary's `version` and gh.
type fakeRunner struct {
	t        *testing.T
	asset    []byte // what the staged file must hold when it runs
	version  string // what the staged binary reports
	smokeErr error
	ghErr    error
	calls    [][]string
}

func (r *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if _, ok := ctx.Deadline(); !ok {
		r.t.Errorf("%s runs without a timeout", name)
	}
	if strings.HasPrefix(filepath.Base(name), ".clawbridge-update-") {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, r.asset) {
			r.t.Errorf("the staged binary runs with %q (%v), not the verified asset", got, err)
		}
		if info, err := os.Stat(name); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o755) {
			r.t.Errorf("the staged binary runs with mode %v (%v), want 0755", info.Mode(), err)
		}
		if !reflect.DeepEqual(args, []string{"version"}) {
			r.t.Errorf("staged binary args = %q", args)
		}
		return []byte(`{"source":"build","data":{"version":"` + r.version + `","commit":"0000000","build_time":"unknown"}}` + "\n"), r.smokeErr
	}
	if name == "/fake/bin/gh" {
		return nil, r.ghErr
	}
	r.t.Errorf("unexpected program %s", name)
	return nil, errors.New("unexpected program")
}

type env struct {
	gh      *fakeGitHub
	runner  *fakeRunner
	dest    string
	tempDir string
	opts    Options
}

// newEnv is a running v1.2.3 at dest (mode 0755, "old binary") and a
// published v1.3.0.
// The Unix replace is the default; actAsWindows, called after newEnv,
// takes the Windows path on any OS.
func newEnv(t *testing.T) *env {
	t.Helper()
	oldOS, oldRoot, oldVerify := hostOS, trustedMaterial, verifyBundle
	t.Cleanup(func() { hostOS, trustedMaterial, verifyBundle = oldOS, oldRoot, oldVerify })
	hostOS = "linux"
	// The testdata trusted root, never Sigstore's TUF.
	trustedMaterial = func(context.Context) (root.TrustedMaterial, error) {
		return root.NewTrustedRootFromPath(filepath.Join("testdata", "trusted_root.json"))
	}
	gh := newFakeGitHub(t)
	gh.latest = "/releases/tag/v1.3.0"
	asset := gh.release("v1.3.0")
	dir := t.TempDir()
	dest := filepath.Join(dir, "clawbridge")
	if err := os.WriteFile(dest, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest, _ = filepath.EvalSymlinks(dest)
	runner := &fakeRunner{t: t, asset: asset, version: "v1.3.0"}
	e := &env{gh: gh, runner: runner, dest: dest, tempDir: t.TempDir()}
	e.opts = Options{
		Current:      "v1.2.3",
		GOOS:         "linux",
		GOARCH:       "amd64",
		BaseURL:      gh.base(),
		AllowedHosts: []string{testHost},
		Client:       withPolicy(gh.srv.Client(), []string{testHost}),
		Executable:   dest,
		TempDir:      e.tempDir,
		Run:          runner.run,
		LookPath: func(name string) (string, error) {
			if name != "gh" {
				t.Errorf("LookPath(%q)", name)
			}
			return "/fake/bin/gh", nil
		},
	}
	return e
}

func (e *env) plan(t *testing.T) Plan {
	t.Helper()
	p, err := PlanUpdate(context.Background(), e.opts)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if p.Action != Update {
		t.Fatalf("plan = %+v, want an update", p)
	}
	return p
}

// assertUntouched checks dest still holds the old binary and no staged
// file is left beside it.
func (e *env) assertUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(e.dest)
	if err != nil || string(got) != "old binary" {
		t.Errorf("dest = %q (%v), want the old binary", got, err)
	}
	e.assertNoStaged(t)
}

func (e *env) assertNoStaged(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(e.dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".clawbridge-update-") {
			t.Errorf("staged file %s left behind", entry.Name())
		}
	}
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want one containing %q", err, substr)
	}
}

func TestPlanUpdateActions(t *testing.T) {
	cases := []struct {
		name, current, version, latest string
		want                           Plan
	}{
		{"latest is newer", "v1.2.3", "", "v1.3.0", Plan{Current: "v1.2.3", Target: "v1.3.0", Action: Update}},
		{"numeric, not string, order", "v0.3.9", "", "v0.3.10", Plan{Current: "v0.3.9", Target: "v0.3.10", Action: Update}},
		{"same version", "v1.3.0", "", "v1.3.0", Plan{Current: "v1.3.0", Target: "v1.3.0", Action: UpToDate}},
		{"same named version", "v1.3.0", "v1.3.0", "", Plan{Current: "v1.3.0", Target: "v1.3.0", Action: UpToDate}},
		{"latest is older", "v1.10.0", "", "v1.9.0", Plan{Current: "v1.10.0", Target: "v1.9.0", Action: DowngradeRefused}},
		{"latest is older by major", "v2.0.0", "", "v1.99.99", Plan{Current: "v2.0.0", Target: "v1.99.99", Action: DowngradeRefused}},
		{"named older version", "v1.10.0", "v1.9.0", "", Plan{Current: "v1.10.0", Target: "v1.9.0", Action: Update, Downgrade: true}},
		{"named newer version", "v1.2.3", "v1.2.4", "", Plan{Current: "v1.2.3", Target: "v1.2.4", Action: Update}},
		{"dev build", "dev", "", "v1.3.0", Plan{Current: "dev", Target: "v1.3.0", Action: DevBuild}},
		{"dev build with version", "dev", "v1.0.0", "", Plan{Current: "dev", Target: "v1.0.0", Action: Update}},
		{"non-canonical current", "v1.2.3-rc1", "", "v1.3.0", Plan{Current: "v1.2.3-rc1", Target: "v1.3.0", Action: DevBuild}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.gh.latest = "/releases/tag/" + c.latest
			e.opts.Current, e.opts.Version = c.current, c.version
			got, err := PlanUpdate(context.Background(), e.opts)
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			if want.Action == Update {
				want.Asset = "clawbridge_" + want.Target + "_linux_amd64"
				want.Destination = e.dest
			}
			if got != want {
				t.Errorf("plan = %+v, want %+v", got, want)
			}
			if requests := e.gh.requests.Load(); c.version != "" && requests != 0 {
				t.Errorf("a named version made %d requests", requests)
			}
		})
	}
}

func TestPlanUpdateResolvesTheDestination(t *testing.T) {
	e := newEnv(t)
	link := filepath.Join(t.TempDir(), "clawbridge-link")
	if err := os.Symlink(e.dest, link); err != nil {
		t.Skipf("no symlink here: %v", err)
	}
	e.opts.Executable = link
	if p := e.plan(t); p.Destination != e.dest {
		t.Errorf("destination = %q, want %q", p.Destination, e.dest)
	}
}

func TestPlanUpdatePlatforms(t *testing.T) {
	for _, c := range []struct{ goos, goarch, asset string }{
		{"darwin", "amd64", "clawbridge_v1.3.0_darwin_amd64"},
		{"darwin", "arm64", "clawbridge_v1.3.0_darwin_arm64"},
		{"linux", "amd64", "clawbridge_v1.3.0_linux_amd64"},
		{"linux", "arm64", "clawbridge_v1.3.0_linux_arm64"},
		{"windows", "amd64", "clawbridge_v1.3.0_windows_amd64.exe"},
		{"windows", "arm64", ""},
		{"linux", "386", ""},
		{"freebsd", "amd64", ""},
	} {
		e := newEnv(t)
		e.opts.GOOS, e.opts.GOARCH = c.goos, c.goarch
		p, err := PlanUpdate(context.Background(), e.opts)
		if c.asset == "" {
			wantErr(t, err, "this platform has no release binary; build from source")
			continue
		}
		if err != nil || p.Asset != c.asset {
			t.Errorf("%s/%s: asset %q (%v), want %q", c.goos, c.goarch, p.Asset, err, c.asset)
		}
	}
}

func TestPlanUpdateRefusals(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(e *env)
		wantErr string
	}{
		{"non-canonical version", func(e *env) { e.opts.Version = "1.3.0" }, "invalid canonical release version"},
		{"leading zero version", func(e *env) { e.opts.Version = "v1.03.0" }, "invalid canonical release version"},
		{"short version", func(e *env) { e.opts.Version = "v1.3" }, "invalid canonical release version"},
		{"pre-release latest", func(e *env) { e.gh.latest = "/releases/tag/v1.3.0-rc1" }, "latest release is not a canonical stable version"},
		{"leading zero latest", func(e *env) { e.gh.latest = "/releases/tag/v01.3.0" }, "latest release is not a canonical stable version"},
		{"latest elsewhere", func(e *env) { e.gh.latest = "/releases/other/v1.3.0" }, "unexpected latest release location"},
		{"latest on another host", func(e *env) {
			e.gh.handlers["/releases/latest"] = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.test/releases/tag/v1.3.0")
				w.WriteHeader(http.StatusFound)
			}
		}, "unexpected latest release location"},
		{"latest not a redirect", func(e *env) {
			e.gh.handlers["/releases/latest"] = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hi")) }
		}, "unexpected latest release location"},
		{"http base", func(e *env) { e.opts.BaseURL = strings.Replace(e.opts.BaseURL, "https://", "http://", 1) }, "only https"},
		{"base host not allowed", func(e *env) { e.opts.AllowedHosts = []string{"github.com"} }, "not a GitHub release host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			_, err := PlanUpdate(context.Background(), e.opts)
			wantErr(t, err, c.wantErr)
		})
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.10.0", "v1.9.0", 1},
		{"v0.3.10", "v0.3.9", 1},
		{"v2.0.0", "v10.0.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v99999999999999999999.0.0", "v99999999999999999998.0.0", 1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRedirectPolicy(t *testing.T) {
	e := newEnv(t)
	o := e.opts.withDefaults()
	get := func(path string) error {
		resp, err := get(context.Background(), o, e.gh.srv.URL+path)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get("/hop/5"); err != nil {
		t.Errorf("5 redirects: %v", err)
	}
	wantErr(t, get("/hop/6"), "more than 5 redirects")
	wantErr(t, get("/to-http"), "only https")
	wantErr(t, get("/to-other"), `host "localhost" is not a GitHub release host`)
	wantErr(t, get("/missing"), "404")
	// The policy holds on a client given without it, too.
	o2 := e.opts
	o2.Client = e.gh.srv.Client()
	o2 = o2.withDefaults()
	resp, err := o2.Client.Get(e.gh.srv.URL + "/to-other")
	if err == nil {
		resp.Body.Close()
	}
	wantErr(t, err, "not a GitHub release host")
}

func TestDefaultClient(t *testing.T) {
	c := defaultClient(DefaultAllowedHosts)
	if c.Timeout != requestTimeout || c.CheckRedirect == nil {
		t.Errorf("default client: timeout %v, redirect policy %v", c.Timeout, c.CheckRedirect != nil)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Error("a redirect to example.test is followed")
	}
	req, _ = http.NewRequest(http.MethodGet, "https://GitHub.com/x", nil)
	if err := c.CheckRedirect(req, nil); err != nil {
		t.Errorf("a redirect to github.com is refused: %v", err)
	}
}

func TestSelectDigest(t *testing.T) {
	h := func(c string) string { return strings.Repeat(c, 64) }
	const asset = "clawbridge_v1.3.0_linux_amd64"
	cases := []struct {
		name, manifest, want string
	}{
		{"valid", h("a") + "  " + asset + "\n" + h("b") + "  other\n", h("a")},
		{"no final newline", h("b") + "  other\n" + h("a") + "  " + asset, h("a")},
		{"one space", h("a") + " " + asset + "\n", ""},
		{"three spaces", h("a") + "   " + asset + "\n", ""},
		{"upper-case hex", strings.ToUpper(h("a")) + "  " + asset + "\n", ""},
		{"short hash", h("a")[1:] + "  " + asset + "\n", ""},
		{"long hash", h("a") + "a  " + asset + "\n", ""},
		{"bad name", h("a") + "  " + asset + "\n" + h("b") + "  ../evil\n", ""},
		{"empty line", h("a") + "  " + asset + "\n\n" + h("b") + "  other\n", ""},
		{"CRLF", h("a") + "  " + asset + "\r\n", ""},
		{"duplicate other name", h("a") + "  " + asset + "\n" + h("b") + "  other\n" + h("c") + "  other\n", ""},
		{"missing asset", h("b") + "  other\n", ""},
		{"two lines for the asset", h("a") + "  " + asset + "\n" + h("b") + "  " + asset + "\n", ""},
		{"same line twice", h("a") + "  " + asset + "\n" + h("a") + "  " + asset + "\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		got, err := selectDigest([]byte(c.manifest), asset)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: accepted, digest %s", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v", c.name, got, err)
		}
	}
}

func TestApplyUnix(t *testing.T) {
	e := newEnv(t)
	res, err := Apply(context.Background(), e.plan(t), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(e.dest)
	if !bytes.Equal(got, e.runner.asset) {
		t.Errorf("dest = %q, want the new asset", got)
	}
	info, err := os.Stat(e.dest)
	if runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o755) {
		t.Errorf("dest mode = %v (%v), want 0755", info.Mode(), err)
	}
	sum := sha256.Sum256(e.runner.asset)
	want := Result{From: "v1.2.3", To: "v1.3.0", Destination: e.dest, SHA256: hex.EncodeToString(sum[:]), Attestation: "not_checked"}
	if res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if len(e.runner.calls) != 1 {
		t.Errorf("runs = %q, want only the smoke test", e.runner.calls)
	}
	e.assertNoStaged(t)
}

func TestApplyFailuresLeaveDestUntouched(t *testing.T) {
	manifest := "/releases/download/v1.3.0/SHA256SUMS"
	assetPath := "/releases/download/v1.3.0/clawbridge_v1.3.0_linux_amd64"
	cases := []struct {
		name    string
		setup   func(e *env)
		wantErr string
	}{
		{"checksum mismatch", func(e *env) { e.gh.files[assetPath] = []byte("tampered") }, "binary checksum mismatch; nothing was changed"},
		{"empty asset", func(e *env) {
			e.gh.files[assetPath] = nil
			e.gh.files[manifest] = []byte(hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }()) + "  clawbridge_v1.3.0_linux_amd64\n")
		}, "binary checksum mismatch; nothing was changed"},
		{"asset missing", func(e *env) { delete(e.gh.files, assetPath) }, "404"},
		{"asset over the limit", func(e *env) {
			old := maxAsset
			maxAsset = int64(len(e.runner.asset) - 1)
			e.t().Cleanup(func() { maxAsset = old })
		}, "larger than allowed"},
		{"asset announced over the limit", func(e *env) {
			e.gh.handlers[assetPath] = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.FormatInt(maxAsset+1, 10))
				w.WriteHeader(http.StatusOK)
			}
		}, "larger than allowed"},
		{"manifest malformed", func(e *env) { e.gh.files[manifest] = append(e.gh.files[manifest], []byte("junk\n")...) }, "manifest must contain exactly one valid selected digest"},
		{"manifest over 1 MiB", func(e *env) {
			e.gh.files[manifest] = append(e.gh.files[manifest], bytes.Repeat([]byte(strings.Repeat("0", 64)+"  x\n"), maxManifest/68+1)...)
		}, "larger than allowed"},
		{"manifest missing", func(e *env) { delete(e.gh.files, manifest) }, "manifest download failed"},
		{"manifest redirected to http", func(e *env) {
			e.gh.handlers[manifest] = func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://"+r.Host+"/x", http.StatusFound)
			}
		}, "only https"},
		{"smoke version mismatch", func(e *env) { e.runner.version = "v1.3.1" }, "the downloaded binary doesn't report v1.3.0; nothing was changed"},
		{"smoke longer version", func(e *env) { e.runner.version = "v1.3.00" }, "the downloaded binary doesn't report v1.3.0"},
		{"smoke run fails", func(e *env) { e.runner.smokeErr = errors.New("exec format error") }, "the downloaded binary doesn't report v1.3.0"},
		{"unix rename fails", func(e *env) {
			old := rename
			rename = func(string, string) error { return errors.New("injected") }
			e.t().Cleanup(func() { rename = old })
		}, "nothing was changed; install"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.runner.t = t
			p := e.plan(t)
			c.setup(e)
			_, err := Apply(context.Background(), p, e.opts)
			wantErr(t, err, c.wantErr)
			e.assertUntouched(t)
		})
	}
}

func (e *env) t() *testing.T { return e.runner.t }

func TestApplyAttestation(t *testing.T) {
	t.Run("verified, with the scripts' arguments", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		res, err := Apply(context.Background(), e.plan(t), e.opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Attestation != "verified" || res.AttestationMethod != "gh" {
			t.Errorf("attestation = %q by %q, want verified by gh (the release has no bundle)", res.Attestation, res.AttestationMethod)
		}
		if len(e.runner.calls) != 2 {
			t.Fatalf("calls = %q, want gh then the smoke test", e.runner.calls)
		}
		gh := e.runner.calls[0]
		staged := gh[3]
		want := []string{"/fake/bin/gh", "attestation", "verify", staged,
			"--repo", "jkai-chen/ClawBridge",
			"--signer-workflow", "jkai-chen/ClawBridge/.github/workflows/release.yml",
			"--source-ref", "refs/tags/v1.3.0"}
		if !reflect.DeepEqual(gh, want) {
			t.Errorf("gh = %q\nwant %q", gh, want)
		}
		if filepath.Dir(staged) != filepath.Dir(e.dest) || !strings.HasPrefix(filepath.Base(staged), ".clawbridge-update-") {
			t.Errorf("gh verifies %s, not the staged file beside dest", staged)
		}
		if e.runner.calls[1][0] != staged {
			t.Errorf("smoke ran %s, gh checked %s", e.runner.calls[1][0], staged)
		}
	})
	t.Run("gh missing", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		e.opts.LookPath = func(string) (string, error) { return "", errors.New("not found") }
		_, err := Apply(context.Background(), e.plan(t), e.opts)
		wantErr(t, err, "could not be verified (gh attestation verify); nothing was installed")
		if len(e.runner.calls) != 0 {
			t.Errorf("ran %q", e.runner.calls)
		}
		e.assertUntouched(t)
	})
	t.Run("gh failing", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		e.runner.ghErr = errors.New("exit status 1")
		_, err := Apply(context.Background(), e.plan(t), e.opts)
		wantErr(t, err, "the build provenance of v1.3.0 could not be verified")
		if len(e.runner.calls) != 1 {
			t.Errorf("calls = %q, want gh only", e.runner.calls)
		}
		e.assertUntouched(t)
	})
}

func TestApplyReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("root, and Windows, write a read-only directory")
	}
	e := newEnv(t)
	p := e.plan(t)
	dir := filepath.Dir(e.dest)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := Apply(context.Background(), p, e.opts)
	wantErr(t, err, "nothing was changed; "+e.dest+" is not writable: reinstall with the install script, or run it where you can write it")
	if e.gh.requests.Load() != 1 { // only PlanUpdate's /latest
		t.Errorf("%d requests: a refused destination downloaded something", e.gh.requests.Load())
	}
	e.assertUntouched(t)
}

func TestApplyRefusesNonUpdatePlans(t *testing.T) {
	for _, a := range []Action{UpToDate, DowngradeRefused, DevBuild, ""} {
		e := newEnv(t)
		p := e.plan(t)
		p.Action = a
		if _, err := Apply(context.Background(), p, e.opts); err == nil {
			t.Errorf("%q: applied", a)
		}
		if e.gh.requests.Load() != 1 {
			t.Errorf("%q: %d requests", a, e.gh.requests.Load())
		}
		e.assertUntouched(t)
	}
}

func TestApplyRefusesAForeignPlan(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"asset path":       func(p *Plan) { p.Asset = "../../evil" },
		"target":           func(p *Plan) { p.Target = "v1.3.0/../x" },
		"no destination":   func(p *Plan) { p.Destination = "" },
		"directory":        func(p *Plan) { p.Destination = filepath.Dir(p.Destination) },
		"missing file":     func(p *Plan) { p.Destination += ".missing" },
		"another platform": func(p *Plan) { p.Asset = "clawbridge_v1.3.0_darwin_arm64" },
	} {
		e := newEnv(t)
		p := e.plan(t)
		mutate(&p)
		if _, err := Apply(context.Background(), p, e.opts); err == nil {
			t.Errorf("%s: applied", name)
		}
		e.assertUntouched(t)
	}
}

// actAsWindows makes Apply take the Windows path, with a fixed aside ID and
// a rename seam that records calls and fails call failOn (1-based, 0 never).
func actAsWindows(t *testing.T, failOn int) *[][2]string {
	t.Helper()
	oldOS, oldRename, oldID, oldVolume := hostOS, rename, asideID, volumeName
	t.Cleanup(func() { hostOS, rename, asideID, volumeName = oldOS, oldRename, oldID, oldVolume })
	hostOS = "windows"
	asideID = func() string { return strings.Repeat("ab", 16) }
	var calls [][2]string
	rename = func(from, to string) error {
		calls = append(calls, [2]string{from, to})
		if len(calls) == failOn {
			return errors.New("injected rename failure")
		}
		return os.Rename(from, to)
	}
	return &calls
}

func TestApplyWindowsSameVolume(t *testing.T) {
	e := newEnv(t)
	calls := actAsWindows(t, 0)
	res, err := Apply(context.Background(), e.plan(t), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(e.tempDir, "clawbridge-previous-"+strings.Repeat("ab", 16)+".exe.old")
	if res.Aside != aside {
		t.Errorf("aside = %s, want %s", res.Aside, aside)
	}
	if len(*calls) != 2 || (*calls)[0] != [2]string{e.dest, aside} || (*calls)[1][1] != e.dest ||
		!strings.HasPrefix(filepath.Base((*calls)[1][0]), ".clawbridge-update-") || !strings.HasSuffix((*calls)[1][0], ".exe") {
		t.Errorf("renames = %q", *calls)
	}
	if got, _ := os.ReadFile(aside); string(got) != "old binary" {
		t.Errorf("aside holds %q", got)
	}
	if got, _ := os.ReadFile(e.dest); !bytes.Equal(got, e.runner.asset) {
		t.Errorf("dest holds %q", got)
	}
	e.assertNoStaged(t)
}

func TestApplyWindowsOtherVolume(t *testing.T) {
	e := newEnv(t)
	actAsWindows(t, 0)
	volumeName = func(path string) string {
		if strings.HasPrefix(path, e.tempDir) {
			return "D:"
		}
		return "C:"
	}
	res, err := Apply(context.Background(), e.plan(t), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(e.dest), ".clawbridge-previous-"+strings.Repeat("ab", 16)+".exe.old")
	if res.Aside != want {
		t.Errorf("aside = %s, want %s", res.Aside, want)
	}
}

func TestAsidePathVolumeIsCaseInsensitive(t *testing.T) {
	oldVolume, oldID := volumeName, asideID
	t.Cleanup(func() { volumeName, asideID = oldVolume, oldID })
	asideID = func() string { return "id" }
	volumeName = func(path string) string {
		if strings.HasPrefix(path, "/temp") {
			return "c:"
		}
		return "C:"
	}
	if got := asidePath("/temp", "/bin/clawbridge.exe"); got != filepath.Join("/temp", "clawbridge-previous-id.exe.old") {
		t.Errorf("aside = %s", got)
	}
	if id := randomHex(); len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("aside ID %q is not 32 hex", id)
	}
}

func TestApplyWindowsRollback(t *testing.T) {
	e := newEnv(t)
	calls := actAsWindows(t, 2)
	_, err := Apply(context.Background(), e.plan(t), e.opts)
	wantErr(t, err, "nothing was changed; install")
	aside := filepath.Join(e.tempDir, "clawbridge-previous-"+strings.Repeat("ab", 16)+".exe.old")
	if len(*calls) != 3 || (*calls)[2] != [2]string{aside, e.dest} {
		t.Errorf("renames = %q, want the old binary moved back", *calls)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Errorf("aside still exists: %v", err)
	}
	e.assertUntouched(t)
}

func TestApplyWindowsFirstRenameFails(t *testing.T) {
	e := newEnv(t)
	calls := actAsWindows(t, 1)
	_, err := Apply(context.Background(), e.plan(t), e.opts)
	wantErr(t, err, "nothing was changed; move")
	if len(*calls) != 1 {
		t.Errorf("renames = %q", *calls)
	}
	e.assertUntouched(t)
}

func TestApplyWindowsRollbackFails(t *testing.T) {
	oldRename := rename
	e := newEnv(t)
	actAsWindows(t, 0)
	n := 0
	rename = func(from, to string) error {
		n++
		if n >= 2 {
			return errors.New("injected")
		}
		return oldRename(from, to)
	}
	_, err := Apply(context.Background(), e.plan(t), e.opts)
	aside := filepath.Join(e.tempDir, "clawbridge-previous-"+strings.Repeat("ab", 16)+".exe.old")
	wantErr(t, err, "restore the previous binary, which is now at "+aside)
	if got, _ := os.ReadFile(aside); string(got) != "old binary" {
		t.Errorf("aside holds %q", got)
	}
	e.assertNoStaged(t)
}

func TestApplyWindowsSweep(t *testing.T) {
	e := newEnv(t)
	actAsWindows(t, 0)
	destDir := filepath.Dir(e.dest)
	write := func(path string) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	swept := []string{
		filepath.Join(e.tempDir, "clawbridge-previous-1111.exe.old"),
		filepath.Join(e.tempDir, "clawbridge-previous-2222.exe.old"),
		filepath.Join(destDir, ".clawbridge-previous-3333.exe.old"),
	}
	kept := []string{
		filepath.Join(e.tempDir, "other.exe.old"),
		filepath.Join(e.tempDir, ".clawbridge-previous-4444.exe.old"),
		filepath.Join(destDir, "clawbridge-previous-5555.exe.old"),
		filepath.Join(e.tempDir, "clawbridge-previous-6666.exe"),
	}
	for _, p := range append(append([]string{}, swept...), kept...) {
		write(p)
	}
	if err := os.Mkdir(filepath.Join(e.tempDir, "clawbridge-previous-dir.exe.old"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), e.plan(t), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Swept != len(swept) {
		t.Errorf("swept = %d, want %d", res.Swept, len(swept))
	}
	for _, p := range swept {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not swept", p)
		}
	}
	for _, p := range append(kept, res.Aside) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}
