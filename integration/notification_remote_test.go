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

// A populated readable mirror is deliberately present; rejected status calls
// must not manufacture authoritative notifications from its unread messages.
func TestNotificationStatusNeverUsesMirror(t *testing.T) {
	path, snapshot := mirrorFixture(t)
	var mode atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := int(mode.Load())
		if status == 0 {
			status = 503
		}
		w.WriteHeader(status)
		io.WriteString(w, `{"error":{"code":"unavailable","message":"synthetic private diagnostic"}}`)
	}))
	defer endpoint.Close()
	snapshot.Binding.Server = endpoint.URL
	if err := mirror.Save(context.Background(), path, snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := snapshot.Binding
	config := remote.Config{URL: endpoint.URL, Audience: b.Audience, Principal: b.Principal, CachePath: path}
	request := bus.Request{Operation: "inbox", Workspace: b.Workspace, Project: b.Project, Session: b.Session, Args: map[string]any{"instance": b.Instance}}
	client := mustRemote(t, config)
	cached, err := client.Execute(context.Background(), request)
	if err != nil || cached.Source != "offline-mirror" {
		t.Fatalf("fixture mirror unavailable: source=%q err=%v", cached.Source, err)
	}
	request.Operation = "inbox_status"
	if mirror.Readable(request.Operation) {
		t.Fatal("status advertised as mirror-readable")
	}
	if _, err := mirror.Offline(context.Background(), path, b, request); err == nil {
		t.Fatal("direct mirror status succeeded")
	}
	for _, code := range []int32{400, 401, 403, 500, 502, 503, 504} {
		mode.Store(code)
		if result, err := client.Execute(context.Background(), request); err == nil || result.Source == "offline-mirror" {
			t.Fatalf("HTTP %d produced cached/accepted status: source=%q err=%v", code, result.Source, err)
		}
	}
	failedCredentials, err := remote.NewWithTokenSource(config, remote.TokenSourceFunc(func(context.Context) (string, error) { return "", errors.New("synthetic credential unavailable") }))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := failedCredentials.Execute(context.Background(), request); err == nil || result.Source == "offline-mirror" {
		t.Fatal("credential failure used mirror status")
	}
	endpoint.Close()
	if result, err := client.Execute(context.Background(), request); err == nil || result.Source == "offline-mirror" {
		t.Fatal("transport outage used mirror status")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("status failure modified mirror snapshot")
	}
}
