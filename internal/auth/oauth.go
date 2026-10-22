// Package auth implements ClawBridge's browser and device OAuth login flows and
// its OS-keychain credential store.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

const (
	DefaultKeyringService = "clawbridge-oauth"
	credentialVersion     = 1
)

var ErrCredentialNotFound = errors.New("ClawBridge OAuth credential not found")

// Endpoints are injectable so callback, exchange, polling and failure behavior
// can be tested without contacting Google.
type Endpoints struct {
	AuthorizationURL string
	TokenURL         string
	DeviceAuthURL    string
}

var GoogleEndpoints = Endpoints{
	AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:         "https://oauth2.googleapis.com/token",
	DeviceAuthURL:    "https://oauth2.googleapis.com/device/code",
}

// Credential contains only durable login material and safe display metadata.
// Access and ID tokens stay in memory.
type Credential struct {
	Version      int    `json:"version"`
	ClientID     string `json:"client_id"`
	Principal    string `json:"principal"`
	Subject      string `json:"subject"`
	RefreshToken string `json:"refresh_token"`
}

type Store interface {
	Load(context.Context, string) (Credential, error)
	Save(context.Context, string, Credential) error
	Delete(context.Context, string) error
}

// CredentialTransaction lets callers update trusted routing and credential
// storage together. Commit removes the temporary prior credential only after the
// trusted configuration write succeeds; Rollback restores the exact prior entry,
// or deletes the newly-created entry when none existed.
type CredentialTransaction struct {
	store    Store
	key      string
	previous Credential
	existed  bool
}

// StageCredential saves a newly acquired credential while retaining any prior
// value for restoration.
func StageCredential(ctx context.Context, store Store, clientID string, credential Credential) (*CredentialTransaction, error) {
	if store == nil {
		store = KeyringStore{}
	}
	if strings.TrimSpace(clientID) == "" {
		return nil, errors.New("an explicit OAuth client ID is required")
	}
	key := Key(clientID)
	previous, err := store.Load(ctx, key)
	existed := true
	if errors.Is(err, ErrCredentialNotFound) {
		existed, err = false, nil
	}
	if err != nil {
		return nil, errors.New("cannot inspect the prior ClawBridge OAuth credential; no credential was changed")
	}
	if err = store.Save(ctx, key, credential); err != nil {
		return nil, err
	}
	return &CredentialTransaction{store: store, key: key, previous: previous, existed: existed}, nil
}

// Commit finalizes a staged credential after trusted routing has been written.
func (t *CredentialTransaction) Commit() {
	if t == nil {
		return
	}
	t.store = nil
}

// Rollback restores the credential state from immediately before staging.
func (t *CredentialTransaction) Rollback(ctx context.Context) error {
	if t == nil || t.store == nil {
		return nil
	}
	var err error
	if t.existed {
		err = t.store.Save(ctx, t.key, t.previous)
	} else {
		err = t.store.Delete(ctx, t.key)
	}
	if err == nil {
		t.store = nil
	}
	return err
}

// KeyringStore uses macOS Keychain, Windows Credential Manager or Linux Secret
// Service through the operating system's native credential API.
type KeyringStore struct {
	Service string
}

func (s KeyringStore) service() string {
	if strings.TrimSpace(s.Service) == "" {
		return DefaultKeyringService
	}
	return s.Service
}

func (s KeyringStore) Load(ctx context.Context, key string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	raw, err := keyring.Get(s.service(), key)
	if errors.Is(err, keyring.ErrNotFound) {
		return Credential{}, ErrCredentialNotFound
	}
	if err != nil {
		return Credential{}, nativeStoreError("read")
	}
	var credential Credential
	if json.Unmarshal([]byte(raw), &credential) != nil || credential.Version != credentialVersion ||
		credential.ClientID == "" || credential.RefreshToken == "" {
		return Credential{}, errors.New("the stored ClawBridge credential is invalid; run login again")
	}
	return credential, nil
}

func (s KeyringStore) Save(ctx context.Context, key string, credential Credential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	credential.Version = credentialVersion
	raw, err := json.Marshal(credential)
	if err != nil {
		return errors.New("cannot encode the ClawBridge credential")
	}
	if err = keyring.Set(s.service(), key, string(raw)); err != nil {
		return nativeStoreError("save")
	}
	return nil
}

func (s KeyringStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := keyring.Delete(s.service(), key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return nativeStoreError("remove")
	}
	return nil
}

func Key(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return "oauth-" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

type Identity struct {
	Principal string
	Subject   string
}

type VerifyFunc func(context.Context, string, string) (Identity, error)

// Config describes one installed-app OAuth client and its trusted local storage.
type Config struct {
	ClientID     string
	ClientSecret string
	// ExpectedPrincipal, when set, must match the verified ID-token email before
	// any durable credential is saved.
	ExpectedPrincipal string
	Endpoints         Endpoints
	HTTPClient        *http.Client
	Store             Store
	OpenBrowser       func(context.Context, string) error
	Verify            VerifyFunc
}

type DevicePrompt struct {
	VerificationURL string
	UserCode        string
	ExpiresIn       time.Duration
}

func (c Config) normalized() (Config, *oauth2.Config, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Config{}, nil, errors.New("an explicit OAuth client ID is required; run setup or select a trusted configuration")
	}
	if c.Endpoints == (Endpoints{}) {
		c.Endpoints = GoogleEndpoints
	}
	if c.Endpoints.AuthorizationURL == "" || c.Endpoints.TokenURL == "" || c.Endpoints.DeviceAuthURL == "" {
		return Config{}, nil, errors.New("the OAuth client configuration is incomplete")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if c.Store == nil {
		c.Store = KeyringStore{}
	}
	if c.OpenBrowser == nil {
		c.OpenBrowser = openBrowser
	}
	if c.Verify == nil {
		c.Verify = verifyGoogleIDToken
	}
	oauthConfig := &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:       c.Endpoints.AuthorizationURL,
			TokenURL:      c.Endpoints.TokenURL,
			DeviceAuthURL: c.Endpoints.DeviceAuthURL,
			AuthStyle:     oauth2.AuthStyleInParams,
		},
		Scopes: []string{"openid", "email"},
	}
	return c, oauthConfig, nil
}

// BrowserConfig validates the Google Desktop-app client used by the loopback
// PKCE flow.
func BrowserConfig(c Config) (Config, error) {
	c, _, err := c.normalized()
	return c, err
}

// DeviceConfig validates the separate TVs and Limited Input devices client used
// by the device authorization flow. A Desktop-app client is not a safe default.
func DeviceConfig(c Config) (Config, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Config{}, errors.New("an explicit OAuth client ID for a TVs and Limited Input devices application is required")
	}
	return BrowserConfig(c)
}

// BrowserLogin opens Google's authorization page and accepts exactly one
// loopback callback carrying the generated state.
func BrowserLogin(ctx context.Context, config Config) (Credential, error) {
	credential, transaction, err := BrowserLoginStaged(ctx, config)
	if err != nil {
		return Credential{}, err
	}
	transaction.Commit()
	return credential, nil
}

// BrowserLoginStaged performs browser login and leaves the saved credential
// staged for a caller that must first write trusted routing. The caller must
// Commit after the route write, or Rollback on failure.
func BrowserLoginStaged(ctx context.Context, config Config) (Credential, *CredentialTransaction, error) {
	config, oauthConfig, err := config.normalized()
	if err != nil {
		return Credential{}, nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Credential{}, nil, errors.New("cannot start the local OAuth callback; use --device on a machine without loopback networking")
	}
	defer listener.Close()
	redirectURL := "http://" + listener.Addr().String() + "/callback"
	oauthConfig.RedirectURL = redirectURL
	verifier := oauth2.GenerateVerifier()
	state, err := randomValue(32)
	if err != nil {
		return Credential{}, nil, errors.New("cannot create OAuth state")
	}

	type callbackResult struct {
		code string
		err  error
	}
	callbacks := make(chan callbackResult, 1)
	var callbackOnce sync.Once
	callbackPath := "/callback"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		if r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		if !isLoopbackRemote(r.RemoteAddr) || r.Method != http.MethodGet {
			http.Error(w, "Invalid OAuth callback.", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.FormValue("state")), []byte(state)) != 1 {
			http.Error(w, "OAuth state did not match. Return to ClawBridge and try again.", http.StatusBadRequest)
			return
		}
		if providerError := r.FormValue("error"); providerError != "" {
			callbackOnce.Do(func() {
				callbacks <- callbackResult{err: fmt.Errorf("Google sign-in was not completed: %s", boundedCode(providerError))}
			})
			http.Error(w, "Google sign-in was not completed. Return to ClawBridge.", http.StatusBadRequest)
			return
		}
		code := r.FormValue("code")
		if code == "" || len(code) > 8192 {
			callbackOnce.Do(func() {
				callbacks <- callbackResult{err: errors.New("Google returned an invalid authorization response")}
			})
			http.Error(w, "Google returned an invalid response.", http.StatusBadRequest)
			return
		}
		callbackOnce.Do(func() {
			callbacks <- callbackResult{code: code}
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>ClawBridge sign-in</title><p>Authorization received. You may close this window and return to your terminal.</p>")
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	serveResult := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveResult <- err
	}()
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(closeContext)
	}()

	authURL := oauthConfig.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
	)
	if err = config.OpenBrowser(ctx, authURL); err != nil {
		return Credential{}, nil, errors.New("cannot open a browser for Google sign-in; use clawbridge login --device")
	}

	var callback callbackResult
	select {
	case <-ctx.Done():
		return Credential{}, nil, ctx.Err()
	case serveErr := <-serveResult:
		if serveErr != nil {
			return Credential{}, nil, errors.New("the local OAuth callback stopped unexpectedly")
		}
		return Credential{}, nil, errors.New("the local OAuth callback stopped before authorization completed")
	case callback = <-callbacks:
	}
	if callback.err != nil {
		return Credential{}, nil, callback.err
	}
	requestContext := context.WithValue(ctx, oauth2.HTTPClient, config.HTTPClient)
	token, err := oauthConfig.Exchange(requestContext, callback.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Credential{}, nil, errors.New("cannot exchange the Google authorization code; retry login")
	}
	return finishLogin(ctx, config, oauthConfig, token)
}

// DeviceLogin presents a short code for approval on another device and polls
// Google until the user approves, denies or the device code expires.
func DeviceLogin(ctx context.Context, config Config, prompt func(DevicePrompt)) (Credential, error) {
	credential, transaction, err := DeviceLoginStaged(ctx, config, prompt)
	if err != nil {
		return Credential{}, err
	}
	transaction.Commit()
	return credential, nil
}

// DeviceLoginStaged performs device authorization and leaves the saved device
// client credential staged for the caller's trusted-route transaction.
func DeviceLoginStaged(ctx context.Context, config Config, prompt func(DevicePrompt)) (Credential, *CredentialTransaction, error) {
	config, oauthConfig, err := config.normalized()
	if err != nil {
		return Credential{}, nil, err
	}
	requestContext := context.WithValue(ctx, oauth2.HTTPClient, config.HTTPClient)
	response, err := oauthConfig.DeviceAuth(requestContext)
	if err != nil {
		return Credential{}, nil, errors.New("cannot start Google device sign-in; verify the OAuth client supports device authorization")
	}
	expires := time.Until(response.Expiry)
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	if prompt != nil {
		prompt(DevicePrompt{
			VerificationURL: response.VerificationURI,
			UserCode:        response.UserCode,
			ExpiresIn:       expires,
		})
	}
	token, err := oauthConfig.DeviceAccessToken(requestContext, response)
	if err != nil {
		return Credential{}, nil, errors.New("Google device sign-in failed, expired or was declined; start a new login")
	}
	return finishLogin(ctx, config, oauthConfig, token)
}

func finishLogin(ctx context.Context, config Config, oauthConfig *oauth2.Config, token *oauth2.Token) (Credential, *CredentialTransaction, error) {
	if token == nil {
		return Credential{}, nil, errors.New("Google returned no OAuth token")
	}
	idToken := rawIDToken(token)
	if idToken == "" {
		return Credential{}, nil, errors.New("Google returned no OpenID Connect identity token")
	}
	identity, err := config.Verify(ctx, idToken, config.ClientID)
	if err != nil {
		return Credential{}, nil, errors.New("Google identity token validation failed; run login again")
	}
	if token.RefreshToken == "" || len(token.RefreshToken) > 16384 {
		return Credential{}, nil, errors.New("Google returned no durable refresh credential; run login again and approve offline access")
	}
	credential := Credential{
		Version:      credentialVersion,
		ClientID:     oauthConfig.ClientID,
		Principal:    strings.ToLower(identity.Principal),
		Subject:      identity.Subject,
		RefreshToken: token.RefreshToken,
	}
	if credential.Principal == "" || credential.Subject == "" {
		return Credential{}, nil, errors.New("Google identity token has no verified principal")
	}
	if config.ExpectedPrincipal != "" && !strings.EqualFold(config.ExpectedPrincipal, credential.Principal) {
		return Credential{}, nil, errors.New("the signed-in Google principal does not match the trusted principal; no credential was saved")
	}
	transaction, err := StageCredential(ctx, config.Store, config.ClientID, credential)
	if err != nil {
		return Credential{}, nil, err
	}
	return credential, transaction, nil
}

// newIDTokenValidator builds the Google ID token validator; tests replace it.
var newIDTokenValidator = func() (*idtoken.Validator, error) {
	return idtoken.NewValidator(context.Background(), idtoken.WithHTTPClient(&http.Client{Timeout: 8 * time.Second}))
}

var idTokenValidator struct {
	sync.Mutex
	v *idtoken.Validator
}

// sharedIDTokenValidator returns one validator for the process, so Google's
// signing certificates stay cached across token refreshes. A failed build is
// not kept, and the next call tries again.
func sharedIDTokenValidator() (*idtoken.Validator, error) {
	idTokenValidator.Lock()
	defer idTokenValidator.Unlock()
	if idTokenValidator.v == nil {
		v, err := newIDTokenValidator()
		if err != nil {
			return nil, err
		}
		idTokenValidator.v = v
	}
	return idTokenValidator.v, nil
}

func verifyGoogleIDToken(ctx context.Context, token, audience string) (Identity, error) {
	validator, err := sharedIDTokenValidator()
	if err != nil {
		return Identity{}, err
	}
	payload, err := validator.Validate(ctx, token, audience)
	if err != nil || payload == nil || payload.Subject == "" {
		return Identity{}, errors.New("invalid Google ID token")
	}
	email, _ := payload.Claims["email"].(string)
	verified, _ := payload.Claims["email_verified"].(bool)
	if !verified || !validEmail(email) {
		return Identity{}, errors.New("a verified Google email is required")
	}
	return Identity{Principal: strings.ToLower(email), Subject: payload.Subject}, nil
}

func validEmail(email string) bool {
	return len(email) <= 320 && strings.Count(email, "@") == 1 &&
		!strings.ContainsAny(email, " \t\r\n\x00") &&
		!strings.HasPrefix(email, "@") && !strings.HasSuffix(email, "@")
}

func rawIDToken(token *oauth2.Token) string {
	if token == nil {
		return ""
	}
	if extra, ok := token.Extra("id_token").(string); ok && len(extra) <= 16384 {
		return extra
	}
	return ""
}

func randomValue(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func boundedCode(value string) string {
	if len(value) > 64 {
		value = value[:64]
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return "provider_error"
		}
	}
	return value
}

func isLoopbackRemote(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openBrowser(ctx context.Context, target string) error {
	if parsed, err := url.Parse(target); err != nil || parsed.Scheme != "https" {
		return errors.New("invalid browser sign-in URL")
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", target)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.CommandContext(ctx, "xdg-open", target)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
