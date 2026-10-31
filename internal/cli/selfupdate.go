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
	"path/filepath"
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
	selfUpdatePlan     = selfupdate.PlanUpdate
	selfUpdateApply    = selfupdate.Apply
	selfUpdateRollback = selfupdate.Rollback
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

const selfUpdateUsage = "usage: clawbridge self-update [--check] [--version vX.Y.Z] [--verify-attestation] [--no-skill], or clawbridge self-update --rollback [--no-skill]  (alias: upgrade)"

// previousDir is the rollback slot, and autoUpdatePath the automatic-update
// state (FR24 §1 and §5), both in the client's state home beside
// config.toml.
func previousDir() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, selfupdate.PreviousDirName), nil
}

func autoUpdatePath() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, selfupdate.AutoUpdateFile), nil
}

// rollbackVersion is the version `self-update --rollback` would restore:
// the kept binary when its SHA-256 matches and it isn't the running
// version; "" otherwise.
func rollbackVersion() string {
	dir, err := previousDir()
	if err != nil {
		return ""
	}
	prev, err := selfupdate.CheckPrevious(dir)
	if err != nil || prev.Version == buildinfo.Version {
		return ""
	}
	return prev.Version
}

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
	rollback := f.Bool("rollback", false, "put back the binary the last update replaced (a person's command)")
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
	if *rollback {
		if *check || *version != "" || *verify {
			return errors.New("--rollback takes only --no-skill")
		}
		return runSelfUpdateRollback(ctx, *noSkill, stdout, stderr)
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
	// Without a state home the update still runs; it keeps no rollback.
	slot, _ := previousDir()
	opts := selfupdate.Options{Current: buildinfo.Version, Version: *version, Attest: *verify, PreviousDir: slot}
	plan, err := selfUpdatePlan(ctx, opts)
	if err != nil {
		return err
	}
	if *check {
		plan.Rollback = rollbackVersion()
		if err := emit(ctx, stdout, "self_update_check", selfUpdateOutput{Source: "self_update_check", Data: plan}); err != nil {
			return err
		}
		fmt.Fprintln(stderr, planLine(plan, true))
		if plan.Rollback != "" {
			fmt.Fprintf(stderr, "rollback available: clawbridge self-update --rollback restores %s\n", plan.Rollback)
		}
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
	switch {
	case result.Previous != "":
		fmt.Fprintf(stderr, "%s is kept: clawbridge self-update --rollback puts it back\n", result.Previous)
	case result.PreviousWarning != "":
		fmt.Fprintln(stderr, result.PreviousWarning)
	}
	if !*noSkill {
		reinstallSkill(ctx, result.Destination, stderr)
	}
	if server := selfUpdateServerVersion(ctx); versionOlder(server, result.To) {
		fmt.Fprintf(stderr, "your server runs %s; the client is now %s; see clawbridge doctor\n", server, result.To)
	}
	fmt.Fprintln(stderr, "agents' running MCP servers (clawbridge mcp) keep the old version until their session restarts")
	return nil
}

// reinstallSkill runs binary's `skill install`; a failure only warns.
func reinstallSkill(ctx context.Context, binary string, stderr io.Writer) {
	if err := selfUpdateSkill(ctx, binary, stderr); err != nil {
		fmt.Fprintln(stderr, "the agent skill was not installed; run `clawbridge skill install --agent claude-code|codex|all` later.")
	}
}

// runSelfUpdateRollback is `self-update --rollback` (FR24 §5): the binary
// the last update replaced goes back in place. It is a person's command:
// refused under any agent's environment, at a terminal too, since an agent
// must not undo a person's upgrade. The version rolled back from is held in
// auto-update.json, so automatic updates don't reinstall it.
func runSelfUpdateRollback(ctx context.Context, noSkill bool, stdout, stderr io.Writer) error {
	if marker := agentMarker(closesEnv()); marker != "" {
		return fmt.Errorf("rolling ClawBridge back is a person's decision, and %s says an agent runs this command; nothing was changed. Tell your human to run `clawbridge self-update --rollback` in their terminal", marker)
	}
	dir, err := previousDir()
	if err != nil {
		return fmt.Errorf("nothing was changed; find the rollback slot: %w", err)
	}
	result, err := selfUpdateRollback(ctx, selfupdate.Options{Current: buildinfo.Version, PreviousDir: dir})
	if err != nil {
		return err
	}
	var holdErr error
	if !releaseTag.MatchString(result.From) {
		holdErr = errors.New("the running version is a development build")
	} else if path, err := autoUpdatePath(); err != nil {
		holdErr = err
	} else if holdErr = selfupdate.SetHoldVersion(path, result.From); holdErr == nil {
		result.HoldVersion = result.From
	}
	if err := emit(ctx, stdout, "self_update_rollback", selfUpdateOutput{Source: "self_update_rollback", Data: result}); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "rolled back %s → %s at %s\n", result.From, result.To, result.Destination)
	fmt.Fprintf(stderr, "sha256 %s (matches the one recorded when it was kept)\n", result.SHA256)
	if result.Aside != "" {
		fmt.Fprintf(stderr, "the replaced binary is at %s until running clawbridge processes exit\n", result.Aside)
	}
	if holdErr != nil {
		fmt.Fprintf(stderr, "%s was not held in %s (%v)\n", result.From, selfupdate.AutoUpdateFile, holdErr)
	} else {
		fmt.Fprintf(stderr, "%s is held: automatic updates won't reinstall it\n", result.From)
	}
	switch {
	case result.Previous != "":
		fmt.Fprintf(stderr, "%s is kept: clawbridge self-update --rollback again puts it back\n", result.Previous)
	case result.PreviousWarning != "":
		fmt.Fprintln(stderr, result.PreviousWarning)
	}
	if !noSkill {
		reinstallSkill(ctx, result.Destination, stderr)
	}
	fmt.Fprintln(stderr, "agents' running MCP servers (clawbridge mcp) keep the old version until their session restarts")
	return nil
}
