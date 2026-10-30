package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/selfupdate"
)

// FR12 (live/FR12-PLAN.md): `clawbridge self-update` replaces this binary
// with a GitHub release. The package does the work; this file is the
// command: flags, the agent guard, the report and the skill re-run. The
// package, the new binary's runner and the server's version are seams so
// tests make no request and run no binary.
var (
	selfUpdatePlan  = selfupdate.PlanUpdate
	selfUpdateApply = selfupdate.Apply
	// selfUpdateSkill runs the new binary's `skill install`.
	selfUpdateSkill = func(ctx context.Context, binary string, stderr io.Writer) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "skill", "install")
		cmd.Stdout, cmd.Stderr = io.Discard, stderr
		return cmd.Run()
	}
	// selfUpdateServerVersion reads the configured hosted server's version
	// from /healthz; "" when there is none or it can't be read.
	selfUpdateServerVersion = hostedServerVersion
)

var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

const selfUpdateUsage = "usage: clawbridge self-update [--check] [--version vX.Y.Z] [--verify-attestation] [--no-skill]  (alias: upgrade)"

// versionOlder reports whether canonical release a is older than b; false
// when either is not a canonical vX.Y.Z (a development build).
func versionOlder(a, b string) bool {
	ma, mb := releaseTag.FindStringSubmatch(a), releaseTag.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

// hostedServerVersion is the version the configured hosted server reports,
// read with the doctor's /healthz read under a 5 s limit.
func hostedServerVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, _, err := resolveConfig(ctx, Config{})
	if err != nil || cfg.Server == "" {
		return ""
	}
	p := &cloudProbe{server: cfg.Server}
	version, r := p.health(ctx)
	if r.Status != "pass" {
		return ""
	}
	return version
}

type selfUpdateOutput struct {
	Source string `json:"source"`
	Data   any    `json:"data"`
}

// planLine is the human line for a plan; "" for an update being carried out.
func planLine(p selfupdate.Plan, check bool) string {
	switch p.Action {
	case selfupdate.Update:
		if check {
			return fmt.Sprintf("update available: %s → %s (run clawbridge self-update)", p.Current, p.Target)
		}
	case selfupdate.UpToDate:
		return fmt.Sprintf("up to date (%s)", p.Current)
	case selfupdate.DowngradeRefused:
		return fmt.Sprintf("%s is older than the running %s; name it with --version to install it", p.Target, p.Current)
	case selfupdate.DevBuild:
		return "a development build; name a release with --version"
	}
	return ""
}

func runSelfUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("self-update", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.Usage = func() { fmt.Fprintln(stderr, selfUpdateUsage) }
	check := f.Bool("check", false, "report the running and latest versions; change nothing")
	version := f.String("version", "", "install this release (vX.Y.Z); the only way to go to an older one")
	verify := f.Bool("verify-attestation", false, "check the build provenance (the release's Sigstore bundle, or gh before v0.3.13); install nothing if it fails")
	noSkill := f.Bool("no-skill", false, "do not re-run skill install afterwards")
	f.Bool("json", true, "return JSON (the default output format)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New(selfUpdateUsage)
	}
	if *version != "" && !releaseTag.MatchString(*version) {
		return fmt.Errorf("--version must be a release tag such as v1.2.3, not %q", *version)
	}
	switch v := os.Getenv("CLAWBRIDGE_VERIFY"); v {
	case "":
	case "required":
		*verify = true
	default:
		return errors.New(`CLAWBRIDGE_VERIFY must be unset or "required"`)
	}
	marker := ""
	if !*check {
		if marker = agentMarker(closesEnv()); marker != "" && !stdinTerminal() {
			return fmt.Errorf("upgrading ClawBridge is a person's decision, and %s says an agent runs this command; tell your human to run `clawbridge self-update` in their terminal", marker)
		}
	}
	opts := selfupdate.Options{Current: buildinfo.Version, Version: *version, Attest: *verify}
	plan, err := selfUpdatePlan(ctx, opts)
	if err != nil {
		return err
	}
	if *check {
		if err := emit(ctx, stdout, "self_update_check", selfUpdateOutput{Source: "self_update_check", Data: plan}); err != nil {
			return err
		}
		fmt.Fprintln(stderr, planLine(plan, true))
		return nil
	}
	if plan.Action != selfupdate.Update {
		if err := emit(ctx, stdout, "self_update_check", selfUpdateOutput{Source: "self_update_check", Data: plan}); err != nil {
			return err
		}
		line := planLine(plan, false)
		if plan.Action == selfupdate.UpToDate {
			fmt.Fprintln(stderr, line)
			return nil
		}
		return errors.New(line)
	}
	if marker != "" {
		fmt.Fprintf(stderr, "%s\nType %s to install it: ", planLine(plan, true), plan.Target)
		line, err := bufio.NewReader(closesStdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no confirmation was typed; nothing was changed")
		}
		if strings.TrimRight(line, "\r\n") != plan.Target {
			return errors.New("nothing was changed")
		}
	}
	result, err := selfUpdateApply(ctx, plan, opts)
	if err != nil {
		return err
	}
	if err := emit(ctx, stdout, "self_update", selfUpdateOutput{Source: "self_update", Data: result}); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "updated %s → %s at %s\n", result.From, result.To, result.Destination)
	fmt.Fprintf(stderr, "sha256 %s\n", result.SHA256)
	switch {
	case result.Attestation == "verified" && result.AttestationMethod == "bundle":
		fmt.Fprintln(stderr, "build provenance: verified (the release's Sigstore bundle, checked in-process)")
	case result.Attestation == "verified" && result.AttestationMethod == "gh":
		fmt.Fprintln(stderr, "build provenance: verified (gh attestation verify)")
	case result.Attestation == "verified":
		fmt.Fprintln(stderr, "build provenance: verified")
	default:
		fmt.Fprintln(stderr, "build provenance: not checked (add --verify-attestation)")
	}
	if result.Aside != "" {
		fmt.Fprintf(stderr, "the old binary is at %s until running clawbridge processes exit\n", result.Aside)
	}
	if !*noSkill {
		if err := selfUpdateSkill(ctx, result.Destination, stderr); err != nil {
			fmt.Fprintln(stderr, "the agent skill was not installed; run `clawbridge skill install --agent claude-code|codex|all` later.")
		}
	}
	if server := selfUpdateServerVersion(ctx); versionOlder(server, result.To) {
		fmt.Fprintf(stderr, "your server runs %s; the client is now %s; see clawbridge doctor\n", server, result.To)
	}
	fmt.Fprintln(stderr, "agents' running MCP servers (clawbridge mcp) keep the old version until their session restarts")
	return nil
}
