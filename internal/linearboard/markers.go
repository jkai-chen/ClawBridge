package linearboard

import (
	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// MarkerApply is what one apply of an assignment's claim or close marker
// did, per part: the bus's go-ahead, and the outcome when one was sent.
type MarkerApply struct {
	AssignmentID string `json:"assignment_id"`
	Phase        string `json:"phase"`
	Comment      string `json:"comment"`
	Label        string `json:"label"`
	State        string `json:"state,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ApplyMarker applies one assignment's claim or close marker as far as the
// bus allows: each part asks marker_go, does what it says, and settles. A
// failure never undoes the bus transition; the row is retried later. fence
// carries the watcher's lease, or is empty for the client that made the
// transition.
func (s *Service) ApplyMarker(ctx context.Context, scope bus.Request, assignment, phase string, fence map[string]any) MarkerApply {
	// Both parts share one connection, one binding proof and one issue
	// read.
	s, release := s.hold()
	defer release()
	out := MarkerApply{AssignmentID: assignment, Phase: phase}
	for _, part := range []string{"comment", "label", "state"} {
		args := map[string]any{"assignment_id": assignment, "phase": phase, "part": part}
		for k, v := range fence {
			args[k] = v
		}
		result, err := s.markerPart(ctx, scope, args)
		if part == "state" && err != nil && strings.Contains(err.Error(), "part must be comment or label") {
			// An authority older than the status policy has no state part.
			result, err = "off", nil
		}
		if err != nil && out.Error == "" {
			out.Error = err.Error()
		}
		switch part {
		case "comment":
			out.Comment = result
		case "label":
			out.Label = result
		default:
			out.State = result
		}
	}
	return out
}

func (s *Service) markerPart(ctx context.Context, scope bus.Request, args map[string]any) (string, error) {
	g, err := s.commentLedger(ctx, scope, "marker_go", args)
	if err != nil {
		return "unavailable", err
	}
	action, _ := g["action"].(string)
	issue, _ := g["issue_uuid"].(string)
	settle := func(outcome string, extra map[string]any, failure error) (string, error) {
		a := map[string]any{"outcome": outcome}
		for k, v := range args {
			a[k] = v
		}
		for k, v := range extra {
			a[k] = v
		}
		if failure != nil {
			a["error"] = safeMarkerError(failure)
		}
		// A row cut by the pass's deadline still records what it knows
		// (uncertain, so the next attempt re-reads before writing again).
		sctx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			sctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), markerSettleTimeout)
			defer cancel()
		}
		if _, err := s.commentLedger(sctx, scope, "marker_settle", a); err != nil {
			return outcome, err
		}
		return outcome, failure
	}
	if action == "check" {
		// An earlier attempt may have landed: its exact body inside its
		// window settles this one without posting again. Otherwise the bus
		// hands this session a new attempt.
		body, _ := g["body"].(string)
		from, _ := g["check_from"].(string)
		to, _ := g["check_to"].(string)
		found, created, err := s.findMarkerComment(ctx, scope, issue, body, from, to)
		if err != nil {
			return settle("uncertain", nil, err)
		}
		if found != "" {
			return settle("done", map[string]any{"comment_id": found, "created_at": created, "via": "body_match"}, nil)
		}
		a := map[string]any{"outcome": "absent"}
		for k, v := range args {
			a[k] = v
		}
		if g, err = s.commentLedger(ctx, scope, "marker_settle", a); err != nil {
			return "unavailable", err
		}
		action, _ = g["action"].(string)
	}
	switch action {
	case "move":
		policy, err := decodeStatusPolicy(g["policy"])
		kind, _ := g["kind"].(string)
		if err != nil || (kind != "claim" && kind != "release_done") {
			return settle("failed", nil, errors.New("the state move is unreadable; nothing was sent"))
		}
		retry, _ := g["state"].(string)
		outcome, extra, err := s.stateMove(ctx, scope, issue, kind, policy, claimOvertaken(g), retry == "in_flight")
		return settle(outcome, extra, err)
	case "post":
		body, _ := g["body"].(string)
		receipt, err := s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: markerOperation(), IssueID: issue, Body: body}, marker: true})
		if err != nil {
			return settle(markerFailure(err), nil, err)
		}
		id := receiptCommentID(receipt)
		if !linearprovider.ValidID(id) {
			return settle("uncertain", nil, errors.New("marker comment receipt unreadable"))
		}
		return settle("done", map[string]any{"comment_id": id}, nil)
	case "add", "remove":
		set, err := s.markerLabel(ctx, scope, issue)
		if errors.Is(err, errNoMarkerLabel) {
			return settle("no_label", nil, err)
		}
		if err != nil {
			// Finding the label only reads: nothing was sent.
			outcome := "failed"
			if markerFailure(err) == "out_of_binding" {
				outcome = "out_of_binding"
			}
			return settle(outcome, nil, err)
		}
		in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: markerOperation(), IssueID: issue}, marker: true}
		// Added: the team's own label, else its parent's. Removed: every
		// one of the set the issue carries (CROSS-TEAM-BINDING §3.2).
		if action == "add" {
			in.AddLabels = []string{set.Apply}
		} else {
			in.RemoveLabels = set.IDs
		}
		if _, err = s.WriteV2(ctx, scope, in); err != nil {
			return settle(markerFailure(err), nil, err)
		}
		return settle("done", nil, nil)
	}
	return action, nil
}

// receiptCommentID is the comment a write's receipt names: a V2 receipt,
// or the legacy receipt a binding the legacy path serves returns (a team
// board, or a project set of one project).
func receiptCommentID(receipt any) string {
	switch r := receipt.(type) {
	case ReceiptV2:
		return r.ID
	case linearprovider.Receipt:
		return r.ID
	}
	return ""
}

var errMarkerWalk = errors.New("the team has too many labels to prove agent-working unique; nothing was sent")

var errNoMarkerLabel = errors.New("label " + bus.MarkerLabel + " is missing in the issue's team, or ambiguous; a Linear admin must create exactly one")

// markerLabel finds the agent-working label set of the issue's team: its
// own label, and its parent's when the binding binds the parent. It is
// resolved by exact name; none to apply, or two in one team, is never
// guessed.
func (s *Service) markerLabel(ctx context.Context, scope bus.Request, issue string) (LabelSetV2, error) {
	current, err := s.IssueV2(ctx, scope, issue)
	if err != nil {
		return LabelSetV2{}, err
	}
	set, err := s.LabelSetV2(ctx, scope, current.TeamID, bus.MarkerLabel)
	switch {
	case errors.Is(err, ErrLabelWalk):
		// A walk cut off by the page bound could hide a second label:
		// nothing is guessed from it. Nothing was sent.
		return set, errMarkerWalk
	case errors.Is(err, ErrLabelAmbiguous):
		return set, errNoMarkerLabel
	case err != nil:
		return set, err
	case set.Apply == "":
		return set, errNoMarkerLabel
	}
	return set, nil
}

// findMarkerComment looks for a comment whose body is exactly the marker's
// and that was created inside [from, to], and returns it with its creation
// time, which the bus re-checks.
func (s *Service) findMarkerComment(ctx context.Context, scope bus.Request, issue, body, from, to string) (string, string, error) {
	start, err1 := time.Parse(time.RFC3339Nano, from)
	end, err2 := time.Parse(time.RFC3339Nano, to)
	if err1 != nil || err2 != nil {
		return "", "", errors.New("invalid marker check window")
	}
	read, err := s.IssueV2WithComments(ctx, scope, issue, CommentsInputV2{Since: start.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return "", "", err
	}
	if read.Truncated && !read.StoppedEarly {
		return "", "", errors.New("marker check read was incomplete")
	}
	want := sha256.Sum256([]byte(body))
	for _, c := range read.Items {
		created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil || created.Before(start) || created.After(end) || c.BodyTruncated {
			continue
		}
		if sha256.Sum256([]byte(c.UntrustedBody)) == want {
			return c.ID, c.CreatedAt, nil
		}
	}
	return "", "", nil
}

// markerOperation is a fresh file-ledger operation for one marker attempt;
// the bus row, not the file ledger, keeps a marker idempotent.
func markerOperation() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "marker-" + hex.EncodeToString(b[:])
}

// markerFailure is how a failed apply settles: out_of_binding, failed when
// the write provably sent nothing, otherwise uncertain.
func markerFailure(err error) string {
	var outside outOfScopeError
	var never notDispatchedError
	var refused refusedBeforeDispatchError
	switch {
	case errors.As(err, &outside):
		return "out_of_binding"
	case errors.As(err, &never), errors.As(err, &refused), errors.Is(err, errMarkerWalk):
		return "failed"
	}
	return "uncertain"
}

// safeMarkerError is a failure's short code for the bus row: never Linear
// text.
func safeMarkerError(err error) string {
	var provider *linearprovider.Error
	switch {
	case errors.Is(err, errNoMarkerLabel):
		return "no_label"
	case errors.As(err, &provider):
		return provider.Detail()
	}
	return markerFailure(err)
}
