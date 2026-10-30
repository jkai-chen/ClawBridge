package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakeGcloud puts a gcloud on PATH that keeps each call's arguments (one
// per line, calls ended by "--end--") and compute ssh's stdin, answers the
// metadata reads with vmMeta and projectMeta, and answers compute ssh with
// answer, or with failure on stderr and exit 1.
func fakeGcloud(t *testing.T, vmMeta, projectMeta, answer, failure string) (dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir = t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$CB_DIR/argv"; done
echo --end-- >> "$CB_DIR/argv"
case "$1 $2" in
"compute instances") [ "$CB_VM_META" = FAIL ] && exit 1; printf '%s\n' "$CB_VM_META" ;;
"compute project-info") printf '%s\n' "$CB_PROJECT_META" ;;
"compute ssh")
	cat > "$CB_DIR/stdin"
	if [ -n "$CB_FAIL" ]; then printf '%s\n' "$CB_FAIL" >&2; exit 1; fi
	printf '%s\n' "$CB_ANSWER" ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CB_DIR", dir)
	t.Setenv("CB_VM_META", vmMeta)
	t.Setenv("CB_PROJECT_META", projectMeta)
	t.Setenv("CB_ANSWER", answer)
	t.Setenv("CB_FAIL", failure)
	withGcpKey(t, dir)
	return dir
}

// withGcpKey gives the test gcloud's own key in dir, as if the user had
// connected once, whatever this machine holds.
func withGcpKey(t *testing.T, dir string) {
	t.Helper()
	key := filepath.Join(dir, "google_compute_engine")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := gcpKey
	t.Cleanup(func() { gcpKey = old })
	gcpKey = func() (string, error) { return key, nil }
}

func gcloudCalls(t *testing.T, dir string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "argv"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	var call []string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "--end--" {
			calls = append(calls, call)
			call = nil
			continue
		}
		call = append(call, line)
	}
	return calls
}

func meta(scope string, items ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(items); i += 2 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"key":"` + items[i] + `","value":"` + items[i+1] + `"}`)
	}
	return `{"` + scope + `":{"items":[` + b.String() + `]}}`
}

const gcpInvite = `invite {"invite_id":"inv_1","email":"owner@example.test","workspace":"acme","workspace_created":true,"expires_at":"2026-10-28T05:00:00Z"}`

var gcpBootstrapArgs = []string{"--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--server", "https://cb.example.test", "--workspace", "acme", "--email", "owner@example.test"}

// On Google Cloud the code goes only on gcloud compute ssh's stdin, over
// IAP with a key that expires, never in any gcloud argv or the output; with
// OS Login on there is no warning.
func TestCloudBootstrapOverIAP(t *testing.T) {
	_, joins := stubBootstrap(t, "")
	dir := fakeGcloud(t, meta("metadata"), meta("commonInstanceMetadata", "enable-oslogin", "TRUE"), gcpInvite, "")
	out, err := bootstrap(gcpBootstrapArgs...)
	if err != nil || len(*joins) != 1 {
		t.Fatalf("%v %s joins %d", err, out, len(*joins))
	}
	code := (*joins)[0].Code
	calls := gcloudCalls(t, dir)
	if len(calls) != 3 {
		t.Fatalf("calls %q", calls)
	}
	ssh := calls[2]
	want := []string{"compute", "ssh", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--tunnel-through-iap", "--quiet", "--ssh-key-expire-after=1h", "--command"}
	if strings.Join(ssh[:len(want)], " ") != strings.Join(want, " ") {
		t.Fatalf("ssh argv %q", ssh)
	}
	for _, call := range calls {
		if strings.Contains(strings.Join(call, "\n"), code) {
			t.Fatalf("the code is in gcloud's argv: %q", call)
		}
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(stdin) != code+"\n" || strings.Contains(out, code) || strings.Contains(out, "OS Login") || (*joins)[0].Email != "owner@example.test" {
		t.Fatalf("stdin %q out %s", stdin, out)
	}
}

// With OS Login off, a one-line warning says where gcloud may put the key;
// the VM's own setting wins over the project's.
func TestCloudBootstrapWarnsWithoutOSLogin(t *testing.T) {
	for name, c := range map[string]struct {
		vm, project, want string
	}{
		"project-wide":  {meta("metadata"), meta("commonInstanceMetadata"), "the project's metadata (every VM in example-project trusts it)"},
		"project off":   {meta("metadata"), meta("commonInstanceMetadata", "enable-oslogin", "FALSE"), "the project's metadata"},
		"vm blocks":     {meta("metadata", "block-project-ssh-keys", "true"), meta("commonInstanceMetadata"), "the VM's own metadata"},
		"vm off":        {meta("metadata", "enable-oslogin", "false"), meta("commonInstanceMetadata", "enable-oslogin", "TRUE"), "the project's metadata"},
		"lowercase on":  {meta("metadata"), meta("commonInstanceMetadata", "enable-oslogin", "true"), ""},
		"vm fails":      {"FAIL", meta("commonInstanceMetadata", "enable-oslogin", "FALSE"), ""},
		"vm on":         {meta("metadata", "enable-oslogin", "TRUE"), meta("commonInstanceMetadata", "enable-oslogin", "FALSE"), ""},
		"unreadable vm": {"not json", meta("commonInstanceMetadata", "enable-oslogin", "TRUE"), ""},
	} {
		_, joins := stubBootstrap(t, "")
		fakeGcloud(t, c.vm, c.project, gcpInvite, "")
		out, err := bootstrap(gcpBootstrapArgs...)
		if err != nil || len(*joins) != 1 {
			t.Fatalf("%s: %v %s", name, err, out)
		}
		warned := strings.Contains(out, "warning: OS Login is off for clawbridge-1")
		if c.want == "" && warned || c.want != "" && (!warned || !strings.Contains(out, c.want) || !strings.Contains(out, "--ssh-key-expire-after=1h")) {
			t.Fatalf("%s: %s", name, out)
		}
		if strings.Count(out, "OS Login") > 2 {
			t.Fatalf("%s: more than one line: %s", name, out)
		}
	}
}

// IAP's "failed to connect to backend" is the firewall, said so; other
// failures keep their advice; nothing joins.
func TestCloudBootstrapIAPFailures(t *testing.T) {
	for name, c := range map[string]struct{ stderr, want string }{
		"firewall":     {"ERROR: (gcloud.compute.start-iap-tunnel) Error while connecting [4003: 'failed to connect to backend'].", "35.235.240.0/20"},
		"sudo":         {"sudo: a password is required", "sudo asked for a password"},
		"backend only": {"ERROR: failed to connect to backend", "35.235.240.0/20"},
		"code only":    {"Error while connecting [4003: 'x'].", "35.235.240.0/20"},
		"other":        {"ERROR: (gcloud.compute.ssh) something else", "may or may not exist"},
	} {
		_, joins := stubBootstrap(t, "")
		fakeGcloud(t, meta("metadata", "enable-oslogin", "TRUE"), meta("commonInstanceMetadata"), "", c.stderr)
		if _, err := bootstrap(gcpBootstrapArgs...); err == nil || !strings.Contains(err.Error(), c.want) || len(*joins) != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The VM's names are all required and checked before gcloud runs at all.
func TestCloudBootstrapGCPInputs(t *testing.T) {
	without := func(flag string) []string {
		var out []string
		for i := 0; i < len(gcpBootstrapArgs); i += 2 {
			if gcpBootstrapArgs[i] != flag {
				out = append(out, gcpBootstrapArgs[i], gcpBootstrapArgs[i+1])
			}
		}
		return out
	}
	with := func(flag, value string) []string {
		out := append([]string{}, gcpBootstrapArgs...)
		for i := 0; i < len(out); i += 2 {
			if out[i] == flag {
				out[i+1] = value
			}
		}
		return out
	}
	for name, args := range map[string][]string{
		"no project":   without("--project"),
		"no zone":      without("--zone"),
		"bad vm":       with("--vm", "clawbridge-1;reboot"),
		"bad project":  with("--project", "Example Project"),
		"bad zone":     with("--zone", "--zone"),
		"two routes":   append(append([]string{}, gcpBootstrapArgs...), "--ssh", "admin@cb.example.test"),
		"project only": without("--vm"),
	} {
		_, joins := stubBootstrap(t, "")
		dir := fakeGcloud(t, "", "", gcpInvite, "")
		if _, err := bootstrap(args...); err == nil || len(*joins) != 0 || gcloudCalls(t, dir) != nil {
			t.Fatalf("%s: %v, gcloud ran %q", name, err, gcloudCalls(t, dir))
		}
	}
}

// The plain SSH route asks gcloud nothing.
func TestCloudBootstrapOverSSHRunsNoGcloud(t *testing.T) {
	_, joins := stubBootstrap(t, "")
	stubExchange(t, gcpInvite+"\n", "", nil)
	dir := fakeGcloud(t, meta("metadata"), meta("commonInstanceMetadata"), gcpInvite, "")
	out, err := bootstrap("--ssh", "admin@cb.example.test", "--server", "https://cb.example.test", "--workspace", "acme")
	if err != nil || len(*joins) != 1 || gcloudCalls(t, dir) != nil || strings.Contains(out, "OS Login") {
		t.Fatalf("%v %s gcloud %q", err, out, gcloudCalls(t, dir))
	}
}

// At sign-in on Google Cloud, a device-sign-in server gets cloud bootstrap
// over --vm; a no leaves the written step, naming the GCP skill and checks.
func TestHandoverOnGCPOffersBootstrap(t *testing.T) {
	for _, c := range []struct {
		answer string
		ran    [][]string
		want   string
	}{
		{"y", [][]string{{"--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--server", "https://bridge.example.test", "--workspace", "my-team", "--email", "o@example.test", "--client-config", "<PROFILE>"}}, "The workspace my-team is created"},
		{"n", nil, "clawbridge-provision-gcp skill (references/signin.md)"},
	} {
		isolated(t)
		s := onboardingState{Cloud: "gcp", Step: "signin", Account: "admin@example.test", Size: "e2-small", Project: "example-project", Region: "europe-west1", Zone: "europe-west1-b", Domain: "bridge.example.test",
			Owner: "o@example.test", Bucket: "clawbridge-backups"}
		if err := saveOnboarding(&s); err != nil {
			t.Fatal(err)
		}
		wizardHooks(t, "3\n\n\n"+c.answer+"\nfirst\nn\n")
		stubHandover(t)
		ran := bootstrapStub(t, []string{"device"}, nil)
		projectStub(t)
		for _, args := range c.ran {
			args[len(args)-1] = clientPath(t)
		}
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*ran, c.ran) || !strings.Contains(out.String(), c.want) {
			t.Fatalf("%s: %v\n%s", c.answer, *ran, out.String())
		}
		if c.answer == "n" && !strings.Contains(out.String(), "cloud check backups --bucket clawbridge-backups --project example-project") {
			t.Fatalf("checks:\n%s", out.String())
		}
	}
}

// A cloud with no bootstrap route is offered nothing: no question, no read of
// the server's sign-in methods and no bootstrap, so the caller prints the
// written step.
func TestOfferBootstrapDeclinesAnUnknownCloud(t *testing.T) {
	ran := bootstrapStub(t, []string{"device"}, nil)
	read := false
	wizardMethods = func(context.Context, string) ([]string, error) { read = true; return []string{"device"}, nil }
	for _, cloud := range []string{"", "azure", "local"} {
		s := onboardingState{Cloud: cloud, Domain: "bridge.example.test", Owner: "o@example.test", Project: "example-project", Zone: "europe-west1-b"}
		var out bytes.Buffer
		done, err := offerBootstrap(context.Background(), &s, nil, filepath.Join(t.TempDir(), "client.json"), &out, io.Discard)
		if done || err != nil || out.Len() != 0 || read || len(*ran) != 0 {
			t.Fatalf("%q: done %v, err %v, methods read %v, ran %v:\n%s", cloud, done, err, read, *ran, out.String())
		}
	}
}

// Without gcloud's own key, nothing reaches gcloud: --quiet would otherwise
// make a key without a passphrase, and stdin carries the code or a secret.
func TestGCPRefusesWithoutGcloudsKey(t *testing.T) {
	_, joins := stubBootstrap(t, "")
	dir := fakeGcloud(t, meta("metadata"), meta("commonInstanceMetadata"), gcpInvite, "")
	if err := os.Remove(filepath.Join(dir, "google_compute_engine")); err != nil {
		t.Fatal(err)
	}
	_, err := bootstrap(gcpBootstrapArgs...)
	if err == nil || !strings.Contains(err.Error(), "connect once in your own terminal with gcloud compute ssh clawbridge-1 --project example-project --zone europe-west1-b --tunnel-through-iap") ||
		len(*joins) != 0 || gcloudCalls(t, dir) != nil {
		t.Fatalf("%v, gcloud ran %q", err, gcloudCalls(t, dir))
	}
	// Every --vm command passes cloudTarget.transport, which refuses.
	if _, err = (cloudTarget{vm: "clawbridge-1", project: "example-project", zone: "europe-west1-b"}).transport(); err == nil || !strings.Contains(err.Error(), "connect once") {
		t.Fatalf("transport: %v", err)
	}
	// cloud check refuses up front, before any check runs gcloud.
	var out bytes.Buffer
	err = Run(context.Background(), []string{"cloud", "check", "infra", "--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--server", "https://cb.example.test"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "connect once") || gcloudCalls(t, dir) != nil {
		t.Fatalf("check: %v %s, gcloud ran %q", err, out.String(), gcloudCalls(t, dir))
	}
}
