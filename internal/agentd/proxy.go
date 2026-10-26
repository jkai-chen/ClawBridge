package agentd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Proxy is the validating socket proxy in front of the rootless dockerd
// [AG-M1 layer 2]. It forwards only the operations agentd needs, on
// clawbridge-agent-<ref> containers and cbnet-<ref> networks, and a container
// create only when its body is exactly agentSpec. Everything else gets 403.
type Proxy struct {
	image, serverURL string
	forward          *httputil.ReverseProxy
	log              *slog.Logger
}

const maxBody = 64 << 10

var apiVersion = regexp.MustCompile(`^/v[0-9]+\.[0-9]+(/.*)$`)

// NewProxy returns a Proxy forwarding to the Docker socket at upstream.
func NewProxy(image, serverURL, upstream string, log *slog.Logger) (*Proxy, error) {
	if err := validImage(image); err != nil {
		return nil, err
	}
	if err := validServerURL(serverURL); err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", upstream)
	}}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "http", "docker", "docker"
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Warn("docker unreachable", "error", err.Error())
			http.Error(w, `{"message":"the agents' docker daemon is unreachable"}`, http.StatusBadGateway)
		},
	}
	return &Proxy{image: image, serverURL: serverURL, forward: forward, log: log}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := p.allow(r); err != nil {
		p.log.Warn("refused", "method", r.Method, "path", r.URL.Path, "reason", err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "{\"message\":%s}\n", strconv.Quote("refused by clawbridge-agentd's proxy: "+err.Error()))
		return
	}
	p.forward.ServeHTTP(w, r)
}

// allow checks r and, for a create, replaces its body with the canonical one.
func (p *Proxy) allow(r *http.Request) error {
	path := r.URL.Path
	if m := apiVersion.FindStringSubmatch(path); m != nil {
		path = m[1]
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// The daemon must see the path the check saw: no escapes, no dot or
	// empty segments.
	if r.URL.RawPath != "" || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("the path is not plain")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("the path is not plain")
		}
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("the query is not plain")
	}
	op := r.Method + " " + parts[0]
	switch {
	case op == "GET containers" && len(parts) == 2 && parts[1] == "json":
		return onlyQuery(query, "all", "filters", "limit")
	case op == "POST containers" && len(parts) == 2 && parts[1] == "create":
		if err := onlyQuery(query, "name"); err != nil {
			return err
		}
		ref, ok := refFromName(query.Get("name"), containerPrefix)
		if !ok {
			return fmt.Errorf("container name is not clawbridge-agent-<owner ref>")
		}
		return p.replaceBody(r, func(body []byte) ([]byte, error) {
			return checkCreate(body, agentSpec(p.image, p.serverURL, ref))
		})
	case op == "POST networks" && len(parts) == 2 && parts[1] == "create":
		if err := onlyQuery(query); err != nil {
			return err
		}
		return p.replaceBody(r, func(body []byte) ([]byte, error) {
			out, _, err := checkNetwork(body)
			return out, err
		})
	case op == "DELETE networks" && len(parts) == 2:
		if _, ok := refFromName(parts[1], networkPrefix); !ok {
			return fmt.Errorf("network name is not cbnet-<owner ref>")
		}
		return emptyRequest(r, query)
	case parts[0] == "containers" && len(parts) >= 2:
		if _, ok := refFromName(parts[1], containerPrefix); !ok {
			return fmt.Errorf("container name is not clawbridge-agent-<owner ref>")
		}
		action := r.Method
		if len(parts) == 3 {
			action += " " + parts[2]
		} else if len(parts) > 3 {
			return fmt.Errorf("endpoint not allowed")
		}
		switch action {
		case "POST start":
			return emptyRequest(r, query)
		case "POST stop":
			return emptyRequest(r, query, "t")
		case "DELETE":
			return emptyRequest(r, query, "force", "v")
		case "GET json":
			return emptyRequest(r, query)
		case "POST attach":
			// stdin only, to hand over the one-time credential: never the
			// container's output or logs.
			if query.Get("stream") != "1" || query.Get("stdin") != "1" {
				return fmt.Errorf("attach must be stream=1&stdin=1")
			}
			return emptyRequest(r, query, "stream", "stdin")
		}
	}
	return fmt.Errorf("endpoint not allowed")
}

func (p *Proxy) replaceBody(r *http.Request, check func([]byte) ([]byte, error)) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("the body could not be read")
	}
	if len(body) > maxBody {
		return fmt.Errorf("the body is too large")
	}
	out, err := check(body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	r.Header.Set("Content-Type", "application/json")
	return nil
}

// onlyQuery refuses any query parameter outside allowed.
func onlyQuery(query url.Values, allowed ...string) error {
	for key, values := range query {
		if len(values) != 1 {
			return fmt.Errorf("query parameter %s is repeated", strconv.Quote(key))
		}
		found := false
		for _, a := range allowed {
			found = found || key == a
		}
		if !found {
			return fmt.Errorf("query parameter %s is not allowed", strconv.Quote(key))
		}
	}
	return nil
}

// emptyRequest refuses a body (old API versions took a HostConfig on start)
// and any query parameter outside allowed.
func emptyRequest(r *http.Request, query url.Values, allowed ...string) error {
	if r.ContentLength > 0 {
		return fmt.Errorf("a body is not allowed here")
	}
	if r.ContentLength < 0 {
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, 1))
		if n > 0 {
			return fmt.Errorf("a body is not allowed here")
		}
	}
	return onlyQuery(query, allowed...)
}
