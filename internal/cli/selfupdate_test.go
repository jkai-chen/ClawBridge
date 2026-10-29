package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/selfupdate"
)

// suFake stands in for the selfupdate package, the new binary and the
// server. It records every call.
type suFake struct {
	plan       selfupdate.Plan
	result     selfupdate.Result
	planErr    error
	applyErr   error
	skillErr   error
	server     string
	planOpts   []selfupdate.Options
	applyOpts  []selfupdate.Options
	applies    int
	skillPaths []string
	serverAsks int
}

func fakeSelfUpdate(t *testing.T, current string, f *suFake) {
	t.Helper()
	oldPlan, oldApply, oldSkill, oldServer, oldVersion := selfUpdatePlan, selfUpdateApply, selfUpdateSkill, selfUpdateServerVersion, buildinfo.Version
	t.Cleanup(func() {
		selfUpdatePlan, selfUpdateApply, selfUpdateSkill, selfUpdateServerVersion, buildinfo.Version = oldPlan, oldApply, oldSkill, oldServer, oldVersion
	})
	t.Setenv("CLAWBRIDGE_VERIFY", "")
	buildinfo.Version = current
	selfUpdatePlan = func(_ context.Context, o selfupdate.Options) (selfupdate.Plan, error) {
		f.planOpts = append(f.planOpts, o)
		return f.plan, f.planErr
	}
	selfUpdateApply = func(_ context.Context, _ selfupdate.Plan, o selfupdate.Options) (selfupdate.Result, error) {
		f.applies++
		f.applyOpts = append(f.applyOpts, o)
		return f.result, f.applyErr
	}
	selfUpdateSkill = func(_ context.Context, binary string, _ io.Writer) error {
		f.skillPaths = append(f.skillPaths, binary)
		return f.skillErr
	}
	selfUpdateServerVersion = func(context.Context) string { f.serverAsks++; return f.server }
}

func updatePlan() selfupdate.Plan {
	return selfupdate.Plan{Current: "v0.3.9", Target: "v0.3.10", Action: selfupdate.Update, Asset: "clawbridge_v0.3.10_linux_amd64", Destination: "/opt/fake/clawbridge"}
}

func updateResult() selfupdate.Result {
	return selfupdate.Result{From: "v0.3.9", To: "v0.3.10", Destination: "/opt/fake/clawbridge", SHA256: strings.Repeat("ab", 32), Attestation: "not_checked"}
}

func runSU(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = Run(context.Background(), args, &out, &errb)
	return out.String(), errb.String(), err
}

func TestSelfUpdateCheckReportsEveryAction(t *testing.T) {
	for name, c := range map[string]struct {
		action  selfupdate.Action
		plan    selfupdate.Plan
		current string
		line    string
	}{
		"update":     {selfupdate.Update, updatePlan(), "v0.3.9", "update available: v0.3.9 → v0.3.10 (run clawbridge self-update)"},
		"up to date": {selfupdate.UpToDate, selfupdate.Plan{Current: "v0.3.9", Target: "v0.3.9", Action: selfupdate.UpToDate}, "v0.3.9", "up to date (v0.3.9)"},
		"older":      {selfupdate.DowngradeRefused, selfupdate.Plan{Current: "v0.3.9", Target: "v0.3.8", Action: selfupdate.DowngradeRefused, Downgrade: true}, "v0.3.9", "v0.3.8 is older than the running v0.3.9; name it with --version to install it"},
		"dev build":  {selfupdate.DevBuild, selfupdate.Plan{Current: "dev", Target: "v0.3.10", Action: selfupdate.DevBuild}, "dev", "a development build; name a release with --version"},
	} {
		for _, environ := range [][]string{nil, {"CLAUDECODE=1"}} {
			f := &suFake{plan: c.plan}
			fakeSelfUpdate(t, c.current, f)
			typing(t, false, "", environ...) // no terminal; an agent may still check
			stdout, stderr, err := runSU(t, "self-update", "--check")
			if err != nil {
				t.Fatal(name, err)
			}
			var got struct {
				Source string          `json:"source"`
				Data   selfupdate.Plan `json:"data"`
			}
			if json.Unmarshal([]byte(stdout), &got) != nil || got.Data != c.plan || got.Source != "self_update_check" {
				t.Fatal(name, stdout)
			}
			if strings.TrimSpace(stderr) != c.line {
				t.Fatal(name, stderr)
			}
			if f.applies != 0 || len(f.skillPaths) != 0 || len(f.planOpts) != 1 || f.planOpts[0].Current != c.current {
				t.Fatal(name, f)
			}
		}
	}
}

func TestSelfUpdateAgentWithoutTerminalIsRefusedBeforeAnything(t *testing.T) {
	for marker, env := range map[string]string{"CLAUDECODE": "CLAUDECODE=1", "CODEX_SANDBOX": "CODEX_SANDBOX=x", "CLAWBRIDGE_SESSION": "CLAWBRIDGE_SESSION=s"} {
		f := &suFake{plan: updatePlan(), result: updateResult()}
		fakeSelfUpdate(t, "v0.3.9", f)
		typing(t, false, "v0.3.10\n", env)
		_, _, err := runSU(t, "self-update")
		if err == nil || !strings.Contains(err.Error(), marker+" says an agent runs this command") || !strings.Contains(err.Error(), "tell your human to run `clawbridge self-update` in their terminal") {
			t.Fatal(marker, err)
		}
		if len(f.planOpts) != 0 || f.applies != 0 {
			t.Fatal("nothing may be planned or applied", marker, f)
		}
	}
}

func TestSelfUpdateAgentOnTerminalTypesTheVersion(t *testing.T) {
	f := &suFake{plan: updatePlan(), result: updateResult()}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "v0.3.10\n", "CLAUDECODE=1")
	_, stderr, err := runSU(t, "self-update")
	if err != nil || f.applies != 1 || !strings.Contains(stderr, "Type v0.3.10 to install it: ") {
		t.Fatal(err, f.applies, stderr)
	}
	for name, input := range map[string]string{"wrong version": "v0.3.9\n", "yes": "y\n", "prefix": "v0.3.1\n", "extra": "v0.3.10 \n", "nothing": ""} {
		f := &suFake{plan: updatePlan(), result: updateResult()}
		fakeSelfUpdate(t, "v0.3.9", f)
		typing(t, true, input, "CLAUDECODE=1")
		_, _, err := runSU(t, "self-update")
		if err == nil || f.applies != 0 {
			t.Fatal(name, err, f.applies)
		}
		if name != "nothing" && !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatal(name, err)
		}
	}
}

func TestSelfUpdateAgentIsNotAskedWhenThereIsNothingToInstall(t *testing.T) {
	f := &suFake{plan: selfupdate.Plan{Current: "v0.3.9", Target: "v0.3.9", Action: selfupdate.UpToDate}}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "", "CLAUDECODE=1")
	_, stderr, err := runSU(t, "self-update")
	if err != nil || strings.Contains(stderr, "Type ") || f.applies != 0 {
		t.Fatal(err, stderr)
	}
}

func TestSelfUpdateAPersonsTerminalIsNotPrompted(t *testing.T) {
	f := &suFake{plan: updatePlan(), result: updateResult()}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	_, stderr, err := runSU(t, "self-update")
	if err != nil || f.applies != 1 || strings.Contains(stderr, "Type ") {
		t.Fatal(err, f.applies, stderr)
	}
	// Not a terminal and no marker (a script) also upgrades, as install.sh does.
	f = &suFake{plan: updatePlan(), result: updateResult()}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, false, "")
	if _, _, err := runSU(t, "upgrade"); err != nil || f.applies != 1 {
		t.Fatal("upgrade alias", err, f.applies)
	}
}

func TestSelfUpdateActionsOtherThanUpdate(t *testing.T) {
	f := &suFake{plan: selfupdate.Plan{Current: "v0.3.9", Target: "v0.3.9", Action: selfupdate.UpToDate}}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	stdout, stderr, err := runSU(t, "self-update")
	if err != nil || f.applies != 0 || !strings.Contains(stdout, `"up_to_date"`) || strings.TrimSpace(stderr) != "up to date (v0.3.9)" {
		t.Fatal(err, f.applies, stdout, stderr)
	}
	for _, p := range []selfupdate.Plan{
		{Current: "v0.3.9", Target: "v0.3.8", Action: selfupdate.DowngradeRefused},
		{Current: "dev", Target: "v0.3.10", Action: selfupdate.DevBuild},
	} {
		f := &suFake{plan: p}
		fakeSelfUpdate(t, p.Current, f)
		typing(t, true, "")
		stdout, _, err := runSU(t, "self-update")
		if err == nil || f.applies != 0 || !strings.Contains(stdout, string(p.Action)) {
			t.Fatal(p.Action, err, f.applies, stdout)
		}
	}
	// A planning failure is returned.
	f = &suFake{planErr: errors.New("boom")}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	if _, _, err := runSU(t, "self-update"); err == nil || f.applies != 0 {
		t.Fatal(err)
	}
}

func TestSelfUpdateVersionAndAttestationWiring(t *testing.T) {
	for name, c := range map[string]struct {
		args   []string
		verify string
		attest bool
	}{
		"none":         {nil, "", false},
		"flag":         {[]string{"--verify-attestation"}, "", true},
		"env":          {nil, "required", true},
		"version":      {[]string{"--version", "v0.3.8"}, "", false},
		"flag and env": {[]string{"--verify-attestation"}, "required", true},
	} {
		f := &suFake{plan: updatePlan(), result: updateResult()}
		fakeSelfUpdate(t, "v0.3.9", f)
		t.Setenv("CLAWBRIDGE_VERIFY", c.verify)
		typing(t, true, "")
		if _, _, err := runSU(t, append([]string{"self-update"}, c.args...)...); err != nil {
			t.Fatal(name, err)
		}
		want := ""
		if len(c.args) == 2 && c.args[0] == "--version" {
			want = c.args[1]
		}
		for _, o := range append(f.planOpts, f.applyOpts...) {
			if o.Attest != c.attest || o.Version != want || o.Current != "v0.3.9" {
				t.Fatal(name, o)
			}
		}
		if len(f.planOpts) != 1 || len(f.applyOpts) != 1 {
			t.Fatal(name)
		}
	}
	for _, bad := range []string{"1", "yes", "Required", "required "} {
		f := &suFake{plan: updatePlan(), result: updateResult()}
		fakeSelfUpdate(t, "v0.3.9", f)
		t.Setenv("CLAWBRIDGE_VERIFY", bad)
		typing(t, true, "")
		if _, _, err := runSU(t, "self-update"); err == nil || !strings.Contains(err.Error(), "CLAWBRIDGE_VERIFY") || len(f.planOpts) != 0 || f.applies != 0 {
			t.Fatal(bad, err)
		}
	}
	// A version that is not a release tag, and stray arguments, are refused.
	for _, args := range [][]string{{"self-update", "--version", "latest"}, {"self-update", "--version", "v1.2"}, {"self-update", "extra"}} {
		f := &suFake{plan: updatePlan()}
		fakeSelfUpdate(t, "v0.3.9", f)
		if _, _, err := runSU(t, args...); err == nil || len(f.planOpts) != 0 {
			t.Fatal(args, err)
		}
	}
}

func TestSelfUpdateReportAndSkillRerun(t *testing.T) {
	f := &suFake{plan: updatePlan(), result: updateResult()}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	stdout, stderr, err := runSU(t, "self-update")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data selfupdate.Result `json:"data"`
	}
	if json.Unmarshal([]byte(stdout), &got) != nil || got.Data != f.result {
		t.Fatal(stdout)
	}
	for _, want := range []string{
		"updated v0.3.9 → v0.3.10 at /opt/fake/clawbridge",
		"sha256 " + f.result.SHA256,
		"build provenance: not checked (add --verify-attestation; needs gh)",
		"agents' running MCP servers (clawbridge mcp) keep the old version until their session restarts",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatal(want, stderr)
		}
	}
	if strings.Contains(stderr, "the old binary is at") || strings.Contains(stderr, "agent skill was not installed") || strings.Contains(stderr, "your server runs") {
		t.Fatal(stderr)
	}
	// The skill is installed by the NEW binary, at the destination.
	if len(f.skillPaths) != 1 || f.skillPaths[0] != "/opt/fake/clawbridge" {
		t.Fatal(f.skillPaths)
	}

	// Verified, with the Windows aside.
	f = &suFake{plan: updatePlan(), result: updateResult()}
	f.result.Attestation, f.result.Aside = "verified", `C:\Temp\clawbridge-previous-1.exe.old`
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	_, stderr, _ = runSU(t, "self-update", "--verify-attestation")
	if !strings.Contains(stderr, "build provenance: verified") || strings.Contains(stderr, "not checked") ||
		!strings.Contains(stderr, `the old binary is at C:\Temp\clawbridge-previous-1.exe.old until running clawbridge processes exit`) {
		t.Fatal(stderr)
	}

	// A failing skill install only warns; the update stands.
	f = &suFake{plan: updatePlan(), result: updateResult(), skillErr: errors.New("exit 1")}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	_, stderr, err = runSU(t, "self-update")
	if err != nil || !strings.Contains(stderr, "the agent skill was not installed; run `clawbridge skill install --agent claude-code|codex|all` later.") {
		t.Fatal(err, stderr)
	}

	// --no-skill skips it.
	f = &suFake{plan: updatePlan(), result: updateResult()}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	if _, _, err = runSU(t, "self-update", "--no-skill"); err != nil || len(f.skillPaths) != 0 {
		t.Fatal(err, f.skillPaths)
	}

	// An Apply failure is returned and nothing is reported or run after it.
	f = &suFake{plan: updatePlan(), applyErr: errors.New("checksum mismatch")}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	stdout, stderr, err = runSU(t, "self-update")
	if err == nil || stdout != "" || strings.Contains(stderr, "updated") || len(f.skillPaths) != 0 {
		t.Fatal(err, stdout, stderr)
	}
}

func TestSelfUpdateServerLine(t *testing.T) {
	for name, c := range map[string]struct {
		server string
		line   bool
	}{
		"older":                   {"v0.3.8", true},
		"older minor numerically": {"v0.3.9", true},
		"same":                    {"v0.3.10", false},
		"newer":                   {"v0.4.0", false},
		"offline":                 {"", false},
		"dev":                     {"dev", false},
	} {
		f := &suFake{plan: updatePlan(), result: updateResult(), server: c.server}
		fakeSelfUpdate(t, "v0.3.9", f)
		typing(t, true, "")
		_, stderr, err := runSU(t, "self-update")
		if err != nil || f.serverAsks != 1 {
			t.Fatal(name, err)
		}
		want := "your server runs " + c.server + "; the client is now v0.3.10; see clawbridge doctor"
		if strings.Contains(stderr, want) != c.line {
			t.Fatal(name, stderr)
		}
	}
	// The default read is silent without a configured server.
	t.Setenv("HOME", t.TempDir())
	if v := hostedServerVersion(context.Background()); v != "" {
		t.Fatal(v)
	}
}

func TestVersionOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.3.9", "v0.3.10", true}, {"v0.3.10", "v0.3.9", false}, {"v1.0.0", "v0.9.9", false},
		{"v0.3.9", "v0.3.9", false}, {"dev", "v0.3.9", false}, {"v0.3.9", "dev", false}, {"", "v0.3.9", false},
		{"v0.3.9-rc1", "v0.3.10", false},
	} {
		if versionOlder(c.a, c.b) != c.want {
			t.Fatal(c)
		}
	}
}

func TestSelfUpdateIsListedInHelp(t *testing.T) {
	if !strings.Contains(Usage, "self-update [--check]") || !strings.Contains(Usage, "alias upgrade") {
		t.Fatal("usage does not list self-update")
	}
}
