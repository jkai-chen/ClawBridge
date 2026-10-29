package linearapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/linearpace"
	"clawbridge/internal/linearprovider"
)

// AppConnection is the server's Linear access as the app. It never exposes
// the token: callers get a provider or a read-only query.
type AppConnection interface {
	// Provider opens MCP over the app token; readonly selects /mcp/readonly.
	Provider(ctx context.Context, readonly bool) (*linearprovider.Provider, error)
	// GraphQL runs one read-only query with the app token.
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
	// Identity is the app user and its organisation.
	Identity(ctx context.Context) (Identity, error)
}

var _ AppConnection = (*App)(nil)

// Identity returns the app's identity, minting a token if none is held.
func (a *App) Identity(ctx context.Context) (Identity, error) {
	_, id, err := a.Token(ctx)
	return id, err
}

// bearer attaches the app token to requests for Linear's MCP endpoint only.
// A 401 re-mints the token for the next request; the refused request itself
// is not resent.
type bearer struct {
	app  *App
	base http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "mcp.linear.app" || (r.URL.Path != "/mcp" && r.URL.Path != "/mcp/readonly") {
		return nil, fail("untrusted_destination")
	}
	token, _, err := b.app.Token(r.Context())
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	res, err := b.base.RoundTrip(r)
	if err == nil && res.StatusCode == http.StatusUnauthorized {
		_, _, _ = b.app.Remint(r.Context(), token)
	}
	return res, err
}

// Provider opens an MCP provider as the app.
func (a *App) Provider(ctx context.Context, readonly bool) (*linearprovider.Provider, error) {
	base := a.cfg.MCPTransport
	if base == nil {
		base = http.DefaultTransport
	}
	return linearprovider.New(ctx, &http.Client{Transport: bearer{a, base}}, readonly)
}

// readOnlyQuery admits a GraphQL query document only: no mutation or
// subscription anywhere in it.
var mutationWord = regexp.MustCompile(`(?i)\b(mutation|subscription)\b`)

// GraphQL runs one read-only query as the app. A 401 re-mints the token and
// runs the query once more: a read changes nothing.
func (a *App) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	q := strings.TrimSpace(query)
	if q == "" || len(q) > 8192 || mutationWord.MatchString(q) || !(strings.HasPrefix(q, "{") || strings.HasPrefix(strings.ToLower(q), "query")) {
		return fail("read_only_query_required")
	}
	body, err := json.Marshal(map[string]any{"query": q, "variables": variables})
	if err != nil {
		return fail("invalid")
	}
	// A query is a read: the pacer may hold it longer, and re-send it once
	// on Linear's 429.
	ctx = linearpace.WithKind(ctx, linearpace.Read)
	for attempt := 0; ; attempt++ {
		token, _, err := a.Token(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := a.cfg.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fail("unavailable")
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if _, _, err = a.Remint(ctx, token); err != nil {
				return err
			}
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests {
			return a.rateLimited(res.Header.Get("Retry-After"))
		}
		var envelope struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message    string `json:"message"`
				Extensions struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"extensions"`
			} `json:"errors"`
		}
		parsed := json.Unmarshal(raw, &envelope) == nil
		if res.StatusCode != http.StatusOK {
			// A 400 naming the query itself as invalid is query_invalid;
			// any other status stays unavailable.
			if res.StatusCode == http.StatusBadRequest && parsed {
				for _, e := range envelope.Errors {
					if queryRejected(e.Message, e.Extensions.Code, e.Extensions.Type) {
						return fail("query_invalid")
					}
				}
			}
			return fail("unavailable")
		}
		if !parsed {
			return fail("query_failed")
		}
		for _, e := range envelope.Errors {
			if e.Extensions.Code == "RATELIMITED" {
				return a.rateLimited(res.Header.Get("Retry-After"))
			}
		}
		for _, e := range envelope.Errors {
			if queryRejected(e.Message, e.Extensions.Code, e.Extensions.Type) {
				return fail("query_invalid")
			}
		}
		if len(envelope.Errors) > 0 || len(envelope.Data) == 0 {
			return fail("query_failed")
		}
		if json.Unmarshal(envelope.Data, out) != nil {
			return fail("invalid_response")
		}
		return nil
	}
}

// queryRejected: Linear refused the query document or its variables (a
// filter or argument its schema doesn't take), not the data or the token.
// Only the error's code, type and a fixed message are compared; no Linear
// text is kept.
func queryRejected(message, code, kind string) bool {
	switch {
	case code == "GRAPHQL_VALIDATION_FAILED", code == "BAD_USER_INPUT", code == "GRAPHQL_PARSE_FAILED":
		return true
	case strings.EqualFold(kind, "graphql error"):
		return true
	}
	return message == "Argument Validation Error"
}

// rateLimited is a read Linear, or the pacer, would not take now: nothing
// was sent, and RetryAt says when it may go (a minute on, when Linear named
// no time).
func (a *App) rateLimited(retryAfter string) error {
	wait := time.Minute
	if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n >= 0 && n <= 24*3600 {
		wait = time.Duration(n) * time.Second
	}
	return &Error{Code: "rate_limited", RetryAt: a.cfg.Now().Add(wait)}
}
