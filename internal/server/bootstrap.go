package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"clawbridge/internal/bus"
	"github.com/charmbracelet/x/term"
)

// stdoutIsTerminal reports whether w is a terminal; tests replace it.
var stdoutIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// runBootstrapInvite is `operator bootstrap-invite`: on the host, it lets
// the bootstrap owner (CLAWBRIDGE_BOOTSTRAP_OWNER) in when no device can
// sign in yet, by a one-time code for their own email. The code is a
// secret: it goes only to a terminal unless --force-stdout asks for it
// alone on one line (for a caller that reads it into memory), and it is
// never logged.
func runBootstrapInvite(ctx context.Context, args []string, stdout io.Writer) error {
	f := flag.NewFlagSet("operator bootstrap-invite", flag.ContinueOnError)
	workspace := f.String("workspace", "", "the workspace to create, or whose owner to let in")
	forceStdout := f.Bool("force-stdout", false, "print the code alone on one line even when stdout is not a terminal")
	ownerEnv := f.String("owner-env", "CLAWBRIDGE_BOOTSTRAP_OWNER", "environment variable holding the bootstrap owner's email")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *workspace == "" {
		return errors.New("usage: clawbridge operator bootstrap-invite --workspace NAME [--force-stdout]")
	}
	// Refuse before any invite exists, so a refused run leaves nothing.
	terminal := stdoutIsTerminal(stdout)
	if !terminal && !*forceStdout {
		return errors.New("bootstrap-invite prints a one-time sign-in code, which is a secret: run it in your own terminal (not through an agent), or pass --force-stdout to a program that keeps it in memory")
	}
	owner := os.Getenv(*ownerEnv)
	if owner == "" {
		return fmt.Errorf("%s is empty; it names the bootstrap owner", *ownerEnv)
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	store, err := bus.OpenPostgresOperator(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot open Postgres: %w", err)
	}
	defer store.Close()
	out, err := store.OperatorBootstrapInvite(ctx, *workspace, owner)
	if err != nil {
		return err
	}
	code := out["code"].(string)
	if !terminal {
		_, err = fmt.Fprintln(stdout, code)
		return err
	}
	verb := "Workspace %s exists"
	if out["workspace_created"] == true {
		verb = "Created workspace %s"
	}
	_, err = fmt.Fprintf(stdout, verb+"; its owner is %s.\n\nOne-time sign-in code (single use, expires %s):\n\n    %s\n\nOn your own computer, run:\n\n    clawbridge join --server https://YOUR-DOMAIN --email %s\n\nIt asks for the code without showing it. The code is a secret: type it yourself at that prompt, never on a command line (where shell history and other users' process lists keep it), and never paste it into a chat or an agent.\n", out["workspace"], out["email"], out["expires_at"], code, out["email"])
	return err
}
