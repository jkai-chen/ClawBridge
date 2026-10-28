package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"clawbridge/internal/linearboard"
)

// The project kind reads as the default until set; set is the owner's,
// refused with a session, without a revision, and before the board is
// linked (a kind belongs to a bound team).
func TestCLIProjectKindShowAndSetRefusals(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"linear", "kind", "show"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"group_name":"Type"`) || !strings.Contains(out.String(), `"revision":0`) {
		t.Fatal(e, out.String(), errs.String())
	}
	for want, args := range map[string][]string{
		"isn't linked":        {"linear", "kind", "set", "--values", "Engineering,Review", "--expected-revision", "0"},
		"--expected-revision": {"linear", "kind", "set", "--values", "Engineering"},
		"without --session":   {"--session", "eng", "--instance", "eng-i", "linear", "kind", "set", "--values", "Engineering", "--expected-revision", "0"},
		"kind show|set|seed":  {"linear", "kind", "drop"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
}

// A seed is the owner's, with their own profile: without --profile, or
// with a session, nothing is read.
func TestCLIProjectKindSeedRefusals(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	for want, args := range map[string][]string{
		"requires --profile": {"linear", "kind", "seed", "--operation-id", "seed-1"},
		"without --session":  {"--session", "eng", "--instance", "eng-i", "linear", "kind", "seed", "--operation-id", "seed-1", "--profile", "p1"},
	} {
		var out, errs bytes.Buffer
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
}

// The preview's next line confirms exactly the plan it showed; a refused
// plan gets none; names are made terminal-safe.
func TestKindSeedOutput(t *testing.T) {
	seed := linearboard.KindSeedResult{Preview: true, PlanSHA: "abc", Untouched: []string{"Old\u202e\x1b[2J"}}
	out, err := kindSeedOutput(seed, linearFlags{operation: "seed-1", profile: "p1"}, nil)
	m := out.(map[string]any)
	if err != nil || m["next"] != "clawbridge linear kind seed --operation-id seed-1 --profile p1 --expected-plan-sha256 abc --confirm" || strings.ContainsAny(m["untouched"].([]any)[0].(string), "\u202e\x1b") {
		t.Fatal(m, err)
	}
	seed.Refused = 1
	out, _ = kindSeedOutput(seed, linearFlags{operation: "seed-1", profile: "p1"}, nil)
	if _, has := out.(map[string]any)["next"]; has {
		t.Fatal("a refused plan offered a confirm")
	}
}

// An apply is the owner's, with a profile and a file; its preview offers a
// canary first.
func TestCLIProjectKindApply(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	for want, args := range map[string][]string{
		"requires --profile": {"linear", "kind", "apply", "--file", "k.json", "--operation-id", "a-1"},
		"requires --file":    {"linear", "kind", "apply", "--operation-id", "a-1", "--profile", "p1"},
		"without --session":  {"--session", "eng", "--instance", "eng-i", "linear", "kind", "apply", "--file", "k.json", "--operation-id", "a-1", "--profile", "p1"},
	} {
		var out, errs bytes.Buffer
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
	out, err := kindApplyOutput(linearboard.KindApplyResult{Preview: true, PlanSHA: "abc"}, linearFlags{filterFile: "kinds.json", operation: "a-1", profile: "p1"}, nil)
	if m := out.(map[string]any); err != nil || m["next"] != `clawbridge linear kind apply --file "kinds.json" --operation-id a-1 --profile p1 --expected-plan-sha256 abc --confirm --canary` {
		t.Fatal(m, err)
	}
}
