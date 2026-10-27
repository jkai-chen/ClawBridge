package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"

	"clawbridge/internal/hostobserve"
)

var runHostObservation = hostobserve.Run

func runHost(ctx context.Context, command string, args []string, stdout, stderr io.Writer) error {
	var c hostobserve.Config
	timeout := 5 * time.Second
	if command == "host-wait" {
		timeout = 30 * time.Second
	}
	_, err := commandFlags(command, args, stderr, func(f *flag.FlagSet) {
		f.StringVar(&c.Binding.Adapter, "adapter", "", "explicit host adapter (codex)")
		f.StringVar(&c.Binding.Endpoint, "endpoint", "", "existing absolute Unix WebSocket socket URL")
		f.StringVar(&c.Binding.Thread, "thread", "", "explicit canonical thread UUID")
		f.DurationVar(&c.Timeout, "timeout", timeout, "finite timeout (1s–3600s)")
		if command == "host-wait" {
			f.StringVar(&c.For, "for", "", "idle or unloaded (not process exit)")
			f.DurationVar(&c.Interval, "interval", time.Second, "poll interval (250ms–30s)")
		}
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if command == "host-status" {
		c.Interval = time.Second
	} else if c.For == "" {
		return errors.New("host-wait requires --for idle or unloaded")
	}
	if err = c.Validate(); err != nil {
		return err
	}
	result, err := runHostObservation(ctx, c)
	if err != nil {
		return err
	}
	if err = emit(ctx, stdout, "host_status", struct {
		Source string `json:"source"`
		Data   any    `json:"data"`
	}{"host-observation", result}); err != nil {
		return err
	}
	if result.Outcome != "matched" {
		return errors.New("host observation ended: " + result.Outcome)
	}
	return nil
}
