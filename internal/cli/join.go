package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"clawbridge/internal/auth"
	"clawbridge/internal/remote"
	"clawbridge/internal/setup"
)

// Kept at the adapter boundary so tests never require a native keychain or Google.
var newJoinExecutor = func(c Config) (setup.Executor, error) { return remote.New(remoteConfig(c)) }

func remoteConfig(c Config) remote.Config {
	r := remote.Config{URL: c.Server, Audience: c.Audience, Account: c.Account, ImpersonateServiceAccount: c.ImpersonateServiceAccount, Principal: c.Principal, CachePath: c.Cache, CredentialSource: c.CredentialSource, CredentialsFile: c.CredentialsFile, OAuthClientID: c.OAuthClientID, OAuthClientSecret: c.OAuthClientSecret, OAuthDeviceClientID: c.OAuthDeviceClientID, OAuthDeviceClientSecret: c.OAuthDeviceClientSecret, OAuthKeyringService: c.OAuthKeyringService}
	if c.CredentialSource == remote.CredentialSourceDevice {
		r.DeviceID, r.KeyStore = c.DeviceID, c.KeyStore
		r.KeyHome, _ = stateHome()
		if at, err := time.Parse(time.RFC3339, c.ClockOffsetAt); err == nil {
			r.ClockOffset, r.ClockOffsetAt = time.Duration(c.ClockOffsetSeconds)*time.Second, at
		}
		if path := c.ClientConfig; path != "" {
			r.SaveClockOffset = func(offset time.Duration, at time.Time) { _ = saveClockOffset(path, offset, at) }
		}
	}
	return r
}

func commandFlags(name string, args []string, stderr io.Writer, bind func(*flag.FlagSet)) (*flag.FlagSet, error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	bind(f)
	ordered, err := interspersedFlags(f, args)
	if err != nil {
		return f, err
	}
	if err = f.Parse(ordered); err != nil {
		return f, err
	}
	if f.NArg() != 0 {
		return f, errors.New(name + " accepts flags only")
	}
	return f, nil
}

func runJoin(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) (retErr error) {
	var login, device bool
	_, err := commandFlags("setup join", args, stderr, func(f *flag.FlagSet) {
		f.BoolVar(&login, "login", false, "acquire fresh browser OAuth credentials")
		f.BoolVar(&device, "device", false, "acquire fresh device OAuth credentials")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if login && device {
		return errors.New("join accepts either --login or --device")
	}
	if flags.Server == "" || flags.Workspace == "" || flags.Project == "" {
		return errors.New("join requires explicit --server, --workspace and --project flags")
	}
	if flags.Mode != "" && flags.Mode != "cloud" {
		return errors.New("join requires cloud mode")
	}
	if (flags.Session == "") != (flags.Instance == "") {
		return errors.New("join requires explicit session and instance together")
	}
	home, err := stateHome()
	if err != nil {
		return err
	}
	path := firstConfigPath(flags.ClientConfig, os.Getenv("CLAWBRIDGE_CONFIG"))
	if path == "" {
		path = filepath.Join(home, "config.toml")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	if err = setup.ValidateJoinTarget(path, flags.Server, flags.Principal); err != nil {
		return err
	}
	// An explicit trusted selector bypasses repository configuration entirely.
	flags.ClientConfig = path
	config, _, err := resolveConfigVariantMode(ctx, flags, func(c Config) Config {
		if c.Mode == "" {
			c.Mode = "cloud"
		}
		if c.CredentialSource == "" {
			c.CredentialSource = "oauth"
		}
		if c.Cache == "" {
			c.Cache = filepath.Join(home, "cache.db")
		}
		return c
	}, true, device)
	// The login resolver requires OAuth; explicit alternate routes use runtime validation.
	if err != nil {
		config, _, err = resolveConfigVariant(ctx, flags, func(c Config) Config {
			if c.Mode == "" {
				c.Mode = "cloud"
			}
			if c.CredentialSource == "" {
				c.CredentialSource = "oauth"
			}
			if c.Cache == "" {
				c.Cache = filepath.Join(home, "cache.db")
			}
			return c
		})
	}
	if err != nil {
		return err
	}
	if config.Mode != "cloud" {
		return errors.New("join refuses non-cloud trusted configuration")
	}
	if err = setup.ValidateJoinTarget(path, config.Server, config.Principal); err != nil {
		return err
	}
	if config.CredentialSource != "oauth" && (login || device) {
		return errors.New("fresh join login requires OAuth; no backend fallback")
	}
	var tx *auth.CredentialTransaction
	defer func() {
		if tx != nil && retErr != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tx.Rollback(cleanup) != nil {
				retErr = errors.New("join failed and keychain rollback failed; partial keychain cleanup requires attention (no credentials logged)")
			}
		}
	}()
	if config.CredentialSource == "oauth" && (login || device || config.OAuthClientID == "") {
		config, tx, err = acquireOAuth(ctx, config, device, stderr)
		if err != nil {
			return err
		}
	}
	if _, err = remote.ValidateRoute(remoteConfig(config)); err != nil {
		return err
	}
	if config.CredentialSource == "oauth" && config.OAuthClientSecret == "" {
		return errors.New("join requires the active installed-app credential; run login or setup join --login")
	}
	executor, err := newJoinExecutor(config)
	if err != nil {
		return errors.New("cannot load join credentials; run login or setup join --login (no backend fallback)")
	}
	result, err := setup.Join(ctx, setup.JoinInput{
		ConfigPath: path, Workspace: flags.Workspace, Project: flags.Project, Session: flags.Session, Instance: flags.Instance,
		Server: config.Server, Audience: config.Audience, Principal: config.Principal, Cache: config.Cache, CredentialSource: config.CredentialSource,
		Account: config.Account, ImpersonateServiceAccount: config.ImpersonateServiceAccount, CredentialsFile: config.CredentialsFile,
		OAuthClientID: config.OAuthClientID, OAuthClientSecret: config.OAuthClientSecret, OAuthDeviceClientID: config.OAuthDeviceClientID, OAuthDeviceClientSecret: config.OAuthDeviceClientSecret, OAuthKeyringService: config.OAuthKeyringService,
	}, executor)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("online join failed; no route published (run login or setup join --login for credential failures): %w", err)
	}
	if tx != nil {
		tx.Commit()
	}
	return emit(ctx, stdout, "join", map[string]any{"source": "server", "data": map[string]any{"joined": true, "config_path": result.ConfigPath, "workspace": result.Workspace, "project": result.Project, "principal": result.Principal, "credential_source": config.CredentialSource, "changed": result.Changed}})
}
