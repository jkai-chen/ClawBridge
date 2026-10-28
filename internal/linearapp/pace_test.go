package linearapp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearpace"
)

// limitedGQL answers GraphQL reads after the viewer with the next
// status, Retry-After and body, and counts what reached it.
type limitedGQL struct {
	*linear
	answers []limitedAnswer
	reads   int
}

type limitedAnswer struct {
	status     int
	retryAfter string
	body       string
}

func (g *limitedGQL) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != graphqlURL {
		return g.linear.RoundTrip(r)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if strings.Contains(string(body), "viewer") {
		return g.linear.RoundTrip(r)
	}
	g.reads++
	a := limitedAnswer{status: 200, body: `{"data":{"issue":{"id":"x"}}}`}
	if len(g.answers) > 0 {
		a, g.answers = g.answers[0], g.answers[1:]
	}
	h := http.Header{}
	if a.retryAfter != "" {
		h.Set("Retry-After", a.retryAfter)
	}
	return &http.Response{StatusCode: a.status, Body: io.NopCloser(strings.NewReader(a.body)), Header: h}, nil
}

// A read Linear rate-limits is rate_limited with when it may go, not
// unavailable; the app's pacer then holds the next read without sending
// it; and a RATELIMITED error in a 200 is rate_limited too.
func TestGraphQLRateLimitedIsTypedAndPaced(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	g := &limitedGQL{linear: &linear{app: true}, answers: []limitedAnswer{{status: 429, retryAfter: "600", body: `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`}}}
	// A client ID of its own: the pacer is per credential in the process.
	a, _ := New(Config{ClientID: "client-paced-" + t.Name(), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: g}, Now: func() time.Time { return now }})
	var out struct{ Issue struct{ ID string } }
	err := a.GraphQL(context.Background(), `{ issue(id: "x") { id } }`, nil, &out)
	if at, ok := RetryAt(err); !ok || !at.Equal(now.Add(600*time.Second)) || g.reads != 1 {
		t.Fatal("Linear's 429:", err, at, g.reads)
	}
	err = a.GraphQL(context.Background(), `{ issue(id: "x") { id } }`, nil, &out)
	if _, ok := RetryAt(err); !ok || g.reads != 1 {
		t.Fatal("the held read was sent, or not rate_limited:", err, g.reads)
	}
	b := &limitedGQL{linear: &linear{app: true}, answers: []limitedAnswer{{status: 200, body: `{"errors":[{"message":"x","extensions":{"code":"RATELIMITED"}}]}`}}}
	c, _ := New(Config{ClientID: "client-paced-200-" + t.Name(), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: b}, Now: func() time.Time { return now }})
	err = c.GraphQL(context.Background(), `{ issue(id: "x") { id } }`, nil, &out)
	if at, ok := RetryAt(err); !ok || !at.Equal(now.Add(time.Minute)) {
		t.Fatal("RATELIMITED in a 200:", err, at)
	}
	if _, ok := RetryAt(fail("unavailable")); ok {
		t.Fatal("another failure read as rate_limited")
	}
}

// Both the app's API and MCP calls go through the one pacer for its app
// user.
func TestAppCallsShareOnePacer(t *testing.T) {
	a, _ := New(Config{ClientID: "client-shared-" + t.Name(), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}})
	want := linearpace.For("app:client-shared-"+t.Name(), linearpace.OAuthPerHour)
	api, ok1 := a.cfg.HTTP.Transport.(linearpace.Transport)
	mcp, ok2 := a.cfg.MCPTransport.(linearpace.Transport)
	if !ok1 || !ok2 || api.Pacer != want || mcp.Pacer != want {
		t.Fatal("the app's calls are not paced by its one pacer:", ok1, ok2)
	}
}

// The scope probe's mutation is sent exactly once on Linear's 429, while a
// read the app marks a read is sent once more within its bound (review M1).
func TestGraphQLMutationIsNeverResent(t *testing.T) {
	g := &limitedGQL{linear: &linear{app: true}, answers: []limitedAnswer{{status: 429, retryAfter: "1"}, {status: 200, body: `{"data":{}}`}}}
	a, _ := New(Config{ClientID: "client-probe-" + t.Name(), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: g}})
	if _, err := a.ProbeProjectUpdateScope(context.Background()); err != nil || g.reads != 1 {
		t.Fatal("the probe mutation was re-sent:", err, g.reads)
	}
	r := &limitedGQL{linear: &linear{app: true}, answers: []limitedAnswer{{status: 429, retryAfter: "1"}}}
	b, _ := New(Config{ClientID: "client-read-" + t.Name(), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: r}})
	var out struct{ Issue struct{ ID string } }
	if err := b.GraphQL(context.Background(), `{ issue(id: "x") { id } }`, nil, &out); err != nil || r.reads != 2 {
		t.Fatal("a read was not re-sent once:", err, r.reads)
	}
}

// viewerLimited answers the first viewer read 429, then passes to Linear.
type viewerLimited struct {
	*linear
	viewers int
}

func (v *viewerLimited) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() == graphqlURL {
		if v.viewers++; v.viewers == 1 {
			h := http.Header{}
			h.Set("Retry-After", "1")
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{}`)), Header: h}, nil
		}
	}
	return v.linear.RoundTrip(r)
}

// The mint's viewer read is a read: Linear's 429 on it is sent once more.
func TestMintViewerIsARead(t *testing.T) {
	v := &viewerLimited{linear: &linear{app: true}}
	a, _ := New(Config{ClientID: "client-viewer-" + t.Name(), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: v}})
	if _, _, err := a.Token(context.Background()); err != nil || v.viewers != 2 {
		t.Fatal("the mint's viewer read was not sent once more:", err, v.viewers)
	}
}
