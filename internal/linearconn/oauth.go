package linearconn

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

type serverMetadata struct {
	Resource                 string   `json:"-"`
	Issuer                   string   `json:"issuer"`
	AuthorizationEndpoint    string   `json:"authorization_endpoint"`
	TokenEndpoint            string   `json:"token_endpoint"`
	RegistrationEndpoint     string   `json:"registration_endpoint"`
	CodeChallengeMethods     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	GrantTypes               []string `json:"grant_types_supported"`
}

func getJSON(ctx context.Context, c *http.Client, path string, v any) error {
	r, e := http.NewRequestWithContext(ctx, http.MethodGet, Origin+path, nil)
	if e != nil {
		return problem("invalid discovery request")
	}
	r.Header.Set("Accept", "application/json")
	resp, e := c.Do(r)
	if e != nil {
		return problem("OAuth discovery unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return problem("OAuth discovery unavailable")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(b) > 65536 {
		return problem("invalid OAuth metadata")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if dec.Decode(v) != nil || dec.Decode(new(any)) != io.EOF {
		return problem("invalid OAuth metadata")
	}
	return nil
}
func (m *Manager) discovery(ctx context.Context) (serverMetadata, error) {
	client := m.baseClient()
	var resource struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if e := getJSON(ctx, client, "/.well-known/oauth-protected-resource", &resource); e != nil {
		return serverMetadata{}, e
	}
	if (resource.Resource != Origin+"/mcp" && resource.Resource != Origin+"/mcp/readonly" && resource.Resource != Origin) || len(resource.AuthorizationServers) != 1 || (resource.AuthorizationServers[0] != Origin && resource.AuthorizationServers[0] != Origin+"/") {
		return serverMetadata{}, problem("untrusted OAuth resource metadata")
	}
	var meta serverMetadata
	if e := getJSON(ctx, client, "/.well-known/oauth-authorization-server", &meta); e != nil {
		return meta, e
	}
	if (meta.Issuer != Origin && meta.Issuer != Origin+"/") || meta.AuthorizationEndpoint != Origin+"/authorize" || meta.TokenEndpoint != Origin+"/token" || meta.RegistrationEndpoint != Origin+"/register" || !slices.Contains(meta.CodeChallengeMethods, "S256") || !slices.Contains(meta.TokenEndpointAuthMethods, "none") || !slices.Contains(meta.GrantTypes, "authorization_code") || !slices.Contains(meta.GrantTypes, "refresh_token") {
		return meta, problem("unsupported or untrusted OAuth metadata")
	}
	meta.Resource = resource.Resource
	return meta, nil
}

func (m *Manager) Connect(ctx context.Context, o ConnectOptions) (Profile, error) {
	if o.Name == "" || !safeText(o.Name, 128) || !validAccess(o.Access) || (o.ProfileID != "" && !idPattern.MatchString(o.ProfileID)) {
		return Profile{}, problem("invalid connection parameters")
	}
	if (o.FetchCode == nil) != (o.RedirectURL == "") {
		return Profile{}, problem("callback injection requires paired redirect and fetcher")
	}
	if o.FetchCode == nil && o.PresentURL == nil {
		return Profile{}, problem("authorization URL presenter required")
	}
	if m.config.Verify == nil {
		return Profile{}, problem("provider verification unavailable")
	}
	if o.ProfileID != "" {
		if _, e := m.Status(ctx, o.ProfileID); e != nil {
			return Profile{}, e
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	meta, e := m.discovery(ctx)
	if e != nil {
		return Profile{}, e
	}
	var stateBytes [32]byte
	if _, e = rand.Read(stateBytes[:]); e != nil {
		return Profile{}, problem("randomness unavailable")
	}
	state := hex.EncodeToString(stateBytes[:])
	verifier := oauth2.GenerateVerifier()
	redirect, fetch := o.RedirectURL, o.FetchCode
	if fetch == nil {
		var cleanup func()
		redirect, fetch, cleanup, e = loopback(state, o.PresentURL)
		if e != nil {
			return Profile{}, e
		}
		defer cleanup()
	} else {
		u, e := url.Parse(redirect)
		if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Profile{}, problem("callback must use IPv4 loopback")
		}
	}
	scopes := []string{"read"}
	if o.Access == Write {
		scopes = append(scopes, "write")
	}
	registration, e := oauthex.RegisterClient(ctx, meta.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none", GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, ClientName: "ClawBridge", ApplicationType: "native", Scope: strings.Join(scopes, " ")}, m.baseClient())
	if e != nil || registration == nil || registration.ClientID == "" || len(registration.ClientID) > 4096 || registration.TokenEndpointAuthMethod != "none" || !slices.Contains(registration.RedirectURIs, redirect) {
		return Profile{}, problem("OAuth client registration failed")
	}
	conf := oauth2.Config{ClientID: registration.ClientID, RedirectURL: redirect, Scopes: scopes, Endpoint: oauth2.Endpoint{AuthURL: meta.AuthorizationEndpoint, TokenURL: meta.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams}}
	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", meta.Resource))
	result, e := fetch(ctx, &auth.AuthorizationArgs{URL: authURL})
	if e != nil {
		return Profile{}, problem("authorization cancelled or denied")
	}
	if result == nil || result.Code == "" || len(result.Code) > 8192 || subtle.ConstantTimeCompare([]byte(result.State), []byte(state)) != 1 || (result.Iss != "" && result.Iss != meta.Issuer) {
		return Profile{}, problem("invalid authorization callback")
	}
	exchangeCtx := context.WithValue(ctx, oauth2.HTTPClient, m.baseClient())
	token, e := conf.Exchange(exchangeCtx, result.Code, oauth2.VerifierOption(verifier), oauth2.SetAuthURLParam("resource", meta.Resource))
	if e != nil || token == nil || !validToken(token.AccessToken) || (token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer")) {
		return Profile{}, problem("OAuth token exchange failed")
	}
	if token.RefreshToken != "" && !validToken(token.RefreshToken) {
		return Profile{}, problem("invalid refresh credential")
	}
	return m.commit(ctx, o.Name, o.ProfileID, o.Access, credential{Version: 1, Token: *token, ClientID: registration.ClientID, RedirectURL: redirect, Scopes: scopes})
}

func loopback(state string, present func(string) error) (string, auth.AuthorizationCodeFetcher, func(), error) {
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return "", nil, nil, problem("loopback callback unavailable")
	}
	var pathBytes [16]byte
	if _, e = rand.Read(pathBytes[:]); e != nil {
		listener.Close()
		return "", nil, nil, problem("randomness unavailable")
	}
	path := "/callback/" + hex.EncodeToString(pathBytes[:])
	host := listener.Addr().String()
	redirect := "http://" + host + path
	type callback struct {
		result *auth.AuthorizationResult
		err    error
	}
	ch := make(chan callback, 1)
	srv := &http.Server{ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet || r.URL.Path != path || r.Host != host || len(r.URL.RawQuery) > 16384 {
			http.Error(w, "Invalid callback", 400)
			return
		}
		q, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || len(q["code"]) > 1 || len(q["iss"]) > 1 || len(q["error"]) > 1 {
			http.Error(w, "Invalid callback", 400)
			return
		}
		v := callback{result: &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}}
		if q.Get("error") != "" || q.Get("code") == "" {
			v.err = problem("authorization denied")
		}
		select {
		case ch <- v:
		default:
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Authorization received. Return to ClawBridge.")
	})
	go func() { _ = srv.Serve(listener) }()
	cleanup := func() { _ = srv.Close(); _ = listener.Close() }
	fetch := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		stop := context.AfterFunc(ctx, cleanup)
		defer stop()
		if e := present(args.URL); e != nil {
			return nil, problem("authorization presentation failed")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case v := <-ch:
			return v.result, v.err
		}
	}
	return redirect, fetch, cleanup, nil
}
