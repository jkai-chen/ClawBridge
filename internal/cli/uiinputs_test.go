package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

func localUIProject(t *testing.T) {
	t.Helper()
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
}

func TestOpenUIInputsLocal(t *testing.T) {
	localUIProject(t)
	in, err := openUIInputs(context.Background(), Config{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, ok := in.Executor.(*bus.Store); !ok {
		t.Fatalf("executor %T, want *bus.Store", in.Executor)
	}
	if !strings.HasPrefix(in.Config.Project, "prj_") || in.Waiting || in.OutboxWaiting != nil || in.Linear == nil || in.Path == "" {
		t.Fatalf("unexpected inputs: %+v", in)
	}
	if in.Info != "Local authority.\nProvisioning unavailable. This interface never creates infrastructure." {
		t.Fatalf("info %q", in.Info)
	}
	if _, err := in.Doctor(in.Context); err != nil {
		t.Fatal(err)
	}
}

func TestOpenUIInputsCloseIsIdempotentAndCancels(t *testing.T) {
	localUIProject(t)
	in, err := openUIInputs(context.Background(), Config{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	in.Close()
	in.Close()
	if !errors.Is(in.Context.Err(), context.Canceled) {
		t.Fatalf("context after Close: %v", in.Context.Err())
	}
}

func TestOpenUIInputsUnknownProjectFailsClosed(t *testing.T) {
	localUIProject(t)
	in, err := openUIInputs(context.Background(), Config{Project: "nope"})
	if err == nil || in != nil {
		t.Fatalf("want an error and no inputs, got %v, %+v", err, in)
	}
	// The store opened before the failed resolve was closed: it opens and
	// writes again without a lock held by the failed attempt.
	invoke(t, "project", "create", "q")
}
