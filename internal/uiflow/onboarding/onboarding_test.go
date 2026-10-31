package onboarding

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/skill"
)

// script answers Yes from a queue (default no) and Ask with the fallback,
// logging every effect in call.
type script struct {
	yes      []bool
	endAfter int // Ended turns true once this many questions were asked; 0 never
	asked    int
	log      *[]string
}

func (s *script) next() bool {
	s.asked++
	return !s.Ended()
}

func (s *script) Yes(q string) (bool, error) {
	if !s.next() || len(s.yes) == 0 {
		return false, nil
	}
	a := s.yes[0]
	s.yes = s.yes[1:]
	return a, nil
}

func (s *script) Ask(q, fallback string) (string, error) {
	if !s.next() {
		return "", nil
	}
	return fallback, nil
}

func (s *script) Ended() bool { return s.endAfter > 0 && s.asked >= s.endAfter }
func (s *script) Suspend() error {
	*s.log = append(*s.log, "Suspend")
	return nil
}
func (s *script) Resume() error {
	*s.log = append(*s.log, "Resume")
	return nil
}

type recorder struct{ lines, records []string }

func (r *recorder) Line(s string)         { r.lines = append(r.lines, s) }
func (r *recorder) Heading(int, string)   {}
func (r *recorder) Record(n, o, _ string) { r.records = append(r.records, n+": "+o) }
func (r *recorder) MCPPreview(install.Result) error {
	r.lines = append(r.lines, "preview")
	return nil
}

func effects(log *[]string, changed bool) Effects {
	note := func(s string) { *log = append(*log, s) }
	return Effects{
		SkillTargets: func() ([]skill.Target, error) {
			return []skill.Target{{Agent: "codex", Dir: "/x/codex"}}, nil
		},
		SkillInstall: func([]skill.Target) ([]skill.Result, error) { note("SkillInstall"); return nil, nil },
		LinearStore: func(context.Context, func(string, string) (string, error)) (string, error) {
			note("LinearStore")
			return "keyring", nil
		},
		LinearConnect: func(context.Context, string, string, string) (string, error) {
			note("LinearConnect")
			return "prof1", nil
		},
		MCPInstall: func(_ context.Context, _, _, _ string, dry bool) (install.Result, error) {
			if dry {
				note("MCPPreview")
			} else {
				note("MCPWrite")
			}
			return install.Result{Path: "/x/mcp", Changed: changed}, nil
		},
		Doctor: func(context.Context, string, []string) (doctor.Report, error) {
			note("Doctor")
			return doctor.Report{Status: "pass"}, nil
		},
		Dispatcher: func(_ context.Context, _ func(string) (bool, error), record func(string, string, string), _ func(error) string) error {
			note("Dispatcher")
			record("Dispatcher", "skipped", "")
			return nil
		},
		AgentNames: map[string]string{"codex": "Codex"},
	}
}

func TestRunOrderAndSummary(t *testing.T) {
	var log []string
	p := &script{yes: []bool{true, true, true, true}, log: &log} // skill, Linear, MCP, write
	out := &recorder{}
	steps, err := Run(context.Background(), p, out, effects(&log, true))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"SkillInstall", "LinearStore", "Suspend", "LinearConnect", "Resume", "Doctor", "MCPPreview", "MCPWrite", "Doctor", "Dispatcher"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("effects = %v, want %v", log, want)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	wantNames := []string{"Agent skill", "Linear", "MCP for Codex", "Doctor (Codex)", "Dispatcher"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("steps = %v, want %v", names, wantNames)
	}
	if len(out.records) != len(steps) {
		t.Fatalf("records = %v for %d steps", out.records, len(steps))
	}
}

func TestRunNoWritesWithoutYes(t *testing.T) {
	var log []string
	out := &recorder{}
	if _, err := Run(context.Background(), &script{log: &log}, out, effects(&log, true)); err != nil {
		t.Fatal(err)
	}
	for _, c := range log {
		switch c {
		case "SkillInstall", "LinearConnect", "MCPPreview", "MCPWrite":
			t.Fatalf("%s ran without a yes: %v", c, log)
		}
	}
}

func TestSuspendResumeAroundConnect(t *testing.T) {
	var log []string
	p := &script{yes: []bool{false, true}, log: &log} // skip the skill, connect Linear
	if _, err := Run(context.Background(), p, &recorder{}, effects(&log, false)); err != nil {
		t.Fatal(err)
	}
	var around []string
	for _, c := range log {
		if c == "Suspend" || c == "LinearConnect" || c == "Resume" {
			around = append(around, c)
		}
	}
	if want := []string{"Suspend", "LinearConnect", "Resume"}; !reflect.DeepEqual(around, want) {
		t.Fatalf("calls = %v, want %v", around, want)
	}
}

func TestEndedAnswersNo(t *testing.T) {
	var log []string
	// Questions: skill yes, Linear yes, name, access. Input ends there.
	p := &script{yes: []bool{true, true}, endAfter: 4, log: &log}
	out := &recorder{}
	steps, err := Run(context.Background(), p, out, effects(&log, true))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Ended() {
		t.Fatal("input should have ended")
	}
	got := map[string]string{}
	for _, s := range steps {
		got[s.Name] = s.Outcome
	}
	if got["Linear"] != "skipped" || got["MCP for Codex"] != "skipped" {
		t.Fatalf("steps after the end = %v", got)
	}
	for _, c := range log {
		if c == "LinearConnect" || c == "MCPWrite" {
			t.Fatalf("%s ran after input ended", c)
		}
	}
}

func TestMCPPreviewUnchangedIsAlreadyConfigured(t *testing.T) {
	var log []string
	p := &script{yes: []bool{false, false, true}, log: &log} // skill no, Linear no, MCP yes
	out := &recorder{}
	if _, err := Run(context.Background(), p, out, effects(&log, false)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(out.records, "\n"), "MCP for Codex: already configured") {
		t.Fatalf("records = %v", out.records)
	}
	for _, c := range log {
		if c == "MCPWrite" {
			t.Fatal("an unchanged entry was written")
		}
	}
}
