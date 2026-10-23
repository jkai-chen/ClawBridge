package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clawbridge/internal/bus"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const goodIntakeToken = "cbi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeIntake struct {
	requests []string
	tokens   []string
	proved   bool
}

func (f *fakeIntake) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("not used")
}
func (f *fakeIntake) IntakeToken(_ context.Context, token string) (bus.IntakeGrant, error) {
	if token != goodIntakeToken {
		return bus.IntakeGrant{}, bus.ErrIntakeUnauthorized
	}
	return bus.IntakeGrant{TokenID: "itk_1", WorkspaceID: "w", ProjectID: "p"}, nil
}
func (f *fakeIntake) IntakeRequest(ctx context.Context, token, issue string, prove IssueProver) (map[string]any, error) {
	f.tokens = append(f.tokens, token)
	f.requests = append(f.requests, issue)
	if prove != nil {
		if _, err := prove(ctx, "w", "p", issue); err == nil {
			f.proved = true
		}
	}
	return map[string]any{"result": "queued", "request_id": "ireq_1"}, nil
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func intakeServer(t *testing.T) (*fakeIntake, *httptest.Server) {
	t.Helper()
	store := &fakeIntake{}
	// The verifier admits any Google-shaped token: intake must not care.
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		if strings.HasPrefix(token, "cbi_") {
			return Identity{}, errors.New("not a Google ID token")
		}
		return Identity{Principal: "alice@example.test", Subject: "sub"}, nil
	})
	prove := func(context.Context, string, string, string) (bus.IntakeIssue, error) {
		return bus.IntakeIssue{UUID: "00000000-0000-4000-8000-0000000000b1"}, nil
	}
	host := httptest.NewServer(NewHandlerWithIntake(store, verifier, OAuthMetadata{}, nil, prove))
	t.Cleanup(host.Close)
	return store, host
}

// Only an intake token opens /mcp/intake, and an intake token opens
// nothing else (IN-L1).
func TestIntakeEndpointAcceptsOnlyIntakeTokens(t *testing.T) {
	store, host := intakeServer(t)
	for name, header := range map[string]string{"none": "", "google token": "Bearer eyJhbGciOiJSUzI1NiJ9.e30.sig", "unknown intake token": "Bearer cbi_" + strings.Repeat("f", 64), "two words": "Basic " + goodIntakeToken} {
		req, _ := http.NewRequest(http.MethodPost, host.URL+IntakePath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Errorf("%s: status %d", name, res.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/execute", strings.NewReader(`{"operation":"whoami"}`))
	req.Header.Set("Authorization", "Bearer "+goodIntakeToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("an intake token was accepted on /v1/execute:", res.StatusCode)
	}
	if len(store.requests) != 0 {
		t.Fatal("a refused caller reached the store:", store.requests)
	}
}

// The endpoint is send-only: one tool, whose answer carries no issue data.
func TestIntakeEndpointHasOneTypedTool(t *testing.T) {
	store, host := intakeServer(t)
	client := mcp.NewClient(&mcp.Implementation{Name: "loop", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: host.URL + IntakePath, HTTPClient: &http.Client{Transport: bearer{goodIntakeToken}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "clawbridge_request_agent" {
		t.Fatal("tools:", tools, err)
	}
	if _, err = session.ListResources(context.Background(), nil); err == nil {
		t.Fatal("the endpoint serves resources")
	}
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "clawbridge_request_agent", Arguments: map[string]any{"issue": " FIX-7 "}})
	if err != nil || out.IsError {
		t.Fatal(out, err)
	}
	got, _ := out.StructuredContent.(map[string]any)
	if got["result"] != "queued" || got["request_id"] != "ireq_1" || len(got) != 2 {
		t.Fatal("tool answer:", got)
	}
	if len(store.requests) != 1 || store.requests[0] != "FIX-7" || store.tokens[0] != goodIntakeToken || !store.proved {
		t.Fatal("the request did not reach the store with its token and prover:", store)
	}
}

// IN-L2: behind a private proxy the limit is keyed on the client the proxy
// saw; a public peer cannot choose its bucket with a forwarded header.
func TestIntakeClientAddress(t *testing.T) {
	for _, c := range []struct{ peer, forwarded, want string }{
		{"203.0.113.9:4000", "", "203.0.113.9"},
		{"203.0.113.9:4000", "198.51.100.1", "203.0.113.9"},
		{"172.18.0.5:4000", "", "172.18.0.5"},
		{"172.18.0.5:4000", "198.51.100.1", "198.51.100.1"},
		{"172.18.0.5:4000", "10.0.0.1, 198.51.100.2", "198.51.100.2"},
		{"127.0.0.1:4000", "198.51.100.3", "198.51.100.3"},
		{"172.18.0.5:4000", "not-an-ip", "172.18.0.5"},
	} {
		r := httptest.NewRequest("POST", IntakePath, nil)
		r.RemoteAddr = c.peer
		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-For", c.forwarded)
		}
		if got := clientAddress(r); got != c.want {
			t.Errorf("%s via %q: got %s, want %s", c.peer, c.forwarded, got, c.want)
		}
	}
}
