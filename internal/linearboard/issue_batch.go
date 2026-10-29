package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// A batch of moves to one team (live/ISSUE-TEAM-MOVE-DESIGN.md r6 B1, P1–P4):
// the CLI's `linear move --file`. The preview plans every issue (an issue
// with sub-issues as its family) and digests the ordered plans. The confirm
// re-plans, refuses a batch that changed or has any refusal, then moves the
// issues one by one, each re-planned at its turn under OP:<index>, and stops
// at the first that isn't accepted. A re-run with the same operation ID
// resumes: a done item answers from the server's record, and an issue
// already in the target team is read back and reported. Agents don't
// batch: they loop over the single move, one previewed call at a time.

// maxBatchIssues bounds a batch, children included (P1).
const maxBatchIssues = 100

// BatchMoveInputV2 is a batch: the target team, the issues by UUID in
// order, the batch's operation ID, and on a confirm the preview's digest.
type BatchMoveInputV2 struct {
	TeamID             string   `json:"team_id"`
	Issues             []string `json:"issues"`
	OperationID        string   `json:"-"`
	Confirm            bool     `json:"-"`
	ExpectedPlanSHA256 string   `json:"-"`
}

// BatchItemV2 is one issue of a batch: its plan, or why it's done,
// already moved or refused, and on a confirm its outcome.
type BatchItemV2 struct {
	Index         int                 `json:"index"`
	IssueID       string              `json:"issue_id"`
	OperationID   string              `json:"operation_id"`
	Status        string              `json:"status"`
	Family        bool                `json:"family,omitempty"`
	Plan          *MovePlanV2         `json:"plan,omitempty"`
	Refusals      []string            `json:"refusals,omitempty"`
	Outcome       string              `json:"outcome,omitempty"`
	NewIdentifier string              `json:"new_identifier,omitempty"`
	Children      []MoveChildResultV2 `json:"children,omitempty"`
	Unverified    []string            `json:"unverified,omitempty"`
	Note          string              `json:"note,omitempty"`
	digest        string
}

// BatchMoveResultV2 is the batch's preview or outcome.
type BatchMoveResultV2 struct {
	Items      []BatchItemV2 `json:"items"`
	Issues     int           `json:"issues"`
	Calls      int           `json:"calls"`
	PlanSHA256 string        `json:"plan_sha256"`
	Outcome    string        `json:"outcome"`
	StoppedAt  *int          `json:"stopped_at,omitempty"`
	Refusals   []string      `json:"refusals,omitempty"`
	Note       string        `json:"note,omitempty"`
}

// ParseBatchMoves reads a batch file: {"team_id": UUID, "issues": [UUID…]}.
func ParseBatchMoves(raw []byte) (BatchMoveInputV2, error) {
	var in BatchMoveInputV2
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e := d.Decode(&in); e != nil {
		return in, errors.New(`a batch file is {"team_id": UUID, "issues": [UUID, …]}`)
	}
	return in, nil
}

// BatchMoveV2 previews a batch, or confirms the one previewed. Each item's
// Linear calls get their own write budget of time, so a long batch is
// bounded per item, never as a whole.
func (s *Service) BatchMoveV2(ctx context.Context, scope bus.Request, in BatchMoveInputV2) (BatchMoveResultV2, error) {
	var out BatchMoveResultV2
	if !linearprovider.ValidID(in.TeamID) {
		return out, errors.New("a batch takes the target team's UUID")
	}
	if len(in.Issues) == 0 || len(in.Issues) > maxBatchIssues {
		return out, fmt.Errorf("a batch moves 1 to %d issues", maxBatchIssues)
	}
	seen := map[string]bool{}
	for i, id := range in.Issues {
		id = strings.ToLower(id)
		if !linearprovider.ValidID(id) || seen[id] {
			return out, fmt.Errorf("issue %d must be a distinct issue UUID", i)
		}
		seen[id], in.Issues[i] = true, id
	}
	if in.OperationID == "" || len(in.OperationID) > 100 {
		return out, errors.New("a batch takes --operation-id (at most 100 characters); a re-run with the same ID resumes it")
	}
	if in.Confirm != (in.ExpectedPlanSHA256 != "") {
		return out, errors.New("a batch confirm takes --confirm with the preview's --expected-plan-sha256")
	}
	out = s.planBatchV2(ctx, scope, in)
	if !in.Confirm {
		out.Outcome = "preview"
		return out, nil
	}
	if out.PlanSHA256 != in.ExpectedPlanSHA256 {
		return out, errors.New("the batch changed since the preview (an issue, a family or a mapping, or a move recorded meanwhile); preview again; nothing was sent")
	}
	if len(out.Refusals) > 0 {
		return out, fmt.Errorf("the batch is refused: %s; nothing was sent", strings.Join(out.Refusals, "; "))
	}
	out.Outcome = "accepted"
	for i := range out.Items {
		it := &out.Items[i]
		if it.Status != "plan" {
			continue
		}
		item, cancel := context.WithTimeout(ctx, WriteTimeout)
		r, e := s.MoveIssueV2(item, scope, MoveInputV2{IssueID: it.IssueID, TeamID: in.TeamID, OperationID: it.OperationID, Confirm: true, ExpectedPlanSHA256: it.Plan.PlanSHA256, Family: it.Family})
		cancel()
		it.Outcome, it.NewIdentifier, it.Children, it.Unverified, it.Note = r.Outcome, r.NewIdentifier, r.Children, r.Unverified, r.Note
		if e != nil || r.Outcome != "accepted" {
			if it.Outcome == "" {
				it.Outcome = "not_sent"
			}
			if e != nil {
				it.Note = strings.TrimSpace(it.Note + " " + e.Error())
			}
			out.Outcome, out.StoppedAt = "stopped", &it.Index
			if it.Outcome == "not_sent" {
				out.Note = fmt.Sprintf("stopped at issue %d: nothing was sent for it or after it (see its note; a spent budget says when to retry). Preview the batch again with the same --operation-id to go on", it.Index)
			} else {
				out.Note = fmt.Sprintf("stopped at issue %d (%s); nothing after it was sent. Read it in Linear; an uncertain move is never retried: acknowledge it with clawbridge linear writes --ack %s, then preview the batch again to resume (an issue that didn't move is then moved on its own with a new operation ID)", it.Index, it.Outcome, it.OperationID)
			}
			return out, nil
		}
	}
	return out, nil
}

// planBatchV2 plans every item in order, with what the server recorded
// for it. Every item's digest goes into the batch's, in order (P3).
func (s *Service) planBatchV2(ctx context.Context, scope bus.Request, in BatchMoveInputV2) BatchMoveResultV2 {
	var out BatchMoveResultV2
	var digests []string
	for i, id := range in.Issues {
		it := BatchItemV2{Index: i, IssueID: id, OperationID: fmt.Sprintf("%s:%d", in.OperationID, i)}
		s.planBatchItemV2(ctx, scope, in.TeamID, &it)
		for _, r := range it.Refusals {
			out.Refusals = append(out.Refusals, fmt.Sprintf("issue %d: %s", i, r))
		}
		if it.Plan != nil {
			out.Calls += it.Plan.Calls
			out.Issues += 1 + len(it.Plan.Children)
		} else {
			out.Issues++
		}
		digests = append(digests, it.digest)
		out.Items = append(out.Items, it)
	}
	if out.Issues > maxBatchIssues {
		out.Refusals = append(out.Refusals, fmt.Sprintf("the batch moves %d issues with their sub-issues; at most %d", out.Issues, maxBatchIssues))
	}
	raw, _ := json.Marshal([]any{in.TeamID, digests})
	sum := sha256.Sum256(raw)
	out.PlanSHA256 = hex.EncodeToString(sum[:])
	return out
}

// planBatchItemV2 plans one item: done when the server recorded it
// accepted; held while an earlier attempt is unsettled or uncertain and
// unacknowledged (P2); already moved when the issue reads in the target
// team; refused after an acknowledged uncertain attempt that left it where
// it was; else its move, as a family when it has sub-issues.
func (s *Service) planBatchItemV2(ctx context.Context, scope bus.Request, team string, it *BatchItemV2) {
	item, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	refuse := func(msg string) {
		it.Status, it.Refusals = "refused", append(it.Refusals, msg)
		it.digest = "refused:" + msg
	}
	st, e := s.moveCall(item, scope, "issue_move_status", map[string]any{"operation_id": it.OperationID})
	if e != nil {
		refuse("its record couldn't be read: " + e.Error())
		return
	}
	state := str(st, "state")
	switch {
	case state == "accepted":
		it.Status, it.Outcome, it.NewIdentifier = "done", "accepted", str(st, "new_identifier")
		it.digest = "done:" + it.OperationID
		return
	case state == "reserved", state == "uncertain" && st["acknowledged"] != true:
		refuse(fmt.Sprintf("an earlier attempt is %s; read the issue in Linear, then acknowledge it with clawbridge linear writes --ack %s", state, it.OperationID))
		return
	}
	r, e := s.MoveIssueV2(item, scope, MoveInputV2{IssueID: it.IssueID, TeamID: team})
	if errors.Is(e, errAlreadyInTeam) {
		// Read back, not assumed: the issue's own read names the team.
		it.Status = "already_moved"
		it.digest = "moved:" + it.IssueID
		return
	}
	// An acknowledged uncertain attempt keeps its operation ID: a new plan
	// under it would only conflict with the plan recorded there.
	if state == "uncertain" {
		refuse(fmt.Sprintf("an earlier attempt under %s was uncertain and is acknowledged, and the issue isn't in the target team; move it on its own with a new operation ID, then preview the batch again with the same --operation-id: it reads as already moved", it.OperationID))
		return
	}
	if e != nil {
		refuse(e.Error())
		return
	}
	if r.Plan.SubIssues > 0 {
		it.Family = true
		if r, e = s.MoveIssueV2(item, scope, MoveInputV2{IssueID: it.IssueID, TeamID: team, Family: true}); e != nil {
			refuse(e.Error())
			return
		}
	}
	plan := r.Plan
	it.Plan, it.Status, it.digest = &plan, "plan", plan.PlanSHA256
	if len(plan.Refusals) > 0 {
		it.Status, it.Refusals = "refused", plan.Refusals
	}
}
