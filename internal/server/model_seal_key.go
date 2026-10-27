package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"clawbridge/internal/bus"
)

// modelSealStore is what `operator model-seal-key` asks of the bus.
type modelSealStore interface {
	OperatorModelSealRotate(ctx context.Context, seal *bus.ModelSeal, confirm, deleteUnreadable bool) (bus.SealRotation, error)
	Close() error
}

// openModelSealStore opens the hosted authority; tests replace it.
var openModelSealStore = func(ctx context.Context, dsn string) (modelSealStore, error) {
	return bus.OpenPostgresOperator(ctx, dsn)
}

const modelSealKeyUsage = "usage: clawbridge operator model-seal-key rotate [--confirm [--delete-unreadable]] [--database-url-env NAME]"

// runModelSealKey is `operator model-seal-key rotate` [AG-L1]: with the new
// seal key in CLAWBRIDGE_MODEL_KEY_SEAL_KEY and the old one in
// CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS (the server's own environment), it
// re-seals every stored provider key under the new one. Without --confirm
// it only counts what it would do. The seal keys come from the environment
// only, never the command line.
func runModelSealKey(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "rotate" {
		return errors.New(modelSealKeyUsage)
	}
	f := flag.NewFlagSet("operator model-seal-key rotate", flag.ContinueOnError)
	confirm := f.Bool("confirm", false, "re-seal; without it, only count the stored keys under each seal key")
	deleteUnreadable := f.Bool("delete-unreadable", false, "delete stored keys that open under neither seal key; their owners run setup again")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *deleteUnreadable && !*confirm {
		return errors.New(modelSealKeyUsage)
	}
	seal, _, err := readModelSettings(os.Getenv)
	if err != nil {
		return err
	}
	if seal == nil {
		return errors.New("CLAWBRIDGE_MODEL_KEY_SEAL_KEY is not set: set the new seal key there and the old one in CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS")
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	store, err := openModelSealStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot open Postgres: %w", err)
	}
	defer store.Close()
	r, err := store.OperatorModelSealRotate(ctx, seal, *confirm, *deleteUnreadable)
	if err != nil {
		return err
	}
	if !*confirm {
		_, err = fmt.Fprintf(stdout, "Dry run: %d stored model keys; %d under CLAWBRIDGE_MODEL_KEY_SEAL_KEY, %d under ..._PREVIOUS, %d under neither. Nothing changed; pass --confirm to re-seal them all under CLAWBRIDGE_MODEL_KEY_SEAL_KEY.\n", r.Keys, r.UnderCurrent, r.UnderPrevious, r.Unreadable)
		return err
	}
	_, err = fmt.Fprintf(stdout, "Re-sealed %d stored model keys under CLAWBRIDGE_MODEL_KEY_SEAL_KEY", r.Resealed)
	if err == nil && r.Deleted > 0 {
		_, err = fmt.Fprintf(stdout, "; deleted %d that opened under neither key (their owners run dispatcher model setup again)", r.Deleted)
	}
	if err != nil {
		return err
	}
	if r.NeedPrevious > 0 {
		_, err = fmt.Fprintf(stdout, ". %d still open only under CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS (changed meanwhile): run rotate again before removing it.\n", r.NeedPrevious)
		return err
	}
	_, err = fmt.Fprintln(stdout, ". None needs CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS now: restart the server without it to finish.")
	return err
}
