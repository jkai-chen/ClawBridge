package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	intoto "github.com/in-toto/attestation/go/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	sigsig "github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// The fixture is the real build provenance of the v0.3.12 release, public
// data: `gh attestation download clawbridge_v0.3.12_linux_amd64 --repo
// jkai-chen/ClawBridge` wrote it as one JSONL line, kept here as JSON. The
// asset itself is not committed; its name and SHA-256 (from the release's
// SHA256SUMS) are below. testdata/trusted_root.json is the Sigstore
// public-good trusted root, fetched through sigstore-go's TUF client.
const (
	fixtureVersion = "v0.3.12"
	fixtureAsset   = "clawbridge_v0.3.12_linux_amd64"
	fixtureSHA     = "ab0d4b726ea469642e88894ab800431ff50b07aa11b0f03a5c516cea3594fa08"
	// Another subject of the same statement.
	fixtureOtherAsset = "clawbridge_v0.3.12_linux_arm64"
	fixtureOtherSHA   = "a2e70d939ccfa9779d6307479c080d108e1d01bc14ba25a4147b1097a1eb1f3f"
)

func fixtureBundle(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "provenance-v0.3.12.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureRoot(t *testing.T) root.TrustedMaterial {
	t.Helper()
	tr, err := root.NewTrustedRootFromPath(filepath.Join("testdata", "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// editBundle decodes the fixture, lets edit change it, and encodes it again.
func editBundle(t *testing.T, edit func(b map[string]any)) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(fixtureBundle(t), &b); err != nil {
		t.Fatal(err)
	}
	edit(b)
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyProvenanceFixture(t *testing.T) {
	if err := verifyProvenance(fixtureBundle(t), fixtureAsset, fixtureSHA, fixtureVersion, fixtureRoot(t)); err != nil {
		t.Fatalf("the real v0.3.12 bundle: %v", err)
	}
	// Another asset of the same release is a subject of the same bundle.
	if err := verifyProvenance(fixtureBundle(t), fixtureOtherAsset, fixtureOtherSHA, fixtureVersion, fixtureRoot(t)); err != nil {
		t.Fatalf("the arm64 subject: %v", err)
	}
}

func TestVerifyProvenanceRefusals(t *testing.T) {
	tamperPayload := func(b map[string]any) {
		env := b["dsseEnvelope"].(map[string]any)
		payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
		if err != nil {
			t.Fatal(err)
		}
		i := bytes.Index(payload, []byte(fixtureSHA))
		if i < 0 {
			t.Fatal("the payload does not name the fixture digest")
		}
		// 'a' becomes '`': the statement no longer matches its signature
		// nor the hash the log entry recorded (sigstore-go checks the log
		// first).
		payload[i] ^= 0x01
		env["payload"] = base64.StdEncoding.EncodeToString(payload)
	}
	stripTlog := func(b map[string]any) {
		delete(b["verificationMaterial"].(map[string]any), "tlogEntries")
	}
	stripProofs := func(b map[string]any) {
		for _, e := range b["verificationMaterial"].(map[string]any)["tlogEntries"].([]any) {
			entry := e.(map[string]any)
			delete(entry, "inclusionProof")
			delete(entry, "inclusionPromise")
		}
	}
	tamperSignature := func(b map[string]any) {
		sig := b["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)
		raw, err := base64.StdEncoding.DecodeString(sig["sig"].(string))
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-1] ^= 0x01
		sig["sig"] = base64.StdEncoding.EncodeToString(raw)
	}
	for name, tc := range map[string]struct {
		bundle  []byte
		asset   string
		sha     string
		version string
		want    string
	}{
		"not a bundle":          {[]byte(`{"mediaType":"text/plain"}`), fixtureAsset, fixtureSHA, fixtureVersion, "bundle: not a Sigstore bundle"},
		"not JSON":              {[]byte("not json"), fixtureAsset, fixtureSHA, fixtureVersion, "bundle: not a Sigstore bundle"},
		"a digest of no asset":  {nil, fixtureAsset, strings.Repeat("0", 64), fixtureVersion, "artifact digest"},
		"another asset's sha":   {nil, fixtureAsset, fixtureOtherSHA, fixtureVersion, "artifact: no subject is clawbridge_v0.3.12_linux_amd64 with sha256 " + fixtureOtherSHA},
		"another asset's name":  {nil, fixtureOtherAsset, fixtureSHA, fixtureVersion, "artifact: no subject is clawbridge_v0.3.12_linux_arm64"},
		"not a sha256":          {nil, fixtureAsset, "abcd", fixtureVersion, `artifact: "abcd" is not a SHA-256`},
		"an older tag":          {nil, fixtureAsset, fixtureSHA, "v0.3.11", "identity: not the release workflow at v0.3.11"},
		"a prefix of the tag":   {nil, fixtureAsset, fixtureSHA, "v0.3.1", "identity: not the release workflow at v0.3.1"},
		"a tampered payload":    {editBundle(t, tamperPayload), fixtureAsset, fixtureSHA, fixtureVersion, "sigstore verification"},
		"a tampered signature":  {editBundle(t, tamperSignature), fixtureAsset, fixtureSHA, fixtureVersion, "sigstore verification"},
		"no tlog entry":         {editBundle(t, stripTlog), fixtureAsset, fixtureSHA, fixtureVersion, "not enough verified log entries from transparency log: 0 < 1"},
		"no inclusion evidence": {editBundle(t, stripProofs), fixtureAsset, fixtureSHA, fixtureVersion, "bundle: not a Sigstore bundle: inclusion proof missing"},
	} {
		t.Run(name, func(t *testing.T) {
			b := tc.bundle
			if b == nil {
				b = fixtureBundle(t)
			}
			err := verifyProvenance(b, tc.asset, tc.sha, tc.version, fixtureRoot(t))
			if err == nil {
				t.Fatal("verified")
			}
			t.Log(err)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// The expected identity is enforced: another issuer or another workflow in
// the policy refuses the same, valid bundle.
func TestVerifyProvenanceIdentity(t *testing.T) {
	for name, set := range map[string]func(){
		"another issuer":   func() { provenanceIssuer = "https://issuer.example.test" },
		"another workflow": func() { provenanceWorkflow = provenanceRepo + "/.github/workflows/ci.yml" },
		"another repo":     func() { provenanceWorkflow = "https://github.com/example/ClawBridge/.github/workflows/release.yml" },
	} {
		t.Run(name, func(t *testing.T) {
			oldIssuer, oldWorkflow := provenanceIssuer, provenanceWorkflow
			t.Cleanup(func() { provenanceIssuer, provenanceWorkflow = oldIssuer, oldWorkflow })
			set()
			err := verifyProvenance(fixtureBundle(t), fixtureAsset, fixtureSHA, fixtureVersion, fixtureRoot(t))
			wantErr(t, err, "identity: not the release workflow at v0.3.12")
		})
	}
	if got := provenanceSAN("v0.3.12"); got != "https://github.com/jkai-chen/ClawBridge/.github/workflows/release.yml@refs/tags/v0.3.12" {
		t.Errorf("SAN = %s", got)
	}
	if provenanceIssuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("issuer = %s", provenanceIssuer)
	}
}

// A trusted root without the public-good Fulcio and Rekor keys refuses it.
func TestVerifyProvenanceUntrustedRoot(t *testing.T) {
	empty, err := root.NewTrustedRoot(root.TrustedRootMediaType01, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyProvenance(fixtureBundle(t), fixtureAsset, fixtureSHA, fixtureVersion, empty)
	wantErr(t, err, "sigstore verification")
}

func TestCheckStatement(t *testing.T) {
	statement := func(edit func(*intoto.Statement)) *intoto.Statement {
		st := &intoto.Statement{
			Type:          statementType,
			PredicateType: slsaPredicate,
			Subject: []*intoto.ResourceDescriptor{
				{Name: fixtureOtherAsset, Digest: map[string]string{"sha256": fixtureOtherSHA}},
				{Name: fixtureAsset, Digest: map[string]string{"sha256": fixtureSHA}},
			},
		}
		if edit != nil {
			edit(st)
		}
		return st
	}
	if err := checkStatement(statement(nil), fixtureAsset, fixtureSHA); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		st   *intoto.Statement
		want string
	}{
		"none":            {nil, "statement: the bundle carries no in-toto statement"},
		"statement v0.1":  {statement(func(s *intoto.Statement) { s.Type = "https://in-toto.io/Statement/v0.1" }), "statement: type"},
		"SLSA v0.2":       {statement(func(s *intoto.Statement) { s.PredicateType = "https://slsa.dev/provenance/v0.2" }), "predicate type:"},
		"another payload": {statement(func(s *intoto.Statement) { s.PredicateType = "https://example.test/predicate" }), "predicate type:"},
		"name only":       {statement(func(s *intoto.Statement) { s.Subject[1].Digest["sha256"] = strings.Repeat("0", 64) }), "artifact: no subject"},
		"digest only":     {statement(func(s *intoto.Statement) { s.Subject[1].Name = "other" }), "artifact: no subject"},
		"no subjects":     {statement(func(s *intoto.Statement) { s.Subject = nil }), "artifact: no subject"},
	} {
		t.Run(name, func(t *testing.T) {
			wantErr(t, checkStatement(tc.st, fixtureAsset, fixtureSHA), tc.want)
		})
	}
}

// tufRepo is a TUF repository in memory, served at tuf.DefaultMirror by
// its fetcher, signed with one ed25519 key for every role. Its root v1 has
// expired, as sigstore-go's embedded root can since its build; 2.root.json
// is current.
type tufRepo struct {
	files   map[string][]byte // by path under the mirror
	signer  sigsig.Signer
	anchor  []byte // the expired root v1
	payload []byte // the trusted_root.json target
	fetches int
}

func newTUFRepo(t *testing.T, payload []byte) *tufRepo {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := sigsig.LoadED25519Signer(priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := metadata.KeyFromPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	r := &tufRepo{files: map[string][]byte{}, signer: signer, payload: payload}
	week := time.Now().Add(7 * 24 * time.Hour)
	for _, v := range []int64{1, 2} {
		md := metadata.Root(week)
		if v == 1 {
			md.Signed.Expires = time.Now().Add(-24 * time.Hour)
		}
		md.Signed.Version = v
		for _, role := range []string{metadata.ROOT, metadata.TIMESTAMP, metadata.SNAPSHOT, metadata.TARGETS} {
			if err := md.Signed.AddKey(key, role); err != nil {
				t.Fatal(err)
			}
		}
		b := r.sign(t, md)
		r.files[strconv.FormatInt(v, 10)+".root.json"] = b
		if v == 1 {
			r.anchor = b
		}
	}
	targets := metadata.Targets(week)
	tf, err := metadata.TargetFile().FromBytes("trusted_root.json", payload, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	targets.Signed.Targets["trusted_root.json"] = tf
	r.files["1.targets.json"] = r.sign(t, targets)
	sum := sha256.Sum256(payload)
	r.files["targets/"+hex.EncodeToString(sum[:])+".trusted_root.json"] = payload
	r.files["1.snapshot.json"] = r.sign(t, metadata.Snapshot(week))
	r.files["timestamp.json"] = r.timestamp(t, week)
	return r
}

type signable interface {
	Sign(sigsig.Signer) (*metadata.Signature, error)
	ToBytes(bool) ([]byte, error)
}

func (r *tufRepo) sign(t *testing.T, md signable) []byte {
	t.Helper()
	if _, err := md.Sign(r.signer); err != nil {
		t.Fatal(err)
	}
	b, err := md.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *tufRepo) timestamp(t *testing.T, expires time.Time) []byte {
	t.Helper()
	return r.sign(t, metadata.Timestamp(expires))
}

func (r *tufRepo) DownloadFile(raw string, maxLength int64, _ time.Duration) ([]byte, error) {
	r.fetches++
	b, ok := r.files[strings.TrimPrefix(raw, tuf.DefaultMirror+"/")]
	if !ok {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: http.StatusNotFound, URL: raw}
	}
	if int64(len(b)) > maxLength {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: raw}
	}
	return b, nil
}

// unreachable is TUF with no network.
type unreachable struct{ fetches int }

func (u *unreachable) DownloadFile(raw string, _ int64, _ time.Duration) ([]byte, error) {
	u.fetches++
	return nil, errors.New("dial tcp: no route to host")
}

func trustedRootJSON(t *testing.T, tm root.TrustedMaterial) []byte {
	t.Helper()
	tr, ok := tm.(*root.TrustedRoot)
	if !ok {
		t.Fatalf("trusted material is %T", tm)
	}
	b, err := tr.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTrustedRootTUF(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := trustedRootJSON(t, fixtureRoot(t))

	t.Run("online, then offline from the cache", func(t *testing.T) {
		repo := newTUFRepo(t, payload)
		cache := t.TempDir()
		tm, err := trustedRootWith(cache, repo.anchor, repo)
		if err != nil {
			t.Fatalf("online: %v", err)
		}
		if !bytes.Equal(trustedRootJSON(t, tm), want) || repo.fetches == 0 {
			t.Fatalf("online: not the repository's trusted root (%d fetches)", repo.fetches)
		}
		meta := filepath.Join(cache, "tuf-repo-cdn.sigstore.dev")
		for _, f := range []string{"root.json", "timestamp.json", "snapshot.json", "targets.json", "targets/trusted_root.json"} {
			if _, err := os.Stat(filepath.Join(meta, f)); err != nil {
				t.Errorf("not cached: %v", err)
			}
		}
		// Offline, from the cache, anchored on the cached root v2 (the
		// anchor v1 has expired).
		off := &unreachable{}
		tm, err = trustedRootWith(cache, repo.anchor, off)
		if err != nil {
			t.Fatalf("offline: %v", err)
		}
		if !bytes.Equal(trustedRootJSON(t, tm), want) {
			t.Fatal("offline: not the cached trusted root")
		}

		// A cached timestamp that has expired is not used: fail closed.
		if err := os.WriteFile(filepath.Join(meta, "timestamp.json"), repo.timestamp(t, time.Now().Add(-time.Hour)), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = trustedRootWith(cache, repo.anchor, &unreachable{})
		wantErr(t, err, "no valid cached root is available")

		// A cached target that no longer matches targets.json is not used.
		if err := os.WriteFile(filepath.Join(meta, "timestamp.json"), repo.files["timestamp.json"], 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(meta, "targets", "trusted_root.json"), []byte(`{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = trustedRootWith(cache, repo.anchor, &unreachable{})
		wantErr(t, err, "no valid cached root is available")
	})

	t.Run("offline with no cache fails closed", func(t *testing.T) {
		repo := newTUFRepo(t, payload)
		off := &unreachable{}
		_, err := trustedRootWith(t.TempDir(), repo.anchor, off)
		wantErr(t, err, "no valid cached root is available")
		if off.fetches == 0 {
			t.Error("TUF was not tried first")
		}
	})

	t.Run("a repository the anchor does not sign is refused", func(t *testing.T) {
		repo, other := newTUFRepo(t, payload), newTUFRepo(t, payload)
		_, err := trustedRootWith(t.TempDir(), repo.anchor, other)
		wantErr(t, err, "no valid cached root is available")
		if other.fetches == 0 {
			t.Error("TUF was not tried")
		}
	})
}

// roundTrip answers every request without a network.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTUFFetcher(t *testing.T) {
	var asked []string
	client := withPolicy(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		switch r.URL.Path {
		case "/2.root.json":
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		case "/big.json":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 11))), Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r}, nil
	})}, []string{tufHost})
	f := &tufFetcher{ctx: context.Background(), client: client}

	b, err := f.DownloadFile("https://tuf-repo-cdn.sigstore.dev/timestamp.json", 100, 0)
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("got %q, %v", b, err)
	}
	_, err = f.DownloadFile("https://tuf-repo-cdn.sigstore.dev/2.root.json", 100, 0)
	var httpErr *metadata.ErrDownloadHTTP
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("404: err = %v, want ErrDownloadHTTP", err)
	}
	_, err = f.DownloadFile("https://tuf-repo-cdn.sigstore.dev/big.json", 10, 0)
	var lenErr *metadata.ErrDownloadLengthMismatch
	if !errors.As(err, &lenErr) {
		t.Fatalf("too large: err = %v", err)
	}
	for _, raw := range []string{
		"http://tuf-repo-cdn.sigstore.dev/timestamp.json",
		"https://github.com/jkai-chen/ClawBridge/releases/download/v0.3.12/SHA256SUMS",
		"https://example.test/timestamp.json",
	} {
		before := len(asked)
		if _, err := f.DownloadFile(raw, 100, 0); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: err = %v, want refused", raw, err)
		}
		if len(asked) != before {
			t.Errorf("%s was requested", raw)
		}
	}
	// The TUF host is not a release-asset host.
	for _, h := range DefaultAllowedHosts {
		if h == tufHost {
			t.Error("the TUF host is in the release-asset allowlist")
		}
	}
}

// bundleCall is one call of a fake verifyBundle.
type bundleCall struct {
	bundle                 string
	asset, sha256, version string
	trusted                bool
}

// fakeVerifier replaces verifyBundle (the fake release's assets have no real
// bundle) and records its calls.
func fakeVerifier(t *testing.T, err error) *[]bundleCall {
	t.Helper()
	var calls []bundleCall
	verifyBundle = func(b []byte, asset, sha, version string, tm root.TrustedMaterial) error {
		calls = append(calls, bundleCall{string(b), asset, sha, version, tm != nil})
		return err
	}
	return &calls
}

func (e *env) bundlePath() string { return "/releases/download/v1.3.0/" + provenanceAsset }

func TestApplyProvenanceBundle(t *testing.T) {
	noGh := func(t *testing.T, e *env) {
		t.Helper()
		e.opts.LookPath = func(name string) (string, error) {
			t.Errorf("LookPath(%q): gh must not be needed", name)
			return "", errors.New("not found")
		}
	}

	t.Run("verified in-process from the release's bundle", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		noGh(t, e)
		e.gh.files[e.bundlePath()] = []byte(`{"bundle":"v1.3.0"}`)
		calls := fakeVerifier(t, nil)
		res, err := Apply(context.Background(), e.plan(t), e.opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Attestation != "verified" || res.AttestationMethod != "bundle" {
			t.Errorf("attestation = %q by %q, want verified by bundle", res.Attestation, res.AttestationMethod)
		}
		sum := sha256.Sum256(e.runner.asset)
		want := []bundleCall{{`{"bundle":"v1.3.0"}`, "clawbridge_v1.3.0_linux_amd64", hex.EncodeToString(sum[:]), "v1.3.0", true}}
		if !reflect.DeepEqual(*calls, want) {
			t.Errorf("verifyBundle calls = %+v\nwant %+v", *calls, want)
		}
		if len(e.runner.calls) != 1 {
			t.Errorf("runs = %q, want only the smoke test", e.runner.calls)
		}
		got, _ := os.ReadFile(e.dest)
		if !bytes.Equal(got, e.runner.asset) {
			t.Errorf("dest = %q, want the new asset", got)
		}
	})

	t.Run("RequireBundle verifies without Attest", func(t *testing.T) {
		e := newEnv(t)
		e.opts.RequireBundle = true
		noGh(t, e)
		e.gh.files[e.bundlePath()] = []byte(`{}`)
		calls := fakeVerifier(t, nil)
		res, err := Apply(context.Background(), e.plan(t), e.opts)
		if err != nil || res.AttestationMethod != "bundle" || len(*calls) != 1 {
			t.Fatalf("res = %+v, err = %v, calls = %d", res, err, len(*calls))
		}
	})

	t.Run("a release without the bundle falls back to gh", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		calls := fakeVerifier(t, nil)
		res, err := Apply(context.Background(), e.plan(t), e.opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.AttestationMethod != "gh" || len(*calls) != 0 {
			t.Errorf("method = %q, bundle calls = %d", res.AttestationMethod, len(*calls))
		}
		if len(e.runner.calls) != 2 || e.runner.calls[0][0] != "/fake/bin/gh" {
			t.Errorf("runs = %q, want gh then the smoke test", e.runner.calls)
		}
	})

	t.Run("a release without the bundle fails with RequireBundle", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest, e.opts.RequireBundle = true, true
		noGh(t, e)
		calls := fakeVerifier(t, nil)
		_, err := Apply(context.Background(), e.plan(t), e.opts)
		wantErr(t, err, "the build provenance of v1.3.0 could not be verified (PROVENANCE.sigstore.json); nothing was installed: the release has no provenance bundle")
		if len(e.runner.calls) != 0 || len(*calls) != 0 {
			t.Errorf("runs = %q, bundle calls = %d", e.runner.calls, len(*calls))
		}
		e.assertUntouched(t)
	})

	for name, status := range map[string]int{"a server error": http.StatusInternalServerError, "forbidden": http.StatusForbidden} {
		t.Run(name+" fails closed, with no fallback", func(t *testing.T) {
			e := newEnv(t)
			e.opts.Attest = true
			noGh(t, e)
			calls := fakeVerifier(t, nil)
			e.gh.handlers[e.bundlePath()] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }
			_, err := Apply(context.Background(), e.plan(t), e.opts)
			wantErr(t, err, "could not be verified (PROVENANCE.sigstore.json); nothing was installed: GET ")
			if len(e.runner.calls) != 0 || len(*calls) != 0 {
				t.Errorf("runs = %q, bundle calls = %d", e.runner.calls, len(*calls))
			}
			e.assertUntouched(t)
		})
	}

	t.Run("a bundle over 1 MiB fails closed", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		noGh(t, e)
		calls := fakeVerifier(t, nil)
		e.gh.files[e.bundlePath()] = bytes.Repeat([]byte(" "), maxBundle+1)
		_, err := Apply(context.Background(), e.plan(t), e.opts)
		wantErr(t, err, "nothing was installed: larger than allowed")
		if len(*calls) != 0 {
			t.Errorf("bundle calls = %d", len(*calls))
		}
		e.assertUntouched(t)
	})

	t.Run("no trusted root fails closed", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		noGh(t, e)
		calls := fakeVerifier(t, nil)
		e.gh.files[e.bundlePath()] = []byte(`{}`)
		trustedMaterial = func(context.Context) (root.TrustedMaterial, error) {
			return nil, errors.New("the Sigstore trusted root: TUF failed")
		}
		_, err := Apply(context.Background(), e.plan(t), e.opts)
		wantErr(t, err, "nothing was installed: the Sigstore trusted root: TUF failed")
		if len(*calls) != 0 {
			t.Errorf("bundle calls = %d", len(*calls))
		}
		e.assertUntouched(t)
	})

	// The real verifier, with the testdata root: the v0.3.12 bundle is not
	// the provenance of the fake v1.3.0 asset.
	t.Run("a bundle that does not verify installs nothing", func(t *testing.T) {
		e := newEnv(t)
		e.opts.Attest = true
		noGh(t, e)
		e.gh.files[e.bundlePath()] = fixtureBundle(t)
		_, err := Apply(context.Background(), e.plan(t), e.opts)
		wantErr(t, err, "the build provenance of v1.3.0 could not be verified (PROVENANCE.sigstore.json); nothing was installed: sigstore verification")
		if len(e.runner.calls) != 0 {
			t.Errorf("runs = %q", e.runner.calls)
		}
		e.assertUntouched(t)
	})
}
