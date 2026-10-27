package cli

import (
	"context"
	"io"

	"clawbridge/internal/clirender"
)

// emit writes a command's result: today's JSON, or the human layout of the
// view called name (live/CLI-OUTPUT-PLAN.md). It is every command's one way
// to print its result.
func emit(ctx context.Context, stdout io.Writer, name string, v any) error {
	return clirender.Write(stdout, clirender.ModeOf(ctx), name, v)
}
