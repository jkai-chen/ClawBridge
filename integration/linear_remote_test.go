package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
	"clawbridge/internal/remote"
)

func TestL1TaskboardAuthorityReadNeverFallsBackToMirror(t *testing.T) {
	path, snapshot := mirrorFixture(t)
	var status atomic.Int32
	status.Store(503)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		io.WriteString(w, `{"error":{"code":"unavailable","message":"SYNTHETIC_PRIVATE_PROVIDER_DIAGNOSTIC"}}`)
	}))
	t.Cleanup(endpoint.Close)
	snapshot.Binding.Server = endpoint.URL
	if err := mirror.Save(context.Background(), path, snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := snapshot.Binding
	cfg := remote.Config{URL: endpoint.URL, Audience: b.Audience, Principal: b.Principal, CachePath: path}
	client, err := remote.NewWithTokenSource(cfg, remote.TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-test-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	r := bus.Request{Operation: "inbox", Workspace: b.Workspace, Project: b.Project, Session: b.Session, Args: map[string]any{"instance": b.Instance}}
	if cached, err := client.Execute(context.Background(), r); err != nil || cached.Source != "offline-mirror" {
		t.Fatal("fixture does not contain a usable scoped mirror", err)
	}
	for _, operation := range []string{"taskboard_get", "taskboard_access"} {
		r.Operation = operation
		if mirror.Readable(r.Operation) {
			t.Fatal("binding advertised as mirror-readable")
		}
		if _, err = mirror.Offline(context.Background(), path, b, r); err == nil {
			t.Fatal("mirror synthesized authoritative binding")
		}
		for _, code := range []int32{400, 401, 403, 404, 429, 500, 502, 503, 504} {
			status.Store(code)
			if result, err := client.Execute(context.Background(), r); err == nil || result.Source == "offline-mirror" {
				t.Fatalf("HTTP %d accepted/fell back to cached binding", code)
			}
		}
		failed, err := remote.NewWithTokenSource(cfg, remote.TokenSourceFunc(func(context.Context) (string, error) { return "", errors.New("SYNTHETIC_PRIVATE_CREDENTIAL_FAILURE") }))
		if err != nil {
			t.Fatal(err)
		}
		if result, err := failed.Execute(context.Background(), r); err == nil || result.Source == "offline-mirror" {
			t.Fatal("credential error fell back to binding mirror")
		}
	}
	endpoint.Close()
	for _, operation := range []string{"taskboard_get", "taskboard_access"} {
		r.Operation = operation
		if result, err := client.Execute(context.Background(), r); err == nil || result.Source == "offline-mirror" {
			t.Fatal("transport error fell back to binding mirror")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("binding failure altered complete mirror")
	}
}
