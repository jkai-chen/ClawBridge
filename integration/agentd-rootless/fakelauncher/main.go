// Command fakelauncher is a stand-in ClawBridge server for agentd's
// real-daemon CI job (integration/agentd-rootless): the three launcher
// calls, over HTTPS under a CA it makes at start, wanting one owner's
// agent. It logs each call as a JSON line; never the token or a credential.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net/http"
	"os"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8443", "")
	host := flag.String("host", "bridge.test", "the server's name, for its certificate")
	caOut := flag.String("ca", "", "where to write the CA certificate (PEM)")
	owner := flag.String("owner", "p_ciowner0001", "the one owner ref wanted")
	provider := flag.String("provider", "api.provider.test:443", "that owner's provider host:port")
	flag.Parse()
	token := os.Getenv("FAKE_LAUNCHER_TOKEN")
	if token == "" || *caOut == "" {
		log.Fatal("FAKE_LAUNCHER_TOKEN and -ca are required")
	}
	cert, caPEM, err := certificates(*host)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*caOut, caPEM, 0o644); err != nil {
		log.Fatal(err)
	}

	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	record := func(call string, status int, extra map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		line := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "call": call, "status": status}
		for k, v := range extra {
			line[k] = v
		}
		_ = out.Encode(line)
	}
	answer := func(w http.ResponseWriter, call string, status int, body any, extra map[string]any) {
		record(call, status, extra)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	authorized := func(w http.ResponseWriter, r *http.Request, call string) bool {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ClawBridge-Model-API") != "1" {
			answer(w, call, http.StatusUnauthorized, map[string]string{"error": "unauthorized"}, nil)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/dispatcher/launcher/wanted", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r, "wanted") {
			return
		}
		answer(w, "wanted", http.StatusOK, map[string]any{
			"agents":     []any{map[string]any{"owner_ref": *owner, "provider": *provider, "config_revision": 1}},
			"max_agents": 1,
		}, nil)
	})
	mux.HandleFunc("POST /v1/dispatcher/launcher/credential", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r, "credential") {
			return
		}
		var in struct {
			OwnerRef string `json:"owner_ref"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || in.OwnerRef != *owner {
			answer(w, "credential", http.StatusNotFound, map[string]string{"error": "not_found"}, map[string]any{"owner_ref": in.OwnerRef})
			return
		}
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		answer(w, "credential", http.StatusOK, map[string]any{
			"credential": "cba_" + base64.RawURLEncoding.EncodeToString(secret),
			"expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		}, map[string]any{"owner_ref": in.OwnerRef})
	})
	mux.HandleFunc("POST /v1/dispatcher/launcher/report", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r, "report") {
			return
		}
		var in struct {
			OwnerRef string `json:"owner_ref"`
			Code     string `json:"code"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
		answer(w, "report", http.StatusOK, map[string]bool{"ok": true}, map[string]any{"owner_ref": in.OwnerRef, "code": in.Code})
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// certificates makes a CA and a certificate for host signed by it.
func certificates(host string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "agentd CI fake CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}
