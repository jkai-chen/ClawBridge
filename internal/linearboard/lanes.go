package linearboard

import (
	"context"
	"errors"
	"strings"

	"clawbridge/internal/bus"
)

// outsideLaneError refuses a write to a write team outside the session's
// lane (CROSS-TEAM-BINDING-DESIGN §3.1, §5); nothing is sent.
type outsideLaneError string

func (e outsideLaneError) Error() string { return string(e) }

// checkLaneV2 is the lane filter on a session's write to team. A binding
// without lanes, and a team that isn't a write team (a comment team takes
// comments from every lane; the scope checks refuse the rest), pass. With
// lanes, the server must have named the session's lane: a client never
// guesses one. A refusal names where the lane came from when the server
// said, and, when the call named no session (session is the request's),
// how to name one.
func checkLaneV2(b bus.TaskboardBindingV2, access bus.TaskboardAccess, session, team string) error {
	if len(b.Lanes) == 0 || b.TeamAccess(team) != bus.TeamAccessWrite {
		return nil
	}
	if access.LaneWriteTeamIDs == nil {
		return outsideLaneError("outside_lane: this binding has lanes, but the ClawBridge server named no lane for this session; upgrade the server; nothing was sent")
	}
	if contains(access.LaneWriteTeamIDs, team) {
		return nil
	}
	var writers []string
	for _, l := range b.Lanes {
		if contains(l.WriteTeamIDs, team) {
			writers = append(writers, l.Name)
		}
	}
	by := "no lane writes team " + team
	if len(writers) > 0 {
		by = "team " + team + " is written by lane " + strings.Join(writers, ", ")
	}
	from := laneFromText[access.LaneFrom]
	hint := ""
	if session == "" {
		hint = "; this call named no session: pass --session, or set CLAWBRIDGE_SESSION"
	}
	if access.Lane == bus.LaneReadOnly || len(access.LaneWriteTeamIDs) == 0 {
		return outsideLaneError("outside_lane: " + by + "; this session has no lane (unpinned sessions are read-only, or its pinned lane is gone)" + from + hint + "; nothing was sent")
	}
	return outsideLaneError("outside_lane: " + by + "; you are in lane " + access.Lane + from + hint + "; nothing was sent")
}

// laneFromText says, in a refusal, where the server found the lane; an
// older server names no source, and the refusal says nothing of it.
var laneFromText = map[string]string{
	bus.LaneFromSessionPin:      " (lane from the session's pin)",
	bus.LaneFromMovedSessionPin: " (lane from a pin of the session it moved from)",
	bus.LaneFromPrincipalPin:    " (lane from your own pin as a person)",
	bus.LaneFromUnpinned:        " (lane from the binding's unpinned setting)",
}

// sameLaneV2: the lane a write was checked against still holds inside the
// write lock; a pin changed meanwhile stops the write.
func sameLaneV2(b bus.TaskboardBindingV2, before, after bus.TaskboardAccess) bool {
	if len(b.Lanes) == 0 {
		return true
	}
	return before.Lane == after.Lane && strings.Join(before.LaneWriteTeamIDs, ",") == strings.Join(after.LaneWriteTeamIDs, ",") && (before.LaneWriteTeamIDs == nil) == (after.LaneWriteTeamIDs == nil)
}

// LanePins reads the binding's lanes and the project's lane pins: any
// member or session. Nothing is read from Linear.
func (s *Service) LanePins(ctx context.Context, scope bus.Request) (any, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	scope.Operation = "lane_pins_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	unpinned := b.Unpinned
	if len(b.Lanes) == 0 {
		unpinned = ""
	}
	return map[string]any{"lanes": b.Lanes, "unpinned": unpinned, "pins": r.Data}, nil
}

// LanePinInput pins a session (by name) or a principal to a lane, or
// clears that pin, at the pins' revision the owner last read.
type LanePinInput struct {
	Kind, Key, Lane string
	Clear, Confirm  bool
	Expected        int64
}

// SetLanePin changes one pin: the project owner, in their own terminal,
// without a session. Without Confirm the server previews it, naming the
// principal a session pin binds (X1). Nothing is sent to Linear.
func (s *Service) SetLanePin(ctx context.Context, scope bus.Request, in LanePinInput) (any, error) {
	if scope.Session != "" {
		return nil, errors.New("a session can't pin lanes; the project owner runs linear lanes pin in their own terminal")
	}
	if in.Expected < 0 {
		return nil, errors.New("lanes pin and unpin require --expected-revision (0 for none)")
	}
	args := map[string]any{"expected_revision": in.Expected, "kind": in.Kind, "key": in.Key, "confirm": in.Confirm}
	scope.Operation = "lane_pin_clear"
	if !in.Clear {
		scope.Operation, args["lane"] = "lane_pin_set", in.Lane
	}
	scope.Args = args
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}
