package main

import (
	"context"
	"errors"
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
		var exit *cli.ExitCodeError
		if errors.As(err, &exit) && exit.Whole {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, "clawbridge:", err)
		}
		if exit != nil {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}
