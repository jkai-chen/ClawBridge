package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// AWS: Start's third choice, then 2, then the defaults: the profile,
// eu-west-1, the profile's own account, its zone a, t4g.small and the
// account-and-region bucket; the domain and owner typed.
const awsHandoverInputs = "3\n2\n\n\n\n\nbridge.example.test\nowner@example.test\n\n\n"

func awsHandoverStub(t *testing.T) (*handoverStub, *awsStub) {
	t.Helper()
	h := stubHandover(t)
	a := &awsStub{out: map[string]string{
		"configure list-profiles": "default\nclawbridge-onboarding\n",
		"sts get-caller-identity": "123456789012\n",
		"ec2 describe-instances":  `["` + ec2ID + `"]`,
	}}
	stubAWS(t, a, nil, nil)
	return h, a
}

func TestHandoverOnAWSRunsTheAgentThenTheChecks(t *testing.T) {
	home := isolated(t)
	wizardHooks(t, awsHandoverInputs+"y\n")
	h, a := awsHandoverStub(t)
	a.out["sts get-caller-identity"] = `{"Account":"123456789012"}`
	// The default account is read as text, in the chosen region; the
	// account check as JSON.
	inner := awsCLI
	awsCLI = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
		if argv[1] == "sts" && strings.Contains(strings.Join(argv, " "), "--output text --profile clawbridge-onboarding --region eu-west-1") {
			return []byte("123456789012\n"), nil
		}
		return inner(ctx, argv, stdin)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	if !reflect.DeepEqual(h.installedCloud, []string{"aws"}) || !reflect.DeepEqual(h.installed, []string{"claude-code"}) {
		t.Fatalf("playbook: %v %v", h.installedCloud, h.installed)
	}
	env := strings.Join(h.env, "\n")
	if !strings.Contains(env, "AWS_PROFILE=clawbridge-onboarding") || !strings.Contains(env, "AWS_REGION=eu-west-1") {
		t.Fatal("the agent does not run with the profile and region")
	}
	task, _ := os.ReadFile(filepath.Join(home, "state", "onboarding", "provision", "TASK.md"))
	for _, want := range []string{"clawbridge-provision-aws", "Account ID: 123456789012", "Zone: eu-west-1a",
		"Backup bucket: clawbridge-backups-123456789012-eu-west-1", "--region eu-west-1 --profile clawbridge-onboarding --account 123456789012"} {
		if !strings.Contains(string(task), want) {
			t.Fatalf("TASK.md lacks %q:\n%s", want, task)
		}
	}
	wantChecks := []string{"infra", "server", "version", "--server", "https://bridge.example.test",
		"--instance", ec2ID, "--region", "eu-west-1", "--profile", "clawbridge-onboarding", "--account", "123456789012"}
	if !reflect.DeepEqual(h.checked, wantChecks) {
		t.Fatalf("checks: %v", h.checked)
	}
	if s := readState(t, home); s.Cloud != "aws" || s.Step != "signin" || s.Instance != ec2ID {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(out.String(), "--bucket clawbridge-backups-123456789012-eu-west-1 --region eu-west-1 --profile clawbridge-onboarding") {
		t.Fatalf("sign-in text: %s", out.String())
	}
	for _, argv := range a.calls {
		if argv[1] == "configure" && argv[2] != "list-profiles" {
			t.Fatalf("the profile was changed: %v", argv)
		}
	}
}

// A profile that is missing, or signed in elsewhere, stops before the agent
// starts; nothing creates or changes it.
func TestHandoverOnAWSNeedsTheUsersProfile(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*awsStub)
		want string
	}{
		"no profile":    {func(a *awsStub) { a.out["configure list-profiles"] = "default\n" }, "no AWS CLI profile clawbridge-onboarding"},
		"other account": {func(a *awsStub) { a.out["sts get-caller-identity"] = `{"Account":"999999999999"}` }, "another account than 123456789012"},
	} {
		isolated(t)
		wizardHooks(t, "3\n2\n\n\n123456789012\n\nbridge.example.test\nowner@example.test\n\n\ny\n")
		h, a := awsHandoverStub(t)
		c.edit(a)
		err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) || h.ran != nil {
			t.Fatalf("%s: %v, agent ran: %v", name, err, h.ran)
		}
	}
}

// Without exactly one tagged instance, the step is not done and no check runs.
func TestHandoverOnAWSFindsTheInstanceItself(t *testing.T) {
	for name, listed := range map[string]string{"none": `[]`, "two": `["` + ec2ID + `","i-0fedcba9876543210"]`} {
		home := isolated(t)
		wizardHooks(t, "3\n2\n\n\n123456789012\n\nbridge.example.test\nowner@example.test\n\n\ny\n")
		h, a := awsHandoverStub(t)
		a.out["sts get-caller-identity"] = `{"Account":"123456789012"}`
		a.out["ec2 describe-instances"] = listed
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if h.checked != nil || !strings.Contains(out.String(), "Not done yet") || readState(t, home).Step != "provision" {
			t.Fatalf("%s: checks %v, %s", name, h.checked, out.String())
		}
	}
}

func TestHandoverOnAWSResumes(t *testing.T) {
	isolated(t)
	s := onboardingState{Cloud: "aws", Step: "signin", Profile: "clawbridge-onboarding", Account: "123456789012", Region: "eu-west-1",
		Zone: "eu-west-1a", Domain: "bridge.example.test", Owner: "o@example.test", Size: "t4g.small", Bucket: "clawbridge-backups", Instance: ec2ID}
	if err := saveOnboarding(&s); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\n")
	h, _ := awsHandoverStub(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if h.ran != nil || !strings.Contains(out.String(), "Saved progress: AWS account 123456789012") ||
		!strings.Contains(out.String(), "clawbridge-provision-aws") {
		t.Fatalf("%s", out.String())
	}
}

func TestValidAWSInputs(t *testing.T) {
	ok := onboardingState{Cloud: "aws", Profile: "clawbridge-onboarding", Account: "123456789012", Region: "eu-west-1", Zone: "eu-west-1a",
		Domain: "bridge.example.test", Owner: "o@example.test", Size: "t4g.small", Bucket: "clawbridge-backups-123456789012-eu-west-1"}
	if err := validInputs(&ok); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*onboardingState){
		"profile":        func(s *onboardingState) { s.Profile = "a profile" },
		"account":        func(s *onboardingState) { s.Account = "12345" },
		"region":         func(s *onboardingState) { s.Region = "europe-west1" },
		"zone":           func(s *onboardingState) { s.Zone = "eu-west-2a" },
		"zone suffix":    func(s *onboardingState) { s.Zone = "eu-west-1-a" },
		"domain":         func(s *onboardingState) { s.Domain = "bridge.example.test --x" },
		"owner":          func(s *onboardingState) { s.Owner = "nobody" },
		"size":           func(s *onboardingState) { s.Size = "e2-small" },
		"bucket":         func(s *onboardingState) { s.Bucket = "Bad_Bucket" },
		"saved instance": func(s *onboardingState) { s.Instance = "i-XYZ" },
		"profile flag":   func(s *onboardingState) { s.Profile = "--debug" },
		"unknown cloud":  func(s *onboardingState) { s.Cloud = "azure" },
		"no cloud":       func(s *onboardingState) { s.Cloud = "" },
	} {
		s := ok
		edit(&s)
		if validInputs(&s) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The cloud answer picks a path by number or name, and refuses anything else
// rather than falling back to Google Cloud.
func TestHandoverCloudAnswer(t *testing.T) {
	for answer, want := range map[string]string{"aws": "Your AWS choices", "AWS": "Your AWS choices", "gcp": "Your Google Cloud choices", "1": "Your Google Cloud choices"} {
		isolated(t)
		// Blank answers after the choice: the inputs are then refused.
		wizardHooks(t, "3\n"+answer+"\n"+strings.Repeat("\n", 10))
		awsHandoverStub(t)
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err == nil || !strings.Contains(out.String(), want) {
			t.Fatalf("%s: %v %s", answer, err, out.String())
		}
	}
	for _, answer := range []string{"3", "azure", "2x"} {
		home := isolated(t)
		wizardHooks(t, "3\n"+answer+"\n")
		h, _ := awsHandoverStub(t)
		var out bytes.Buffer
		err := Run(context.Background(), []string{"onboard"}, &out, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "answer 1 or 2") || h.ran != nil || strings.Contains(out.String(), "choices") {
			t.Fatalf("%s: %v %s", answer, err, out.String())
		}
		if _, err := os.Stat(filepath.Join(home, "state", "onboarding")); !os.IsNotExist(err) {
			t.Fatalf("%s: progress saved", answer)
		}
	}
}

// On Windows the AWS choice is refused before any AWS question, and a saved
// AWS progress before the agent starts.
func TestHandoverOnAWSRefusesWindows(t *testing.T) {
	old := awsGOOS
	awsGOOS = "windows"
	t.Cleanup(func() { awsGOOS = old })
	isolated(t)
	wizardHooks(t, "3\n2\n")
	h, _ := awsHandoverStub(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "WSL") ||
		strings.Contains(out.String(), "choices") || h.ran != nil {
		t.Fatalf("%v %s", err, out.String())
	}
	isolated(t)
	s := onboardingState{Cloud: "aws", Step: "provision", Profile: "clawbridge-onboarding", Account: "123456789012", Region: "eu-west-1",
		Zone: "eu-west-1a", Domain: "bridge.example.test", Owner: "o@example.test", Size: "t4g.small", Bucket: "clawbridge-backups"}
	if err := saveOnboarding(&s); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\ny\n")
	h, _ = awsHandoverStub(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "WSL") || h.ran != nil {
		t.Fatal(err)
	}
}
