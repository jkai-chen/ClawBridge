package linearapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// gql answers GraphQL queries after the viewer: status codes in order.
type gql struct {
	*linear
	statuses []int
	queries  []string
	tokens   []string
}

func (g *gql) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != graphqlURL {
		return g.linear.RoundTrip(r)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if strings.Contains(string(body), "viewer") {
		return g.linear.RoundTrip(r)
	}
	g.queries = append(g.queries, string(body))
	g.tokens = append(g.tokens, r.Header.Get("Authorization"))
	status := 200
	if len(g.statuses) > 0 {
		status, g.statuses = g.statuses[0], g.statuses[1:]
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "x"}}})
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}, nil
}

func TestGraphQLIsReadOnlyAndRemintsOnce(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	g := &gql{linear: &linear{app: true}}
	a, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: g}, Now: func() time.Time { return now }})
	var out struct{ Issue struct{ ID string } }
	for _, q := range []string{"mutation { issueDelete(id: \"x\") { success } }", "{ issue(id: $id) { id } } subscription { x }", "", "issueDelete"} {
		var e *Error
		if err := a.GraphQL(context.Background(), q, nil, &out); !errors.As(err, &e) || e.Code != "read_only_query_required" {
			t.Fatal("not refused:", q, err)
		}
	}
	if len(g.queries) != 0 {
		t.Fatal("a refused query reached Linear")
	}
	if err := a.GraphQL(context.Background(), "query($id: String!) { issue(id: $id) { id } }", map[string]any{"id": "x"}, &out); err != nil || out.Issue.ID != "x" {
		t.Fatal(err, out)
	}
	// A 401 re-mints and runs the read once more with the new token.
	now = now.Add(2 * time.Minute)
	g.statuses = []int{401}
	if err := a.GraphQL(context.Background(), "{ issue(id: \"x\") { id } }", nil, &out); err != nil || len(g.queries) != 3 || g.tokens[1] == g.tokens[2] {
		t.Fatal("401 not re-minted once", err, g.tokens)
	}
	now = now.Add(2 * time.Minute)
	g.statuses = []int{401, 401}
	before := len(g.queries)
	var e *Error
	if err := a.GraphQL(context.Background(), "{ issue(id: \"x\") { id } }", nil, &out); !errors.As(err, &e) || e.Code != "unavailable" || len(g.queries) != before+2 {
		t.Fatal("a second 401 was retried again", err, len(g.queries)-before)
	}
}

type capture struct{ auth []string }

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

// The app token goes only to Linear's MCP endpoint.
func TestBearerOnlyReachesLinearMCP(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	c := &capture{}
	a, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: &linear{app: true}}, Now: func() time.Time { return now }})
	b := bearer{a, c}
	for _, u := range []string{"https://evil.example/mcp", "http://mcp.linear.app/mcp", "https://mcp.linear.app/other"} {
		req, _ := http.NewRequest(http.MethodPost, u, nil)
		if _, err := b.RoundTrip(req); err == nil {
			t.Fatal("token sent to", u)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, "https://mcp.linear.app/mcp/readonly", nil)
	if _, err := b.RoundTrip(req); err != nil || len(c.auth) != 1 || c.auth[0] != "Bearer lin_oauth_token_1" {
		t.Fatal(err, c.auth)
	}
}
