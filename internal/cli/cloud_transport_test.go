package cli

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

// The shell transport runs exactly the target's own argv: the fixed script
// in argv, a secret's value on stdin only.
func TestShellTransportUsesTheTargetsArgv(t *testing.T) {
	oldOut, oldRun := remoteOutput, runRemote
	t.Cleanup(func() { remoteOutput, runRemote = oldOut, oldRun })
	withGcpKey(t, t.TempDir())
	for _, target := range []cloudTarget{
		{host: "admin@bridge.example.test"},
		{vm: "clawbridge-1", project: "example-project", zone: "europe-west1-b"},
	} {
		tr, err := target.transport()
		if err != nil {
			t.Fatal(err)
		}
		var ran []string
		var stdin string
		remoteOutput = func(_ context.Context, argv []string, _ io.Writer) ([]byte, error) {
			ran = argv
			return []byte("ok"), nil
		}
		runRemote = func(_ context.Context, argv []string, in io.Reader, _ io.Writer) error {
			ran = argv
			b, _ := io.ReadAll(in)
			stdin = string(b)
			return nil
		}
		if out, err := tr.run(context.Background(), factsScript(), nil); err != nil || string(out) != "ok" {
			t.Fatalf("run = %q, %v", out, err)
		}
		want, _ := target.argv(tr.(shellTransport).sudo() + factsScript() + `'`)
		if !reflect.DeepEqual(ran, want) {
			t.Fatalf("run argv = %v, want %v", ran, want)
		}
		const value = "s3cr3t-value-0123456789"
		if err := tr.setSecret(context.Background(), "CLAWBRIDGE_OAUTH_CLIENT_SECRET", value, nil); err != nil {
			t.Fatal(err)
		}
		want, _ = target.argv(tr.(shellTransport).sudo() + remoteSetScript("CLAWBRIDGE_OAUTH_CLIENT_SECRET", serverEnvFile) + `'`)
		if !reflect.DeepEqual(ran, want) || stdin != value+"\n" || strings.Contains(strings.Join(ran, " "), value) {
			t.Fatalf("setSecret argv = %v stdin = %q", ran, stdin)
		}
	}
}

// An invalid target fails as cloudTarget.argv does, before anything runs.
func TestCloudTransportRefusesWhatArgvRefuses(t *testing.T) {
	for _, target := range []cloudTarget{
		{host: "bad host"},
		{vm: "clawbridge-1", project: "example-project"},
		{project: "example-project"},
	} {
		_, want := target.argv("")
		if _, err := target.transport(); err == nil || want == nil || err.Error() != want.Error() {
			t.Fatalf("%+v: transport() = %v, argv = %v", target, err, want)
		}
	}
}

// wantSSH is the full --ssh argv, every option spelled out (ANY-VM M1): on
// the command line they win over ~/.ssh/config, so a user's
// StrictHostKeyChecking=no, ForwardAgent=yes, forwardings, LocalCommand or
// RemoteCommand can't apply.
func wantSSH(host, command string) []string {
	return []string{"ssh", "-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "RemoteCommand=none",
		"--", host, command}
}

// The --ssh argv is exactly wantSSH's, for the facts, a secret and the
// host facts alike.
func TestSSHArgvOptions(t *testing.T) {
	target := cloudTarget{host: "admin@bridge.example.test"}
	if got := (shellTransport{target: target}).sudo(); got != `env LC_ALL=C sudo -n sh -c '` {
		t.Fatalf("sudo over ssh: %q", got)
	}
	if got := (shellTransport{target: cloudTarget{vm: "v", project: "p", zone: "z"}}).sudo(); got != `sudo -n sh -c '` {
		t.Fatalf("sudo over IAP: %q", got)
	}
	for _, script := range []string{factsScript(), hostFactsScript(), remoteSetScript("CLAWBRIDGE_OAUTH_CLIENT_SECRET", serverEnvFile)} {
		got, err := target.argv(`sudo -n sh -c '` + script + `'`)
		if err != nil || !reflect.DeepEqual(got, wantSSH("admin@bridge.example.test", `sudo -n sh -c '`+script+`'`)) {
			t.Fatalf("%v %v", got, err)
		}
	}
}
