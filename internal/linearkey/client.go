package linearkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Endpoint is Linear's GraphQL API.
const Endpoint = "https://api.linear.app/graphql"

// queries are the only GraphQL texts the key is ever sent with, by name.
var queries = map[string]string{
	"viewer": `query Viewer { viewer { id } }`,
}

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
)

// Do sends the named allow-listed query and decodes its data into out.
func (c *Client) Do(ctx context.Context, name string, variables map[string]any, out any) error {
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
		if envelope.Errors[0].Extensions.Code == "AUTHENTICATION_ERROR" {
			return ErrRejected
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
	if err := c.Do(ctx, "viewer", map[string]any{}, &out); err != nil {
		return "", err
	}
	if out.Viewer == nil || out.Viewer.ID == "" {
		return "", ErrUnavailable
	}
	return out.Viewer.ID, nil
}
