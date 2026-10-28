package linearkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/linearpace"
)

// Endpoint is Linear's GraphQL API.
const Endpoint = "https://api.linear.app/graphql"

// queries are the only GraphQL texts the key is ever sent with, by name.
// The view texts are the probe's recorded ones (LINEAR-VIEWS-PLAN §10b); a
// create always names its team and is always shared (§14).
var queries = map[string]string{
	"viewer":     `query Viewer { viewer { id } }`,
	"viewCreate": `mutation ViewCreate($input: CustomViewCreateInput!) { customViewCreate(input: $input) { success customView { ` + viewFields + ` } } }`,
	"viewUpdate": `mutation ViewUpdate($id: String!, $input: CustomViewUpdateInput!) { customViewUpdate(id: $id, input: $input) { success customView { ` + viewFields + ` } } }`,
	"view":       `query View($id: String!) { customView(id: $id) { ` + viewFields + ` } }`,
	"viewDelete": `mutation ViewDelete($id: String!) { customViewDelete(id: $id) { success } }`,
	// projectLabels is read only for a project view's write (Q-D (b)), to
	// prove each filter label's team and group.
	"projectLabels": `query ProjectLabels($after: String) { projectLabels(first: 250, after: $after) { nodes { ` + projectLabelFields + ` } pageInfo { hasNextPage endCursor } } }`,
}

// projectLabelFields are from the tester's introspection of ProjectLabel
// (27 Oct, live/keyprobe-ledger.txt): team is null for a workspace label,
// and groupType (singleSelect or multiSelect) is set on a group.
const projectLabelFields = `id name isGroup groupType archivedAt retiredAt parent { id isGroup groupType } team { id }`

const viewFields = `id name shared updatedAt modelName team { id } creator { id name } filterData projectFilterData`

// Client sends the person's key with an allow-listed query.
type Client struct {
	key  string
	http *http.Client
}

// httpClient is the transport; tests replace it.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Errors are fixed strings: nothing Linear answers, and never the key.
var (
	ErrRejected    = errors.New("Linear refused the API key: it is wrong, revoked or expired; create a new key in Linear (Settings, Security & access, Personal API keys), then clawbridge linear key set")
	ErrUnavailable = errors.New("Linear's API could not be reached or answered an error; try again")
	errUnknown     = errors.New("not an allow-listed Linear API query")
	// ErrViewRefused is Linear refusing a view's input, such as its filter.
	ErrViewRefused = errors.New("Linear refused the view (its input error): check the filter and the name")
	// ErrViewNotFound is a view Linear doesn't have, or the key can't see.
	ErrViewNotFound = errors.New("Linear has no such view, or this key can't see it")
	errInput        = errors.New("input error")
)

// RateLimitedError is Linear's rate limit for the key, or the pacer's
// hold on it: nothing was sent.
type RateLimitedError struct{ RetryAfter string }

func (e RateLimitedError) Error() string {
	if e.RetryAfter == "" {
		return "Linear's rate limit for this API key has no room now; nothing was sent; try again shortly"
	}
	return "Linear's rate limit for this API key has no room now; nothing was sent; try again in " + e.RetryAfter + " s"
}

var retryAfterSeconds = regexp.MustCompile(`^[0-9]{1,6}$`)

func rateLimited(v string) error {
	v = strings.TrimSpace(v)
	if !retryAfterSeconds.MatchString(v) {
		v = ""
	}
	return RateLimitedError{RetryAfter: v}
}

// do sends the named allow-listed query and decodes its data into out.
func (c *Client) do(ctx context.Context, name string, variables map[string]any, out any) error {
	if err := guard(); err != nil {
		return err
	}
	text, ok := queries[name]
	if !ok {
		return errUnknown
	}
	body, err := json.Marshal(map[string]any{"query": text, "variables": variables})
	if err != nil {
		return errUnknown
	}
	// GraphQL is paced as a write unless it is a query: a mutation is
	// never re-sent on a 429.
	if strings.HasPrefix(strings.TrimSpace(text), "query") {
		ctx = linearpace.WithKind(ctx, linearpace.Read)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	// A personal API key goes in Authorization as it is, without Bearer.
	req.Header.Set("Authorization", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrRejected
	case resp.StatusCode == http.StatusTooManyRequests:
		return rateLimited(resp.Header.Get("Retry-After"))
	case err != nil || resp.StatusCode != http.StatusOK:
		return ErrUnavailable
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return ErrUnavailable
	}
	if len(envelope.Errors) > 0 {
		switch envelope.Errors[0].Extensions.Code {
		case "AUTHENTICATION_ERROR":
			return ErrRejected
		case "INPUT_ERROR":
			return errInput
		case "RATELIMITED":
			return rateLimited(resp.Header.Get("Retry-After"))
		}
		return ErrUnavailable
	}
	if json.Unmarshal(envelope.Data, out) != nil {
		return ErrUnavailable
	}
	return nil
}

// Viewer is the Linear user the key acts as.
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var out struct {
		Viewer *struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := c.do(ctx, "viewer", map[string]any{}, &out); err != nil {
		return "", viewErr(err, ErrUnavailable)
	}
	if out.Viewer == nil || out.Viewer.ID == "" {
		return "", ErrUnavailable
	}
	return out.Viewer.ID, nil
}
