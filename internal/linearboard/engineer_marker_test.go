package linearboard

import (
	"context"
	"errors"
	"testing"

	"clawbridge/internal/bus"
)

// The marker names the engineer the authority reports for the session; the
// caller's arguments never reach the access request.
func TestMarkerNamesTheEngineerFromTheAuthority(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.engineer = "Ada Lovelace"
	scope.Args["engineer"] = "Mallory"
	_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "c", IssueID: issueID, Body: "hello"}})
	if e != nil || len(p.bodies) != 1 || p.bodies[0] != "hello\n\n— 🤖 ClawBridge agent · session ses · Ada Lovelace" {
		t.Fatal(e, p.bodies)
	}
	if a.accessArgs["engineer"] != 1 {
		t.Fatal("the access request did not ask for the engineer as a flag", a.accessArgs)
	}
}

type preEngineerServer struct{ *authorityV2 }

func (o preEngineerServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "taskboard_access" && r.Args["engineer"] != nil {
		return bus.Result{}, errors.New(`unsupported argument "engineer" for taskboard_access`)
	}
	return o.authorityV2.Execute(ctx, r)
}

// A server older than v0.2.8 names no engineer: the comment is still
// posted, with the session-only marker.
func TestPreEngineerServerPostsTheSessionMarker(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.engineer = "Ada Lovelace"
	s.cfg.Executor = preEngineerServer{a}
	_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "c", IssueID: issueID, Body: "hello"}})
	if e != nil || len(p.bodies) != 1 || p.bodies[0] != "hello\n\n— 🤖 ClawBridge agent · session ses" {
		t.Fatal(e, p.bodies)
	}
}
