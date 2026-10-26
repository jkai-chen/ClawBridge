package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/watcher"
)

// The fallback build: an app holding v0.3.3's five-scope token refuses a
// relation write with capability_unavailable, before anything is sent,
// rather than letting Linear drop the edge.
func TestFallbackRelationWriteIsRefused(t *testing.T) {
	linear := &fiveScopes{}
	app, err := linearapp.New(linearapp.Config{
		ClientID: "client", Secret: "secret", Key: make([]byte, 32),
		Store: &oneToken{}, HTTP: &http.Client{Transport: linear}, MCPTransport: noLinear{t}, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &AppWriter{App: app}
	for _, op := range []string{"relation_add", "relation_remove"} {
		_, err = w.Execute(context.Background(), bus.Identity{Principal: "a@example.com"}, bus.Request{Operation: "linear_write", Args: map[string]any{"operation": op, "args": map[string]any{}}})
		if code, _ := serviceError(err); code != "invalid" || !strings.HasPrefix(responseMessage(code, err), "capability_unavailable:") {
			t.Fatal(op, err)
		}
	}
	if linear.mints != 1 || strings.Contains(linear.form, "write") {
		t.Fatal("the mint asked for", linear.form, linear.mints)
	}
}

// A health post in the fallback build: the watcher asks watcherApp whether
// the token holds write before sending anything, and with the five-scope
// token the answer is no, so postHealth settles it refused (app_scope)
// (watcher TestHealthPostFallsBackToTheNotice) and nothing reaches Linear.
func TestFallbackHealthPostIsRefused(t *testing.T) {
	linear := &fiveScopes{}
	app, err := linearapp.New(linearapp.Config{
		ClientID: "client", Secret: "secret", Key: make([]byte, 32),
		Store: &oneToken{}, HTTP: &http.Client{Transport: linear}, MCPTransport: noLinear{t}, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var w watcher.App = watcherApp{app}
	for range 2 {
		if granted, err := w.HasScope(context.Background(), "write"); granted || err != nil {
			t.Fatal("write granted:", granted, err)
		}
	}
	if granted, err := w.HasScope(context.Background(), "read"); !granted || err != nil {
		t.Fatal("read not granted:", granted, err)
	}
	if linear.mints != 1 || strings.Contains(linear.form, "write") {
		t.Fatal("the mint asked for", linear.form, linear.mints)
	}
}

// fiveScopes is Linear granting the five scopes asked for, and the app's
// viewer; anything else fails.
type fiveScopes struct {
	mu    sync.Mutex
	mints int
	form  string
}

func (f *fiveScopes) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) (*http.Response, error) {
		raw, _ := json.Marshal(v)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{"Content-Type": {"application/json"}}, Request: r}, nil
	}
	switch r.URL.String() {
	case "https://api.linear.app/oauth/token":
		f.mints++
		raw, _ := io.ReadAll(r.Body)
		f.form = string(raw)
		return reply(map[string]any{"access_token": "lin_oauth_fallback", "token_type": "Bearer", "expires_in": 2591999, "scope": "app:assignable app:mentionable comments:create issues:create read"})
	case "https://api.linear.app/graphql":
		return reply(map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "a0000000-0000-4000-8000-0000000000a1", "app": true, "organization": map[string]any{"id": "b0000000-0000-4000-8000-0000000000b1"}}}})
	}
	return nil, errorString("unexpected Linear request " + r.URL.String())
}

// oneToken keeps the one published token in memory.
type oneToken struct {
	mu  sync.Mutex
	row *bus.AppToken
}

func (o *oneToken) RecordMintedAppToken(context.Context, string, string, time.Time, time.Time) error {
	return nil
}

func (o *oneToken) LiveAppToken(context.Context, string, string) (bus.AppToken, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.row == nil {
		return bus.AppToken{}, false, nil
	}
	return *o.row, true, nil
}

func (o *oneToken) PublishAppToken(_ context.Context, t bus.AppToken, _ string) (bus.AppToken, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.row = &t
	return t, true, nil
}

func (o *oneToken) AppTokensMinted(context.Context, string, time.Time) (int, error) { return 0, nil }
