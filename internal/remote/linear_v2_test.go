package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
)

func TestLinearV2BindingPreservesAuthoritativeFailure(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 409, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"synthetic"}`))
			}))
			defer server.Close()
			client, err := NewWithTokenSource(Config{URL: server.URL, Audience: "synthetic", Principal: "operator@example.test", CachePath: filepath.Join(t.TempDir(), "unused.db")}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic", nil }))
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Execute(context.Background(), bus.Request{Operation: "taskboard_get_v2", Workspace: "workspace", Project: "project"})
			if err == nil || result.Source != "" || calls != 1 {
				t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
			}
			if status == 200 {
				var metadata *metadataError
				if !errors.As(err, &metadata) || metadata.Code() != "invalid" {
					t.Fatalf("malformed authority response: %T %v", err, err)
				}
			} else {
				var rejected *HTTPError
				if !errors.As(err, &rejected) || rejected.Status != status {
					t.Fatalf("lost authority rejection: %T %v", err, err)
				}
			}
		})
	}
}
