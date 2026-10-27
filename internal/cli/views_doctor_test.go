package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
)

// O3's drift guard: every check ID doctor emits has an entry in
// doctorAnswers, so a new check gets its wording or is consciously left
// with an empty one, and no entry names a check that no longer exists.
func TestDoctorAnswersCoverEveryCheck(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "doctor", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatal(err, files)
	}
	literal := regexp.MustCompile(`ID: +"([a-z][a-z._-]*)"`)
	emitted := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllSubmatch(src, -1) {
			emitted[string(m[1])] = true
		}
		// The one built ID: an MCP entry, mcp.AGENT.NAME.
		if bytes.Contains(src, []byte(`id := "mcp." + mcp.Agent + "." + mcp.Name`)) {
			emitted["mcp.entry"] = true
		}
	}
	if len(emitted) < 20 || !emitted["mcp.entry"] {
		t.Fatalf("the scan found too few checks, so it is out of date: %v", emitted)
	}
	for id := range emitted {
		if _, ok := doctorAnswers[id]; !ok {
			t.Errorf("doctor check %q has no entry in doctorAnswers: give it an answer per status, or {} to keep its message", id)
		}
	}
	for id := range doctorAnswers {
		if !emitted[id] {
			t.Errorf("doctorAnswers has %q, which doctor no longer emits", id)
		}
	}
}

// An answer takes the line and the message, with its values, goes under
// it; a check whose message already reads as one keeps it on the line.
// --json is the report as it was.
func TestDoctorViewAnswers(t *testing.T) {
	report := doctor.Report{Status: doctor.Fail, Checks: []doctor.Check{
		{ID: "authority.health", Title: "Authority health", Status: doctor.Pass, Message: "The cloud authority is reachable and returned 3 visible project(s)."},
		{ID: "mcp.codex.clawbridge", Title: "Installed MCP entry", Status: doctor.Warn, Message: "No MCP configuration file exists at the selected path.", Path: "/home/a/.codex/config.toml", Action: "Run clawbridge mcp install."},
		{ID: "linear.watch", Title: "Linear comment watch", Status: doctor.Pass, Message: "4 assigned issues are watched for new Linear comments."},
		{ID: "config.trusted", Title: "Trusted configuration", Status: doctor.Fail, Message: "Trusted storage mode is absent or unsupported.", Action: "Select explicit local or cloud mode."},
	}}
	var human bytes.Buffer
	if err := clirender.Write(&human, clirender.Human, "doctor", report); err != nil {
		t.Fatal(err)
	}
	want := "Doctor: 2 pass · 1 warn · 1 fail\n" +
		"FAIL Trusted configuration: Your ClawBridge settings can't be used.\n" +
		"     Trusted storage mode is absent or unsupported.\n" +
		"     → Select explicit local or cloud mode.\n" +
		"WARN Installed MCP entry: This agent's ClawBridge connection is missing or can't start.\n" +
		"     No MCP configuration file exists at the selected path.\n" +
		"     /home/a/.codex/config.toml\n" +
		"     → Run clawbridge mcp install.\n" +
		"PASS Authority health: Your workspace answers.\n" +
		"     The cloud authority is reachable and returned 3 visible project(s).\n" +
		"PASS Linear comment watch: 4 assigned issues are watched for new Linear comments.\n\n" +
		"Doctor found failures that need fixing; the actions above say how.\n"
	if human.String() != want {
		t.Fatalf("%q\n%q", human.String(), want)
	}
	var got, was bytes.Buffer
	if err := clirender.Write(&got, clirender.JSON, "doctor", report); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(&was).Encode(report); err != nil {
		t.Fatal(err)
	}
	if got.String() != was.String() {
		t.Fatalf("--json changed:\n%s\n%s", got.String(), was.String())
	}
}
