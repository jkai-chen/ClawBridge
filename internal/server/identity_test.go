package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"google.golang.org/api/idtoken"
)

type boundExecutor struct{ got *bus.Identity }

func (b boundExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	panic("an identity-capable executor was called without the account")
}
func (b boundExecutor) ExecuteAsIdentity(_ context.Context, id bus.Identity, _ bus.Request) (bus.Result, error) {
	*b.got = id
	return bus.Result{Data: map[string]any{}}, nil
}

// The verified Google account reaches an authority that can bind it.
func TestServerPassesTheVerifiedAccountToTheAuthority(t *testing.T) {
	var got bus.Identity
	handler := NewHandler(boundExecutor{&got}, VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: "alice@example.test", Subject: "sub-alice", HostedDomain: "example.test"}, nil
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"operation":"list_projects","workspace":"w"}`))
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || got != (bus.Identity{Principal: "alice@example.test", Issuer: bus.GoogleIssuer, Subject: "sub-alice", HostedDomain: "example.test"}) {
		t.Fatalf("status=%d identity=%+v body=%s", recorder.Code, got, recorder.Body.String())
	}
}

func TestOperatorUnbindNeedsConfirmOneAddressAndADatabase(t *testing.T) {
	t.Setenv("CLAWBRIDGE_TEST_EMPTY_DSN", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "usage"},
		{[]string{"drop-everything"}, "usage"},
		{[]string{"principal-unbind", "--confirm"}, "usage"},
		{[]string{"principal-unbind", "a@example.test", "b@example.test", "--confirm"}, "usage"},
		{[]string{"principal-unbind", "a@example.test"}, "--confirm"},
		{[]string{"principal-unbind", "a@example.test", "--confirm", "--database-url-env", "CLAWBRIDGE_TEST_EMPTY_DSN"}, "environment variable is empty"},
		{[]string{"workspace-clear-domains", "w"}, "--confirm"},
		{[]string{"workspace-clear-domains", "w", "--allow-previous", "--confirm"}, "allow-previous"},
		{[]string{"workspace-clear-domains", "w", "--confirm", "--database-url-env", "CLAWBRIDGE_TEST_EMPTY_DSN"}, "environment variable is empty"},
	} {
		var out bytes.Buffer
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) || out.Len() != 0 {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

func TestServingStoreRunsRequestsOnlyWithTheVerifiedAccount(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	store, err := openServingStore(context.Background(), dsn, "owner@serving-test.example")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ExecuteAs(context.Background(), "owner@serving-test.example", bus.Request{Operation: "list_projects", Workspace: "w"}); err == nil || !strings.Contains(err.Error(), "ExecuteAsIdentity") {
		t.Fatal("served store ran a request without the account binding", err)
	}
}

func TestVerifiedIdentityCarriesTheNormalHostedDomain(t *testing.T) {
	now := time.Now()
	payload := &idtoken.Payload{Issuer: "accounts.google.com", Audience: "aud", Subject: "sub", Expires: now.Add(time.Hour).Unix(), Claims: map[string]any{"email": "a@corp.example", "email_verified": true, "hd": " Corp.Example. "}}
	for hd, want := range map[any]string{" Corp.Example. ": "corp.example", "not a domain": "", 42: ""} {
		payload.Claims["hd"] = hd
		id, err := verifiedIdentity(payload, "aud", now)
		if err != nil || id.HostedDomain != want {
			t.Fatal(hd, id, err)
		}
	}
	delete(payload.Claims, "hd")
	if id, err := verifiedIdentity(payload, "aud", now); err != nil || id.HostedDomain != "" {
		t.Fatal("consumer account given a domain", id, err)
	}
}

// The operator command passes the authority's own reason through, so a
// database without the ClawBridge schema is named as such.
func TestOperatorCommandShowsWhyTheDatabaseWasRefused(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" || !strings.Contains(dsn, "://") {
		t.Skip("reviewer supplies disposable Postgres URL DSN")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("opempty_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	t.Setenv("CLAWBRIDGE_TEST_EMPTY_SCHEMA_DSN", dsn+sep+"search_path="+schema)
	var out bytes.Buffer
	err = RunOperator(context.Background(), []string{"principal-unbind", "a@example.test", "--confirm", "--database-url-env", "CLAWBRIDGE_TEST_EMPTY_SCHEMA_DSN"}, &out)
	if err == nil || !strings.Contains(err.Error(), "cannot read the ClawBridge schema version") {
		t.Fatal(err)
	}
}
