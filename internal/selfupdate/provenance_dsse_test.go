package selfupdate

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"
)

// synthWorld is a Sigstore of its own, offline: a test Fulcio CA (root and
// intermediate), a CT log that embeds an SCT in the leaf, and a Rekor log
// key that signs an inclusion promise (SET) over whatever entry it is given.
// The leaf certificate carries the release workflow's identity for version,
// so every check but the one a test breaks passes. It isolates the DSSE
// signature check: the log entry is built over the bundle's own envelope,
// valid or not, as a log that doesn't check signatures would record it.
type synthWorld struct {
	tm       root.TrustedMaterial
	leaf     *x509.Certificate
	leafKey  *ecdsa.PrivateKey
	rekorKey *ecdsa.PrivateKey
	rekorID  []byte
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// logID is a log's ID: the SHA-256 of its public key, PKIX DER.
func logID(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return sum[:]
}

func createCert(t *testing.T, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func utf8Ext(t *testing.T, oid asn1.ObjectIdentifier, s string) pkix.Extension {
	t.Helper()
	v, err := asn1.MarshalWithParams(s, "utf8")
	if err != nil {
		t.Fatal(err)
	}
	return pkix.Extension{Id: oid, Value: v}
}

func newSynthWorld(t *testing.T, version string) *synthWorld {
	t.Helper()
	now := time.Now()
	rootKey, interKey := newKey(t), newKey(t)
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-fulcio-root", Organization: []string{"example.test"}},
		NotBefore:             now.Add(-5 * time.Hour),
		NotAfter:              now.Add(5 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootCert := createCert(t, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	inter := createCert(t, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-fulcio-intermediate", Organization: []string{"example.test"}},
		NotBefore:             now.Add(-2 * time.Minute),
		NotAfter:              now.Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}, rootCert, &interKey.PublicKey, rootKey)
	leafKey := newKey(t)
	san, err := url.Parse(provenanceSAN(version))
	if err != nil {
		t.Fatal(err)
	}
	// Fulcio's OIDs: the issuer (v2), the source repository URI and ref.
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{san},
		ExtraExtensions: []pkix.Extension{
			utf8Ext(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}, provenanceIssuer),
			utf8Ext(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 12}, provenanceRepo),
			utf8Ext(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 14}, "refs/tags/"+version),
		},
	}
	// The precertificate: the leaf without its SCT list, which the CT log
	// signs; the SCT list is the last extension of the final leaf.
	preDER, err := x509.CreateCertificate(rand.Reader, tmpl, inter, &leafKey.PublicKey, interKey)
	if err != nil {
		t.Fatal(err)
	}
	ctKey := newKey(t)
	var ctLogID [32]byte
	copy(ctLogID[:], logID(t, ctKey))
	chain, err := ctx509.ParseCertificates(append(preDER, inter.Raw...))
	if err != nil {
		t.Fatal(err)
	}
	ts := uint64(now.UnixMilli())
	issuerKeyHash := sha256.Sum256(inter.RawSubjectPublicKeyInfo)
	mtl := &ct.MerkleTreeLeaf{Version: ct.V1, LeafType: ct.TimestampedEntryLeafType, TimestampedEntry: &ct.TimestampedEntry{
		Timestamp:    ts,
		EntryType:    ct.PrecertLogEntryType,
		PrecertEntry: &ct.PreCert{IssuerKeyHash: issuerKeyHash, TBSCertificate: chain[0].RawTBSCertificate},
	}}
	sct := ct.SignedCertificateTimestamp{SCTVersion: ct.V1, LogID: ct.LogID{KeyID: ctLogID}, Timestamp: ts}
	input, err := ct.SerializeSCTSignatureInput(sct, ct.LogEntry{Leaf: *mtl})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(input)
	sctSig, err := ecdsa.SignASN1(rand.Reader, ctKey, h[:])
	if err != nil {
		t.Fatal(err)
	}
	sct.Signature = ct.DigitallySigned{Algorithm: cttls.SignatureAndHashAlgorithm{Hash: cttls.SHA256, Signature: cttls.ECDSA}, Signature: sctSig}
	serialized, err := cttls.Marshal(sct)
	if err != nil {
		t.Fatal(err)
	}
	list, err := cttls.Marshal(ctx509.SignedCertificateTimestampList{SCTList: []ctx509.SerializedSCT{{Val: serialized}}})
	if err != nil {
		t.Fatal(err)
	}
	listExt, err := asn1.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, pkix.Extension{Id: asn1.ObjectIdentifier(ctx509.OIDExtensionCTSCT), Value: listExt})
	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, inter, &leafKey.PublicKey, interKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}

	rekorKey := newKey(t)
	rekorID := logID(t, rekorKey)
	rekorLogs := map[string]*root.TransparencyLog{hex.EncodeToString(rekorID): {
		BaseURL:             "https://rekor.example.test",
		ID:                  rekorID,
		ValidityPeriodStart: now.Add(-time.Hour),
		ValidityPeriodEnd:   now.Add(time.Hour),
		HashFunc:            crypto.SHA256,
		PublicKey:           &rekorKey.PublicKey,
		SignatureHashFunc:   crypto.SHA256,
	}}
	ctLogs := map[string]*root.TransparencyLog{hex.EncodeToString(ctLogID[:]): {
		BaseURL:             "https://ct.example.test",
		ID:                  ctLogID[:],
		ValidityPeriodStart: now.Add(-time.Hour),
		ValidityPeriodEnd:   now.Add(time.Hour),
		HashFunc:            crypto.SHA256,
		PublicKey:           &ctKey.PublicKey,
		SignatureHashFunc:   crypto.SHA256,
	}}
	fulcio := &root.FulcioCertificateAuthority{
		Root:                rootCert,
		Intermediates:       []*x509.Certificate{inter},
		ValidityPeriodStart: now.Add(-time.Hour),
		ValidityPeriodEnd:   now.Add(time.Hour),
		URI:                 "https://fulcio.example.test",
	}
	tm, err := root.NewTrustedRoot(root.TrustedRootMediaType01, []root.CertificateAuthority{fulcio}, ctLogs, nil, rekorLogs)
	if err != nil {
		t.Fatal(err)
	}
	return &synthWorld{tm: tm, leaf: leaf, leafKey: leafKey, rekorKey: rekorKey, rekorID: rekorID}
}

const synthPayloadType = "application/vnd.in-toto+json"

// sign is the leaf's DSSE signature over payload (PAE, ECDSA P-256 SHA-256).
func (w *synthWorld) sign(t *testing.T, payload []byte) []byte {
	t.Helper()
	pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(synthPayloadType), synthPayloadType, len(payload), payload)
	h := sha256.Sum256([]byte(pae))
	sig, err := ecdsa.SignASN1(rand.Reader, w.leafKey, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// bundle is a bundle of the envelope (payload, sig), logged by the test
// Rekor as a dsse v0.0.1 entry over exactly that envelope: the payload hash,
// the signature and the leaf as the verifier, with an inclusion promise. The log does not check the signature, so
// an invalid one is logged as faithfully as a valid one.
func (w *synthWorld) bundle(t *testing.T, payload, sig []byte) []byte {
	t.Helper()
	envelope := &protodsse.Envelope{
		Payload:     payload,
		PayloadType: synthPayloadType,
		Signatures:  []*protodsse.Signature{{Sig: sig}},
	}
	envJSON, err := json.Marshal(map[string]any{
		"payload":     base64.StdEncoding.EncodeToString(payload),
		"payloadType": synthPayloadType,
		"signatures":  []map[string]string{{"sig": base64.StdEncoding.EncodeToString(sig)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pemCert(w.leaf)
	envHash, payloadHash := sha256.Sum256(envJSON), sha256.Sum256(payload)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "0.0.1",
		"kind":       "dsse",
		"spec": map[string]any{
			"envelopeHash": map[string]string{"algorithm": "sha256", "value": hex.EncodeToString(envHash[:])},
			"payloadHash":  map[string]string{"algorithm": "sha256", "value": hex.EncodeToString(payloadHash[:])},
			"signatures": []map[string]string{{
				"signature": base64.StdEncoding.EncodeToString(sig),
				"verifier":  base64.StdEncoding.EncodeToString(certPEM),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The SET: the log's signature over the canonical JSON of the entry.
	integrated := time.Now().Unix()
	payloadJSON, err := json.Marshal(tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(body),
		IntegratedTime: integrated,
		LogIndex:       0,
		LogID:          hex.EncodeToString(w.rekorID),
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanonicalizer.Transform(payloadJSON)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(canonical)
	set, err := ecdsa.SignASN1(rand.Reader, w.rekorKey, h[:])
	if err != nil {
		t.Fatal(err)
	}
	// Bundle v0.1: an inclusion promise is enough (v0.2 and later want a
	// proof against a signed checkpoint, which adds nothing to this test).
	b := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: w.leaf.Raw}},
			TlogEntries: []*protorekor.TransparencyLogEntry{{
				LogIndex:          0,
				LogId:             &protocommon.LogId{KeyId: w.rekorID},
				KindVersion:       &protorekor.KindVersion{Kind: "dsse", Version: "0.0.1"},
				IntegratedTime:    integrated,
				InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: set},
				CanonicalizedBody: body,
			}},
		},
		Content: &protobundle.Bundle_DsseEnvelope{DsseEnvelope: envelope},
	}
	out, err := protojson.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pemCert(c *x509.Certificate) []byte {
	var buf bytes.Buffer
	buf.WriteString("-----BEGIN CERTIFICATE-----\n")
	enc := base64.StdEncoding.EncodeToString(c.Raw)
	for len(enc) > 64 {
		buf.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	buf.WriteString(enc + "\n-----END CERTIFICATE-----\n")
	return buf.Bytes()
}

// synthStatement is an in-toto v1 SLSA v1 statement over one asset.
func synthStatement(t *testing.T, asset, sha string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"_type":         statementType,
		"subject":       []map[string]any{{"name": asset, "digest": map[string]string{"sha256": sha}}},
		"predicateType": slsaPredicate,
		"predicate":     map[string]any{"buildDefinition": map[string]any{"buildType": "https://example.test/synthetic"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVerifyProvenanceDSSESignature isolates the DSSE signature check: the
// chain, the SCT, the log entry (promise and proof, over this very
// envelope), the integrated time and the identity all hold, and only the
// envelope's signature is wrong.
func TestVerifyProvenanceDSSESignature(t *testing.T) {
	const version = "v9.9.9"
	asset := assetName(version, "linux", "amd64")
	sum := sha256.Sum256([]byte("synthetic asset"))
	sha := hex.EncodeToString(sum[:])
	w := newSynthWorld(t, version)
	payload := synthStatement(t, asset, sha)

	t.Run("a valid signature verifies", func(t *testing.T) {
		if err := verifyProvenance(w.bundle(t, payload, w.sign(t, payload)), asset, sha, version, w.tm); err != nil {
			t.Fatalf("the control bundle: %v", err)
		}
	})

	// The world is not permissive: the same valid bundle for another tag
	// fails the identity.
	t.Run("the valid bundle for another tag is refused", func(t *testing.T) {
		err := verifyProvenance(w.bundle(t, payload, w.sign(t, payload)), asset, sha, "v9.9.8", w.tm)
		wantErr(t, err, "identity: not the release workflow at v9.9.8")
	})

	tampered := bytes.Clone(payload)
	i := bytes.Index(tampered, []byte("synthetic"))
	if i < 0 {
		t.Fatal("no predicate byte to flip")
	}
	tampered[i] ^= 0x20 // 's' becomes 'S': still JSON, still the same subject

	otherKey := newKey(t)
	pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(synthPayloadType), synthPayloadType, len(payload), payload)
	h := sha256.Sum256([]byte(pae))
	otherSig, err := ecdsa.SignASN1(rand.Reader, otherKey, h[:])
	if err != nil {
		t.Fatal(err)
	}

	for name, b := range map[string][]byte{
		// Signed, then a payload byte flipped; the log entry records the
		// flipped payload's hash.
		"a payload changed after signing": w.bundle(t, tampered, w.sign(t, payload)),
		// The payload as signed, but by a key that is not the leaf's.
		"a signature from another key": w.bundle(t, payload, otherSig),
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyProvenance(b, asset, sha, version, w.tm)
			if err == nil {
				t.Fatal("verified")
			}
			t.Log(err)
			msg := err.Error()
			// sigstore-go's own wording for a DSSE signature failure, after
			// every earlier stage passed.
			if !strings.Contains(msg, "failed to verify signature: could not verify envelope:") {
				t.Fatalf("err = %v, want the DSSE signature check", err)
			}
			for _, stage := range []string{"failed to verify log inclusion", "failed to verify timestamps", "failed to verify leaf certificate", "failed to verify signed certificate timestamp", "does not match any digest"} {
				if strings.Contains(msg, stage) {
					t.Errorf("err = %v: an earlier stage (%s) failed, not the signature", err, stage)
				}
			}
		})
	}
}
