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
	OperatorModelSealRotate(ctx context.Context, seal *bus.ModelSeal, deleteUnreadable bool) (resealed, deleted int, err error)
	Close() error
}

// openModelSealStore opens the hosted authority; tests replace it.
var openModelSealStore = func(ctx context.Context, dsn string) (modelSealStore, error) {
	return bus.OpenPostgresOperator(ctx, dsn)
}

const modelSealKeyUsage = "usage: clawbridge operator model-seal-key rotate [--delete-unreadable] [--database-url-env NAME]"

// runModelSealKey is `operator model-seal-key rotate` [AG-L1]: with the new
// seal key in CLAWBRIDGE_MODEL_KEY_SEAL_KEY and the old one in
// CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS (the server's own environment), it
// re-seals every stored provider key under the new one. The seal keys come
// from the environment only, never the command line.
func runModelSealKey(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "rotate" {
		return errors.New(modelSealKeyUsage)
	}
	f := flag.NewFlagSet("operator model-seal-key rotate", flag.ContinueOnError)
	deleteUnreadable := f.Bool("delete-unreadable", false, "delete stored keys that open under neither seal key; their owners run setup again")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
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
	resealed, deleted, err := store.OperatorModelSealRotate(ctx, seal, *deleteUnreadable)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Re-sealed %d stored model keys under CLAWBRIDGE_MODEL_KEY_SEAL_KEY", resealed)
	if err == nil && deleted > 0 {
		_, err = fmt.Fprintf(stdout, "; deleted %d that opened under neither key (their owners run dispatcher model setup again)", deleted)
	}
	if err == nil {
		_, err = fmt.Fprintln(stdout, ". Restart the server without CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS to finish.")
	}
	return err
}
