package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// A bound team added to a Linear project of the binding (FR10,
// live/FR10-PLAN.md). The preview reads the project and sends nothing.
// The confirm reads it again, refuses a plan that changed since the
// preview, has the server reserve it (the server decides who may confirm,
// and refuses until its operator proves team adds), sends one save_project
// {id, addTeams: [team]} through the person's own Linear profile, reads the
// project back and settles the reservation with what the read shows. A
// settled outcome is final; nothing is retried, and no team is ever
// removed.

// teamAdderV2 is a provider that can add a team to a project and read the
// project back.
type teamAdderV2 interface {
	ProjectTeamAddReason() string
	AddProjectTeam(ctx context.Context, project, team string) error
	ProjectSnapshot(ctx context.Context, project string) (linearprovider.ProjectSnapshot, error)
}

// ProjectTeamAddInput is an add: the Linear project and the bound team by
// UUID, and on a confirm the operation ID and the preview's plan digest.
type ProjectTeamAddInput struct {
	ProjectID, TeamID, OperationID string
	Confirm                        bool
	ExpectedPlanSHA256             string
}

// ProjectTeamAddPlan is what an add would do: the project's teams now, the
// team added, and the fields sent (nothing else is).
type ProjectTeamAddPlan struct {
	ProjectID  string   `json:"project_id"`
	TeamID     string   `json:"team_id"`
	Teams      []string `json:"teams"`
	Sends      []string `json:"sends"`
	PlanSHA256 string   `json:"plan_sha256"`
	before     linearprovider.ProjectSnapshot
}

// ProjectTeamAddResult is a preview or an add's outcome: preview,
// already (the team is there; nothing sent), accepted,
// accepted_with_changes (Changed names the other fields Linear changed),
// uncertain or not_sent.
type ProjectTeamAddResult struct {
	Plan    ProjectTeamAddPlan `json:"plan"`
	Outcome string             `json:"outcome"`
	Changed []string           `json:"changed,omitempty"`
	// Unverified names fields a read left out (lead, members): the add
	// couldn't prove them unchanged.
	Unverified []string `json:"unverified,omitempty"`
	By         string   `json:"by,omitempty"`
	Lane       string   `json:"lane,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// AddProjectTeam previews adding a team to a project, or confirms the add
// previewed. A confirm counts against the project's hourly budget (FR8):
// the add, and its snapshot reads before and after.
func (s *Service) AddProjectTeam(ctx context.Context, scope bus.Request, in ProjectTeamAddInput) (ProjectTeamAddResult, error) {
	if !in.Confirm || in.OperationID == "" {
		return s.addProjectTeam(ctx, scope, in)
	}
	var out ProjectTeamAddResult
	err := s.budgeted(ctx, scope, "project_team_add", in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.addProjectTeam(ctx, scope, in)
		return e
	})
	return out, err
}

func (s *Service) addProjectTeam(ctx context.Context, scope bus.Request, in ProjectTeamAddInput) (ProjectTeamAddResult, error) {
	var out ProjectTeamAddResult
	// R1: only UUIDs; Linear's addTeams would resolve a name itself.
	if !linearprovider.ValidID(in.ProjectID) || !linearprovider.ValidID(in.TeamID) {
		return out, errors.New("adding a team takes the Linear project's UUID and the bound team's UUID, never a name; nothing was sent")
	}
	in.ProjectID, in.TeamID = strings.ToLower(in.ProjectID), strings.ToLower(in.TeamID)
	if in.Confirm && (in.OperationID == "" || in.ExpectedPlanSHA256 == "") {
		return out, errors.New("a confirm takes --operation-id and the preview's --expected-plan-sha256; nothing was sent")
	}
	if !in.Confirm && in.ExpectedPlanSHA256 != "" {
		return out, errors.New("--expected-plan-sha256 goes with --confirm")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return out, e
	}
	switch {
	case !b.Configured:
		return out, errors.New("task-board is not linked")
	case b.Mode != "project_set":
		return out, errors.New("a team board has no projects; link a project set to add a team to one of its projects")
	case !contains(b.LinearProjectIDs, in.ProjectID):
		return out, fmt.Errorf("project %s isn't in this binding; nothing was sent", in.ProjectID)
	case b.TeamAccess(in.TeamID) != bus.TeamAccessWrite:
		return out, fmt.Errorf("team %s isn't a write team of this binding; nothing was sent", in.TeamID)
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return out, e
	}
	open := s.open
	if in.Confirm {
		open = s.openForWrite
	}
	profile, p, done, e := open(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	adder, ok := p.(teamAdderV2)
	if !ok {
		return out, errors.New("this Linear connection can't add a team to a project")
	}
	if reason := adder.ProjectTeamAddReason(); reason != "" {
		return out, fmt.Errorf("this Linear connection can't add a team to a project (%s); nothing was sent", reason)
	}
	before, e := adder.ProjectSnapshot(ctx, in.ProjectID)
	if e != nil {
		return out, e
	}
	plan := ProjectTeamAddPlan{ProjectID: in.ProjectID, TeamID: in.TeamID, Teams: sortedTeams(before.TeamIDs), Sends: append([]string{}, linearprovider.ProjectTeamAddFields...), before: before}
	plan.PlanSHA256 = teamAddDigest(plan)
	out.Plan = plan
	if before.HasTeam(in.TeamID) {
		out.Outcome, out.Note = "already", "the team is already in the project; nothing was sent"
		return out, nil
	}
	if !in.Confirm {
		out.Outcome = "preview"
		out.Note = teamAddWho(b, access)
		return out, s.unchangedV2(ctx, scope, b)
	}
	if plan.PlanSHA256 != in.ExpectedPlanSHA256 {
		return out, errors.New("the project's teams changed since the preview; preview again; nothing was sent")
	}
	if profile.Access != "write" {
		return out, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	// The server decides who confirms; this only spares a call it would
	// refuse anyway.
	if access.SessionID == "" && access.ProjectRole != "owner" && access.ProjectRole != "lead" {
		// R3: a team's lead in the team roles confirms only while Linear
		// states them an active, non-guest user; the server checks the
		// mapping when it reserves.
		if e = confirmerEligible(ctx, p); e != nil {
			return out, e
		}
	}
	if access.SessionID != "" {
		if !access.Coordinator {
			return out, errors.New("only the project's coordinator session (or, while the dispatcher is on, a domain's) adds a team to a project; preview it and ask the project owner or a lead; nothing was sent")
		}
		if len(b.Lanes) == 0 {
			return out, outsideLaneError("outside_lane: this binding has no lanes, so a coordinator adds no team to a project; nothing was sent")
		}
		if e = checkLaneV2(b, access, in.TeamID); e != nil {
			return out, e
		}
		if !slices.ContainsFunc(plan.Teams, func(t string) bool { return contains(access.LaneWriteTeamIDs, t) }) {
			return out, outsideLaneError("outside_lane: the project holds no write team of this session's lane; the project owner or a lead adds a team to another lane's project; nothing was sent")
		}
	}
	reserved, e := s.teamAddCall(ctx, scope, "project_team_add_reserve", map[string]any{"operation_id": in.OperationID, "linear_project": plan.ProjectID, "team_id": plan.TeamID, "project_teams": plan.Teams, "plan_sha256": plan.PlanSHA256})
	if e != nil {
		return out, fmt.Errorf("the add could not be reserved; nothing was sent: %w", e)
	}
	out.By, out.Lane = addStr(reserved, "by"), addStr(reserved, "lane")
	if reserved["replay"] == true {
		out.Outcome = addStr(reserved, "state")
		out.Changed = stringList(reserved["changed"])
		if out.Outcome == "reserved" {
			out.Outcome = "uncertain"
			out.Note = "an earlier confirm with this operation ID may have sent the add and never settled; read the project in Linear; nothing was sent again"
		}
		return out, nil
	}
	settle := func(outcome string, changed, unverified []string) error {
		_, err := s.teamAddCall(ctx, scope, "project_team_add_settle", map[string]any{"operation_id": in.OperationID, "outcome": outcome, "changed": changed, "unverified": unverified})
		return err
	}
	// The lane and the binding hold right up to the send.
	fresh, e := s.writeAccessV2(ctx, scope, b)
	if e == nil && (fresh.SessionID != access.SessionID || !sameLaneV2(b, access, fresh)) {
		e = errLaneChanged
	}
	if e == nil {
		e = s.unchangedV2(ctx, scope, b)
	}
	if e == nil {
		e = ctx.Err()
	}
	if e != nil {
		out.Outcome = "not_sent"
		return out, errors.Join(fmt.Errorf("nothing was sent: %w", e), settle("not_sent", nil, nil))
	}
	sendErr := adder.AddProjectTeam(ctx, plan.ProjectID, plan.TeamID)
	// The read-back comes before the settle, so the settle states what
	// the read shows (L1): it is final. Linear's reads can lag its write,
	// so a team not there yet is read again, unless Linear refused the add
	// outright (F1).
	refused := definiteRefusal(sendErr)
	after, readErr := adder.ProjectSnapshot(ctx, plan.ProjectID)
	for _, wait := range teamAddRereads {
		if refused || (readErr == nil && after.HasTeam(plan.TeamID)) {
			break
		}
		if e = teamAddWait(ctx, wait); e != nil {
			break
		}
		after, readErr = adder.ProjectSnapshot(ctx, plan.ProjectID)
	}
	switch {
	case readErr == nil && !after.HasTeam(plan.TeamID) && refused:
		out.Outcome = "not_sent"
		out.Note = "Linear refused the add, and the project doesn't hold the team"
		return out, errors.Join(sendErr, settle("not_sent", nil, nil))
	case readErr != nil || !after.HasTeam(plan.TeamID):
		out.Outcome = "uncertain"
		out.Note = "the read-back doesn't show the team, so whether Linear added it is unknown; read the project in Linear; nothing is retried"
		return out, errors.Join(sendErr, readErr, settle("uncertain", nil, nil))
	}
	out.Changed = linearprovider.TeamAddChanges(before, after, plan.TeamID)
	out.Unverified = linearprovider.TeamAddUnverified(before, after)
	out.Outcome = "accepted"
	if len(out.Changed) > 0 {
		out.Outcome = "accepted_with_changes"
		out.Note = "Linear also changed: " + strings.Join(out.Changed, ", ") + "; ClawBridge sent only the team, and never undoes Linear's changes"
	}
	if len(out.Unverified) > 0 {
		out.Note = strings.TrimPrefix(out.Note+"; ", "; ") + "Linear's read left out " + strings.Join(out.Unverified, " and ") + ", so the add couldn't prove them unchanged"
	}
	if e = settle("accepted", out.Changed, out.Unverified); e != nil {
		return out, fmt.Errorf("the team was added (%s) but the add wasn't settled on the server: %w", out.Outcome, e)
	}
	return out, nil
}

// teamAddRereads are the waits before reading a project again when the
// team isn't there yet: 2 s, then 4 s more.
var teamAddRereads = []time.Duration{2 * time.Second, 4 * time.Second}

// teamAddWait waits d or until ctx ends; tests replace it.
var teamAddWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// definiteRefusal is a send Linear provably didn't apply: it never reached
// Linear's tool, or the tool answered with an error.
func definiteRefusal(err error) bool {
	var pe *linearprovider.Error
	return errors.As(err, &pe) && (pe.NotSent || pe.Kind == "provider_rejected")
}

func teamAddWho(b bus.TaskboardBindingV2, access bus.TaskboardAccess) string {
	switch {
	case access.SessionID == "":
		return "Only the project owner, a lead, or the added team's own lead in the team roles confirms an add, from their own terminal, once the server's operator has proven team adds."
	case access.Coordinator && len(b.Lanes) > 0:
		return "As the project's coordinator (or, while the dispatcher is on, a domain's) you add only your lane's teams, to a project that already holds one, when your person or the owner asked for it."
	default:
		return "A session previews an add only; the project owner or a lead confirms it."
	}
}

func (s *Service) teamAddCall(ctx context.Context, scope bus.Request, op string, args map[string]any) (map[string]any, error) {
	scope.Operation = op
	scope.Args = instanceArgs(scope.Args)
	for k, v := range args {
		scope.Args[k] = v
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	m, _ := r.Data.(map[string]any)
	if m == nil {
		return nil, errors.New("the server's answer was not a team-add record")
	}
	return m, nil
}

// ProjectTeamAdds lists the settled adds and how many came back with other
// fields changed.
func (s *Service) ProjectTeamAdds(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "project_team_add_list"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	return r.Data, nil
}

func sortedTeams(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		id = strings.ToLower(id)
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func teamAddDigest(plan ProjectTeamAddPlan) string {
	raw, _ := json.Marshal([]any{plan.ProjectID, plan.TeamID, plan.Teams, plan.Sends})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func addStr(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func stringList(v any) []string {
	out := []string{}
	switch list := v.(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// viewerProvider reads the connected Linear user.
type viewerProvider interface {
	Viewer(context.Context) (linearprovider.Viewer, error)
}

// confirmerEligible refuses unless the profile's own Linear user is an
// active, non-guest user of the workspace, as Linear states it: a team's
// lead in the team roles adds that team only while they are one.
func confirmerEligible(ctx context.Context, p Provider) error {
	vp, okV := p.(viewerProvider)
	wp, okW := p.(WorkspaceUsersProvider)
	if !okV || !okW {
		return errors.New("this Linear connection can't say whether you are an active, non-guest user; nothing was sent")
	}
	me, e := vp.Viewer(ctx)
	if e != nil {
		return fmt.Errorf("your Linear user couldn't be read; nothing was sent: %w", e)
	}
	cursor := ""
	for page := 0; page < maxUserPages; page++ {
		got, e := wp.WorkspaceUsers(ctx, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return fmt.Errorf("the workspace's users couldn't be read; nothing was sent: %w", e)
		}
		for _, u := range got.Items {
			if u.ID == me.ID {
				if !u.Eligible() {
					return errors.New("a team's lead adds their team only as an active, non-guest Linear user, and Linear doesn't say you are one; nothing was sent")
				}
				return nil
			}
		}
		if got.Complete || got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}
	return errors.New("your Linear user isn't among the workspace's users; nothing was sent")
}
