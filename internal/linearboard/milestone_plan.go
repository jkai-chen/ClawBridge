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

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearprovider"
)

// Creating and updating a project's milestones (FR55 PR 2,
// live/FR55-PLAN.md §2 and its r1), FR13's flow with a milestone as the
// subject. The preview reads the project's milestones and the project,
// shows each field current → planned, and asks the server whether this
// caller may confirm (milestone_plan_reserve without confirm: the same
// refusals, nothing recorded); it sends nothing. The confirm reads again,
// refuses a plan whose current values changed since the preview, has the
// server reserve it, sends one save_milestone {project, id?, <only the
// given fields>} through the person's own Linear profile, reads the
// milestones back and settles the reservation with what the read shows.
// A settled outcome is final; an uncertain plan is never sent again.

// milestonePlannerV2 is a provider that can save a milestone and read the
// project's milestones and the project back.
type milestonePlannerV2 interface {
	MilestonePlanReason(update bool, fields []string) string
	SaveMilestone(ctx context.Context, plan linearprovider.MilestonePlan) (string, error)
	ListMilestones(ctx context.Context, project string) ([]linearprovider.Milestone, error)
	ProjectSnapshot(ctx context.Context, project string) (linearprovider.ProjectSnapshot, error)
}

// MilestonePlanInput is a plan: the Linear project by UUID, the milestone
// by UUID for an update (empty creates one), the fields to set (a nil Name
// and an absent date are left alone; a date Present with a nil Value is
// cleared), the operation ID, and on a confirm the preview's plan digest.
type MilestonePlanInput struct {
	ProjectID          string
	MilestoneID        string
	Name               *string
	TargetDate         linearprovider.Field[*string]
	OperationID        string
	Confirm            bool
	ExpectedPlanSHA256 string
}

func (in MilestonePlanInput) update() bool { return in.MilestoneID != "" }

// fields are the planned fields' names, as save_milestone and the server
// spell them, sorted.
func (in MilestonePlanInput) fields() []string {
	out := []string{}
	if in.Name != nil {
		out = append(out, "name")
	}
	if in.TargetDate.Present {
		out = append(out, "targetDate")
	}
	return out
}

// MilestonePlanChange is one planned field: its value now, as read, and the
// value planned. Names are text written by others and shown terminal-safe.
type MilestonePlanChange struct {
	Field   string `json:"field"`
	Current string `json:"current"`
	Planned string `json:"planned"`
}

// MilestonePlanPreview is what a plan would do: the operation, the project,
// each planned field current → planned, the fields sent (nothing else is),
// flags a person should read, and the digest a confirm repeats.
type MilestonePlanPreview struct {
	Op          string                `json:"op"`
	ProjectID   string                `json:"project_id"`
	MilestoneID string                `json:"milestone_id,omitempty"`
	Teams       []string              `json:"teams"`
	Changes     []MilestonePlanChange `json:"changes"`
	Sends       []string              `json:"sends"`
	// Flags are allowed but worth a look: a milestone date after the
	// project's target date (FR55 Q4).
	Flags      []string `json:"flags,omitempty"`
	PlanSHA256 string   `json:"plan_sha256"`
}

// MilestonePlanResult is a preview or a plan's outcome: preview, already (the
// milestone reads as planned; nothing sent), accepted, accepted_with_changes
// (Changed names the other fields Linear changed), uncertain or not_sent.
// Ask is set on a preview this caller may not confirm.
type MilestonePlanResult struct {
	Plan        MilestonePlanPreview `json:"plan"`
	Outcome     string               `json:"outcome"`
	MilestoneID string               `json:"milestone_id,omitempty"`
	Changed     []string             `json:"changed,omitempty"`
	By          string               `json:"by,omitempty"`
	Lane        string               `json:"lane,omitempty"`
	Ask         bool                 `json:"ask,omitempty"`
	Note        string               `json:"note,omitempty"`
}

// SetMilestonePlan previews a milestone plan, or confirms the plan
// previewed. A confirm counts against the project's hourly budget (FR8).
func (s *Service) SetMilestonePlan(ctx context.Context, scope bus.Request, in MilestonePlanInput) (MilestonePlanResult, error) {
	if !in.Confirm || in.OperationID == "" {
		return s.setMilestonePlan(ctx, scope, in)
	}
	var out MilestonePlanResult
	err := s.budgeted(ctx, scope, "milestone_plan", in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.setMilestonePlan(ctx, scope, in)
		return e
	})
	return out, err
}

// checkMilestonePlanInput is the checks that need no read: the UUIDs, an
// operation ID, the name's rules, a create's name, an update's field and
// real dates.
func checkMilestonePlanInput(in *MilestonePlanInput) error {
	if !linearprovider.ValidID(in.ProjectID) {
		return errors.New("a milestone plan takes the Linear project's UUID, never its name; nothing was sent")
	}
	in.ProjectID = strings.ToLower(in.ProjectID)
	if in.MilestoneID != "" {
		if !linearprovider.ValidID(in.MilestoneID) {
			return errors.New("a milestone is named by its UUID from linear milestone list, never by its name; nothing was sent")
		}
		in.MilestoneID = strings.ToLower(in.MilestoneID)
	}
	if in.OperationID == "" {
		return errors.New("a milestone plan takes an operation ID, on the preview and its confirm; nothing was sent")
	}
	if in.Confirm && in.ExpectedPlanSHA256 == "" {
		return errors.New("a confirm takes the preview's --expected-plan-sha256; nothing was sent")
	}
	if !in.Confirm && in.ExpectedPlanSHA256 != "" {
		return errors.New("--expected-plan-sha256 goes with --confirm")
	}
	if in.Name != nil {
		name, e := linearprovider.CleanMilestoneName(*in.Name)
		if e != nil {
			return fmt.Errorf("%w; nothing was sent", e)
		}
		in.Name = &name
	}
	switch {
	case !in.update() && in.Name == nil:
		return errors.New("a new milestone needs a name; nothing was sent")
	case in.update() && len(in.fields()) == 0:
		return errors.New("an update changes the name or the target date; nothing was sent")
	}
	if d := in.TargetDate; d.Present && d.Value != nil && !validDueDate(*d.Value) {
		return errors.New("target date must be a calendar date YYYY-MM-DD from 2000 to 2100; nothing was sent")
	}
	return nil
}

func (s *Service) setMilestonePlan(ctx context.Context, scope bus.Request, in MilestonePlanInput) (MilestonePlanResult, error) {
	var out MilestonePlanResult
	if e := checkMilestonePlanInput(&in); e != nil {
		return out, e
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return out, e
	}
	switch {
	case !b.Configured:
		return out, errors.New("task-board is not linked")
	case b.Mode != "project_set":
		return out, errors.New("a team board has no projects; link a project set to change one of its milestones")
	case !contains(b.LinearProjectIDs, in.ProjectID):
		return out, fmt.Errorf("project %s isn't in this binding; nothing was sent", in.ProjectID)
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
	fields := in.fields()
	planner, ok := p.(milestonePlannerV2)
	if !ok {
		return out, errors.New(linearprovider.MilestoneCreateRefused)
	}
	if reason := planner.MilestonePlanReason(in.update(), fields); reason != "" {
		return out, fmt.Errorf("%s (%s)", linearprovider.MilestoneCreateRefused, reason)
	}
	rows, project, e := readMilestonePlan(ctx, planner, in.ProjectID)
	if e != nil {
		return out, e
	}
	current, e := checkMilestonePlan(in, rows)
	if e != nil {
		return out, e
	}
	plan := newMilestonePlan(in, rows, current, project)
	out.Plan = plan
	if in.update() && milestoneSent(in, current) {
		out.Outcome, out.Note = "already", "the milestone already reads as planned; nothing was sent"
		return out, nil
	}
	reserveArgs := func(confirm bool) map[string]any {
		args := map[string]any{"operation_id": in.OperationID, "linear_project": plan.ProjectID, "team_ids": plan.Teams, "op": plan.Op, "fields": fields, "plan_sha256": plan.PlanSHA256, "confirm": confirm, "agent_marker": AgentMarker(agentEnviron())}
		if in.update() {
			args["milestone"] = in.MilestoneID
		} else {
			args["name_sha256"] = linearprovider.MilestoneNameHash(*in.Name)
		}
		return args
	}
	if !in.Confirm {
		out.Outcome = "preview"
		// The server's check, recording nothing: a caller it refuses
		// still gets the preview, and asks.
		check, e := s.teamAddCall(ctx, scope, "milestone_plan_reserve", reserveArgs(false))
		if e != nil {
			refusal, refused := planRefusal(e)
			if !refused {
				return out, e
			}
			out.Ask = true
			out.Note = "You may preview this plan but not confirm it (" + refusal + "): ask the project owner or a lead. Nothing was sent."
			return out, s.unchangedV2(ctx, scope, b)
		}
		out.By, out.Lane = addStr(check, "by"), addStr(check, "lane")
		out.Note = milestoneWho(out.By, out.Lane, addStr(check, "team_id"), check["proven"] != false)
		return out, s.unchangedV2(ctx, scope, b)
	}
	if plan.PlanSHA256 != in.ExpectedPlanSHA256 {
		return out, errors.New("the project's milestones changed since the preview; preview again; nothing was sent")
	}
	if profile.Access != "write" {
		return out, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	// The server decides who confirms; these only spare a call it would
	// refuse anyway.
	if access.SessionID == "" && access.ProjectRole != "owner" && access.ProjectRole != "lead" {
		if e = confirmerEligible(ctx, p); e != nil {
			return out, e
		}
	}
	if access.SessionID != "" {
		if len(b.Lanes) == 0 {
			return out, outsideLaneError("outside_lane: this binding has no lanes, so a session changes no milestone; the project owner, a lead or a team's lead does, in their own terminal; nothing was sent")
		}
		if !slices.ContainsFunc(plan.Teams, func(t string) bool { return contains(access.LaneWriteTeamIDs, t) }) {
			return out, outsideLaneError("outside_lane: the project holds no write team of this session's lane; the project owner or a lead changes another lane's project; nothing was sent")
		}
	}
	reserved, e := s.teamAddCall(ctx, scope, "milestone_plan_reserve", reserveArgs(true))
	if e != nil {
		return out, fmt.Errorf("the milestone plan could not be reserved; nothing was sent: %w", e)
	}
	out.By, out.Lane = addStr(reserved, "by"), addStr(reserved, "lane")
	if reserved["replay"] == true {
		out.Outcome = addStr(reserved, "state")
		out.Changed = stringList(reserved["changed"])
		out.MilestoneID = addStr(reserved, "milestone")
		if out.Outcome == "reserved" {
			out.Outcome = "uncertain"
			out.Note = "an earlier confirm with this operation ID may have sent the plan and never settled; read the project's milestones in Linear and tell the owner; nothing was sent again"
		}
		return out, nil
	}
	settle := func(outcome string, changed []string, milestone string) error {
		args := map[string]any{"operation_id": in.OperationID, "outcome": outcome, "changed": changed}
		if milestone != "" {
			args["milestone"] = milestone
		}
		_, err := s.teamAddCall(ctx, scope, "milestone_plan_settle", args)
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
		return out, errors.Join(fmt.Errorf("nothing was sent: %w", e), settle("not_sent", nil, ""))
	}
	known := map[string]bool{}
	for _, m := range rows {
		known[m.ID] = true
	}
	savedID, sendErr := planner.SaveMilestone(ctx, linearprovider.MilestonePlan{Project: plan.ProjectID, ID: in.MilestoneID, Name: in.Name, TargetDate: in.TargetDate})
	// The read-back comes before the settle, so the settle states what the
	// read shows: it is final. Linear's reads can lag its write, so a field
	// not as sent yet is read again, unless Linear refused the write.
	refused := definiteRefusal(sendErr)
	var after []linearprovider.Milestone
	var found *linearprovider.Milestone
	var ambiguous bool
	read := func() error {
		var e error
		if after, e = planner.ListMilestones(ctx, plan.ProjectID); e != nil {
			return e
		}
		found, ambiguous = findSaved(in, savedID, known, after)
		return nil
	}
	readErr := read()
	for _, wait := range teamAddRereads {
		if refused || (readErr == nil && found != nil && milestoneSent(in, *found)) {
			break
		}
		if e = teamAddWait(ctx, wait); e != nil {
			break
		}
		readErr = read()
	}
	uncertain := "; whether Linear saved the milestone is unknown: read the project's milestones in Linear and tell the owner; nothing is retried"
	switch {
	case readErr == nil && refused && milestoneUnsent(in, current, found, after):
		out.Outcome = "not_sent"
		out.Note = "Linear refused the plan, and the project's milestones read as before"
		return out, errors.Join(sendErr, settle("not_sent", nil, ""))
	case readErr != nil:
		out.Outcome = "uncertain"
		out.Note = "the project's milestones couldn't be read back" + uncertain
		return out, errors.Join(sendErr, readErr, settle("uncertain", nil, ""))
	case ambiguous:
		out.Outcome = "uncertain"
		out.Note = "more than one new milestone reads with that name, so which one this plan made can't be told" + uncertain
		return out, errors.Join(sendErr, settle("uncertain", nil, ""))
	case found == nil:
		out.Outcome = "uncertain"
		out.Note = "the read-back doesn't show the milestone" + uncertain
		return out, errors.Join(sendErr, settle("uncertain", nil, ""))
	case !milestoneSent(in, *found):
		out.Outcome = "uncertain"
		out.Note = "the read-back doesn't show " + milestoneWords(milestoneMissing(in, *found)) + " as sent" + uncertain
		return out, errors.Join(sendErr, settle("uncertain", nil, ""))
	}
	out.MilestoneID = found.ID
	if in.update() {
		out.Changed = milestoneChanges(in, current, *found)
	}
	out.Outcome = "accepted"
	if len(out.Changed) > 0 {
		out.Outcome = "accepted_with_changes"
		out.Note = "Linear also changed: " + strings.Join(out.Changed, ", ") + "; ClawBridge sent only the plan's fields, and never undoes Linear's changes"
	}
	created := ""
	if !in.update() {
		created = found.ID
	}
	if e = settle("accepted", out.Changed, created); e != nil {
		return out, fmt.Errorf("the plan was saved (%s) but wasn't settled on the server: %w", out.Outcome, e)
	}
	return out, nil
}

// readMilestonePlan reads the project's milestones and the project itself
// (its teams and target date).
func readMilestonePlan(ctx context.Context, planner milestonePlannerV2, project string) ([]linearprovider.Milestone, linearprovider.ProjectSnapshot, error) {
	rows, e := planner.ListMilestones(ctx, project)
	if e != nil {
		return nil, linearprovider.ProjectSnapshot{}, e
	}
	snap, e := planner.ProjectSnapshot(ctx, project)
	return rows, snap, e
}

// checkMilestonePlan is the checks that need the read: an update's
// milestone is one of the project's (the milestone it returns), and no
// other milestone already has the planned name once case is folded and the
// text is NFC (a create's, or a rename's).
func checkMilestonePlan(in MilestonePlanInput, rows []linearprovider.Milestone) (linearprovider.Milestone, error) {
	var current linearprovider.Milestone
	if in.update() {
		i := slices.IndexFunc(rows, func(m linearprovider.Milestone) bool { return m.ID == in.MilestoneID })
		if i < 0 {
			return current, errors.New("milestone_not_in_project: that milestone is not one of the project's milestones; list them first; nothing was sent")
		}
		current = rows[i]
	}
	if in.Name != nil {
		key := linearprovider.MilestoneNameKey(*in.Name)
		for _, m := range rows {
			if m.ID != in.MilestoneID && linearprovider.MilestoneNameKey(m.Name) == key {
				return current, fmt.Errorf("the project already has a milestone named %q (names are compared without case); nothing was sent", clirender.Safe(m.Name))
			}
		}
	}
	return current, nil
}

// findSaved is the milestone a confirm made or changed in the read-back. An
// update's is its own. A create's is the one the save named by ID; when the
// save named none, the one new milestone (not among the rows read before the
// send) whose name folds to the planned one, which the plan's digest proved
// had no namesake before. found is nil when there is none; ambiguous when
// more than one fits.
func findSaved(in MilestonePlanInput, saved string, known map[string]bool, after []linearprovider.Milestone) (found *linearprovider.Milestone, ambiguous bool) {
	for i, m := range after {
		switch {
		case in.update():
			if m.ID == in.MilestoneID {
				return &after[i], false
			}
		case saved != "":
			if m.ID == saved {
				return &after[i], false
			}
		case !known[m.ID] && linearprovider.MilestoneNameKey(m.Name) == linearprovider.MilestoneNameKey(*in.Name):
			if found != nil {
				return nil, true
			}
			found = &after[i]
		}
	}
	return found, false
}

func milestoneDate(m linearprovider.Milestone) (string, bool) {
	if !m.TargetDateKnown {
		return "", false
	}
	return derefDate(m.TargetDate), true
}

// milestoneSent reports whether every planned field reads as planned.
func milestoneSent(in MilestonePlanInput, m linearprovider.Milestone) bool {
	return len(milestoneMissing(in, m)) == 0
}

// milestoneMissing names the planned fields that don't read as planned.
func milestoneMissing(in MilestonePlanInput, m linearprovider.Milestone) []string {
	out := []string{}
	if in.Name != nil && m.Name != *in.Name {
		out = append(out, "name")
	}
	if in.TargetDate.Present {
		if got, ok := milestoneDate(m); !ok || got != derefDate(in.TargetDate.Value) {
			out = append(out, "targetDate")
		}
	}
	return out
}

// milestoneUnsent reports whether the project's milestones read as before
// the send: an update's own row is unchanged in each planned field; a
// create left no new milestone with the planned name.
func milestoneUnsent(in MilestonePlanInput, before linearprovider.Milestone, found *linearprovider.Milestone, after []linearprovider.Milestone) bool {
	if !in.update() {
		return found == nil
	}
	if found == nil {
		return false
	}
	if in.Name != nil && found.Name != before.Name {
		return false
	}
	if in.TargetDate.Present {
		b, bOK := milestoneDate(before)
		a, aOK := milestoneDate(*found)
		if !bOK || !aOK || a != b {
			return false
		}
	}
	return true
}

// milestoneChanges names the other fields of an updated milestone that
// differ between the reads: the name and the target date when the plan
// didn't send them, and the sort order. Names only, never values.
func milestoneChanges(in MilestonePlanInput, before, after linearprovider.Milestone) []string {
	out := []string{}
	if in.Name == nil && after.Name != before.Name {
		out = append(out, "name")
	}
	if !in.TargetDate.Present {
		b, bOK := milestoneDate(before)
		a, aOK := milestoneDate(after)
		if bOK && aOK && a != b {
			out = append(out, "targetDate")
		}
	}
	if before.SortOrder != nil && after.SortOrder != nil && *before.SortOrder != *after.SortOrder {
		out = append(out, "sortOrder")
	}
	slices.Sort(out)
	return out
}

// newMilestonePlan is the preview: each planned field current → planned,
// what is sent, the flags, and the digest over the project, its teams, the
// operation, the planned values, the current values read, the project's
// milestones (as IDs with their names' hashes: a create's "no namesake
// existed" is in it) and the project's target date.
func newMilestonePlan(in MilestonePlanInput, rows []linearprovider.Milestone, current linearprovider.Milestone, project linearprovider.ProjectSnapshot) MilestonePlanPreview {
	plan := MilestonePlanPreview{Op: "create", ProjectID: in.ProjectID, MilestoneID: in.MilestoneID, Teams: sortedTeams(project.TeamIDs), Changes: []MilestonePlanChange{}, Sends: append([]string{"project"}, in.fields()...)}
	subject := "new"
	if in.update() {
		plan.Op, subject = "update", in.MilestoneID
		plan.Sends = append([]string{"project", "id"}, in.fields()...)
	}
	planned := [][]any{}
	if in.Name != nil {
		cur := "-"
		if in.update() {
			cur = clirender.Safe(current.Name)
		}
		plan.Changes = append(plan.Changes, MilestonePlanChange{Field: "name", Current: cur, Planned: clirender.Safe(*in.Name)})
		planned = append(planned, []any{"name", *in.Name})
	}
	if in.TargetDate.Present {
		cur := "none"
		if in.update() {
			cur = "unknown"
			if d, ok := milestoneDate(current); ok {
				cur = dateWord(d)
			}
		}
		plan.Changes = append(plan.Changes, MilestonePlanChange{Field: "targetDate", Current: cur, Planned: dateWord(derefDate(in.TargetDate.Value))})
		var v any
		if in.TargetDate.Value != nil {
			v = *in.TargetDate.Value
		}
		planned = append(planned, []any{"targetDate", v})
	}
	// Q4: a milestone date after the project's own target date is allowed,
	// and flagged.
	date, dateOK := derefDate(in.TargetDate.Value), in.TargetDate.Present
	if !in.TargetDate.Present && in.update() {
		date, dateOK = milestoneDate(current)
	}
	projectTarget, projectTargetOK := project.TargetDate()
	if dateOK && date != "" && projectTargetOK && projectTarget != "" && date > projectTarget {
		plan.Flags = append(plan.Flags, fmt.Sprintf("the milestone's target date %s is after the project's target date %s; Linear allows it", date, projectTarget))
	}
	cur := map[string]any{"name": "unknown", "targetDate": "unknown"}
	if in.update() {
		cur["name"] = current.Name
		if d, ok := milestoneDate(current); ok {
			cur["targetDate"] = d
		}
	}
	baseline := []string{}
	for _, m := range rows {
		baseline = append(baseline, m.ID+":"+linearprovider.MilestoneNameHash(m.Name))
	}
	slices.Sort(baseline)
	projectDate := "unknown"
	if projectTargetOK {
		projectDate = projectTarget
	}
	raw, _ := json.Marshal([]any{plan.ProjectID, plan.Teams, plan.Op, subject, planned, cur, baseline, projectDate})
	sum := sha256.Sum256(raw)
	plan.PlanSHA256 = hex.EncodeToString(sum[:])
	return plan
}

// milestoneWords names plan fields as a person reads them.
func milestoneWords(fields []string) string {
	words := []string{}
	for _, f := range fields {
		if f == "targetDate" {
			f = "target date"
		}
		words = append(words, f)
	}
	return strings.Join(words, ", ")
}

// milestoneWho says who confirms a preview the server allows.
func milestoneWho(by, lane, team string, proven bool) string {
	var who string
	switch by {
	case "owner":
		who = "You confirm this as the project owner, from your own terminal."
	case "project_lead":
		who = "You confirm this as a project lead, from your own terminal."
	case "team_lead":
		who = "You confirm this as team " + team + "'s lead in the team roles, from your own terminal."
	case "coordinator":
		who = "You confirm this as the project's coordinator, inside lane " + lane + "."
	case "team_lead_session":
		who = "You confirm this as a session of team " + team + "'s lead, inside lane " + lane + "."
	default:
		who = "The server allows your confirm."
	}
	who += " Nothing else changes: only the fields shown are sent."
	if !proven {
		who += " This server creates and updates no milestone until its operator sets serve --milestone-plan-proven (milestone_plan_unproven); the preview works."
	}
	return who
}
