package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/selfupdate"
)

func rollbackResult() selfupdate.RollbackResult {
	return selfupdate.RollbackResult{From: "v0.3.10", To: "v0.3.9", Destination: "/opt/fake/clawbridge", SHA256: strings.Repeat("cd", 32), Previous: "v0.3.10"}
}

func holdVersion(t *testing.T) string {
	t.Helper()
	st, err := selfupdate.ReadAutoUpdateState(filepath.Join(os.Getenv("CLAWBRIDGE_HOME"), selfupdate.AutoUpdateFile))
	if err != nil {
		t.Fatal(err)
	}
	return st.HoldVersion
}

// writeSlot keeps content as version in the test state home's slot.
func writeSlot(t *testing.T, version, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAWBRIDGE_HOME"), selfupdate.PreviousDirName)
	name := "clawbridge-" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	b, _ := json.Marshal(selfupdate.Previous{Version: version, SHA256: hex.EncodeToString(sum[:]), File: name})
	if err := os.WriteFile(filepath.Join(dir, "previous.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// FR24 §5: rolling back is a person's command; an agent is refused even
// at a terminal, before anything is read or changed.
func TestSelfUpdateRollbackRefusesAnAgentEvenAtATerminal(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for marker, env := range map[string]string{"CLAUDECODE": "CLAUDECODE=1", "CODEX_SANDBOX": "CODEX_SANDBOX=x", "CLAWBRIDGE_SESSION": "CLAWBRIDGE_SESSION=s", "CLAWBRIDGE_INSTANCE": "CLAWBRIDGE_INSTANCE=i"} {
			f := &suFake{rollbackResult: rollbackResult()}
			fakeSelfUpdate(t, "v0.3.10", f)
			typing(t, terminal, "v0.3.9\n", env)
			stdout, _, err := runSU(t, "self-update", "--rollback")
			if err == nil || !strings.Contains(err.Error(), marker+" says an agent runs this command; nothing was changed") ||
				!strings.Contains(err.Error(), "`clawbridge self-update --rollback`") {
				t.Fatal(marker, terminal, err)
			}
			if len(f.rollbackOpts) != 0 || len(f.skillPaths) != 0 || stdout != "" || holdVersion(t) != "" {
				t.Fatal("nothing may change", marker, terminal, f)
			}
		}
	}
}

func TestSelfUpdateRollbackReportHoldAndSkill(t *testing.T) {
	f := &suFake{rollbackResult: rollbackResult()}
	fakeSelfUpdate(t, "v0.3.10", f)
	typing(t, true, "")
	stdout, stderr, err := runSU(t, "self-update", "--rollback")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.rollbackOpts) != 1 || f.rollbackOpts[0].Current != "v0.3.10" ||
		f.rollbackOpts[0].PreviousDir != filepath.Join(os.Getenv("CLAWBRIDGE_HOME"), "previous") {
		t.Fatal(f.rollbackOpts)
	}
	if hold := holdVersion(t); hold != "v0.3.10" {
		t.Fatalf("hold_version = %q, want the version rolled back from", hold)
	}
	var got struct {
		Source string                    `json:"source"`
		Data   selfupdate.RollbackResult `json:"data"`
	}
	want := rollbackResult()
	want.HoldVersion = "v0.3.10"
	if json.Unmarshal([]byte(stdout), &got) != nil || got.Source != "self_update_rollback" || got.Data != want {
		t.Fatal(stdout)
	}
	for _, line := range []string{
		"rolled back v0.3.10 → v0.3.9 at /opt/fake/clawbridge",
		"sha256 " + want.SHA256,
		"v0.3.10 is held: automatic updates won't reinstall it",
		"v0.3.10 is kept: clawbridge self-update --rollback again puts it back",
		"agents' running MCP servers (clawbridge mcp) keep the old version until their session restarts",
	} {
		if !strings.Contains(stderr, line) {
			t.Fatal(line, stderr)
		}
	}
	// The skill is reinstalled by the rolled-back binary.
	if len(f.skillPaths) != 1 || f.skillPaths[0] != "/opt/fake/clawbridge" {
		t.Fatal(f.skillPaths)
	}

	// --no-skill skips it; a script (no terminal, no marker) may roll back.
	f = &suFake{rollbackResult: rollbackResult()}
	fakeSelfUpdate(t, "v0.3.10", f)
	typing(t, false, "")
	if _, _, err := runSU(t, "upgrade", "--rollback", "--no-skill"); err != nil || len(f.rollbackOpts) != 1 || len(f.skillPaths) != 0 {
		t.Fatal(err, f)
	}

	// The slot couldn't be kept after the rollback: the warning is shown.
	f = &suFake{rollbackResult: rollbackResult()}
	f.rollbackResult.Previous, f.rollbackResult.PreviousWarning = "", "rollback isn't available: boom"
	fakeSelfUpdate(t, "v0.3.10", f)
	typing(t, true, "")
	if _, stderr, err := runSU(t, "self-update", "--rollback"); err != nil || !strings.Contains(stderr, "rollback isn't available: boom") || strings.Contains(stderr, "again puts it back") {
		t.Fatal(err, stderr)
	}
}

func TestSelfUpdateRollbackFailureChangesNothingElse(t *testing.T) {
	f := &suFake{rollbackErr: errors.New("nothing was changed; the kept v0.3.9 doesn't match the sha256 in previous.json")}
	fakeSelfUpdate(t, "v0.3.10", f)
	typing(t, true, "")
	stdout, stderr, err := runSU(t, "self-update", "--rollback")
	if err == nil || !strings.Contains(err.Error(), "doesn't match the sha256") || stdout != "" || strings.Contains(stderr, "rolled back") {
		t.Fatal(err, stdout, stderr)
	}
	if holdVersion(t) != "" || len(f.skillPaths) != 0 {
		t.Fatal("a failed rollback held a version or reinstalled the skill")
	}
}

func TestSelfUpdateRollbackHoldFailureOnlyWarns(t *testing.T) {
	f := &suFake{rollbackResult: rollbackResult()}
	fakeSelfUpdate(t, "v0.3.10", f)
	typing(t, true, "")
	path := filepath.Join(os.Getenv("CLAWBRIDGE_HOME"), "auto-update.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runSU(t, "self-update", "--rollback")
	if err != nil || !strings.Contains(stderr, "v0.3.10 was not held in auto-update.json") || strings.Contains(stdout, "hold_version") {
		t.Fatal(err, stdout, stderr)
	}
	// A development build has no version to hold.
	f = &suFake{rollbackResult: rollbackResult()}
	f.rollbackResult.From = "dev"
	fakeSelfUpdate(t, "dev", f)
	typing(t, true, "")
	if _, stderr, err := runSU(t, "self-update", "--rollback"); err != nil || !strings.Contains(stderr, "dev was not held") || holdVersion(t) != "" {
		t.Fatal(err, stderr)
	}
}

func TestSelfUpdateRollbackTakesOnlyNoSkill(t *testing.T) {
	for _, args := range [][]string{
		{"--rollback", "--check"}, {"--rollback", "--version", "v0.3.8"}, {"--rollback", "--verify-attestation"}, {"--rollback", "extra"},
	} {
		f := &suFake{rollbackResult: rollbackResult()}
		fakeSelfUpdate(t, "v0.3.10", f)
		typing(t, true, "")
		if _, _, err := runSU(t, append([]string{"self-update"}, args...)...); err == nil || len(f.rollbackOpts) != 0 || len(f.planOpts) != 0 {
			t.Fatal(args, err)
		}
	}
}

func TestSelfUpdateCheckSaysWhetherARollbackIsAvailable(t *testing.T) {
	check := func(t *testing.T) (selfupdate.Plan, string) {
		t.Helper()
		stdout, stderr, err := runSU(t, "self-update", "--check")
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Data selfupdate.Plan `json:"data"`
		}
		if json.Unmarshal([]byte(stdout), &got) != nil {
			t.Fatal(stdout)
		}
		return got.Data, stderr
	}
	f := &suFake{plan: updatePlan()}
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, false, "", "CLAUDECODE=1") // an agent may check
	writeSlot(t, "v0.3.8", "kept binary")
	plan, stderr := check(t)
	if plan.Rollback != "v0.3.8" || !strings.Contains(stderr, "rollback available: clawbridge self-update --rollback restores v0.3.8") {
		t.Fatal(plan, stderr)
	}

	for name, setup := range map[string]func(t *testing.T){
		"no slot":         func(t *testing.T) {},
		"the running one": func(t *testing.T) { writeSlot(t, "v0.3.9", "kept binary") },
		"tampered": func(t *testing.T) {
			writeSlot(t, "v0.3.8", "kept binary")
			name := "clawbridge-v0.3.8"
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			if err := os.WriteFile(filepath.Join(os.Getenv("CLAWBRIDGE_HOME"), "previous", name), []byte("other"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		f := &suFake{plan: updatePlan()}
		fakeSelfUpdate(t, "v0.3.9", f)
		typing(t, false, "")
		setup(t)
		plan, stderr := check(t)
		if plan.Rollback != "" || strings.Contains(stderr, "rollback available") {
			t.Fatal(name, plan, stderr)
		}
	}
}

func TestSelfUpdateKeepsTheReplacedBinaryAndSaysSo(t *testing.T) {
	f := &suFake{plan: updatePlan(), result: updateResult()}
	f.result.Previous = "v0.3.9"
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	_, stderr, err := runSU(t, "self-update")
	if err != nil || !strings.Contains(stderr, "v0.3.9 is kept: clawbridge self-update --rollback puts it back") {
		t.Fatal(err, stderr)
	}
	if len(f.applyOpts) != 1 || f.applyOpts[0].PreviousDir != filepath.Join(os.Getenv("CLAWBRIDGE_HOME"), "previous") {
		t.Fatal(f.applyOpts)
	}
	// Failing to keep it only warns.
	f = &suFake{plan: updatePlan(), result: updateResult()}
	f.result.PreviousWarning = "rollback isn't available: the replaced binary could not be kept"
	fakeSelfUpdate(t, "v0.3.9", f)
	typing(t, true, "")
	if _, stderr, err := runSU(t, "self-update"); err != nil || !strings.Contains(stderr, f.result.PreviousWarning) || strings.Contains(stderr, "is kept") {
		t.Fatal(err, stderr)
	}
}
