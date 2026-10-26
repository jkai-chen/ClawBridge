package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The hand-over on AWS (live/AWS-PLAYBOOK-DESIGN.md D9): the same shape as on
// Google Cloud. The user's own named AWS CLI profile replaces the gcloud
// configuration: ClawBridge checks that it exists and is signed in to the
// chosen account, never creates it and never reads ~/.aws. After the agent
// exits, ClawBridge finds the instance by its fixed tags with the user's own
// credentials, and the checks decide.

func askAWSInputs(ctx context.Context, prompt *wizardPrompt, stdout io.Writer) (*onboardingState, error) {
	fmt.Fprintln(stdout, "\nYour AWS choices. None of these is a secret. The profile is a named AWS CLI profile you set up")
	fmt.Fprintln(stdout, "yourself (for example with aws configure sso --profile "+defaultAWSProfile+"); ClawBridge never creates it.")
	s := &onboardingState{Cloud: "aws"}
	for _, q := range []struct {
		label, fallback string
		dest            *string
	}{
		{"AWS CLI profile", defaultAWSProfile, &s.Profile},
		{"Account ID", "", &s.Account},
		{"Region", "eu-west-1", &s.Region},
		{"Zone", "", &s.Zone},
		{"Domain for the server", "", &s.Domain},
		{"Owner's email (becomes workspace owner)", "", &s.Owner},
		{"Instance size, t4g.small or t4g.medium", "t4g.small", &s.Size},
		{"Backup bucket", "", &s.Bucket},
	} {
		fallback := q.fallback
		switch q.dest {
		case &s.Account:
			// The profile's own account, when it can be read.
			if awsProfilePattern.MatchString(s.Profile) {
				if out, err := awsCLI(ctx, []string{"aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text", "--profile", s.Profile}, nil); err == nil {
					if id := strings.TrimSpace(string(out)); accountPattern.MatchString(id) {
						fallback = id
					}
				}
			}
		case &s.Zone:
			fallback = s.Region + "a"
		case &s.Bucket:
			fallback = "clawbridge-backups-" + s.Account + "-" + s.Region
		}
		label := q.label + ": "
		if fallback != "" {
			label = q.label + " [" + fallback + "]: "
		}
		answer, err := prompt.ask(label)
		if err != nil {
			return nil, err
		}
		if *q.dest = strings.TrimSpace(answer); *q.dest == "" {
			*q.dest = fallback
		}
	}
	if err := validInputs(s); err != nil {
		return nil, fmt.Errorf("%w; run clawbridge onboard to start again", err)
	}
	return s, nil
}

// awsTaskText is the TASK.md the agent starts from on AWS.
func awsTaskText(s *onboardingState) string {
	return fmt.Sprintf(`# ClawBridge onboarding: provision on AWS

Load the clawbridge-provision-aws skill and follow its hard rules; say them to
the user first. Work through its steps 1 to 6 with these inputs:

- AWS CLI profile: %s
- Account ID: %s
- Region: %s
- Zone: %s
- Domain: %s
- Owner email: %s
- Instance size: %s
- Backup bucket: %s

AWS_PROFILE and AWS_REGION name the profile and region in this shell; still
name --profile and --region in every command.

Record every resource you create in created.txt in this directory.

Finish by running, and reporting verbatim, with the instance ID you created:

    clawbridge cloud check infra server version --server https://%s --instance INSTANCE_ID --region %s --profile %s --account %s

Stop there. The user runs the sign-in step (the skill's step 7) themselves,
after you exit.
`, s.Profile, s.Account, s.Region, s.Zone, s.Domain, s.Owner, s.Size, s.Bucket, s.Domain, s.Region, s.Profile, s.Account)
}

// awsProfileReady: the profile exists and is signed in to the chosen account.
func awsProfileReady(ctx context.Context, s *onboardingState) error {
	out, err := awsCLI(ctx, []string{"aws", "configure", "list-profiles"}, nil)
	if err != nil {
		return errors.New("could not list AWS CLI profiles; is the aws CLI installed?")
	}
	found := false
	for _, p := range strings.Fields(string(out)) {
		found = found || p == s.Profile
	}
	if !found {
		return fmt.Errorf("no AWS CLI profile %s; create it in your own terminal (for example aws configure sso --profile %s), then run clawbridge onboard again", s.Profile, s.Profile)
	}
	target := cloudTarget{region: s.Region, profile: s.Profile, account: s.Account}
	if err := target.awsAccount(ctx); err != nil {
		if errors.Is(err, errCloudAuth) {
			return fmt.Errorf("the profile %s is signed in to another account than %s; sign it in to that account, then run clawbridge onboard again", s.Profile, s.Account)
		}
		return fmt.Errorf("the profile %s could not be read; sign it in (for example aws sso login --profile %s), then run clawbridge onboard again", s.Profile, s.Profile)
	}
	return nil
}

// findInstance returns the one instance the playbook created, by its fixed
// tags, as the user's own profile sees it.
func findInstance(ctx context.Context, s *onboardingState) (string, error) {
	target := cloudTarget{region: s.Region, profile: s.Profile}
	out, err := awsCLI(ctx, target.aws("ec2", "describe-instances", "--filters",
		"Name=tag:Name,Values=clawbridge-1", "Name=tag:clawbridge,Values=1",
		"Name=instance-state-name,Values=pending,running,stopping,stopped",
		"--query", "Reservations[].Instances[].InstanceId"), nil)
	var ids []string
	if err != nil || json.Unmarshal(out, &ids) != nil {
		return "", errors.New("the instances could not be listed with the profile")
	}
	if len(ids) != 1 || !instancePattern.MatchString(ids[0]) {
		return "", fmt.Errorf("found %d instances named clawbridge-1 with the clawbridge tag, not one", len(ids))
	}
	return ids[0], nil
}

// handOverAWS is handOver for AWS.
func handOverAWS(ctx context.Context, s *onboardingState, bin, skillAgent string, stdout, stderr io.Writer) error {
	if err := installPlaybook("aws", skillAgent); err != nil {
		return fmt.Errorf("the playbook could not be installed for the agent: %w", err)
	}
	if err := awsProfileReady(ctx, s); err != nil {
		return err
	}
	base, err := onboardingDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "provision")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(awsTaskText(s)), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nStarting %s in %s. Exit it when it is done; ClawBridge then runs the checks.\n\n", bin, dir)
	env := append(os.Environ(), "AWS_PROFILE="+s.Profile, "AWS_REGION="+s.Region, "AWS_DEFAULT_REGION="+s.Region)
	if err = runAgent(ctx, bin, dir, env, "Read TASK.md in this directory and follow it."); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	// Whatever the agent said or how it exited, ClawBridge finds the
	// instance itself, and the checks decide.
	fmt.Fprintln(stdout, "\nChecking the server…")
	instance, err := findInstance(ctx, s)
	if err == nil {
		s.Instance = instance
		args := []string{"infra", "server", "version", "--server", "https://" + s.Domain,
			"--instance", instance, "--region", s.Region, "--profile", s.Profile, "--account", s.Account}
		err = runHandoverChecks(ctx, args, stdout, stderr)
	} else {
		fmt.Fprintln(stdout, err.Error()+".")
	}
	if err != nil {
		fmt.Fprintln(stdout, "\nNot done yet. Run clawbridge onboard to hand over again (your agent resumes from created.txt),")
		fmt.Fprintln(stdout, "or follow docs/operations/first-deployment.md yourself.")
		return nil
	}
	s.Step = "signin"
	if err = saveOnboarding(s); err != nil {
		return err
	}
	return finishSignin(s, stdout)
}
