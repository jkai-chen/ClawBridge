package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/clirender"
	"clawbridge/internal/secretenv"
	"clawbridge/internal/skill"
)

// The own-server path's hand-over (live/ONBOARDING-DESIGN.md §6, §10): the
// user's own coding agent provisions with the playbook, in this terminal,
// and the checks, not the agent, decide when it is done. Progress is saved
// so `clawbridge onboard` resumes it; it holds names and choices only,
// never a secret.

type onboardingState struct {
	Version string `json:"version"`
	Cloud   string `json:"cloud"`
	Step    string `json:"step"` // provision, then signin
	Account string `json:"account"`
	Project string `json:"project"`
	Region  string `json:"region"`
	Zone    string `json:"zone"`
	Domain  string `json:"domain"`
	Owner   string `json:"owner"`
	Size    string `json:"size"`
	Bucket  string `json:"bucket"`
	// AWS only: the named profile, and the instance the checks found.
	Profile  string `json:"profile,omitempty"`
	Instance string `json:"instance,omitempty"`
}

const gcloudConfiguration = "clawbridge-onboarding"

var (
	regionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	emailPattern  = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,63}$`)
	vmSizes       = map[string]bool{"e2-small": true, "e2-medium": true}
	awsSizes      = map[string]bool{"t4g.small": true, "t4g.medium": true}
)

// handoverAgents are the agent CLIs a hand-over can start, with the skill
// target each one reads.
var handoverAgents = []struct{ bin, name, skillAgent string }{
	{"claude", "Claude Code", "claude-code"},
	{"codex", "Codex", "codex"},
}

var (
	lookAgent = exec.LookPath
	// runAgent runs the agent CLI in the foreground of this terminal.
	runAgent = func(ctx context.Context, bin, dir string, env []string, prompt string) error {
		cmd := exec.CommandContext(ctx, bin, prompt)
		cmd.Dir, cmd.Env = dir, env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	runGcloud = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "gcloud", args...).Output()
	}
	installPlaybook = func(cloud, agent string) error {
		targets, err := skill.TargetsFor(skill.Playbooks[cloud], agent, "")
		if err != nil {
			return err
		}
		results, err := skill.Install(targets, false)
		for _, r := range results {
			if r.Refused != "" && err == nil {
				err = errors.New(r.Refused)
			}
		}
		return err
	}
	runHandoverChecks = func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		return runCloudCheck(ctx, args, stdout, stderr)
	}
)

func onboardingDir() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "onboarding"), nil
}

func loadOnboarding() (*onboardingState, error) {
	dir, err := onboardingDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "onboarding.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s onboardingState
	if err = json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("the saved onboarding progress is unreadable; remove it to start over")
	}
	return &s, nil
}

func saveOnboarding(s *onboardingState) error {
	dir, err := onboardingDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.Version = buildinfo.Version
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".onboarding.json.tmp")
	if err = os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "onboarding.json"))
}

// inputQuestion is one own-server input: asked in order, checked as it is
// typed (U45), and checked again when saved progress is resumed. fallback
// and ok see the answers given before it.
type inputQuestion struct {
	label    string
	field    func(*onboardingState) *string
	fallback func(*onboardingState) string
	ok       func(*onboardingState, string) bool
	why      string
}

func fixed(v string) func(*onboardingState) string { return func(*onboardingState) string { return v } }

func matches(p interface{ MatchString(string) bool }) func(*onboardingState, string) bool {
	return func(_ *onboardingState, v string) bool { return p.MatchString(v) }
}

var (
	domainQuestion = inputQuestion{"Domain for the server", func(s *onboardingState) *string { return &s.Domain }, fixed(""), matches(domainPattern),
		"the domain must be a DNS name you control, like clawbridge.example.com"}
	ownerQuestion = inputQuestion{"Owner's email (becomes workspace owner)", func(s *onboardingState) *string { return &s.Owner }, fixed(""), matches(emailPattern),
		"the owner must be an email address"}
)

// gcpQuestions are Google Cloud's inputs; account is the signed-in gcloud
// account, offered first.
func gcpQuestions(account string) []inputQuestion {
	owner := ownerQuestion
	owner.fallback = func(s *onboardingState) string { return s.Account }
	return []inputQuestion{
		{"Google account", func(s *onboardingState) *string { return &s.Account }, fixed(account), matches(emailPattern),
			"the Google account must be an email address"},
		// U49: the Google Cloud project, not a ClawBridge project.
		{"Google Cloud project ID", func(s *onboardingState) *string { return &s.Project }, fixed(""), matches(projectPattern),
			"the Google Cloud project ID is lowercase letters, digits and dashes, like my-team-123456"},
		{"Region", func(s *onboardingState) *string { return &s.Region }, fixed("europe-west1"), matches(regionPattern),
			"the region must look like europe-west1"},
		{"Zone", func(s *onboardingState) *string { return &s.Zone }, func(s *onboardingState) string { return s.Region + "-b" },
			func(s *onboardingState, v string) bool {
				return zonePattern.MatchString(v) && strings.HasPrefix(v, s.Region+"-")
			},
			"the zone must be in the region, like europe-west1-b"},
		domainQuestion,
		owner,
		{"VM size, e2-small or e2-medium", func(s *onboardingState) *string { return &s.Size }, fixed("e2-small"),
			func(_ *onboardingState, v string) bool { return vmSizes[v] }, "the VM size must be e2-small or e2-medium"},
		{"Backup bucket", func(s *onboardingState) *string { return &s.Bucket }, func(s *onboardingState) string { return s.Project + "-clawbridge-backups" },
			matches(bucketPattern), "the bucket must be a valid, globally unique bucket name"},
	}
}

// askQuestions asks each question, with its fallback, until its answer
// passes its check.
func askQuestions(prompt *wizardPrompt, stdout io.Writer, s *onboardingState, questions []inputQuestion) error {
	for _, q := range questions {
		fallback := q.fallback(s)
		label := q.label + ": "
		if fallback != "" {
			label = q.label + " [" + fallback + "]: "
		}
		v, err := askUntil(prompt.ask, stdout, label, func(a string) (string, error) {
			if a == "" {
				a = fallback
			}
			if !q.ok(s, a) {
				return "", errors.New(q.why)
			}
			return a, nil
		})
		if err != nil {
			return err
		}
		*q.field(s) = v
	}
	return nil
}

// validInputs reports the first input that is not well formed.
func validInputs(s *onboardingState) error {
	var questions []inputQuestion
	switch s.Cloud {
	case "aws":
		if awsGOOS == "windows" {
			return errAWSOnWindows
		}
		if s.Instance != "" && !instancePattern.MatchString(s.Instance) {
			return errors.New("the saved instance is not an EC2 instance ID")
		}
		questions = awsQuestions(nil)
	case "gcp":
		questions = gcpQuestions("")
	default:
		return errors.New("the saved progress names an unknown cloud; remove it to start over")
	}
	for _, q := range questions {
		if !q.ok(s, *q.field(s)) {
			return errors.New(q.why)
		}
	}
	return nil
}

// handoverControls is what the user reads before the agent takes over.
func handoverControls(cloud string) string {
	what := `
What your agent will do: load the clawbridge-provision-gcp playbook and follow it in your project. It
creates a VM, a static IP, a data disk, firewall rules, a backup bucket and a service account, after
showing you the cost and getting your yes, then installs the server.
`
	if cloud == "aws" {
		what = `
What your agent will do: load the clawbridge-provision-aws playbook and follow it in your account. It
creates a network, an HTTPS-only security group, a backup bucket, a role, a data volume, an instance
reached over SSM (no SSH) and an Elastic IP, after showing you the cost and getting your yes, then
installs the server.
`
	}
	return what + handoverRules
}

const handoverRules = `
The real controls: the playbook's rules guide the agent, but what protects you is that you approve
each of its commands, and that ClawBridge's checks, not the agent, decide when a step is done. Keep the
agent in its normal approval mode; avoid bypass or auto-approve modes for this hand-over.

The agent holds this terminal. When it asks you to enter a secret (clawbridge cloud secret set KEY), run that
in a second terminal: secrets never go through the agent.
`

// summary names the saved progress in one line.
func (s *onboardingState) summary() string {
	if s.Cloud == "aws" {
		return "AWS account " + clirender.Safe(s.Account) + ", domain " + clirender.Safe(s.Domain) + ", step " + clirender.Safe(s.Step)
	}
	return "project " + clirender.Safe(s.Project) + ", domain " + clirender.Safe(s.Domain) + ", step " + clirender.Safe(s.Step)
}

// startOwnServer is Start's third choice.
func startOwnServer(ctx context.Context, prompt *wizardPrompt, path string, stdout, stderr io.Writer) error {
	var agents []int
	for i, a := range handoverAgents {
		if _, err := lookAgent(a.bin); err == nil {
			agents = append(agents, i)
		}
	}
	if len(agents) == 0 {
		_, err := io.WriteString(stdout, ownServerText+"\nNo Claude Code or Codex found on PATH, so follow the written guide.\n")
		return err
	}
	state, err := loadOnboarding()
	if err != nil {
		return err
	}
	if state != nil {
		fmt.Fprintf(stdout, "\nSaved progress: %s.\n", state.summary())
		if state.Version != buildinfo.Version {
			fmt.Fprintln(stdout, "It was saved by another ClawBridge version, so the step starts again.")
		}
		answer, err := prompt.ask("Resume it? [Y/n] (n starts over): ")
		if err != nil {
			return err
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a == "n" || a == "no" {
			state = nil
		} else if err = validInputs(state); err != nil {
			// Saved values reach gcloud's argv and TASK.md: check them again.
			return fmt.Errorf("the saved progress is not valid (%w); run clawbridge onboard and start over", err)
		}
	}
	if state == nil {
		cloud, err := askUntil(prompt.ask, stdout, "Which cloud? 1. Google Cloud or 2. AWS [1]: ", func(a string) (string, error) {
			if v, ok := pick(a, "1", cloudChoices); ok {
				return v, nil
			}
			return "", errors.New("choose 1 or 2")
		})
		if err != nil {
			return err
		}
		if cloud == "aws" {
			// Before any question, and long before the agent creates anything.
			if awsGOOS == "windows" {
				return errAWSOnWindows
			}
			state, err = askAWSInputs(ctx, prompt, stdout)
		} else {
			state, err = askInputs(ctx, prompt, stdout)
		}
		if err != nil {
			return err
		}
		state.Step = "provision"
		if err = saveOnboarding(state); err != nil {
			return err
		}
	}
	if state.Step == "signin" {
		return finishSignin(ctx, state, prompt, path, stdout, stderr)
	}
	agent := handoverAgents[agents[0]]
	if len(agents) > 1 {
		answer, err := prompt.ask("Hand over to 1. Claude Code or 2. Codex? [1]: ")
		if err != nil {
			return err
		}
		if strings.TrimSpace(answer) == "2" {
			agent = handoverAgents[agents[1]]
		}
	}
	fmt.Fprint(stdout, handoverControls(state.Cloud))
	answer, err := prompt.ask("Hand over to " + agent.name + " now? [y/N] (n shows the written steps): ")
	if err != nil {
		return err
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		_, err = io.WriteString(stdout, ownServerText)
		return err
	}
	if err = prompt.close(); err != nil {
		return err
	}
	return handOver(ctx, state, agent.bin, agent.skillAgent, path, stdout, stderr)
}

func askInputs(ctx context.Context, prompt *wizardPrompt, stdout io.Writer) (*onboardingState, error) {
	fmt.Fprintln(stdout, "\nYour Google Cloud choices. None of these is a secret.")
	account := ""
	if out, err := runGcloud(ctx, "config", "get", "account"); err == nil {
		account = strings.TrimSpace(string(out))
	}
	s := &onboardingState{Cloud: "gcp"}
	if err := askQuestions(prompt, stdout, s, gcpQuestions(account)); err != nil {
		return nil, err
	}
	return s, nil
}

// taskText is the TASK.md the agent starts from: the step and the user's
// choices, never a secret or a token.
func taskText(s *onboardingState) string {
	return fmt.Sprintf(`# ClawBridge onboarding: provision on Google Cloud

Load the clawbridge-provision-gcp skill and follow its hard rules; say them to
the user first. Work through its steps 1 to 6 with these inputs:

- Google account: %s
- Project: %s
- Region: %s
- Zone: %s
- Domain: %s
- Owner email: %s
- VM size: %s
- Backup bucket: %s

The gcloud configuration %s is active in this shell and already names the
account, project, region and zone; still name them in every command.

Record every resource you create in created.txt in this directory.

Finish by running, and reporting verbatim:

    clawbridge cloud check infra server version --server https://%s --vm clawbridge-1 --project %s --zone %s

Stop there. The user runs the sign-in step (the skill's step 7) themselves,
after you exit.
`, s.Account, s.Project, s.Region, s.Zone, s.Domain, s.Owner, s.Size, s.Bucket, gcloudConfiguration, s.Domain, s.Project, s.Zone)
}

// handOver prepares the agent's working directory, the playbook and a
// dedicated gcloud configuration, runs the agent in this terminal, then runs
// the step's checks itself.
func handOver(ctx context.Context, s *onboardingState, bin, skillAgent, path string, stdout, stderr io.Writer) error {
	if s.Cloud == "aws" {
		return handOverAWS(ctx, s, bin, skillAgent, path, stdout, stderr)
	}
	if err := installPlaybook("gcp", skillAgent); err != nil {
		return fmt.Errorf("the playbook could not be installed for the agent: %w", err)
	}
	if _, err := runGcloud(ctx, "config", "configurations", "describe", gcloudConfiguration); err != nil {
		if _, err = runGcloud(ctx, "config", "configurations", "create", gcloudConfiguration, "--no-activate"); err != nil {
			return errors.New("could not create the gcloud configuration " + gcloudConfiguration + "; is gcloud installed and signed in?")
		}
	}
	for _, kv := range [][2]string{{"account", s.Account}, {"project", s.Project}, {"compute/region", s.Region}, {"compute/zone", s.Zone}} {
		if _, err := runGcloud(ctx, "--configuration="+gcloudConfiguration, "config", "set", kv[0], kv[1]); err != nil {
			return errors.New("could not set " + kv[0] + " in the gcloud configuration " + gcloudConfiguration)
		}
	}
	base, err := onboardingDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "provision")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(taskText(s)), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nStarting %s in %s. Exit it when it is done; ClawBridge then runs the checks.\n\n", bin, dir)
	// G9 F3: the agent gets the user's environment without ClawBridge's
	// own secrets.
	env := append(secretenv.Scrub(os.Environ()), "CLOUDSDK_ACTIVE_CONFIG_NAME="+gcloudConfiguration)
	if err = runAgent(ctx, bin, dir, env, "Read TASK.md in this directory and follow it."); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	// Whatever the agent said or how it exited, the checks decide.
	fmt.Fprintln(stdout, "\nChecking the server…")
	args := []string{"infra", "server", "version", "--server", "https://" + s.Domain, "--vm", "clawbridge-1", "--project", s.Project, "--zone", s.Zone}
	// The checks' gcloud calls use the same configuration as the agent's.
	restore := setEnv("CLOUDSDK_ACTIVE_CONFIG_NAME", gcloudConfiguration)
	err = runHandoverChecks(ctx, args, stdout, stderr)
	restore()
	if err != nil {
		fmt.Fprintln(stdout, "\nNot done yet. Run clawbridge onboard to hand over again (your agent resumes from created.txt),")
		fmt.Fprintln(stdout, "or follow docs/operations/first-deployment.md yourself.")
		return nil
	}
	s.Step = "signin"
	if err = saveOnboarding(s); err != nil {
		return err
	}
	return finishSignin(ctx, s, nil, path, stdout, stderr)
}

// setEnv sets one variable for this process and its children, and returns
// how to put it back.
func setEnv(key, value string) func() {
	old, had := os.LookupEnv(key)
	os.Setenv(key, value)
	return func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	}
}

// finishSignin is the sign-in step. Once the workspace exists, made here
// by cloud bootstrap or by the owner by hand, the path goes on to the first
// project and the offer to open ClawBridge (U48). Otherwise it names the
// step the owner runs in their own terminal; onboard resumes after it.
// prompt may be nil: after a hand-over the agent had the terminal, so it
// opens one.
func finishSignin(ctx context.Context, s *onboardingState, prompt *wizardPrompt, path string, stdout, stderr io.Writer) (retErr error) {
	if prompt == nil {
		var err error
		if prompt, err = newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; the server is up, restore the terminal before retrying"); err != nil {
			return err
		}
		defer func() {
			if e := prompt.close(); e != nil && !errors.Is(retErr, e) {
				retErr = errors.Join(retErr, e)
			}
		}()
	}
	if signedIn(path, s) {
		return firstProject(ctx, prompt, path, stdout, stderr)
	}
	// cloud bootstrap runs over SSM, so only on AWS for now; on Google
	// Cloud the owner runs the written step.
	if s.Cloud == "aws" {
		done, err := offerBootstrap(ctx, s, prompt, path, stdout, stderr)
		if err != nil {
			return err
		}
		if done {
			return firstProject(ctx, prompt, path, stdout, stderr)
		}
	}
	playbook, checks := "clawbridge-provision-gcp", fmt.Sprintf("  clawbridge cloud check workspace\n  clawbridge cloud check backups --bucket %s --project %s\n", s.Bucket, s.Project)
	if s.Cloud == "aws" {
		playbook, checks = "clawbridge-provision-aws", signinChecks(s)
	}
	_, err := fmt.Fprintf(stdout, `
The server is up. Last step, in your own terminal (not your agent's): create the workspace as its owner,
following the sign-in section of the %s skill (references/signin.md). Then check:

%s
Then run clawbridge onboard again: it goes on to your first project and opens ClawBridge.
`, playbook, checks)
	return err
}

// signedIn reports whether the profile at path names a workspace on this
// server: the sign-in step is done.
func signedIn(path string, s *onboardingState) bool {
	c, err := readConfig(path, false)
	return err == nil && c.Mode == "cloud" && c.Workspace != "" && strings.TrimSuffix(c.Server, "/") == "https://"+s.Domain
}

// wizardCreateProject is `clawbridge project create`; tests replace it. It
// is set in init: Run reaches onboarding, which uses it.
var wizardCreateProject func(ctx context.Context, path, project string, stderr io.Writer) error

func init() {
	wizardCreateProject = func(ctx context.Context, path, project string, stderr io.Writer) error {
		return Run(ctx, []string{"--client-config", path, "project", "create", project}, io.Discard, stderr)
	}
}

// firstProject creates the workspace's first project, unless the profile
// already has one, and offers to open ClawBridge.
func firstProject(ctx context.Context, prompt *wizardPrompt, path string, stdout, stderr io.Writer) error {
	c, err := readConfig(path, false)
	if err != nil {
		return err
	}
	if c.Project == "" {
		fmt.Fprintln(stdout, "\nYour workspace is ready. A project is one codebase your agents share; create the first one.")
		fallback := defaultProject()
		project, err := askUntil(prompt.ask, stdout, "First project ["+fallback+"]: ", askProjectSlug(fallback))
		if err != nil {
			return err
		}
		if err = prompt.close(); err != nil {
			return err
		}
		if err = wizardCreateProject(ctx, path, project, stderr); err != nil {
			return fmt.Errorf("the workspace is ready, but creating the project failed (retry: clawbridge project create %s): %w", project, err)
		}
		if err = setProfileValue(path, "project", project); err != nil {
			return fmt.Errorf("project %s is created, but it could not be saved as your default; set project = %q in %s: %w", project, project, path, err)
		}
		fmt.Fprintf(stdout, "Project %s is ready.\n", clirender.Safe(project))
	}
	if err = prompt.close(); err != nil {
		return err
	}
	return openNow(ctx, path, stdout, stderr)
}

func signinChecks(s *onboardingState) string {
	return fmt.Sprintf("  clawbridge cloud check workspace\n  clawbridge cloud check backups --bucket %s --region %s --profile %s\n",
		s.Bucket, s.Region, s.Profile)
}

// cloudBootstrap is the in-process `clawbridge cloud bootstrap`.
var cloudBootstrap = runCloudBootstrap

// offerBootstrap runs cloud bootstrap for the owner when the server offers
// device sign-in and they say yes. done says the step ran and its outcome
// was printed; otherwise the caller prints the written step. Ended input,
// like any other answer but yes, is a no.
func offerBootstrap(ctx context.Context, s *onboardingState, prompt *wizardPrompt, path string, stdout, stderr io.Writer) (done bool, err error) {
	server := "https://" + s.Domain
	if methods, err := wizardMethods(ctx, server); err != nil || !slices.Contains(methods, "device") {
		return false, ctx.Err()
	}
	fmt.Fprintf(stdout, "\nThe server is up and offers device sign-in. ClawBridge can create your workspace now: the server\n"+
		"makes a one-time invite for %s, and this device joins with it (nothing to copy or paste).\n", clirender.Safe(s.Owner))
	workspace, err := askUntil(prompt.ask, stdout, "Workspace name [my-team]: ", func(a string) (string, error) {
		if a == "" {
			a = "my-team"
		}
		if !bootstrapWorkspace(a) {
			return "", errors.New("a workspace name is letters, digits, '.', '_' and '-', starting with a letter or digit")
		}
		return a, nil
	})
	if err != nil {
		// Three wrong names, or ended input: the written step instead.
		if ctx.Err() == nil {
			fmt.Fprintln(stdout, clirender.Safe(err.Error())+".")
		}
		return false, ctx.Err()
	}
	answer, err := prompt.ask("Create the workspace and join this device now? [y/N]: ")
	if err != nil {
		return false, ctx.Err()
	}
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return false, nil
	}
	err = cloudBootstrap(ctx, []string{"--instance", s.Instance, "--region", s.Region, "--profile", s.Profile,
		"--account", s.Account, "--server", server, "--workspace", workspace, "--email", s.Owner, "--client-config", path}, stdout, stderr)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		fmt.Fprintln(stdout, "\nThe workspace was not created: "+clirender.Safe(err.Error()))
		return false, nil
	}
	_, err = fmt.Fprintf(stdout, "\nThe workspace %s is created and this device has joined it. Check:\n\n%s", workspace, signinChecks(s))
	return true, err
}
