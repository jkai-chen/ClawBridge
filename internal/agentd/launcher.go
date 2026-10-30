package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/transporterr"
)

// launcherClient is Server over the ClawBridge launcher endpoints (§11.8),
// with the launcher token: which owners need an agent, a one-time agent
// credential, and a report. Its errors carry the call and a status or a
// fixed reason, never a body, a credential or the token.
type launcherClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

// errLauncherRefused is the server refusing the launcher token.
var errLauncherRefused = errors.New("the server refused the launcher token")

// errNotWanted is the server declining to mint for an owner it no longer
// wants an agent for.
var errNotWanted = errors.New("the server no longer wants an agent for this owner")

var credentialPattern = regexp.MustCompile(`^cba_[A-Za-z0-9_-]{16,256}$`)

const (
	// launcherTimeout is longer than the server's 15 s request deadline, so
	// an answer the server sends, a minted credential included, arrives
	// before the client gives up on it.
	launcherTimeout = 20 * time.Second
	maxLauncherBody = 1 << 20
)

// NewLauncherClient returns the Server for serverURL, which LoadConfig
// validated, with the launcher token.
func NewLauncherClient(serverURL, token string) Server {
	base, _ := url.Parse(strings.TrimSuffix(serverURL, "/") + "/")
	// The token goes straight to the server, never through a proxy named
	// in the environment.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &launcherClient{base: base, token: token, http: &http.Client{
		Transport: transport,
		Timeout:   launcherTimeout,
		// The token goes to the configured server only, never where a
		// redirect points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// call sends one request and decodes a 2xx answer into out. Answers are
// open: unknown fields are ignored.
func (c *launcherClient) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	endpoint := c.base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("ClawBridge-Model-API", "1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	name := method + " " + path
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("launcher %s: %s", name, transportReason(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLauncherBody+1))
	switch {
	case err != nil || len(data) > maxLauncherBody:
		return resp.StatusCode, fmt.Errorf("launcher %s: unreadable answer", name)
	case resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, errLauncherRefused
	case resp.StatusCode == http.StatusTooManyRequests:
		// The server limits refused tokens; the next round tries again.
		return resp.StatusCode, fmt.Errorf("launcher %s: rate limited", name)
	case resp.StatusCode/100 != 2:
		return resp.StatusCode, fmt.Errorf("launcher %s: status %d", name, resp.StatusCode)
	case json.Unmarshal(data, out) != nil:
		return resp.StatusCode, fmt.Errorf("launcher %s: unreadable answer", name)
	}
	return resp.StatusCode, nil
}

// transportReason names why a request got no answer, from a fixed set, so
// a log tells a DNS or TLS problem from a timeout without echoing the raw
// error.
func transportReason(err error) string {
	return transporterr.Classify(err).Reason()
}

func (c *launcherClient) Wanted(ctx context.Context) ([]Wanted, int, error) {
	var out struct {
		Agents *[]struct {
			OwnerRef       string `json:"owner_ref"`
			Provider       string `json:"provider"`
			ConfigRevision int64  `json:"config_revision"`
		} `json:"agents"`
		MaxAgents *int `json:"max_agents"`
	}
	if _, err := c.call(ctx, http.MethodGet, "v1/dispatcher/launcher/wanted", nil, &out); err != nil {
		return nil, 0, err
	}
	if out.MaxAgents == nil {
		return nil, 0, errors.New("launcher GET v1/dispatcher/launcher/wanted: no max_agents")
	}
	// An answer without the list is not an empty wanted set: read as one,
	// it would stop every agent.
	if out.Agents == nil {
		return nil, 0, errors.New("launcher GET v1/dispatcher/launcher/wanted: no agents list")
	}
	wanted := make([]Wanted, 0, len(*out.Agents))
	for _, a := range *out.Agents {
		// Each value is checked by the reconciler.
		wanted = append(wanted, Wanted{Ref: a.OwnerRef, Provider: a.Provider, Revision: a.ConfigRevision})
	}
	return wanted, *out.MaxAgents, nil
}

func (c *launcherClient) Credential(ctx context.Context, ref string) (string, error) {
	var out struct {
		Credential string `json:"credential"`
	}
	status, err := c.call(ctx, http.MethodPost, "v1/dispatcher/launcher/credential", map[string]string{"owner_ref": ref}, &out)
	if status == http.StatusNotFound {
		return "", errNotWanted
	}
	if err != nil {
		return "", err
	}
	if !credentialPattern.MatchString(out.Credential) {
		// The value is never echoed.
		return "", errors.New("launcher POST v1/dispatcher/launcher/credential: not an agent credential")
	}
	return out.Credential, nil
}

func (c *launcherClient) Report(ctx context.Context, ref, code string) error {
	var out struct {
		OK bool `json:"ok"`
	}
	if _, err := c.call(ctx, http.MethodPost, "v1/dispatcher/launcher/report", map[string]string{"owner_ref": ref, "code": code}, &out); err != nil {
		return err
	}
	if !out.OK {
		return errors.New("launcher POST v1/dispatcher/launcher/report: not ok")
	}
	return nil
}
