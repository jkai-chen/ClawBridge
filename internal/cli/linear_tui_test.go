package cli

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/tui"
)

// An issue opened from a session (i) before a connection is chosen takes
// the only one; with none, it asks for one on the Linear screen, as
// intake does.
func TestLinearTUIIssueWithoutAConnectionAsks(t *testing.T) {
	// The state home isolated(t) gives, as the other Linear tests use: a
	// chmod'ed TempDir isn't private on Windows.
	isolated(t)
	dir, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	s := &linearTUISource{directory: dir, manager: newLinearManager(dir)}
	for _, op := range []string{"issue", "intake"} {
		_, err := s.Do(context.Background(), tui.LinearRequest{Operation: op, IssueID: "00000000-0000-4000-8000-0000000000c1"})
		if err == nil || !strings.Contains(err.Error(), "choose your Linear connection on the Linear screen first") {
			t.Fatal(op, err)
		}
	}
}
