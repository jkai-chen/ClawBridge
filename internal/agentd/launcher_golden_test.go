package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"clawbridge/internal/bus"
)

// The shared contract (api/dispatcher-model/golden): the server's Go tests
// write its launcher answers there, and the launcher client must decode
// each one, so a drift on either side fails CI. The requests are pinned
// here too: method, the token, the API header and the exact body the
// server's closed requests take [R94-M1].
func TestLauncherClientDecodesTheGoldenAnswers(t *testing.T) {
	dir := filepath.Join("..", "..", "api", "dispatcher-model", "golden")
	type endpoint struct {
		method, file string
		// fields is the request body's exact field set, nil for none.
		fields []string
	}
	endpoints := map[string]endpoint{
		"/v1/dispatcher/launcher/wanted":     {method: http.MethodGet, file: "launcher-wanted.json"},
		"/v1/dispatcher/launcher/credential": {method: http.MethodPost, file: "launcher-credential.json", fields: []string{"owner_ref"}},
		"/v1/dispatcher/launcher/report":     {method: http.MethodPost, file: "launcher-report.json", fields: []string{"code", "owner_ref"}},
	}
	var mu sync.Mutex
	var bodies []map[string]string
	wantedFile := "launcher-wanted.json"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, ok := endpoints[r.URL.Path]
		if !ok || r.Method != e.method || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("ClawBridge-Model-API") != "1" {
			t.Errorf("%s %s with %q %q", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ClawBridge-Model-API"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(r.Body)
		if e.fields == nil {
			if raw.Len() != 0 {
				t.Errorf("%s has a body", r.URL.Path)
			}
		} else {
			var body map[string]string
			if r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(raw.Bytes(), &body) != nil {
				t.Errorf("%s: not a JSON object of strings: %s", r.URL.Path, raw.String())
			}
			if keys := slices.Sorted(maps.Keys(body)); !slices.Equal(keys, e.fields) {
				t.Errorf("%s: fields %v, not %v", r.URL.Path, keys, e.fields)
			}
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		file := e.file
		if file == "launcher-wanted.json" {
			mu.Lock()
			file = wantedFile
			mu.Unlock()
		}
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	c := NewLauncherClient(srv.URL, testToken).(*launcherClient)
	c.http.Transport = srv.Client().Transport
	ctx := context.Background()

	wanted, max, err := c.Wanted(ctx)
	if err != nil || len(wanted) == 0 || max < 1 {
		t.Fatalf("wanted: %+v %d %v", wanted, max, err)
	}
	for _, w := range wanted {
		if !ownerRefPattern.MatchString(w.Ref) || w.Provider == "" || w.Revision < 1 {
			t.Fatalf("a wanted agent the reconciler would refuse: %+v", w)
		}
	}
	ref := wanted[0].Ref
	if cred, err := c.Credential(ctx, ref); err != nil || !credentialPattern.MatchString(cred) {
		t.Fatalf("credential: %v", err)
	}
	// Every code agentd reports is one the server takes.
	for _, code := range []string{codeCrashloop, codeStartFailing} {
		if !slices.Contains(bus.LauncherReportCodes, code) {
			t.Fatalf("the server does not take %s", code)
		}
		if err := c.Report(ctx, ref, code); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	for _, body := range bodies {
		if body["owner_ref"] != ref {
			t.Fatalf("a request for %q, not the wanted %q", body["owner_ref"], ref)
		}
	}
	// No owner wanted: an empty list, read as such [R94-L1].
	mu.Lock()
	wantedFile = "launcher-wanted-empty.json"
	mu.Unlock()
	if wanted, max, err := c.Wanted(ctx); err != nil || len(wanted) != 0 || max < 1 {
		t.Fatalf("empty wanted set: %+v %d %v", wanted, max, err)
	}
}
