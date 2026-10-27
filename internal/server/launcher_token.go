package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"clawbridge/internal/bus"
)

// launcherTokenStore is what `operator launcher-token` asks of the bus.
type launcherTokenStore interface {
	OperatorLauncherTokenCreate(ctx context.Context) (id, token string, err error)
	OperatorLauncherTokenRevoke(ctx context.Context, id string) error
	Close() error
}

// openLauncherTokenStore opens the hosted authority; tests replace it.
var openLauncherTokenStore = func(ctx context.Context, dsn string) (launcherTokenStore, error) {
	return bus.OpenPostgresOperator(ctx, dsn)
}

const launcherTokenUsage = "usage: clawbridge operator launcher-token create --out PATH [--print] | launcher-token revoke ID [--database-url-env NAME]"

// runLauncherToken is `operator launcher-token`: the host operator mints
// agentd's launcher token, or revokes one by ID. The token is a secret
// that never passes through an agent's shell (ON-M1): create writes it
// straight into a new agentd env file (0600, never over an existing one)
// as CLAWBRIDGE_LAUNCHER_TOKEN, and prints only its ID. --print shows it
// instead, and only on a terminal, for a person. The database keeps only
// its hash.
func runLauncherToken(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "create" && args[0] != "revoke" {
		return errors.New(launcherTokenUsage)
	}
	action := args[0]
	f := flag.NewFlagSet("operator launcher-token "+action, flag.ContinueOnError)
	out, print := "", false
	if action == "create" {
		f.StringVar(&out, "out", "", "a new agentd env file to write CLAWBRIDGE_LAUNCHER_TOKEN into (0600; never overwritten)")
		f.BoolVar(&print, "print", false, "show the token on this terminal instead of writing a file")
	}
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	var positional []string
	rest := args[1:]
	for {
		if err := f.Parse(rest); err != nil {
			return err
		}
		if f.NArg() == 0 {
			break
		}
		positional, rest = append(positional, f.Arg(0)), f.Args()[1:]
	}
	if action == "revoke" && len(positional) != 1 || action == "create" && (len(positional) != 0 || (out == "") == !print) {
		return errors.New(launcherTokenUsage)
	}
	// Refuse before a token exists, so a refused run leaves nothing.
	if print && !stdoutIsTerminal(stdout) {
		return errors.New("launcher-token create --print shows a secret, so it runs only on your own terminal; use --out PATH to write it into agentd's env file")
	}
	var file *os.File
	if out != "" {
		var err error
		if file, err = os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return fmt.Errorf("cannot create %s (it must not exist yet): %w", out, errors.Unwrap(err))
		}
		defer file.Close()
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		discard(file)
		return errors.New("Postgres connection environment variable is empty")
	}
	store, err := openLauncherTokenStore(ctx, dsn)
	if err != nil {
		discard(file)
		return fmt.Errorf("cannot open Postgres: %w", err)
	}
	defer store.Close()
	if action == "revoke" {
		if err = store.OperatorLauncherTokenRevoke(ctx, positional[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Revoked launcher token %s.\n", positional[0])
		return err
	}
	id, token, err := store.OperatorLauncherTokenCreate(ctx)
	if err != nil {
		discard(file)
		return err
	}
	expires := time.Now().Add(bus.LauncherTokenTTL).UTC().Format("2006-01-02")
	if print {
		_, err = fmt.Fprintf(stdout, "Launcher token %s, expiring on %s (shown once; put it in agentd's env file as CLAWBRIDGE_LAUNCHER_TOKEN):\n\n    %s\n\nNever paste it into a chat or an agent.\n", id, expires, token)
		return err
	}
	if _, err = fmt.Fprintf(file, "CLAWBRIDGE_LAUNCHER_TOKEN=%s\n", token); err == nil {
		err = file.Sync()
	}
	if err != nil {
		discard(file)
		// The token reached no file: revoke it rather than leave it live.
		_ = store.OperatorLauncherTokenRevoke(ctx, id)
		return fmt.Errorf("could not write %s; the new token was revoked", out)
	}
	_, err = fmt.Fprintf(stdout, "Created launcher token %s, expiring on %s, and wrote it to %s (0600). Point agentd's EnvironmentFile at it; create the next one and revoke this one before then.\n", id, expires, out)
	return err
}

// discard removes a file this command created and has not finished.
func discard(file *os.File) {
	if file != nil {
		file.Close()
		os.Remove(file.Name())
	}
}
