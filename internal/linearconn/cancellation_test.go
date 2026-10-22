package linearconn

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestLinearCancelledRefreshWaitersReturnBeforeRefreshAndNeverDispatch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var mutations, providerCalls, cancelledDispatch atomic.Int64
	m := testManager(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() != nil {
			cancelledDispatch.Add(1)
		}
		if r.URL.Path == "/token" {
			mutations.Add(1)
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return response(`{"access_token":"refreshed","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`), nil
		}
		providerCalls.Add(1)
		// Deliberately ignore request cancellation: the credential transport must
		// enforce its own no-dispatch boundary before entering this fake sink.
		return response(`{}`), nil
	}))
	m.config.Verify = func(context.Context, *http.Client, Access) (Identity, error) { return Identity{}, nil }
	p, err := m.Import(context.Background(), "work", Write, strings.NewReader("initial"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.readCredential(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Imported = false
	c.ClientID = "public-client"
	c.Token.RefreshToken = "old-refresh"
	c.Token.Expiry = time.Unix(1, 0)
	if err = m.writeCredential(p.ID, c); err != nil {
		t.Fatal(err)
	}
	client, err := m.HTTPClient(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		r, _ := http.NewRequestWithContext(firstCtx, http.MethodPost, Endpoint(Write), nil)
		resp, e := client.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		firstDone <- e
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("controlled refresh did not start")
	}
	operations := map[string]func(context.Context) error{
		"provider request": func(ctx context.Context) error {
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint(Write), nil)
			resp, e := client.Do(r)
			if resp != nil {
				resp.Body.Close()
			}
			return e
		},
		"status": func(ctx context.Context) error { _, e := m.Status(ctx, p.ID); return e },
		"logout": func(ctx context.Context) error { return m.Logout(ctx, p.ID) },
		"replacement commit": func(ctx context.Context) error {
			_, e := m.commit(ctx, "changed", p.ID, Write, credential{Version: 1, Imported: true, Token: oauth2.Token{AccessToken: "replacement", TokenType: "Bearer"}})
			return e
		},
	}
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(operations))
	for name, operation := range operations {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			results <- result{name, operation(ctx)}
		}()
	}
	deadline := time.NewTimer(400 * time.Millisecond)
	defer deadline.Stop()
	for range operations {
		select {
		case r := <-results:
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Errorf("%s returned %v", r.name, r.err)
			}
		case <-deadline.C:
			t.Fatal("cancelled waiter remained blocked behind held refresh")
		}
	}
	if cancelledDispatch.Load() != 0 || providerCalls.Load() != 0 {
		t.Fatal("cancelled or waiting operation reached provider")
	}
	unblock()
	select {
	case err = <-firstDone:
		if err != nil {
			t.Fatal("original refresh failed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("original refresh failed cleanup")
	}
	if cancelledDispatch.Load() != 0 || providerCalls.Load() != 1 || mutations.Load() != 1 {
		t.Fatal("unexpected dispatch/refresh counts", cancelledDispatch.Load(), providerCalls.Load(), mutations.Load())
	}
	stored, err := m.readCredential(p.ID)
	if err != nil || stored.Token.RefreshToken != "rotated" {
		t.Fatal("original rotation not preserved", err)
	}
	profile, err := m.Status(context.Background(), p.ID)
	if err != nil || profile.Name != "work" {
		t.Fatal("cancelled manager mutation changed profile", err)
	}
}

func TestLinearCredentialTransportsRejectAlreadyCancelledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("cancelled request entered base transport")
		return nil, nil
	})
	for name, transport := range map[string]http.RoundTripper{"guarded": guardedTransport{base: sink}, "token": tokenTransport{base: sink, token: "secret"}} {
		t.Run(name, func(t *testing.T) {
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint(Write), nil)
			if _, err := transport.RoundTrip(r); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
