package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
		"kind show|set":       {"linear", "kind", "seed"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
}
