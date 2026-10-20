package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// TokenSource loads the refresh token from the OS keychain and returns
// short-lived ID tokens. Only the ID token is cached, and only in memory.
type TokenSource struct {
	config      Config
	oauthConfig *oauth2.Config
	credential  Credential
	mu          sync.Mutex
	token       string
	until       time.Time
}

func NewTokenSource(ctx context.Context, config Config) (*TokenSource, error) {
	config, oauthConfig, err := config.normalized()
	if err != nil {
		return nil, err
	}
	credential, err := config.Store.Load(ctx, Key(config.ClientID))
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return nil, errors.New("no ClawBridge OAuth credential is stored; run clawbridge login")
		}
		return nil, err
	}
	if credential.ClientID != config.ClientID {
		return nil, errors.New("the stored OAuth credential belongs to a different client; run login again")
	}
	if config.ExpectedPrincipal != "" && !strings.EqualFold(config.ExpectedPrincipal, credential.Principal) {
		return nil, errors.New("the stored OAuth credential belongs to a different principal; run login again")
	}
	return &TokenSource{config: config, oauthConfig: oauthConfig, credential: credential}, nil
}

func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("OAuth token source is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(time.Minute).Before(s.until) {
		return s.token, nil
	}
	requestContext := context.WithValue(ctx, oauth2.HTTPClient, s.config.HTTPClient)
	token, err := s.oauthConfig.TokenSource(requestContext, &oauth2.Token{RefreshToken: s.credential.RefreshToken}).Token()
	if err != nil {
		return "", errors.New("cannot refresh the ClawBridge OAuth credential; run login again")
	}
	idToken := rawIDToken(token)
	if idToken == "" || len(idToken) > 16384 || strings.ContainsAny(idToken, " \r\n\t") {
		return "", errors.New("the ClawBridge OAuth refresh returned no valid identity token; run login again")
	}
	identity, err := s.config.Verify(ctx, idToken, s.config.ClientID)
	if err != nil || !strings.EqualFold(identity.Principal, s.credential.Principal) || identity.Subject != s.credential.Subject {
		return "", errors.New("the refreshed ClawBridge OAuth identity token is invalid or belongs to a different account; run login again")
	}
	until := token.Expiry
	if until.IsZero() {
		until = time.Now().Add(5 * time.Minute)
	}
	s.token = idToken
	s.until = until
	return idToken, nil
}

func Logout(ctx context.Context, store Store, clientID string) error {
	if strings.TrimSpace(clientID) == "" {
		return errors.New("an explicit OAuth client ID is required")
	}
	if store == nil {
		store = KeyringStore{}
	}
	return store.Delete(ctx, Key(clientID))
}
