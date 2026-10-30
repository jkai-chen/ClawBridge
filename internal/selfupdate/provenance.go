package selfupdate

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	intoto "github.com/in-toto/attestation/go/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

const (
	// provenanceAsset is the release asset that holds the Sigstore bundle of
	// the release's build provenance (every release from v0.3.13).
	provenanceAsset = "PROVENANCE.sigstore.json"
	// maxBundle bounds the bundle download.
	maxBundle = 1 << 20

	statementType  = "https://in-toto.io/Statement/v1"
	slsaPredicate  = "https://slsa.dev/provenance/v1"
	provenanceRepo = "https://github.com/jkai-chen/ClawBridge"

	// tufHost is the Sigstore TUF repository. It has its own client and
	// host list: it is never a release-asset host.
	tufHost = "tuf-repo-cdn.sigstore.dev"
)

// The identity a release's provenance must carry, fixed in code: the GitHub
// Actions OIDC issuer and the release workflow at the release's tag (the
// certificate's SAN is provenanceWorkflow + "@refs/tags/" + version). Tests
// change them to check the identity is enforced.
var (
	provenanceIssuer   = "https://token.actions.githubusercontent.com"
	provenanceWorkflow = provenanceRepo + "/.github/workflows/release.yml"
)

// verifyProvenance checks bundleJSON, a Sigstore bundle, against the trusted
// material: the signature and certificate chain, at least one transparency
// log entry with its inclusion proof or promise, the integrated time inside
// the certificate's validity, the issuer and the exact SAN for version, a
// statement subject named assetName with sha256hex, and the SLSA provenance
// v1 predicate. Every failure names the check; nothing is partly verified.
func verifyProvenance(bundleJSON []byte, assetName, sha256hex, version string, tm root.TrustedMaterial) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("bundle: not a Sigstore bundle: %w", err)
	}
	digest, err := hex.DecodeString(sha256hex)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("artifact: %q is not a SHA-256", sha256hex)
	}
	v, err := verify.NewVerifier(tm,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithIntegratedTimestamps(1))
	if err != nil {
		return fmt.Errorf("verifier: %w", err)
	}
	identity, err := verify.NewCertificateIdentity(
		verify.SubjectAlternativeNameMatcher{SubjectAlternativeName: provenanceSAN(version)},
		verify.IssuerMatcher{Issuer: provenanceIssuer},
		certificate.Extensions{
			SourceRepositoryURI: provenanceRepo,
			SourceRepositoryRef: "refs/tags/" + version,
		})
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	// Signature, chain, SCT, log, time and digest first, with no identity
	// policy: sigstore-go's error then names the stage that failed.
	if _, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithoutIdentitiesUnsafe())); err != nil {
		return fmt.Errorf("sigstore verification: %w", err)
	}
	res, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(identity)))
	if err != nil {
		return fmt.Errorf("identity: not the release workflow at %s: %w", version, err)
	}
	return checkStatement(res.Statement, assetName, sha256hex)
}

// provenanceSAN is the certificate SAN of version's release workflow.
func provenanceSAN(version string) string {
	return provenanceWorkflow + "@refs/tags/" + version
}

// checkStatement checks the verified statement itself: in-toto v1, the SLSA
// provenance v1 predicate, and a subject with both the asset's name and its
// digest (sigstore-go matches the digest against any subject).
func checkStatement(st *intoto.Statement, assetName, sha256hex string) error {
	if st == nil {
		return errors.New("statement: the bundle carries no in-toto statement")
	}
	if st.GetType() != statementType {
		return fmt.Errorf("statement: type %q is not %s", st.GetType(), statementType)
	}
	if st.GetPredicateType() != slsaPredicate {
		return fmt.Errorf("predicate type: %q is not %s", st.GetPredicateType(), slsaPredicate)
	}
	for _, s := range st.GetSubject() {
		if s.GetName() == assetName && strings.EqualFold(s.GetDigest()["sha256"], sha256hex) {
			return nil
		}
	}
	return fmt.Errorf("artifact: no subject is %s with sha256 %s", assetName, sha256hex)
}

// trustedMaterial loads the Sigstore public-good trusted root (a test
// replaces it with a testdata root).
var trustedMaterial = func(ctx context.Context) (root.TrustedMaterial, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("the Sigstore trusted root: no user cache directory: %w", err)
	}
	return trustedRoot(ctx, filepath.Join(dir, "clawbridge", "sigstore-tuf"))
}

// trustedRoot is the public-good trusted root from Sigstore's TUF
// repository, starting from sigstore-go's embedded root and caching in
// cacheDir. When TUF can't be reached it uses the cache while its metadata
// is valid; otherwise it fails.
func trustedRoot(ctx context.Context, cacheDir string) (root.TrustedMaterial, error) {
	return trustedRootWith(cacheDir, tuf.DefaultRoot(), &tufFetcher{ctx: ctx, client: defaultClient([]string{tufHost})})
}

func trustedRootWith(cacheDir string, anchor []byte, f fetcher.Fetcher) (root.TrustedMaterial, error) {
	// The root.json TUF verified and stored on an earlier run, read before
	// the TUF client overwrites it with the anchor.
	cached, cachedErr := os.ReadFile(filepath.Join(cacheDir, tuf.URLToPath(tuf.DefaultMirror), "root.json"))
	opts := &tuf.Options{Root: anchor, CachePath: cacheDir, RepositoryBaseURL: tuf.DefaultMirror, Fetcher: f}
	tr, onlineErr := fetchTrustedRoot(opts)
	if onlineErr == nil {
		return tr, nil
	}
	// Offline: only the cache, anchored on that cached root.json (the
	// embedded root may have expired since it was built), with a fetcher
	// that refuses, so a stale cache fails instead of refreshing.
	offline := *opts
	offline.ForceCache = true
	offline.Fetcher = offlineFetcher{}
	if cachedErr == nil {
		offline.Root = cached
	}
	tr, err := fetchTrustedRoot(&offline)
	if err != nil {
		return nil, fmt.Errorf("the Sigstore trusted root: TUF failed (%v) and no valid cached root is available: %w", onlineErr, err)
	}
	return tr, nil
}

func fetchTrustedRoot(opts *tuf.Options) (*root.TrustedRoot, error) {
	c, err := tuf.New(opts)
	if err != nil {
		return nil, err
	}
	return root.GetTrustedRoot(c)
}

// tufFetcher fetches TUF metadata over https from tufHost only, with the
// release client's timeouts and redirect policy.
type tufFetcher struct {
	ctx    context.Context
	client *http.Client
}

func (f *tufFetcher) DownloadFile(raw string, maxLength int64, _ time.Duration) ([]byte, error) {
	req, err := newRequest(f.ctx, raw, []string{tufHost})
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// TUF reads a 404 for the next root.json as "no newer root".
		return nil, &metadata.ErrDownloadHTTP{StatusCode: resp.StatusCode, URL: raw}
	}
	var buf bytes.Buffer
	if _, err := copyLimited(&buf, resp.Body, maxLength); err != nil {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: fmt.Sprintf("%s: %v", raw, err)}
	}
	return buf.Bytes(), nil
}

// offlineFetcher refuses every download.
type offlineFetcher struct{}

func (offlineFetcher) DownloadFile(raw string, _ int64, _ time.Duration) ([]byte, error) {
	return nil, fmt.Errorf("offline: not fetching %s", raw)
}
