package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxOAuthDiscoveryResponse = 16 << 10

// OAuthDiscovery is the installed-app metadata returned by GET /v1/config.
// Google treats installed-app client credentials as distributed application
// identifiers rather than confidential server secrets.
type OAuthDiscovery struct {
	BrowserClientID     string
	BrowserClientSecret string
	DeviceClientID      string
	DeviceClientSecret  string
	// AuthMethods are the sign-in methods the server accepts (google,
	// device); nil from a server older than device sign-in.
	AuthMethods []string
	// PublicOrigin is the origin device signatures cover; empty when the
	// server has none configured.
	PublicOrigin string
}

// DiscoverOAuthClient retrieves the public OAuth client metadata from the
// explicitly trusted cloud server. The server URL is revalidated here so this
// function cannot be repurposed as a general-purpose HTTP client.
func DiscoverOAuthClient(ctx context.Context, serverURL string, httpClient *http.Client) (OAuthDiscovery, error) {
	base, err := validateDiscoveryServerURL(serverURL)
	if err != nil {
		return OAuthDiscovery{}, err
	}
	client := discoveryHTTPClient(httpClient)
	endpoint := base + "/v1/config"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return OAuthDiscovery{}, errors.New("cannot construct OAuth discovery request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return OAuthDiscovery{}, ctx.Err()
		}
		if isTransportFailure(err) {
			return OAuthDiscovery{}, errUnreachable
		}
		return OAuthDiscovery{}, errors.New("cannot reach trusted server OAuth discovery; check the configured server and network")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery returned a non-success status")
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery did not return JSON")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthDiscoveryResponse+1))
	if err != nil {
		return OAuthDiscovery{}, errors.New("cannot read trusted server OAuth discovery response")
	}
	if len(raw) > maxOAuthDiscoveryResponse {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response exceeds the safety limit")
	}
	discovery, err := parseOAuthDiscovery(raw)
	if err != nil {
		return OAuthDiscovery{}, err
	}
	return discovery, nil
}

func discoveryHTTPClient(provided *http.Client) *http.Client {
	client := &http.Client{}
	if provided != nil {
		*client = *provided
	}
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func validateDiscoveryServerURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("trusted server URL is invalid for OAuth discovery")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return "", errors.New("OAuth discovery requires HTTPS; HTTP is permitted only for numeric loopback")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func parseOAuthDiscovery(raw []byte) (OAuthDiscovery, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response is not a JSON object")
	}
	seen := make(map[string]struct{}, 6)
	var browserID, browserSecret, deviceID, deviceSecret, publicOrigin string
	var methods []string
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response contains an invalid field name")
		}
		key, ok := keyToken.(string)
		if !ok {
			return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response contains an invalid field name")
		}
		if _, duplicate := seen[key]; duplicate {
			return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response contains a duplicate field")
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response contains an invalid field value")
		}
		switch key {
		case "browser_client_id":
			browserID, ok = strictJSONString(value)
			if !ok {
				return OAuthDiscovery{}, errors.New("trusted server OAuth discovery browser_client_id is missing or invalid")
			}
		case "browser_client_secret":
			browserSecret, ok = strictJSONString(value)
			if !ok {
				return OAuthDiscovery{}, errors.New("trusted server OAuth discovery browser_client_secret is missing or invalid")
			}
		case "device_client_id":
			deviceID, ok = strictJSONString(value)
			if !ok {
				return OAuthDiscovery{}, errors.New("trusted server OAuth discovery device_client_id is missing or invalid")
			}
		case "device_client_secret":
			deviceSecret, ok = strictJSONString(value)
			if !ok {
				return OAuthDiscovery{}, errors.New("trusted server OAuth discovery device_client_secret is missing or invalid")
			}
		case "auth_methods":
			methods, ok = strictJSONStrings(value)
			if !ok {
				return OAuthDiscovery{}, errors.New("trusted server OAuth discovery auth_methods is invalid")
			}
		case "public_origin":
			publicOrigin, ok = strictJSONString(value)
			if !ok {
				return OAuthDiscovery{}, errors.New("trusted server OAuth discovery public_origin is invalid")
			}
		default:
			return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response contains an unknown field")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response is not a complete JSON object")
	}
	if trailing, err := decoder.Token(); err != io.EOF {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response has trailing content")
	} else if trailing != nil {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery response has trailing content")
	}
	if _, ok := seen["browser_client_id"]; !ok {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery browser_client_id is missing or invalid")
	}
	if _, ok := seen["browser_client_secret"]; !ok {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery browser_client_secret is missing or invalid")
	}
	if _, ok := seen["device_client_id"]; !ok {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery device_client_id is missing or invalid")
	}
	if _, ok := seen["device_client_secret"]; !ok {
		return OAuthDiscovery{}, errors.New("trusted server OAuth discovery device_client_secret is missing or invalid")
	}
	return OAuthDiscovery{
		BrowserClientID:     browserID,
		BrowserClientSecret: browserSecret,
		DeviceClientID:      deviceID,
		DeviceClientSecret:  deviceSecret,
		AuthMethods:         methods,
		PublicOrigin:        publicOrigin,
	}, nil
}

// strictJSONStrings reads an array of at most 8 short strings.
func strictJSONStrings(raw json.RawMessage) ([]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var items []json.RawMessage
	if json.Unmarshal(trimmed, &items) != nil || len(items) > 8 {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		v, ok := strictJSONString(item)
		if !ok || v == "" || len(v) > 32 {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

func strictJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	trimmed := bytes.TrimSpace(raw)
	return value, len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"'
}

// errUnreachable marks a discovery request that never reached the server (DNS,
// connection refused, timeout), as opposed to a TLS or answer problem.
var errUnreachable = errors.New("cannot reach trusted server OAuth discovery; check the configured server and network")

func isTransportFailure(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
