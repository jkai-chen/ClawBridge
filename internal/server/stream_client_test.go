package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// streamClientFor is a hosted client of the fixture's server, signed in as
// principal.
func streamClientFor(t *testing.T, url, principal string) *remote.Client {
	t.Helper()
	client, err := remote.NewWithTokenSource(remote.Config{URL: url, Audience: "audience", Principal: principal, CachePath: filepath.Join(t.TempDir(), "cache.db")}, remote.TokenSourceFunc(func(context.Context) (string, error) { return "tok-" + principal, nil }))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The client end to end against the real server: an observe stream's
// counts, a consume stream's message, and the end of the stream when the
// member is removed, with no retry after.
func TestPostgresRemoteClientStream(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	client := streamClientFor(t, f.host.URL, f.member)
	scope := remote.StreamScope{Workspace: f.workspace, Project: "p", Session: "m", Instance: "m-inst", Mode: bus.StreamConsume}
	events := make(chan remote.StreamEvent, 10)
	done := make(chan error, 1)
	go func() {
		done <- client.Stream(context.Background(), scope, remote.StreamOptions{}, func(e remote.StreamEvent) error {
			events <- e
			return nil
		})
	}()
	// The stream is open once the hub has it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.hub.mu.Lock()
		n := f.hub.open[f.member]
		f.hub.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never opened")
		}
		time.Sleep(20 * time.Millisecond)
	}
	id := f.send("m", "for the member")
	select {
	case e := <-events:
		var data struct {
			Message struct{ ID, Body string } `json:"message"`
			LeaseID string                    `json:"lease_id"`
		}
		if e.Name != "message" || json.Unmarshal(e.Data, &data) != nil || data.Message.ID != id || data.Message.Body != "for the member" || data.LeaseID == "" || e.ID != "1" {
			t.Fatal(e.Name, string(e.Data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
	if resp := f.post(f.owner, bus.Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": f.member}}); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	select {
	case err := <-done:
		var ended *remote.StreamEnded
		if !errors.As(err, &ended) || (ended.Reason != "revoked" && ended.Reason != "refused") {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream outlived the member's removal")
	}
}

// A server without the stream (an older one) reads as unsupported, so the
// client polls.
func TestRemoteClientStreamOldServer(t *testing.T) {
	host := httptest.NewServer(NewHandlerWithOptions(nil, VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: "ada@example.com", Subject: "sub-ada"}, nil
	}), HandlerOptions{}))
	defer host.Close()
	client := streamClientFor(t, host.URL, "ada@example.com")
	err := client.Stream(context.Background(), remote.StreamScope{Workspace: "w", Project: "p", Session: "s", Instance: "i", Mode: bus.StreamObserve}, remote.StreamOptions{}, func(remote.StreamEvent) error { return nil })
	if !errors.Is(err, remote.ErrStreamUnsupported) {
		t.Fatal(err)
	}
}
