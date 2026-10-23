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

// markerLabelPages bounds the label catalog walk that finds agent-working.
const markerLabelPages = 5

// MarkerApply is what one apply of an assignment's claim or close marker
// did, per part: the bus's go-ahead, and the outcome when one was sent.
type MarkerApply struct {
	AssignmentID string `json:"assignment_id"`
	Phase        string `json:"phase"`
	Comment      string `json:"comment"`
	Label        string `json:"label"`
	Error        string `json:"error,omitempty"`
}

// ApplyMarker applies one assignment's claim or close marker as far as the
// bus allows: each part asks marker_go, does what it says, and settles. A
// failure never undoes the bus transition; the row is retried later. fence
// carries the watcher's lease, or is empty for the client that made the
// transition.
func (s *Service) ApplyMarker(ctx context.Context, scope bus.Request, assignment, phase string, fence map[string]any) MarkerApply {
	out := MarkerApply{AssignmentID: assignment, Phase: phase}
	for _, part := range []string{"comment", "label"} {
		args := map[string]any{"assignment_id": assignment, "phase": phase, "part": part}
		for k, v := range fence {
			args[k] = v
		}
		result, err := s.markerPart(ctx, scope, args)
		if err != nil && out.Error == "" {
			out.Error = err.Error()
		}
		if part == "comment" {
			out.Comment = result
		} else {
			out.Label = result
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
		if _, err := s.commentLedger(ctx, scope, "marker_settle", a); err != nil {
			return outcome, err
		}
		return outcome, failure
	}
	switch action {
	case "post":
		body, _ := g["body"].(string)
		from, _ := g["check_from"].(string)
		to, _ := g["check_to"].(string)
		if from != "" {
			// An earlier attempt may have landed: its exact body inside its
			// window settles this one without posting again.
			found, err := s.findMarkerComment(ctx, scope, issue, body, from, to)
			if err != nil {
				return settle("uncertain", nil, err)
			}
			if found != "" {
				return settle("done", map[string]any{"comment_id": found, "via": "body_match"}, nil)
			}
		}
		receipt, err := s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: markerOperation(), IssueID: issue, Body: body}, marker: true})
		if err != nil {
			return settle(markerFailure(err), nil, err)
		}
		r, ok := receipt.(ReceiptV2)
		if !ok || !linearprovider.ValidID(r.ID) {
			return settle("uncertain", nil, errors.New("marker comment receipt unreadable"))
		}
		return settle("done", map[string]any{"comment_id": r.ID}, nil)
	case "add", "remove":
		label, err := s.markerLabel(ctx, scope, issue)
		if errors.Is(err, errNoMarkerLabel) {
			return settle("no_label", nil, err)
		}
		if err != nil {
			return settle(markerFailure(err), nil, err)
		}
		in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: markerOperation(), IssueID: issue}, marker: true}
		if action == "add" {
			in.AddLabels = []string{label}
		} else {
			in.RemoveLabels = []string{label}
		}
		if _, err = s.WriteV2(ctx, scope, in); err != nil {
			return settle(markerFailure(err), nil, err)
		}
		return settle("done", nil, nil)
	}
	return action, nil
}

var errNoMarkerLabel = errors.New("label " + bus.MarkerLabel + " is missing in the issue's team, or ambiguous; a Linear admin must create exactly one")

// markerLabel finds the one agent-working label the issue's team can use.
// It is resolved by exact name among eligible labels; none, or more than
// one, is never guessed.
func (s *Service) markerLabel(ctx context.Context, scope bus.Request, issue string) (string, error) {
	current, err := s.IssueV2(ctx, scope, issue)
	if err != nil {
		return "", err
	}
	found := []string{}
	cursor := ""
	for page := 0; page < markerLabelPages; page++ {
		labels, err := s.LabelsV2(ctx, scope, current.TeamID, 100, cursor)
		if err != nil {
			return "", err
		}
		for _, l := range labels.Items {
			if l.Name == bus.MarkerLabel && eligibleLabelV2(l) == nil {
				found = append(found, l.ID)
			}
		}
		if labels.NextCursor == "" {
			break
		}
		cursor = labels.NextCursor
	}
	if len(found) != 1 {
		return "", errNoMarkerLabel
	}
	return found[0], nil
}

// findMarkerComment looks for a comment whose body is exactly the marker's
// and that was created inside [from, to].
func (s *Service) findMarkerComment(ctx context.Context, scope bus.Request, issue, body, from, to string) (string, error) {
	start, err1 := time.Parse(time.RFC3339Nano, from)
	end, err2 := time.Parse(time.RFC3339Nano, to)
	if err1 != nil || err2 != nil {
		return "", errors.New("invalid marker check window")
	}
	read, err := s.IssueV2WithComments(ctx, scope, issue, CommentsInputV2{Since: start.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return "", err
	}
	if read.Truncated && !read.StoppedEarly {
		return "", errors.New("marker check read was incomplete")
	}
	want := sha256.Sum256([]byte(body))
	for _, c := range read.Items {
		created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil || created.Before(start) || created.After(end) || c.BodyTruncated {
			continue
		}
		if sha256.Sum256([]byte(c.UntrustedBody)) == want {
			return c.ID, nil
		}
	}
	return "", nil
}

// markerOperation is a fresh file-ledger operation for one marker attempt;
// the bus row, not the file ledger, keeps a marker idempotent.
func markerOperation() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "marker-" + hex.EncodeToString(b[:])
}

// markerFailure is how a failed apply settles: out_of_binding, failed when
// nothing was sent, otherwise uncertain.
func markerFailure(err error) string {
	var outside outOfScopeError
	if errors.As(err, &outside) {
		return "out_of_binding"
	}
	if strings.Contains(err.Error(), "nothing was sent") {
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
