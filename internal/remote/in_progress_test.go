package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// A write another request is still sending is a 409 in_progress whose
// reason reaches the caller, apart from an uncertain conflict.
func TestInProgressKeepsItsCodeAndReason(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"in_progress","message":"in_progress: retry the same operation_id shortly"}}`))
	}))
	defer host.Close()
	_, err := publicTestClient(t, host.URL).Execute(context.Background(), bus.Request{Operation: "linear_write", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i"}})
	var rejected *HTTPError
	if !errors.As(err, &rejected) || rejected.Status != 409 || rejected.Code != "in_progress" || !strings.HasPrefix(rejected.Message, "in_progress: retry") {
		t.Fatal(rejected, err)
	}
	if status, code, message := rejected.Rejection(); status != 409 || code != "in_progress" || message != rejected.Message {
		t.Fatal("rejection", status, code, message)
	}
}
