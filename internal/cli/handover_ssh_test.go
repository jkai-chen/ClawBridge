package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The SSH route: Start's third choice, then 4, then the host, domain,
// owner and backup store typed.
const sshHandoverInputs = "3\n4\nadmin@bridge.example.test\nbridge.example.test\nowner@example.test\nhttps://s3.eu-central-003.example.test\nclawbridge-backups\neu-central-003\n"

func sshCheckOutput(t *testing.T, results ...cloudResult) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"source": "cloud_check", "data": results})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

var sshPassing = []cloudResult{
	{Check: "infra", Status: "warn", Reason: "data_on_root_disk", Advice: "the database is on the root disk"},
	{Check: "server", Status: "pass"}, {Check: "version", Status: "pass"}, {Check: "backups", Status: "pass"},
}

// The agent runs with the vm playbook and no cloud CLI scope; then the
// checks run over the user's ssh, and the summary shows the host, the
// store, the key scope as the user confirmed it, and every warning.
func TestHandoverOverSSHRunsTheAgentThenTheChecks(t *testing.T) {
	for name, c := range map[string]struct {
		scope, want string
	}{
		"confirmed":     {"y\n", "put and list only, confirmed by you"},
		"not confirmed": {"\n", "NOT confirmed: give the host a key that can't delete"},
	} {
		home := isolated(t)
		wizardHooks(t, sshHandoverInputs+"y\n"+c.scope)
		h := stubHandover(t)
		h.checkOut = sshCheckOutput(t, sshPassing...)
		scrubbed := secretsInEnv(t)
		bootstrapStub(t, []string{"google"}, nil)
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
			t.Fatal(name, err, out.String())
		}
		if !reflect.DeepEqual(h.installedCloud, []string{"vm"}) {
			t.Fatalf("%s: playbook %v", name, h.installedCloud)
		}
		for _, kv := range h.env {
			if strings.HasPrefix(kv, "AWS_PROFILE=") || strings.HasPrefix(kv, "CLOUDSDK_ACTIVE_CONFIG_NAME=") {
				t.Fatalf("%s: a cloud scope in the agent's env: %s", name, kv)
			}
		}
		scrubbed(h.env)
		task, _ := os.ReadFile(filepath.Join(home, "state", "onboarding", "provision", "TASK.md"))
		for _, want := range []string{"clawbridge-provision-vm", "SSH host: admin@bridge.example.test", "Backup endpoint: https://s3.eu-central-003.example.test",
			"never\nprint or read a value from /etc/clawbridge/clawbridge.env", "the skill's step 5, Sign-in",
			"clawbridge cloud check infra server version backups --ssh admin@bridge.example.test --server https://bridge.example.test"} {
			if !strings.Contains(string(task), want) {
				t.Fatalf("%s: TASK.md lacks %q:\n%s", name, want, task)
			}
		}
		wantChecks := []string{"infra", "server", "version", "backups", "--ssh", "admin@bridge.example.test", "--server", "https://bridge.example.test", "--json"}
		if !reflect.DeepEqual(h.checked, wantChecks) {
			t.Fatalf("%s: checks %v", name, h.checked)
		}
		if s := readState(t, home); s.Cloud != "ssh" || s.Step != "signin" || s.Host != "admin@bridge.example.test" || s.Endpoint != "https://s3.eu-central-003.example.test" {
			t.Fatalf("%s: %+v", name, s)
		}
		for _, want := range []string{"could read the\nserver's env file", "Hand-over summary", "admin@bridge.example.test", "s3://clawbridge-backups at https://s3.eu-central-003.example.test",
			c.want, "server, version, backups", "WARN infra", "data_on_root_disk: the database is on the root disk",
			"clawbridge-provision-vm skill", "clawbridge cloud check backups --ssh admin@bridge.example.test --server https://bridge.example.test"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s: lacks %q:\n%s", name, want, out.String())
			}
		}
	}
}

// A key question left unanswered (input ends) is an error that says how to
// finish, never a silent exit; the step stays at sign-in for the resume.
func TestHandoverOverSSHUnansweredKeyQuestionSaysSo(t *testing.T) {
	home := isolated(t)
	wizardHooks(t, sshHandoverInputs+"y\n")
	h := stubHandover(t)
	h.checkOut = sshCheckOutput(t, sshPassing...)
	var out bytes.Buffer
	err := Run(context.Background(), []string{"onboard"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "run clawbridge onboard again to finish the sign-in") {
		t.Fatalf("an unanswered question exited silently: %v\n%s", err, out.String())
	}
	if readState(t, home).Step != "signin" {
		t.Fatal("the step isn't sign-in for the resume")
	}
}

// A failing check, or no results at all, leaves the step undone.
func TestHandoverOverSSHChecksDecide(t *testing.T) {
	for name, c := range map[string]struct {
		out string
		err error
	}{
		"fail":       {"", errors.New("cloud check failed")},
		"no results": {"", nil},
		"empty list": {`{"source":"cloud_check","data":[]}`, nil},
	} {
		home := isolated(t)
		wizardHooks(t, sshHandoverInputs+"y\n")
		h := stubHandover(t)
		if name == "fail" {
			c.out = sshCheckOutput(t, cloudResult{Check: "infra", Status: "fail", Reason: "port_closed", Advice: "open 443"})
		}
		h.checkOut, h.checkErr = c.out, c.err
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
			t.Fatal(name, err)
		}
		if !strings.Contains(out.String(), "Not done yet") || strings.Contains(out.String(), "Hand-over summary") || readState(t, home).Step != "provision" {
			t.Fatalf("%s: %s", name, out.String())
		}
		if name == "fail" && !strings.Contains(out.String(), "port_closed") {
			t.Fatalf("the failing check is not shown: %s", out.String())
		}
	}
}

// At sign-in, a device-sign-in server gets cloud bootstrap over --ssh.
func TestHandoverOverSSHOffersBootstrap(t *testing.T) {
	isolated(t)
	s := onboardingState{Cloud: "ssh", Step: "signin", Host: "admin@bridge.example.test", Domain: "bridge.example.test", Owner: "o@example.test",
		Endpoint: "https://s3.example.test", Bucket: "clawbridge-backups", Region: "auto"}
	if err := saveOnboarding(&s); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\n\ny\nfirst\nn\n")
	stubHandover(t)
	ran := bootstrapStub(t, []string{"device"}, nil)
	projectStub(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"--ssh", "admin@bridge.example.test", "--server", "https://bridge.example.test", "--workspace", "my-team", "--email", "o@example.test", "--client-config", clientPath(t)}}
	if !reflect.DeepEqual(*ran, want) || !strings.Contains(out.String(), "The workspace my-team is created") {
		t.Fatalf("%v\n%s", *ran, out.String())
	}
}

// Each input is checked before anything is saved; the endpoint is what
// the backup runner takes.
func TestSSHInputsAreValidated(t *testing.T) {
	good := onboardingState{Cloud: "ssh", Host: "admin@bridge.example.test", Domain: "bridge.example.test", Owner: "o@example.test",
		Endpoint: "https://s3.eu-central-003.example.test", Bucket: "clawbridge-backups", Region: "eu-central-003"}
	if err := validInputs(&good); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"https://s3.example.test:9000", "https://203.0.113.9", "https://s3.example.test/"} {
		if s := good; true {
			s.Endpoint = ok
			if err := validInputs(&s); err != nil {
				t.Fatalf("%s: %v", ok, err)
			}
		}
	}
	for name, edit := range map[string]func(*onboardingState){
		"host flag":     func(s *onboardingState) { s.Host = "-oProxyCommand=x" },
		"host space":    func(s *onboardingState) { s.Host = "admin@bridge example" },
		"domain":        func(s *onboardingState) { s.Domain = "localhost" },
		"owner":         func(s *onboardingState) { s.Owner = "nobody" },
		"http":          func(s *onboardingState) { s.Endpoint = "http://s3.example.test" },
		"userinfo":      func(s *onboardingState) { s.Endpoint = "https://k:s@s3.example.test" },
		"path":          func(s *onboardingState) { s.Endpoint = "https://s3.example.test/bucket" },
		"query":         func(s *onboardingState) { s.Endpoint = "https://s3.example.test?x=1" },
		"empty query":   func(s *onboardingState) { s.Endpoint = "https://s3.example.test?" },
		"fragment":      func(s *onboardingState) { s.Endpoint = "https://s3.example.test#x" },
		"empty port":    func(s *onboardingState) { s.Endpoint = "https://s3.example.test:" },
		"localhost":     func(s *onboardingState) { s.Endpoint = "https://localhost:9000" },
		"sub-localhost": func(s *onboardingState) { s.Endpoint = "https://s3.localhost" },
		"loopback":      func(s *onboardingState) { s.Endpoint = "https://127.0.0.1:9000" },
		"zero":          func(s *onboardingState) { s.Endpoint = "https://0.0.0.0" },
		"ipv6":          func(s *onboardingState) { s.Endpoint = "https://[::1]:9000" },
		"bucket":        func(s *onboardingState) { s.Bucket = "Bad_Bucket" },
		"region":        func(s *onboardingState) { s.Region = "eu central" },
		"region empty":  func(s *onboardingState) { s.Region = "" },
	} {
		s := good
		edit(&s)
		if err := validInputs(&s); err == nil {
			t.Fatalf("%s accepted: %+v", name, s)
		}
	}
}
