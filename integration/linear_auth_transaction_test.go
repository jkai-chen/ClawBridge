package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"clawbridge/internal/linearconn"
)

func TestL1NativeOAuthConfigFailureRollsBackStagedCredential(t *testing.T) {
	for _, mode := range []string{"new", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			f := newL1OAuth(t, linearconn.Read, "")
			opts := f.options()
			var previous []byte
			if mode == "replacement" {
				p, err := f.manager.Connect(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				opts.ProfileID = p.ID
				previous, err = os.ReadFile(filepath.Join(f.dir, "profiles.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			before := f.store.snapshot()
			// Synthetic config-write fault exactly after secure staging. Every path
			// is owned temporary data; the unrelated symlink target must survive.
			target := filepath.Join(t.TempDir(), "unrelated-metadata")
			if err := os.WriteFile(target, []byte("preserve this sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			var faultErr error
			f.store.afterSet = func() {
				path := filepath.Join(f.dir, "profiles.json")
				if mode == "replacement" {
					faultErr = os.Remove(path)
					if faultErr != nil {
						return
					}
				}
				faultErr = os.Symlink(target, path)
			}
			p, err := f.manager.Connect(context.Background(), opts)
			if faultErr != nil {
				t.Fatal(faultErr)
			}
			if err == nil || p.ID != "" {
				t.Fatal("staged credential with failed config publication reported connected")
			}
			if !reflect.DeepEqual(before, f.store.snapshot()) {
				t.Fatal("config failure did not delete new credential/restore exact previous credential")
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "preserve this sentinel" {
				t.Fatal("config failure overwrote unrelated target")
			}
			if mode == "replacement" {
				if _, err := f.manager.Status(context.Background(), opts.ProfileID); err == nil {
					t.Fatal("unsafe config path did not fail closed")
				}
				// Remove only the injected fault, then check the prior saved route.
				if err := os.Remove(filepath.Join(f.dir, "profiles.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.dir, "profiles.json"), previous, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.manager.Status(context.Background(), opts.ProfileID); err != nil {
					t.Fatal("restored prior route cannot use rolled-back credential", err)
				}
			}
		})
	}
}

func TestL1NativeOAuthCancelledRefreshWaiterReturnsPromptly(t *testing.T) {
	f := newL1OAuth(t, linearconn.Write, "")
	p, err := f.manager.Connect(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	client, err := f.manager.HTTPClient(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	entered, release := make(chan struct{}), make(chan struct{})
	f.refreshHook = func(r *http.Request) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer firstCancel()
	firstDone := make(chan error, 1)
	go func() {
		r, _ := http.NewRequestWithContext(firstCtx, http.MethodGet, linearconn.Endpoint(linearconn.Write), nil)
		response, err := client.Do(r)
		if response != nil {
			response.Body.Close()
		}
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not enter")
	}
	waiterCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	waiterDone := make(chan error, 1)
	go func() {
		r, _ := http.NewRequestWithContext(waiterCtx, http.MethodGet, linearconn.Endpoint(linearconn.Write), nil)
		response, err := client.Do(r)
		if response != nil {
			response.Body.Close()
		}
		waiterDone <- err
	}()
	var waiterErr error
	prompt := false
	select {
	case waiterErr = <-waiterDone:
		prompt = true
	case <-time.After(400 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Error("first controlled refresh failed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first refresh failed cleanup")
	}
	if !prompt {
		select {
		case waiterErr = <-waiterDone:
		case <-time.After(time.Second):
			t.Fatal("cancelled waiter failed cleanup")
		}
	}
	if !prompt {
		t.Error("cancelled profile refresh waiter remained blocked on another request's refresh beyond its own deadline")
	}
	if waiterErr == nil {
		t.Error("cancelled waiter reported successful provider request")
	}
}
