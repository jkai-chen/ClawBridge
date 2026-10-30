// Package linearapp holds the hosted server's Linear app identity: it mints
// a client-credentials token, seals it at rest and shares it across server
// instances. The token never leaves the server; callers get it only to
// attach to the server's own Linear calls.
package linearapp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearpace"
)

// Scopes is the one scope set every mint requests. Minting another set
// revokes every token of the app, so it changes only in a release that
// says so. Linear takes it comma-separated and answers space-separated.
// write (v0.3.4) lets the app post project status updates and create
// issue relations; Linear drops those quietly without it. It is the one
// optional scope: a token Linear grants without it is used for the rest.
var Scopes = []string{"read", "write", "comments:create", "issues:create", "app:assignable", "app:mentionable"}

// releasedScopes is Scopes as this release ships it. A mint checks Scopes
// against it first: a token minted with any other set revokes every token
// the app holds, production's included. It catches only a change to Scopes
// at run time; a release that edits both changes the scopes on purpose.
const releasedScopes = "read,write,comments:create,issues:create,app:assignable,app:mentionable"

// optionalScope may be missing from a granted token. A token granted
// without it is used for everything else, and asked again for it once
// writeRetry has passed since it was minted, so a refusal costs about four
// mints a month of mintLimit's 50.
const (
	optionalScope = "write"
	writeRetry    = 7 * 24 * time.Hour
)

const (
	tokenURL   = "https://api.linear.app/oauth/token"
	graphqlURL = "https://api.linear.app/graphql"
	// mintLimit bounds the app's tokens minted in the last 30 days; the
	// normal count is one. Linear allows 1000.
	mintLimit = 50
	// mintPace is the shortest gap between two mints by one server.
	mintPace = time.Minute
	// refreshMargin re-mints a token this long before it expires.
	refreshMargin = 5 * 24 * time.Hour
	// failureBackoff stops minting for a while after failuresBeforeBackoff
	// mints in a row that Linear issued but a later step lost.
	failureBackoff        = time.Hour
	failuresBeforeBackoff = 2
)

// Error is a failure with a stable code and no Linear text or secret.
// RetryAt is set for rate_limited: when Linear, or this process's pacer,
// will take the call.
type Error struct {
	Code    string
	RetryAt time.Time
	// NotFound marks a query_failed whose every error is Linear's
	// entity-not-found: the entity is missing or hidden from the app. No
	// Linear text is kept.
	NotFound bool
}

// IsNotFound reports a query Linear answered with entity-not-found only.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "query_failed" && e.NotFound
}

func (e *Error) Error() string { return "Linear app: " + e.Code }

// RetryAt is when a rate-limited app call may go again; false for any other
// error.
func RetryAt(err error) (time.Time, bool) {
	var e *Error
	if errors.As(err, &e) && e.Code == "rate_limited" {
		return e.RetryAt, true
	}
	return time.Time{}, false
}

// IsQueryInvalid reports a query Linear refused as invalid (query_invalid):
// a caller may retry with a simpler query shape.
func IsQueryInvalid(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "query_invalid"
}

func fail(code string) error { return &Error{Code: code} }

// Store keeps the sealed token; *bus.Store implements it on a hosted server.
type Store interface {
	RecordMintedAppToken(ctx context.Context, app, sha string, minted, expires time.Time) error
	LiveAppToken(ctx context.Context, app, org string) (bus.AppToken, bool, error)
	PublishAppToken(ctx context.Context, t bus.AppToken, replaces string) (bus.AppToken, bool, error)
	AppTokensMinted(ctx context.Context, app string, since time.Time) (int, error)
}

// Config configures the app identity. Secret and Key come from the
// environment only, never a flag, file or the database.
type Config struct {
	ClientID string
	Secret   string
	// Key is the 32-byte AES key that seals tokens at rest.
	Key   []byte
	Store Store
	// HTTP reaches api.linear.app; tests replace it.
	HTTP *http.Client
	// MCPTransport carries MCP requests; tests replace it.
	MCPTransport http.RoundTripper
	Now          func() time.Time
	// NoMint uses only the live token the server published: a missing,
	// expiring or refused one is an error, never a new mint.
	NoMint bool
}

// Identity is what the server learns about the app at mint time: the app
// user that authors its comments and the Linear organisation it is in.
type Identity struct {
	AppUserID   string `json:"app_user_id"`
	LinearOrgID string `json:"linear_org_id"`
}

// App is the server's app identity. It is safe for concurrent use.
type App struct {
	cfg      Config
	aead     cipher.AEAD
	mu       sync.Mutex
	token    string
	sha      string
	identity Identity
	minted   time.Time
	expires  time.Time
	// granted is the in-use token's scope set: Scopes, or Scopes without
	// optionalScope.
	granted  []string
	lastMint time.Time
	// lost counts consecutive mints lost after Linear issued them;
	// backoffUntil stops minting after too many.
	lost         int
	backoffUntil time.Time
	// sessions keeps write sessions between writes (WriteSessions).
	sessionsOnce sync.Once
	sessions     *Sessions
}

// New checks the configuration. A missing ID, secret or key leaves the app
// identity off (nil, nil): the caller then answers app_unavailable.
func New(c Config) (*App, error) {
	if c.ClientID == "" && c.Secret == "" && len(c.Key) == 0 {
		return nil, nil
	}
	if c.ClientID == "" || c.Secret == "" || len(c.Key) != 32 || c.Store == nil {
		return nil, errors.New("the Linear app needs a client ID, a secret and a 32-byte token key")
	}
	block, err := aes.NewCipher(c.Key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	// Every API and MCP call as the app goes through one pacer for the app
	// user, shared by every App of this client ID in the process.
	pacer := linearpace.For("app:"+c.ClientID, linearpace.OAuthPerHour)
	paced := *c.HTTP
	paced.Transport = linearpace.Transport{Base: c.HTTP.Transport, Pacer: pacer}
	c.HTTP = &paced
	c.MCPTransport = linearpace.Transport{Base: c.MCPTransport, Pacer: pacer}
	return &App{cfg: c, aead: aead}, nil
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func scopesDigest() string { return scopesDigestOf(Scopes) }

func scopesDigestOf(scopes []string) string { return digest(strings.Join(scopes, " ")) }

// withoutOptional is Scopes without optionalScope, in Scopes' order.
func withoutOptional() []string {
	out := []string{}
	for _, s := range Scopes {
		if s != optionalScope {
			out = append(out, s)
		}
	}
	return out
}

// deniedDigest is the digest of a token asked for Scopes and granted them
// without optionalScope. It differs from the digest of that smaller set,
// which is what an older release minted on purpose, so such a token is
// asked again at once.
func deniedDigest() string { return scopesDigestOf(append(withoutOptional(), "-"+optionalScope)) }

// grantDigest is the digest a token of the granted set is published under.
func grantDigest(granted []string) string {
	switch {
	case granted == nil:
		return ""
	case len(granted) == len(Scopes):
		return scopesDigest()
	}
	return deniedDigest()
}

// grantFor is the scope set a published digest names: Scopes, Scopes
// without optionalScope, or nil for any other.
func grantFor(scopesDigest string) []string {
	switch scopesDigest {
	case grantDigest(Scopes):
		return Scopes
	case deniedDigest():
		return withoutOptional()
	}
	return nil
}

// usable reports whether a token of this digest and mint time is used
// rather than replaced: the full set always, a denied optionalScope until
// writeRetry has passed.
func usable(scopesDigest string, minted, now time.Time) bool {
	return scopesDigest == grantDigest(Scopes) || scopesDigest == deniedDigest() && now.Sub(minted) < writeRetry
}

// HasScope reports whether the token in use was granted scope, getting a
// token first if none is held.
func (a *App) HasScope(ctx context.Context, scope string) (bool, error) {
	if _, _, err := a.Token(ctx); err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.granted, scope), nil
}

// aad binds a sealed token to its row: a ciphertext moved to another row
// does not open.
func aad(t bus.AppToken) []byte {
	return []byte(strings.Join([]string{t.AppClientID, t.LinearOrgID, t.ScopesDigest, t.MintedAt.UTC().Format(time.RFC3339Nano)}, "\x00"))
}

func (a *App) seal(token string, t *bus.AppToken) error {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	t.Nonce = base64.StdEncoding.EncodeToString(nonce)
	t.Ciphertext = base64.StdEncoding.EncodeToString(a.aead.Seal(nil, nonce, []byte(token), aad(*t)))
	return nil
}

func (a *App) open(t bus.AppToken) (string, error) {
	nonce, err := base64.StdEncoding.DecodeString(t.Nonce)
	if err != nil {
		return "", fail("token_unreadable")
	}
	sealed, err := base64.StdEncoding.DecodeString(t.Ciphertext)
	if err != nil {
		return "", fail("token_unreadable")
	}
	plain, err := a.aead.Open(nil, nonce, sealed, aad(t))
	if err != nil || digest(string(plain)) != t.TokenSHA256 {
		return "", fail("token_unreadable")
	}
	return string(plain), nil
}

// Token returns the app token and identity for the server's own Linear
// calls: the cached one, the live one another instance published, or a new
// mint. It never returns a token within refreshMargin of expiry.
func (a *App) Token(ctx context.Context) (string, Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.cfg.Now()
	if a.token != "" && now.Add(refreshMargin).Before(a.expires) && usable(grantDigest(a.granted), a.minted, now) {
		return a.token, a.identity, nil
	}
	// Another instance may have published a token already; a new instance
	// learns the organisation from it.
	live, found, err := a.cfg.Store.LiveAppToken(ctx, a.cfg.ClientID, a.identity.LinearOrgID)
	if err != nil {
		return "", Identity{}, err
	}
	if found && now.Add(refreshMargin).Before(live.ExpiresAt) && usable(live.ScopesDigest, live.MintedAt, now) {
		if err = a.adopt(ctx, live); err == nil {
			return a.token, a.identity, nil
		}
	}
	replaces := a.sha
	if found {
		replaces = live.TokenSHA256
	}
	if err := a.mint(ctx, replaces); err != nil {
		// When the only reason to mint was asking for optionalScope again,
		// the token without it stays in use until a mint succeeds.
		if found && live.ScopesDigest == deniedDigest() && now.Add(refreshMargin).Before(live.ExpiresAt) && a.adopt(ctx, live) == nil {
			return a.token, a.identity, nil
		}
		return "", Identity{}, err
	}
	return a.token, a.identity, nil
}

// Remint replaces a token Linear refused (401). Only the token that failed
// is replaced: when another instance already published a newer one, that
// one is used, so a rotation never hands back the dead token.
func (a *App) Remint(ctx context.Context, failed string) (string, Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	dead := digest(failed)
	live, found, err := a.cfg.Store.LiveAppToken(ctx, a.cfg.ClientID, a.identity.LinearOrgID)
	if err != nil {
		return "", Identity{}, err
	}
	if found && live.TokenSHA256 != dead {
		if err = a.adopt(ctx, live); err == nil {
			return a.token, a.identity, nil
		}
	}
	if err := a.mint(ctx, dead); err != nil {
		return "", Identity{}, err
	}
	return a.token, a.identity, nil
}

// adopt uses a published token. Its identity was read when it was minted:
// the organisation is the row's, and the app user is read again only when
// this instance has not seen it.
func (a *App) adopt(ctx context.Context, t bus.AppToken) error {
	token, err := a.open(t)
	if err != nil {
		return err
	}
	if a.identity.AppUserID == "" || a.identity.LinearOrgID != t.LinearOrgID {
		id, err := a.viewer(ctx, token)
		if err != nil {
			return err
		}
		if id.LinearOrgID != t.LinearOrgID {
			return fail("organization_changed")
		}
		a.identity = id
	}
	a.token, a.sha, a.minted, a.expires, a.granted = token, t.TokenSHA256, t.MintedAt, t.ExpiresAt, grantFor(t.ScopesDigest)
	return nil
}

// mint asks Linear for a new token, outside any lock, reads the app's
// identity with it, and publishes it in place of replaces. If another
// instance published first, its token is used instead.
func (a *App) mint(ctx context.Context, replaces string) error {
	if a.cfg.NoMint {
		return fail("mint_disabled")
	}
	if strings.Join(Scopes, ",") != releasedScopes {
		return fail("scopes_changed")
	}
	now := a.cfg.Now()
	if !a.lastMint.IsZero() && now.Sub(a.lastMint) < mintPace {
		return fail("mint_paced")
	}
	if now.Before(a.backoffUntil) {
		return fail("mint_backoff")
	}
	n, err := a.cfg.Store.AppTokensMinted(ctx, a.cfg.ClientID, now.Add(-30*24*time.Hour))
	if err != nil {
		return err
	}
	if n >= mintLimit {
		return fail("app_token_limit")
	}
	a.lastMint = now
	token, expires, granted, err := a.request(ctx)
	if err != nil {
		return err
	}
	// Count the token before anything else can fail; if it cannot be
	// counted, it is not used (fail closed).
	if err = a.cfg.Store.RecordMintedAppToken(ctx, a.cfg.ClientID, digest(token), now.UTC(), expires.UTC()); err != nil {
		a.lostMint(now)
		return err
	}
	if granted == nil {
		// Linear issued a token of another scope set: counted above, and
		// lost like any other.
		a.lostMint(now)
		return fail("scopes_mismatch")
	}
	err = a.publish(ctx, token, expires, now, replaces, granted)
	if err != nil {
		a.lostMint(now)
		return err
	}
	a.lost = 0
	return nil
}

// lostMint notes a token Linear issued that this server could not use.
func (a *App) lostMint(now time.Time) {
	a.lost++
	if a.lost >= failuresBeforeBackoff {
		a.backoffUntil, a.lost = now.Add(failureBackoff), 0
	}
}

// publish reads a counted token's identity, seals it and publishes it.
func (a *App) publish(ctx context.Context, token string, expires, now time.Time, replaces string, granted []string) error {
	id, err := a.viewer(ctx, token)
	if err != nil {
		return err
	}
	if a.identity.LinearOrgID != "" && id.LinearOrgID != a.identity.LinearOrgID {
		return fail("organization_changed")
	}
	row := bus.AppToken{AppClientID: a.cfg.ClientID, LinearOrgID: id.LinearOrgID, TokenSHA256: digest(token), ScopesDigest: grantDigest(granted), MintedAt: now.UTC(), ExpiresAt: expires.UTC()}
	if err = a.seal(token, &row); err != nil {
		return err
	}
	current, published, err := a.cfg.Store.PublishAppToken(ctx, row, replaces)
	if err != nil {
		return err
	}
	a.identity = id
	if published {
		a.token, a.sha, a.minted, a.expires, a.granted = token, row.TokenSHA256, row.MintedAt, row.ExpiresAt, granted
		return nil
	}
	return a.adopt(ctx, current)
}

// request mints a client-credentials token: HTTP Basic client auth and the
// fixed scope set. granted is the answered set when it is Scopes or Scopes
// without optionalScope, and nil for any other: Linear issued that token
// all the same.
func (a *App) request(ctx context.Context) (string, time.Time, []string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {strings.Join(Scopes, ",")}}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", time.Time{}, nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(a.cfg.ClientID, a.cfg.Secret)
		res, err := a.cfg.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", time.Time{}, nil, ctx.Err()
			}
			return "", time.Time{}, nil, fail("unavailable")
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		switch {
		case res.StatusCode >= 500 && attempt == 0:
			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return "", time.Time{}, nil, ctx.Err()
			}
		case res.StatusCode >= 500:
			return "", time.Time{}, nil, fail("unavailable")
		case res.StatusCode != http.StatusOK:
			return "", time.Time{}, nil, fail("app_unavailable")
		}
		if json.NewDecoder(bytes.NewReader(body)).Decode(&out) != nil || out.AccessToken == "" || !strings.EqualFold(out.TokenType, "Bearer") || out.ExpiresIn <= 0 {
			return "", time.Time{}, nil, fail("invalid_response")
		}
		got := strings.Fields(out.Scope)
		sort.Strings(got)
		var granted []string
		for _, set := range [][]string{Scopes, withoutOptional()} {
			want := slices.Clone(set)
			sort.Strings(want)
			if slices.Equal(got, want) {
				granted = set
				break
			}
		}
		return out.AccessToken, a.cfg.Now().Add(time.Duration(out.ExpiresIn) * time.Second), granted, nil
	}
}

// viewer reads the app user and its organisation with the new token.
func (a *App) viewer(ctx context.Context, token string) (Identity, error) {
	payload, _ := json.Marshal(map[string]string{"query": "{ viewer { id app organization { id } } }"})
	req, err := http.NewRequestWithContext(linearpace.WithKind(ctx, linearpace.Read), http.MethodPost, graphqlURL, bytes.NewReader(payload))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := a.cfg.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Identity{}, ctx.Err()
		}
		return Identity{}, fail("unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return Identity{}, fail("token_rejected")
	}
	if res.StatusCode != http.StatusOK {
		return Identity{}, fail("unavailable")
	}
	var out struct {
		Data struct {
			Viewer struct {
				ID           string `json:"id"`
				App          bool   `json:"app"`
				Organization struct {
					ID string `json:"id"`
				} `json:"organization"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out) != nil {
		return Identity{}, fail("invalid_response")
	}
	v := out.Data.Viewer
	if !validUUID(v.ID) || !validUUID(v.Organization.ID) || !v.App {
		return Identity{}, fail("not_an_app")
	}
	return Identity{AppUserID: strings.ToLower(v.ID), LinearOrgID: strings.ToLower(v.Organization.ID)}, nil
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range strings.ToLower(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
