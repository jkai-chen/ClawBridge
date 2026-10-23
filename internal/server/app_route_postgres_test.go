package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/remote"
)

// noProfile fails any use of a member's own Linear profile.
type noProfile struct{ used *int }

func (n noProfile) Status(context.Context, string) (linearconn.Profile, error) {
	*n.used++
	return linearconn.Profile{}, errors.New("the member's profile was used")
}
func (n noProfile) HTTPClient(context.Context, string) (*http.Client, error) {
	*n.used++
	return nil, errors.New("the member's profile was used")
}

// A hosted client's agent write crosses the real handler as linear_write
// and is made as the app; its retry answers the recorded receipt, and the
// member's own profile is never touched.
func TestPostgresClientWritesThroughTheApp(t *testing.T) {
	f := newAWFixture(t, awOrg)
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: f.owner, Subject: "sub-owner"}, nil
	})
	host := httptest.NewServer(NewHandlerWithApp(f.store, verifier, OAuthMetadata{}, f.writer))
	defer host.Close()
	client, err := remote.NewWithTokenSource(remote.Config{URL: host.URL, Audience: "audience", Principal: f.owner, CachePath: filepath.Join(t.TempDir(), "cache.db")}, remote.TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	used := 0
	service := linearboard.New(linearboard.Config{Authority: "client", Executor: client, Connections: noProfile{&used}, ProfileID: "profile-1", LedgerDirectory: filepath.Join(t.TempDir(), "ledger"), AppRoute: true})
	scope := bus.Request{Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}}
	args := map[string]any{"id": awIssue, "body": "from a hosted client", "operation_id": "client-1"}
	first, err := service.ExecuteV2(context.Background(), scope, "comment", args)
	if err != nil {
		t.Fatal(err)
	}
	receipt := awJSON(t, first)
	if receipt["id"] != awComment || receipt["issue_id"] != awIssue || f.mcp.count("save_comment") != 1 || used != 0 {
		t.Fatal("receipt", receipt, f.mcp.count("save_comment"), used)
	}
	again, err := service.ExecuteV2(context.Background(), scope, "comment", args)
	if err != nil || awJSON(t, again)["id"] != awComment || f.mcp.count("save_comment") != 1 {
		t.Fatal("retry", again, err)
	}
	// Outside the binding: refused by the server, nothing sent, not uncertain.
	if _, err = service.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": awForeignIssue, "body": "x", "operation_id": "client-2"}); err == nil || linearboard.IsUncertain(err) || f.mcp.count("save_comment") != 1 {
		t.Fatal("foreign issue", err)
	}
}
