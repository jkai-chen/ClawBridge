package auth

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// browserCommand is the system's URL opener; tests replace it. It is built
// without a context so that ending or cancelling a sign-in never kills the
// browser the opener started.
var browserCommand = func(target string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return exec.Command("xdg-open", target)
	}
}

// browserWait is how long LaunchBrowser waits for the opener to finish.
var browserWait = 5 * time.Second

// LaunchBrowser asks the system to open target and waits up to wait for the
// opener itself to exit. A clean exit reports opened; a failed start or
// exit is an error. An opener still running when the wait ends (xdg-open may
// stay in the foreground with the browser it started), or a canceled
// context, reports not opened and no error: the outcome is unknown, so the
// caller should show the URL. The opener is never killed.
func LaunchBrowser(ctx context.Context, target string, wait time.Duration) (bool, error) {
	command := browserCommand(target)
	if err := command.Start(); err != nil {
		return false, errors.New("cannot start the system browser opener")
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-exited:
		if err != nil {
			return false, errors.New("the system browser opener failed")
		}
		return true, nil
	case <-timer.C:
	case <-ctx.Done():
	}
	return false, nil
}
