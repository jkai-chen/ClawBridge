package linearboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

const linearWorkspace = "f0000000-0000-4000-8000-0000000000f1"

// workspaceProvider reports the connection's Linear workspace.
type workspaceProvider struct {
	*providerV2
	workspace string
	err       error
}

func (p workspaceProvider) Workspace(context.Context) (linearprovider.Workspace, error) {
	return linearprovider.Workspace{ID: p.workspace, Name: "Acme"}, p.err
}

// setRecorder answers taskboard_set_v2 and keeps its arguments.
type setRecorder struct {
	*authorityV2
	set map[string]any
}

func (a *setRecorder) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	if r.Operation == "taskboard_set_v2" {
		a.set = r.Args
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": 2}}, nil
	}
	return a.authorityV2.Execute(ctx, r)
}

func pinTest(t *testing.T, w workspaceProvider) (*Service, *setRecorder, bus.Request, LinkInputV2) {
	t.Helper()
	s, a, p, scope := testV2(t)
	w.providerV2 = p
	rec := &setRecorder{authorityV2: a}
	s.cfg.Executor = rec
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return w, nil }
	return s, rec, scope, LinkInputV2{TeamIDs: []string{otherTeamID, teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectB, projectA}, ExpectedRevision: 1}
}

// A link records the workspace Linear reports for the connection, shown in
// the preview first.
func TestLinkPinsTheLinearWorkspace(t *testing.T) {
	s, rec, scope, in := pinTest(t, workspaceProvider{workspace: linearWorkspace})
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil || r.(LinkPreviewV2).LinearWorkspaceID != linearWorkspace || rec.set != nil {
		t.Fatal("preview", r, e)
	}
	in.Confirm = true
	if _, e = s.LinkV2(context.Background(), scope, in); e != nil || rec.set["linear_workspace_id"] != linearWorkspace {
		t.Fatal("the link did not record the workspace", e, rec.set)
	}
}

func TestLinkRefusesAnotherWorkspace(t *testing.T) {
	// A binding pinned to another workspace is not relinked over.
	s, rec, scope, in := pinTest(t, workspaceProvider{workspace: linearWorkspace})
	rec.b.LinearWorkspaceID = "f0000000-0000-4000-8000-0000000000f2"
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "another Linear workspace") || rec.set != nil {
		t.Fatal("relinked across workspaces", e)
	}
	// A read error refuses rather than linking unpinned.
	s, rec, scope, in = pinTest(t, workspaceProvider{err: &linearprovider.Error{Kind: "unavailable"}})
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || rec.set != nil {
		t.Fatal("linked without reading the workspace", e)
	}
	// Without get_workspace the link proceeds as before, unpinned.
	s, rec, scope, in = pinTest(t, workspaceProvider{err: &linearprovider.Error{Kind: "capability_unavailable"}})
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || rec.set == nil || rec.set["linear_workspace_id"] != nil {
		t.Fatal("link without get_workspace", e, rec.set)
	}
}

// workspaceConnections is a write profile that names its workspace.
type workspaceConnections struct{ fakeConnections }

func (workspaceConnections) Status(context.Context, string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: "profile-1", Name: "test", Access: linearconn.Write, WorkspaceID: "f0000000-0000-4000-8000-0000000000f3"}, nil
}

// A profile whose recorded workspace differs from what Linear reports is
// refused rather than pinned to either.
func TestLinkRefusesAProfileFromAnotherWorkspace(t *testing.T) {
	s, rec, scope, in := pinTest(t, workspaceProvider{workspace: linearWorkspace})
	s.cfg.Connections = workspaceConnections{}
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "disagree") || rec.set != nil {
		t.Fatal(e, rec.set)
	}
}

// PR7-M1: a pinned binding is never relinked unpinned.
func TestLinkKeepsAnExistingPin(t *testing.T) {
	s, rec, scope, in := pinTest(t, workspaceProvider{err: &linearprovider.Error{Kind: "capability_unavailable"}})
	rec.b.LinearWorkspaceID = linearWorkspace
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "cannot report its workspace") || rec.set != nil {
		t.Fatal("a pinned binding was relinked without its workspace", e, rec.set)
	}
	// The same workspace relinks and keeps the pin.
	s, rec, scope, in = pinTest(t, workspaceProvider{workspace: linearWorkspace})
	rec.b.LinearWorkspaceID = linearWorkspace
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || rec.set["linear_workspace_id"] != linearWorkspace {
		t.Fatal(e, rec.set)
	}
}
