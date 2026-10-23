package remote

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

// A refused comment reservation says why on the CLI: the cap's next slot
// (429) and a conflicting retry (409) keep the bus's public reason.
func TestCommentRefusalsKeepTheirReasonThroughTheServer(t *testing.T) {
	store, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, r := range []bus.Request{{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}, {Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}}, {Operation: "register_session", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i"}}} {
		if _, err = store.Execute(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(op, digest string) error {
		_, e := store.Execute(ctx, bus.Request{Operation: "linear_comment_reserve", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i", "issue_uuid": "00000000-0000-4000-8000-0000000000a1", "operation_id": op, "body_sha256": digest}})
		return e
	}
	one := strings.Repeat("1", 64)
	for _, op := range []string{"a", "b", "c"} {
		if err = reserve(op, one); err != nil {
			t.Fatal(err)
		}
	}
	capped, conflict := reserve("d", one), reserve("a", strings.Repeat("2", 64))
	verifier := server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
		return server.Identity{Principal: "alice@example.test", Subject: "sub"}, nil
	})
	for name, c := range map[string]struct {
		refusal error
		status  int
		want    string
	}{
		"cap":      {capped, 429, "next slot at"},
		"conflict": {conflict, 409, "already used for a different comment"},
	} {
		if c.refusal == nil {
			t.Fatal(name, "no refusal to forward")
		}
		host := httptest.NewServer(server.NewHandler(errorExecutor{c.refusal}, verifier))
		_, err := publicTestClient(t, host.URL).Execute(ctx, bus.Request{Operation: "linear_comment_reserve", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i"}})
		host.Close()
		var rejected *HTTPError
		if !errors.As(err, &rejected) || rejected.Status != c.status || !strings.Contains(rejected.Message, c.want) {
			t.Errorf("%s: %+v %v", name, rejected, err)
		}
	}
}
