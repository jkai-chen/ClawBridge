package cli

import (
	"errors"
	"flag"
	"io"
	"time"

	"clawbridge/internal/remote"
)

// googleCommandRemoved ends a command that needed Google sign-in, removed in
// v1.0, with exit 2 and the swap runbook's text
// (docs/operations/google-sign-in-removal.md).
func googleCommandRemoved(command string) error {
	return &ExitCodeError{Code: 2, Whole: true, Err: errors.New("clawbridge " + command + ": Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md")}
}

// remoteConfig is a profile's device route; a profile without one is
// refused by remote.New.
func remoteConfig(c Config) remote.Config {
	r := remote.Config{URL: c.Server, Principal: c.Principal, CachePath: c.Cache, CredentialSource: c.CredentialSource}
	if c.CredentialSource == remote.CredentialSourceDevice {
		r.DeviceID, r.KeyStore = c.DeviceID, c.KeyStore
		r.KeyHome, _ = stateHome()
		if at, err := time.Parse(time.RFC3339, c.ClockOffsetAt); err == nil {
			r.ClockOffset, r.ClockOffsetAt = time.Duration(c.ClockOffsetSeconds)*time.Second, at
		}
		if path := c.ClientConfig; path != "" {
			r.SaveClockOffset = func(offset time.Duration, at time.Time) { _ = saveClockOffset(path, offset, at) }
		}
	}
	return r
}

func commandFlags(name string, args []string, stderr io.Writer, bind func(*flag.FlagSet)) (*flag.FlagSet, error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	bind(f)
	ordered, err := interspersedFlags(f, args)
	if err != nil {
		return f, err
	}
	if err = f.Parse(ordered); err != nil {
		return f, err
	}
	if f.NArg() != 0 {
		return f, errors.New(name + " accepts flags only")
	}
	return f, nil
}
