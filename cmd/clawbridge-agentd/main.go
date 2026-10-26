// clawbridge-agentd launches the dispatcher agent containers, one per
// key-holder, under a rootless Docker daemon behind a validating proxy.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"clawbridge/internal/agentd"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := agentd.Main(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "clawbridge-agentd:", err)
		os.Exit(1)
	}
}
