package cli

import (
	"clawbridge/internal/auth"
	"clawbridge/internal/remote"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

// launchLoginBrowser opens the Google sign-in page; a variable so tests never
// start a real browser.
var launchLoginBrowser = auth.LaunchBrowser

// loginBrowserWait matches the wait the auth package uses for its own opener.
const loginBrowserWait = 5 * time.Second

// showAndOpenBrowser always prints the sign-in URL, then asks the system to
// open it. A browser opening is no proof it is the right browser or Google
// account, so the URL is there to copy into another browser profile on this
// computer. It carries the client ID, the PKCE challenge (never the verifier),
// state and the loopback address, and no secret. The loopback callback keeps
// waiting whether or not the launch worked.
func showAndOpenBrowser(stderr io.Writer) func(context.Context, string) error {
	return func(ctx context.Context, target string) error {
		if parsed, err := url.Parse(target); err != nil || parsed.Scheme != "https" {
			return errors.New("invalid browser sign-in URL")
		}
		_, _ = fmt.Fprintf(stderr, "To sign in with another browser or Google account, open this URL on this computer instead:\n%s\n", target)
		_, _ = launchLoginBrowser(ctx, target, loginBrowserWait)
		return nil
	}
}

// acquireOAuth stages credentials but never publishes routing or commits them.
func acquireOAuth(ctx context.Context, config Config, device bool, stderr io.Writer) (Config, *auth.CredentialTransaction, error) {
	var err error
	applicableID := config.OAuthClientID
	applicableSecret := config.OAuthClientSecret
	if device {
		applicableID = config.OAuthDeviceClientID
		applicableSecret = config.OAuthDeviceClientSecret
	} else if applicableID != "" && applicableID == config.OAuthDeviceClientID {
		// Device promotion selects the runtime client, not a Desktop client.
		// Rediscover the browser pair rather than attempting loopback with it.
		applicableID, applicableSecret = "", ""
	}
	var discovery *remote.OAuthDiscovery
	if applicableID == "" {
		discovered, discoveryErr := discoverLoginOAuth(ctx, config.Server, nil)
		if discoveryErr != nil {
			kind := "Desktop-app browser"
			field := "browser_client_id"
			if device {
				kind = "TVs and Limited Input devices"
				field = "device_client_id"
			}
			return config, nil, fmt.Errorf("cannot discover the %s OAuth client because no trusted oauth client ID is configured; set the applicable trusted ID or fix %s discovery: %w", kind, field, discoveryErr)
		}
		discovery = &discovered
		if device {
			applicableID = discovered.DeviceClientID
			applicableSecret = discovered.DeviceClientSecret
		} else {
			applicableID = discovered.BrowserClientID
			applicableSecret = discovered.BrowserClientSecret
		}
		if applicableID == "" {
			field := "browser_client_id"
			if device {
				field = "device_client_id"
			}
			return config, nil, fmt.Errorf("trusted server OAuth discovery returned an empty %s; configure the applicable trusted client ID or fix server discovery before login", field)
		}
		if applicableSecret == "" {
			field := "browser_client_secret"
			if device {
				field = "device_client_secret"
			}
			return config, nil, fmt.Errorf("trusted server OAuth discovery returned an empty %s; configure the applicable installed-app credential or fix server discovery before login", field)
		}
	}
	if !device && applicableID == config.OAuthDeviceClientID {
		return config, nil, errors.New("browser OAuth client must be distinct from the known device client; fix trusted Desktop-app discovery before login")
	}
	if applicableSecret == "" {
		field := "oauth_client_secret"
		if device {
			field = "oauth_device_client_secret"
		}
		return config, nil, fmt.Errorf("the configured OAuth client requires a matching trusted %s before login", field)
	}
	keyring := loginCredentialStore(config.OAuthKeyringService)
	loginConfig := auth.Config{
		ClientID:          applicableID,
		ClientSecret:      applicableSecret,
		ExpectedPrincipal: config.Principal,
		Store:             keyring,
		OpenBrowser:       showAndOpenBrowser(stderr),
	}
	var credential auth.Credential
	var transaction *auth.CredentialTransaction
	if device {
		loginConfig.ClientID = applicableID
		credential, transaction, err = deviceLoginStaged(ctx, loginConfig, func(prompt auth.DevicePrompt) {
			_, _ = fmt.Fprintf(stderr, "Open %s and enter code %s (expires in %s).\n", prompt.VerificationURL, prompt.UserCode, prompt.ExpiresIn.Round(time.Second))
		})
	} else {
		_, _ = fmt.Fprintln(stderr, "Opening Google sign-in in your browser...")
		credential, transaction, err = browserLoginStaged(ctx, loginConfig)
	}
	if err != nil {
		return config, nil, err
	}
	config.CredentialSource = "oauth"
	config.Account = ""
	config.ImpersonateServiceAccount = ""
	config.CredentialsFile = ""
	config.Principal = credential.Principal
	if device {
		config.OAuthDeviceClientID = applicableID
		config.OAuthDeviceClientSecret = applicableSecret
		config.OAuthClientID = applicableID
		config.OAuthClientSecret = applicableSecret
	} else {
		config.OAuthClientID = applicableID
		config.OAuthClientSecret = applicableSecret
		if config.OAuthDeviceClientID == "" && discovery != nil && discovery.DeviceClientID != "" {
			config.OAuthDeviceClientID = discovery.DeviceClientID
			config.OAuthDeviceClientSecret = discovery.DeviceClientSecret
		} else if config.OAuthDeviceClientSecret == "" && discovery != nil &&
			config.OAuthDeviceClientID == discovery.DeviceClientID {
			config.OAuthDeviceClientSecret = discovery.DeviceClientSecret
		}
	}
	config.Audience = applicableID
	return config, transaction, nil
}
