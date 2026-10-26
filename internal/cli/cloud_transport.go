package cli

import (
	"context"
	"io"
	"strings"
)

// cloudTransport runs ClawBridge's fixed scripts on the server through the
// user's own access. Each cloud has its own: a plain SSH host, a Compute
// Engine VM over IAP and, next, an EC2 instance over SSM. Nothing passed to
// one is ever assembled from input: script is always a compiled-in script.
type cloudTransport interface {
	// run runs a fixed script as root and returns its output (64 KiB at most).
	run(ctx context.Context, script string, stderr io.Writer) ([]byte, error)
	// setSecret writes one allowed key's value into the server's env file.
	// The value is never in argv, an environment variable or a log.
	setSecret(ctx context.Context, key, value string, stderr io.Writer) error
}

// shellTransport reaches a shell that takes the remote command as its last
// argument and the value on stdin: ssh, or gcloud compute ssh over IAP. Its
// argv is the target's own (cloudTarget.argv), unchanged.
type shellTransport struct{ target cloudTarget }

func (s shellTransport) run(ctx context.Context, script string, stderr io.Writer) ([]byte, error) {
	argv, err := s.target.argv(`sudo -n sh -c '` + script + `'`)
	if err != nil {
		return nil, err
	}
	return remoteOutput(ctx, argv, stderr)
}

func (s shellTransport) setSecret(ctx context.Context, key, value string, stderr io.Writer) error {
	argv, err := s.target.argv(`sudo -n sh -c '` + remoteSetScript(key, serverEnvFile) + `'`)
	if err != nil {
		return err
	}
	return runRemote(ctx, argv, strings.NewReader(value+"\n"), stderr)
}

// transport is the target's route to the server's shell, its inputs
// validated: cloudTarget.argv checks the SSH host or the VM's names.
func (t cloudTarget) transport() (cloudTransport, error) {
	if _, err := t.argv(""); err != nil {
		return nil, err
	}
	return shellTransport{target: t}, nil
}
