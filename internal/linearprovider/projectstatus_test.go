package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A4: a project status move sends save_project the project's ID and the
// status ID, nothing else, against the recorded catalog; without the tool
// nothing is sent.
func TestSetProjectStatusSendsTheIDsOnly(t *testing.T) {
	const status = "f4000000-0000-4000-8000-000000000004"
	p, f := statusProvider(t, []*mcp.Tool{fixtureTool(t, "save-project-20261022.json")}, map[string][]any{
		"save_project": {map[string]any{"id": suProject}},
	})
	if e := p.SetProjectStatus(context.Background(), suProject, status); e != nil {
		t.Fatal(e)
	}
	if want := map[string]any{"id": suProject, "state": status}; len(f.args) != 1 || !reflect.DeepEqual(f.args[0], want) {
		t.Fatal(f.args)
	}
	p, f = statusProvider(t, fakeStatusTools([]any{"project"}, []any{"onTrack"}), nil)
	var refused *Error
	if e := p.SetProjectStatus(context.Background(), suProject, status); !errors.As(e, &refused) || refused.Kind != "capability_unavailable" || len(f.args) != 0 {
		t.Fatal("moved without save_project:", e, f.args)
	}
	if e := p.SetProjectStatus(context.Background(), suProject, "Started"); e == nil {
		t.Fatal("a status by name was sent")
	}
}
