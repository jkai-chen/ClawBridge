package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"clawbridge/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "clawbridge:", err)
		os.Exit(1)
	}
}
