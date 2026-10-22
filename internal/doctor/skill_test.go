package doctor

import (
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/skill"
)

func TestDoctorWarnsWhenTheAgentSkillIsMissingStaleOrChanged(t *testing.T) {
	state := func(agent string, installed bool, version string, current bool, modified ...string) skill.State {
		return skill.State{Target: skill.Target{Agent: agent}, Installed: installed, InstalledVersion: version, BinaryVersion: "v1.1.0", Current: current, Modified: modified}
	}
	cases := map[string]struct {
		states []skill.State
		err    error
		status string
		want   []string
	}{
		"current":       {[]skill.State{state("claude-code", true, "v1.1.0", true), state("codex", false, "", false)}, nil, Pass, []string{"claude-code: current (v1.1.0)", "codex: not installed"}},
		"none":          {[]skill.State{state("codex", false, "", false)}, nil, Warn, []string{"No ClawBridge agent skill is installed"}},
		"no agents":     {nil, nil, Warn, []string{"No ClawBridge agent skill is installed"}},
		"stale":         {[]skill.State{state("claude-code", true, "v1.1.0", true), state("codex", true, "v1.0.0", false)}, nil, Warn, []string{"codex: installed v1.0.0, binary v1.1.0"}},
		"changed":       {[]skill.State{state("codex", true, "v1.1.0", false, "SKILL.md")}, nil, Warn, []string{"SKILL.md changed since installation", "--force"}},
		"same version":  {[]skill.State{state("codex", true, "v1.1.0", false)}, nil, Warn, []string{"installed v1.1.0, binary v1.1.0"}},
		"status failed": {nil, errors.New("permission denied"), Warn, []string{"could not be read"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var checks []Check
			checkSkills(func() ([]skill.State, error) { return c.states, c.err }, func(x Check) { checks = append(checks, x) })
			if len(checks) != 1 || checks[0].ID != "skill.version" || checks[0].Status != c.status {
				t.Fatal(checks)
			}
			if (c.status == Warn) != (checks[0].Action != "") {
				t.Fatal("a warning needs an action, a pass none", checks[0])
			}
			for _, want := range c.want {
				if !strings.Contains(checks[0].Message+" "+checks[0].Action, want) {
					t.Fatalf("%q lacks %q", checks[0].Message+" "+checks[0].Action, want)
				}
			}
		})
	}
}
