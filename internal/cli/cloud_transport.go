package cli

import (
	"context"
	"errors"
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
	// describe is the advice when a write fails: what access to check.
	describe() string
}

// shellTransport reaches a shell that takes the remote command as its last
// argument and the value on stdin: ssh, or gcloud compute ssh over IAP. Its
// argv is the target's own (cloudTarget.argv), unchanged.
type shellTransport struct{ target cloudTarget }

func (s shellTransport) run(ctx context.Context, script string, stderr io.Writer) ([]byte, error) {
	argv, err := s.target.argv(s.sudo() + script + `'`)
	if err != nil {
		return nil, err
	}
	if s.target.host != "" {
		return sshRun(ctx, argv, nil, stderr)
	}
	return remoteOutput(ctx, argv, stderr)
}

func (s shellTransport) setSecret(ctx context.Context, key, value string, stderr io.Writer) error {
	argv, err := s.target.argv(s.sudo() + remoteSetScript(key, serverEnvFile) + `'`)
	if err != nil {
		return err
	}
	if s.target.host != "" {
		_, err = sshRun(ctx, argv, strings.NewReader(value+"\n"), stderr)
		return err
	}
	return runRemote(ctx, argv, strings.NewReader(value+"\n"), stderr)
}

// sudo opens the remote command. Over plain SSH it runs in the C locale,
// so sudo's refusals are the English phrases sshFailure reads, whatever
// LANG the user's ssh sends.
func (s shellTransport) sudo() string {
	if s.target.host != "" {
		return `env LC_ALL=C sudo -n sh -c '`
	}
	return `sudo -n sh -c '`
}

func (shellTransport) describe() string {
	return "check SSH access and passwordless sudo on the server"
}

// transport is the target's route to the server's shell, its inputs
// validated: one route only; an instance's ID here (the AWS inputs when it
// runs); otherwise cloudTarget.argv checks the SSH host or the VM's names.
func (t cloudTarget) transport() (cloudTransport, error) {
	routes := 0
	for _, r := range []string{t.host, t.vm, t.instance} {
		if r != "" {
			routes++
		}
	}
	if routes > 1 {
		return nil, errors.New("pass one route to the server: --ssh HOST, --vm NAME or --instance ID")
	}
	if t.instance != "" {
		if !instancePattern.MatchString(t.instance) || t.project != "" || t.zone != "" {
			return nil, errors.New("--instance must be an EC2 instance ID (i-…), with --region and without --project or --zone")
		}
		if err := t.awsInputs(); err != nil {
			return nil, err
		}
		return ssmTransport{target: t, account: new(bool)}, nil
	}
	if t.region != "" || t.profile != "" || t.account != "" {
		return nil, errors.New("--region, --profile and --account go with --instance")
	}
	if _, err := t.argv(""); err != nil {
		return nil, err
	}
	return shellTransport{target: t}, nil
}
