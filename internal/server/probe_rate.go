package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// P-Q1 (live/LINEAR-QUEUE-DESIGN.md §11): which rate-limit headers Linear
// sends the app, on its GraphQL API and on its MCP server, so the pacer
// knows what it can read. Two reads as the app, never a mint, and only the
// rate-limit headers are printed.

// rateHeaderNames are the only response headers --rate-headers prints:
// Linear's documented rate-limit headers (linear.app/developers/rate-limiting).
var rateHeaderNames = []string{
	"X-RateLimit-Requests-Limit", "X-RateLimit-Requests-Remaining", "X-RateLimit-Requests-Reset",
	"X-Complexity", "X-RateLimit-Complexity-Limit", "X-RateLimit-Complexity-Remaining", "X-RateLimit-Complexity-Reset",
	"X-RateLimit-Endpoint-Requests-Limit", "X-RateLimit-Endpoint-Requests-Remaining", "X-RateLimit-Endpoint-Requests-Reset", "X-RateLimit-Endpoint-Name",
	"Retry-After",
}

// rateHeaderValue admits a header value as Linear documents them: a number,
// or an endpoint's name. Anything else prints as unprintable.
var rateHeaderValue = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// Headers Linear doesn't document are printed too when their name looks
// like a rate limit's, so an MCP server's own scheme doesn't read as
// "none" (review M1): the name in a plain form, and the value only in a
// number or policy shape (`100;w=60`, `"default";q=5`).
var (
	otherRateName  = regexp.MustCompile(`(?i)rate|limit|retry|quota|complex|throttl`)
	plainName      = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	otherRateValue = regexp.MustCompile(`^[A-Za-z0-9_.:;=, "-]{1,64}$`)
)

// printable is a header's value as the probe may print it.
func printable(name, v string) string {
	switch {
	case rateHeaderValue.MatchString(v):
		return v
	case name == "Retry-After":
		// Retry-After may be an HTTP date (L1).
		if _, err := http.ParseTime(v); err == nil {
			return v
		}
	case otherRateValue.MatchString(v) && !isDocumented(name):
		return v
	}
	return "(unprintable)"
}

func isDocumented(name string) bool {
	for _, n := range rateHeaderNames {
		if http.CanonicalHeaderKey(n) == http.CanonicalHeaderKey(name) {
			return true
		}
	}
	return false
}

// rateCall is one request the probe saw go out: its path, method, status
// and the rate-limit headers of the answer.
type rateCall struct {
	Path    string            `json:"path"`
	Method  string            `json:"method"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	// Other are headers Linear doesn't document whose name looks like a
	// rate limit's.
	Other map[string]string `json:"other_headers"`
}

// rateRecorder keeps each answer's rate-limit headers, and nothing else,
// for every transport the probe's app sends through.
type rateRecorder struct {
	mu    sync.Mutex
	calls []rateCall
}

// through is base, recording into r.
func (r *rateRecorder) through(base http.RoundTripper) http.RoundTripper {
	return recordingTransport{r, base}
}

type recordingTransport struct {
	r    *rateRecorder
	base http.RoundTripper
}

func (t recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return res, err
	}
	call := rateCall{Path: req.URL.Path, Method: req.Method, Status: res.StatusCode, Headers: map[string]string{}, Other: map[string]string{}}
	for _, name := range rateHeaderNames {
		if v := res.Header.Get(name); v != "" {
			call.Headers[name] = printable(name, v)
		}
	}
	for name := range res.Header {
		if isDocumented(name) || !otherRateName.MatchString(name) || !plainName.MatchString(name) {
			continue
		}
		call.Other[name] = printable(name, res.Header.Get(name))
	}
	t.r.mu.Lock()
	t.r.calls = append(t.r.calls, call)
	t.r.mu.Unlock()
	return res, nil
}

func (r *rateRecorder) take() []rateCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	if out == nil {
		out = []rateCall{}
	}
	return out
}

// rateProbeApp is what the probe asks of the app.
type rateProbeApp interface {
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
	ReadTeams(ctx context.Context) error
}

type appRateProbe struct{ *linearapp.App }

// ReadTeams connects to the read-only MCP endpoint and lists one team.
func (a appRateProbe) ReadTeams(ctx context.Context) error {
	p, err := a.Provider(ctx, true)
	if err != nil {
		return err
	}
	defer p.Close()
	_, err = p.Teams(ctx, linearprovider.PageOptions{Limit: 1})
	return err
}

// rateProbeBase reaches Linear; tests replace it.
var rateProbeBase http.RoundTripper = http.DefaultTransport

// newRateProbeApp builds the app with its API and MCP requests through
// wrap; tests replace it. Like the other probes, it never mints.
var newRateProbeApp = func(store linearapp.Store, wrap func(http.RoundTripper) http.RoundTripper) (rateProbeApp, error) {
	config, set, err := appConfig(store)
	if err != nil {
		return nil, err
	}
	if !set {
		return nil, errors.New("the Linear app is not configured on this server")
	}
	config.NoMint = true
	config.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: wrap(rateProbeBase)}
	config.MCPTransport = wrap(rateProbeBase)
	app, err := linearapp.New(config)
	if err != nil {
		return nil, err
	}
	return appRateProbe{app}, nil
}

// rateViewerQuery is the GraphQL read: the app user's own ID, which is
// read and dropped.
const rateViewerQuery = `query RateProbe { viewer { id } }`

// linearAppRateHeaders reads once over GraphQL (viewer) and once over the
// read-only MCP endpoint (list_teams, one team), as the app, and prints
// each request's path, status and Linear's rate-limit headers. It prints no
// ID, name, body or token, writes nothing, and never mints. A read that
// fails still prints what its requests answered.
func linearAppRateHeaders(ctx context.Context, store linearapp.Store, stdout io.Writer) error {
	recorder := &rateRecorder{}
	app, err := newRateProbeApp(store, recorder.through)
	if err != nil {
		return err
	}
	out := map[string]any{}
	var viewer struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	graphql := map[string]any{"result": "ok"}
	if err := app.GraphQL(ctx, rateViewerQuery, nil, &viewer); err != nil {
		graphql["result"] = probeCode(err)
	}
	graphql["requests"] = recorder.take()
	out["graphql_viewer"] = graphql
	mcp := map[string]any{"result": "ok"}
	if err := app.ReadTeams(ctx); err != nil {
		mcp["result"] = probeCode(err)
	}
	mcp["requests"] = recorder.take()
	out["mcp_list_teams"] = mcp
	return json.NewEncoder(stdout).Encode(map[string]any{"rate_headers": out})
}

// probeCode is an error's stable class: the app's or provider's code, or
// "failed". Never Linear's text.
func probeCode(err error) string {
	var app *linearapp.Error
	if errors.As(err, &app) {
		return app.Code
	}
	var provider *linearprovider.Error
	if errors.As(err, &provider) {
		return provider.Kind
	}
	return "failed"
}
