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
		oldOut, oldRun := remoteOutput, runRemote
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
		want, _ := target.argv(`sudo -n sh -c '` + factsScript() + `'`)
		if !reflect.DeepEqual(ran, want) {
			t.Fatalf("run argv = %v, want %v", ran, want)
		}
		const value = "s3cr3t-value-0123456789"
		if err := tr.setSecret(context.Background(), "CLAWBRIDGE_OAUTH_CLIENT_SECRET", value, nil); err != nil {
			t.Fatal(err)
		}
		want, _ = target.argv(`sudo -n sh -c '` + remoteSetScript("CLAWBRIDGE_OAUTH_CLIENT_SECRET", serverEnvFile) + `'`)
		if !reflect.DeepEqual(ran, want) || stdin != value+"\n" || strings.Contains(strings.Join(ran, " "), value) {
			t.Fatalf("setSecret argv = %v stdin = %q", ran, stdin)
		}
		remoteOutput, runRemote = oldOut, oldRun
	}
}

// An invalid target fails as cloudTarget.argv does, before anything runs.
func TestCloudTransportRefusesWhatArgvRefuses(t *testing.T) {
	for _, target := range []cloudTarget{
		{host: "bad host"},
		{vm: "clawbridge-1", project: "example-project"},
		{host: "admin@bridge.example.test", vm: "clawbridge-1", project: "example-project", zone: "europe-west1-b"},
		{project: "example-project"},
	} {
		_, want := target.argv("")
		if _, err := target.transport(); err == nil || want == nil || err.Error() != want.Error() {
			t.Fatalf("%+v: transport() = %v, argv = %v", target, err, want)
		}
	}
}
