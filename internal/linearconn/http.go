package linearconn

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

func officialURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Host == "mcp.linear.app" && u.User == nil && u.Fragment == ""
}

type boundedBody struct {
	io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, problem("response limit exceeded")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, e := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, e
}

type guardedTransport struct{ base http.RoundTripper }

func (t guardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !officialURL(r.URL) {
		return nil, problem("untrusted provider destination")
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	resp, e := t.base.RoundTrip(r)
	if e != nil {
		return nil, problem("provider transport unavailable")
	}
	if resp.Body != nil {
		resp.Body = &boundedBody{ReadCloser: resp.Body, left: 2 << 20}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return nil, problem("provider redirect refused")
	}
	return resp, nil
}
func (m *Manager) baseClient() *http.Client {
	c := http.Client{Timeout: 20 * time.Second}
	if m.config.HTTPClient != nil {
		c = *m.config.HTTPClient
	}
	if c.Timeout <= 0 || c.Timeout > 30*time.Second {
		c.Timeout = 20 * time.Second
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = guardedTransport{base: rt}
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return problem("provider redirect refused") }
	return &c
}

type tokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !officialURL(r.URL) || (r.URL.Path != "/mcp" && r.URL.Path != "/mcp/readonly") {
		return nil, problem("untrusted credential destination")
	}
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(r)
}
func (m *Manager) tokenClient(token string) *http.Client {
	c := m.baseClient()
	c.Transport = tokenTransport{base: c.Transport, token: token}
	return c
}

type profileTransport struct {
	m      *Manager
	id     string
	access Access
}

func (t *profileTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !officialURL(r.URL) || r.URL.String() != Endpoint(t.access) {
		return nil, problem("untrusted credential destination")
	}
	token, e := t.m.accessToken(r.Context(), t.id, t.access)
	if e != nil {
		return nil, e
	}
	c := t.m.tokenClient(token)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return c.Transport.RoundTrip(r)
}
func (m *Manager) accessToken(ctx context.Context, id string, expectedAccess Access) (string, error) {
	if e := ctx.Err(); e != nil {
		return "", e
	}
	d, e := m.dir()
	if e != nil {
		return "", e
	}
	mu := mutex(d)
	if e = lockContext(ctx, mu); e != nil {
		return "", e
	}
	defer mu.Unlock()
	unlock, e := m.diskLock(ctx)
	if e != nil {
		return "", e
	}
	defer unlock()
	if e = ctx.Err(); e != nil {
		return "", e
	}
	p, e := m.profile(id)
	if e != nil {
		return "", e
	}
	if p.Access != expectedAccess {
		return "", problem("profile access changed; reopen connection")
	}
	c, e := m.readCredential(id)
	if e != nil {
		return "", e
	}
	if e = ctx.Err(); e != nil {
		return "", e
	}
	if c.Token.Expiry.IsZero() || c.Token.Expiry.After(m.config.Now().Add(30*time.Second)) {
		return c.Token.AccessToken, nil
	}
	if c.Imported || c.Token.RefreshToken == "" || c.ClientID == "" {
		return "", problem("reauthentication required")
	}
	if !validToken(c.Token.RefreshToken) {
		return "", problem("invalid secure credential")
	}
	conf := oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: c.RedirectURL, Scopes: c.Scopes, Endpoint: oauth2.Endpoint{TokenURL: Origin + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	httpctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	httpctx = context.WithValue(httpctx, oauth2.HTTPClient, m.baseClient())
	// Force the OAuth library's clock-independent refresh decision: Now is an
	// injectable test seam, while expired credentials always refresh here.
	old := c.Token
	old.Expiry = time.Unix(1, 0)
	next, e := conf.TokenSource(httpctx, &old).Token()
	if e != nil {
		return "", problem("reauthentication required")
	}
	if !validToken(next.AccessToken) || (next.TokenType != "" && !strings.EqualFold(next.TokenType, "Bearer")) {
		return "", problem("invalid token response")
	}
	if next.RefreshToken == "" {
		next.RefreshToken = c.Token.RefreshToken
	}
	if !validToken(next.RefreshToken) {
		return "", problem("invalid token response")
	}
	c.Token = *next
	if e = m.writeCredential(id, c); e != nil {
		return "", e
	}
	return next.AccessToken, nil
}
