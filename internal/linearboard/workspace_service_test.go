package linearboard

import (
	"context"
	"strings"
	"testing"
)

// A service bound to one Linear workspace (the app's) serves only bindings
// pinned to it, on every binding read, so no write reaches Linear otherwise.
func TestServiceWorkspaceAdmitsOnlyItsPinnedBindings(t *testing.T) {
	const ws = "f0000000-0000-4000-8000-0000000000f1"
	for name, c := range map[string]struct {
		pinned, want string
	}{
		"unpinned": {"", "workspace_unpinned"},
		"another":  {"f0000000-0000-4000-8000-0000000000f2", "workspace_mismatch"},
		"pinned":   {ws, ""},
	} {
		s, a, p, scope := testV2(t)
		s.cfg.Workspace = ws
		a.b.LinearWorkspaceID = c.pinned
		_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "ws-" + name, IssueID: issueID, Body: "hello"}})
		switch {
		case c.want == "" && e != nil:
			t.Fatal(name, e)
		case c.want != "" && (e == nil || !strings.Contains(e.Error(), c.want) || p.writes != 0):
			t.Fatal(name, "written outside the app's workspace", e, p.writes)
		}
	}
	// Without a workspace the service serves any binding, as before.
	s, a, _, scope := testV2(t)
	a.b.LinearWorkspaceID = ""
	if _, e := s.BindingV2(context.Background(), scope); e != nil {
		t.Fatal(e)
	}
}
