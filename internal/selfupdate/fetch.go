package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	connectTimeout = 10 * time.Second
	requestTimeout = 120 * time.Second
	maxRedirects   = 5
	maxManifest    = 1 << 20 // 1 MiB, as install.sh
)

// maxAsset is 512 MiB, as install.sh (a variable so a test can stream past
// a smaller one).
var maxAsset int64 = 512 << 20

// defaultClient is the client for GitHub: 10 s to connect, 120 s a
// request, and the redirect policy.
func defaultClient(hosts []string) *http.Client {
	dialer := &net.Dialer{Timeout: connectTimeout}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: connectTimeout,
		ForceAttemptHTTP2:   true,
	}
	return withPolicy(&http.Client{Transport: transport, Timeout: requestTimeout}, hosts)
}

// withPolicy is a copy of c that follows a redirect only to https on one of
// hosts, at most 5 times. c itself is not changed.
func withPolicy(c *http.Client, hosts []string) *http.Client {
	cp := *c
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("refused: more than %d redirects", maxRedirects)
		}
		return checkURL(req.URL, hosts)
	}
	return &cp
}

// checkURL refuses anything but https to an allowed host.
func checkURL(u *url.URL, hosts []string) error {
	if u.Scheme != "https" {
		return fmt.Errorf("refused %s: only https is allowed", u.Redacted())
	}
	host := u.Hostname()
	for _, h := range hosts {
		if strings.EqualFold(host, h) {
			return nil
		}
	}
	return fmt.Errorf("refused %s: host %q is not a GitHub release host", u.Redacted(), host)
}

func newRequest(ctx context.Context, raw string, hosts []string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL, hosts); err != nil {
		return nil, err
	}
	return req, nil
}

// latestVersion reads the /latest redirect without following it, as
// install.sh: the Location must be BaseURL/tag/vX.Y.Z, canonical.
func latestVersion(ctx context.Context, o Options) (string, error) {
	req, err := newRequest(ctx, o.BaseURL+"/latest", o.AllowedHosts)
	if err != nil {
		return "", fmt.Errorf("latest release discovery failed: %w", err)
	}
	noFollow := *o.Client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest release discovery failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxManifest))
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", errors.New("unexpected latest release location")
	}
	loc, err := resp.Location()
	if err != nil {
		return "", errors.New("unexpected latest release location")
	}
	prefix := o.BaseURL + "/tag/"
	got := loc.String()
	if !strings.HasPrefix(got, prefix) {
		return "", errors.New("unexpected latest release location")
	}
	version := strings.TrimPrefix(got, prefix)
	if !validVersion(version) {
		return "", errors.New("latest release is not a canonical stable version")
	}
	return version, nil
}

// get fetches raw and returns the response of a 2xx answer, after the
// redirects the policy allows. The caller closes the body.
func get(ctx context.Context, o Options, raw string) (*http.Response, error) {
	req, err := newRequest(ctx, raw, o.AllowedHosts)
	if err != nil {
		return nil, err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", req.URL.Redacted(), resp.Status)
	}
	return resp, nil
}

var errTooLarge = errors.New("larger than allowed")

// copyLimited copies at most limit bytes of r to w; more is errTooLarge.
func copyLimited(w io.Writer, r io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(w, io.LimitReader(r, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, errTooLarge
	}
	return n, nil
}
