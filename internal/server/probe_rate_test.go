package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

const rateProbeViewer = "a9000000-0000-4000-8000-0000000000a9"

// rateFakeLinear answers every request with the headers it is given.
type rateFakeLinear struct {
	headers map[string]http.Header
	status  map[string]int
}

func (f rateFakeLinear) RoundTrip(r *http.Request) (*http.Response, error) {
	status := f.status[r.URL.Path]
	if status == 0 {
		status = 200
	}
	h := f.headers[r.URL.Path].Clone()
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(`{"data":{"viewer":{"id":"` + rateProbeViewer + `"}}}`))}, nil
}

// fakeRateApp sends its reads through the probe's wrapped transport, as
// the real app does: one GraphQL POST, and an MCP connect plus one call.
type fakeRateApp struct {
	t          *testing.T
	rt         http.RoundTripper
	graphqlErr error
	teamsErr   error
}

func (a fakeRateApp) send(path string) {
	a.t.Helper()
	r, _ := http.NewRequest(http.MethodPost, "https://example.test"+path, strings.NewReader("{}"))
	res, err := a.rt.RoundTrip(r)
	if err != nil {
		a.t.Fatal(err)
	}
	res.Body.Close()
}
func (a fakeRateApp) GraphQL(context.Context, string, map[string]any, any) error {
	a.send("/graphql")
	return a.graphqlErr
}
func (a fakeRateApp) ReadTeams(context.Context) error {
	a.send("/mcp/readonly")
	a.send("/mcp/readonly")
	return a.teamsErr
}

func withRateApp(t *testing.T, linear rateFakeLinear, graphqlErr, teamsErr error) {
	t.Helper()
	old := newRateProbeApp
	newRateProbeApp = func(_ linearapp.Store, wrap func(http.RoundTripper) http.RoundTripper) (rateProbeApp, error) {
		return fakeRateApp{t: t, rt: wrap(linear), graphqlErr: graphqlErr, teamsErr: teamsErr}, nil
	}
	t.Cleanup(func() { newRateProbeApp = old })
}

// P-Q1: each request's path, status and rate-limit headers, and nothing
// else: no other header, no ID, no body.
func TestRateHeadersPrintsOnlyRateLimitHeaders(t *testing.T) {
	graphql := http.Header{}
	graphql.Set("X-RateLimit-Requests-Limit", "5000")
	graphql.Set("X-RateLimit-Requests-Remaining", "4987")
	graphql.Set("X-RateLimit-Requests-Reset", "1761652800000")
	graphql.Set("X-Complexity", "2")
	graphql.Set("X-Request-Id", "req-"+rateProbeViewer)
	graphql.Set("Set-Cookie", "session=secret")
	graphql.Set("Retry-After", "in a bit <b>")
	graphql.Set("RateLimit-Remaining", "42")
	graphql.Set("RateLimit-Policy", `"default";q=100;w=60`)
	graphql.Set("X-Quota-Foo", "<script>")
	graphql["X-Throttle bad name"] = []string{"1"}
	mcp := http.Header{}
	mcp.Set("X-RateLimit-Endpoint-Name", "list teams; <script>")
	mcp.Set("Retry-After", "Wed, 28 Oct 2026 12:30:00 GMT")
	withRateApp(t, rateFakeLinear{headers: map[string]http.Header{"/graphql": graphql, "/mcp/readonly": mcp}, status: map[string]int{"/mcp/readonly": 429}}, &linearapp.Error{Code: "query_failed"}, &linearapp.Error{Code: "rate_limited"})
	var out bytes.Buffer
	if err := linearAppRateHeaders(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, banned := range []string{rateProbeViewer, "secret", "Set-Cookie", "X-Request-Id", "<script>", "bad name", "<b>"} {
		if strings.Contains(text, banned) {
			t.Fatalf("printed %q: %s", banned, text)
		}
	}
	for _, want := range []string{`"X-Ratelimit-Requests-Remaining":"4987"`, `"X-Complexity":"2"`, `"path":"/graphql"`, `"status":429`, `"Retry-After":"Wed, 28 Oct 2026 12:30:00 GMT"`, `"Ratelimit-Remaining":"42"`, `"Ratelimit-Policy":"\"default\";q=100;w=60"`, `"X-Quota-Foo":"(unprintable)"`, `"Retry-After":"(unprintable)"`, `"X-Ratelimit-Endpoint-Name":"(unprintable)"`, `"result":"rate_limited"`, `"result":"query_failed"`} {
		if !strings.Contains(strings.ReplaceAll(text, "X-RateLimit", "X-Ratelimit"), want) {
			t.Fatalf("missing %s: %s", want, text)
		}
	}
	if n := strings.Count(text, `"path":"/mcp/readonly"`); n != 2 {
		t.Fatal("MCP requests recorded:", n, text)
	}
}

// The probe changes nothing without --confirm, and takes no other mode.
func TestRateHeadersFlags(t *testing.T) {
	err := RunOperator(context.Background(), []string{"linear-app-probe", "--rate-headers"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "prints only Linear's rate-limit headers") || !strings.Contains(err.Error(), "repeat with --confirm") {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--project", "c7000000-0000-4000-8000-0000000000c1"}, {"--p8"}, {"--health-order"}} {
		err = RunOperator(context.Background(), append([]string{"linear-app-probe", "--rate-headers", "--confirm"}, args...), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--rate-headers takes no") {
			t.Fatal(args, err)
		}
	}
}

// rateTokenStore has no live token: the server published none.
type rateTokenStore struct{ failingTokenStore }

func (rateTokenStore) LiveAppToken(context.Context, string, string) (bus.AppToken, bool, error) {
	return bus.AppToken{}, false, nil
}

// AppTokensMinted allows a mint: only NoMint may stop one.
func (rateTokenStore) AppTokensMinted(context.Context, string, time.Time) (int, error) { return 0, nil }

// pathLog answers 200 to everything and keeps each request's path.
type pathLog struct{ paths *[]string }

func (l pathLog) RoundTrip(r *http.Request) (*http.Response, error) {
	*l.paths = append(*l.paths, r.URL.Path)
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

// The probe never mints: with no published token it sends nothing at all,
// and says so for each read.
func TestRateHeadersNeverMints(t *testing.T) {
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID", "probe-client")
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET", "probe-secret")
	t.Setenv("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	paths := []string{}
	old := rateProbeBase
	rateProbeBase = pathLog{&paths}
	t.Cleanup(func() { rateProbeBase = old })
	var out bytes.Buffer
	if err := linearAppRateHeaders(context.Background(), rateTokenStore{}, &out); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatal("the probe sent requests without a published token (a mint?):", paths)
	}
	if strings.Count(out.String(), `"requests":[]`) != 2 || strings.Contains(out.String(), `"result":"ok"`) {
		t.Fatal(out.String())
	}
}
