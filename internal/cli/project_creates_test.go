package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// project creates lists a coordinator's creates (none yet); --ack is the
// owner's, from their own terminal, and needs an operation ID that is
// unconfirmed; --kind belongs to project create.
func TestCLIProjectCreates(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"linear", "project", "creates"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"unconfirmed":[]`) || !strings.Contains(out.String(), `"keep_days":90`) {
		t.Fatal(e, out.String(), errs.String())
	}
	// linear writes is the same list and ack, for every kind of write.
	out.Reset()
	if e := Run(context.Background(), []string{"linear", "writes"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"unconfirmed":[]`) {
		t.Fatal(e, out.String(), errs.String())
	}
	for want, args := range map[string][]string{
		"no unacknowledged":                         {"linear", "project", "creates", "--ack", "op-1"},
		"no unacknowledged, unsettled Linear write": {"linear", "writes", "--ack", "op-1"},
		"can't acknowledge ":                        {"--session", "eng", "--instance", "eng-i", "linear", "writes", "--ack", "op-1"},
		"can't acknowledge":                         {"--session", "eng", "--instance", "eng-i", "linear", "project", "creates", "--ack", "op-1"},
		"--ack takes":                               {"linear", "project", "creates", "--ack", "bad id!"},
		"unknown flag --kind":                       {"linear", "label", "create", "Bug", "--team", "11111111-1111-4111-8111-111111111111", "--operation-id", "x", "--kind", "Engineering"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
}
