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
	"clawbridge/internal/linearprovider"
)

// A Linear project's priority, target date and start date (FR13,
// live/FR13-PLAN.md §2 and r1). The preview reads the project, shows each
// field's current and planned value, and asks the server whether this
// caller may confirm (project_plan_reserve without confirm: the same
// refusals, nothing recorded); it sends nothing. The confirm reads the
// project again, refuses a plan whose current values changed since the
// preview, has the server reserve it (the server decides who confirms, and
// refuses until its operator proves plans), sends one save_project {id,
// <only the given fields>} through the person's own Linear profile, reads
// the project back and settles the reservation with what the read shows.
// A settled outcome is final; an uncertain plan is never sent again.

// projectPlannerV2 is a provider that can set a project's plan fields and
// read the project back.
type projectPlannerV2 interface {
	ProjectPlanReason(fields []string) string
	SaveProjectPlan(ctx context.Context, project string, plan linearprovider.ProjectPlan) error
	ProjectSnapshot(ctx context.Context, project string) (linearprovider.ProjectSnapshot, error)
}

// ProjectPlanInput is a plan: the Linear project by UUID and the fields to
// set (a nil Priority and an absent date are left alone; a date Present
// with a nil Value is cleared), the operation ID, and on a confirm the
// preview's plan digest.
type ProjectPlanInput struct {
	ProjectID             string
	Priority              *int
	TargetDate, StartDate linearprovider.Field[*string]
	OperationID           string
	Confirm               bool
	ExpectedPlanSHA256    string
}

// fields are the planned fields' names, as save_project and the server
// spell them, sorted.
func (in ProjectPlanInput) fields() []string {
	out := []string{}
	if in.Priority != nil {
		out = append(out, "priority")
	}
	if in.StartDate.Present {
		out = append(out, "startDate")
	}
	if in.TargetDate.Present {
		out = append(out, "targetDate")
	}
	return out
}

// ProjectPlanChange is one planned field: its value now, as read, and the
// value planned. A date's resolution is shown when Linear holds it at
// another precision than a day.
type ProjectPlanChange struct {
	Field      string `json:"field"`
	Current    string `json:"current"`
	Planned    string `json:"planned"`
	Resolution string `json:"resolution,omitempty"`
}

// ProjectPlanPreview is what a plan would do: the project's teams, each
// planned field current → planned, the fields sent (nothing else is), and
// the digest a confirm repeats.
type ProjectPlanPreview struct {
	ProjectID  string              `json:"project_id"`
	Teams      []string            `json:"teams"`
	Changes    []ProjectPlanChange `json:"changes"`
	Sends      []string            `json:"sends"`
	PlanSHA256 string              `json:"plan_sha256"`
	before     linearprovider.ProjectSnapshot
}

// ProjectPlanResult is a preview or a plan's outcome: preview, already
// (the project reads as planned; nothing sent), accepted,
// accepted_with_changes (Changed names the other fields Linear changed),
// uncertain or not_sent. Ask is set on a preview this caller may not
// confirm: it asks the project owner or a lead.
type ProjectPlanResult struct {
	Plan    ProjectPlanPreview `json:"plan"`
	Outcome string             `json:"outcome"`
	Changed []string           `json:"changed,omitempty"`
	// Unverified names fields a read left out (lead, members): the plan
	// couldn't prove them unchanged.
	Unverified []string `json:"unverified,omitempty"`
	By         string   `json:"by,omitempty"`
	Lane       string   `json:"lane,omitempty"`
	Ask        bool     `json:"ask,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// SetProjectPlan previews a project's plan, or confirms the plan
// previewed. A confirm counts against the project's hourly budget (FR8):
// the write, and its snapshot reads before and after.
func (s *Service) SetProjectPlan(ctx context.Context, scope bus.Request, in ProjectPlanInput) (ProjectPlanResult, error) {
	if !in.Confirm || in.OperationID == "" {
		return s.setProjectPlan(ctx, scope, in)
	}
	var out ProjectPlanResult
	err := s.budgeted(ctx, scope, "project_plan", in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.setProjectPlan(ctx, scope, in)
		return e
	})
	return out, err
}

// checkPlanInput is §2.2's checks that need no read: the project's UUID,
// an operation ID, at least one field, a priority 0-4 and real dates.
func checkPlanInput(in *ProjectPlanInput) error {
	if !linearprovider.ValidID(in.ProjectID) {
		return errors.New("a project's plan takes the Linear project's UUID, never its name; nothing was sent")
	}
	in.ProjectID = strings.ToLower(in.ProjectID)
	if in.OperationID == "" {
		return errors.New("a project's plan takes an operation ID, on the preview and its confirm; nothing was sent")
	}
	if in.Confirm && in.ExpectedPlanSHA256 == "" {
		return errors.New("a confirm takes the preview's --expected-plan-sha256; nothing was sent")
	}
	if !in.Confirm && in.ExpectedPlanSHA256 != "" {
		return errors.New("--expected-plan-sha256 goes with --confirm")
	}
	if len(in.fields()) == 0 {
		return errors.New("a project's plan sets at least one of priority, target date and start date; nothing was sent")
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 4) {
		return errPriority
	}
	for _, d := range []struct {
		name string
		f    linearprovider.Field[*string]
	}{{"target date", in.TargetDate}, {"start date", in.StartDate}} {
		if d.f.Present && d.f.Value != nil && !validDueDate(*d.f.Value) {
			return errors.New(d.name + " must be a calendar date YYYY-MM-DD; nothing was sent")
		}
	}
	return nil
}

// ParsePlanDate reads a project's target or start date: a real calendar
// date written YYYY-MM-DD, from 2000-01-01 to 2100-12-31.
func ParsePlanDate(name string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || !validDueDate(s) {
		return "", errors.New(name + " must be a calendar date YYYY-MM-DD")
	}
	return s, nil
}

func (s *Service) setProjectPlan(ctx context.Context, scope bus.Request, in ProjectPlanInput) (ProjectPlanResult, error) {
	var out ProjectPlanResult
	if e := checkPlanInput(&in); e != nil {
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
		return out, errors.New("a team board has no projects; link a project set to plan one of its projects")
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
	planner, ok := p.(projectPlannerV2)
	fields := in.fields()
	if !ok {
		return out, fmt.Errorf("this Linear connection can't set project %s; nothing was sent", planWords(fields))
	}
	if reason := planner.ProjectPlanReason(fields); reason != "" {
		return out, fmt.Errorf("this Linear connection can't set project %s (%s); nothing was sent", planWords(fields), reason)
	}
	before, e := planner.ProjectSnapshot(ctx, in.ProjectID)
	if e != nil {
		return out, e
	}
	if e = checkPlanDates(in, before); e != nil {
		return out, e
	}
	plan := newProjectPlan(in, before)
	out.Plan = plan
	if planSent(in, before) {
		out.Outcome, out.Note = "already", "the project already reads as planned; nothing was sent"
		return out, nil
	}
	reserveArgs := func(confirm bool) map[string]any {
		return map[string]any{"operation_id": in.OperationID, "linear_project": plan.ProjectID, "team_ids": plan.Teams, "fields": fields, "plan_sha256": plan.PlanSHA256, "confirm": confirm, "agent_marker": AgentMarker(agentEnviron())}
	}
	if !in.Confirm {
		out.Outcome = "preview"
		// The server's check, recording nothing: a caller it refuses
		// still gets the preview, and asks.
		check, e := s.teamAddCall(ctx, scope, "project_plan_reserve", reserveArgs(false))
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
		out.Note = planWho(out.By, out.Lane, addStr(check, "team_id"), check["proven"] != false)
		return out, s.unchangedV2(ctx, scope, b)
	}
	if plan.PlanSHA256 != in.ExpectedPlanSHA256 {
		return out, errors.New("the project changed since the preview; preview again; nothing was sent")
	}
	if profile.Access != "write" {
		return out, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	// The server decides who confirms; these only spare a call it would
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
		if len(b.Lanes) == 0 {
			return out, outsideLaneError("outside_lane: this binding has no lanes, so a session sets no project plan; the project owner, a lead or a team's lead does, in their own terminal; nothing was sent")
		}
		if !slices.ContainsFunc(plan.Teams, func(t string) bool { return contains(access.LaneWriteTeamIDs, t) }) {
			return out, outsideLaneError("outside_lane: the project holds no write team of this session's lane; the project owner or a lead sets another lane's project; nothing was sent")
		}
	}
	reserved, e := s.teamAddCall(ctx, scope, "project_plan_reserve", reserveArgs(true))
	if e != nil {
		return out, fmt.Errorf("the plan could not be reserved; nothing was sent: %w", e)
	}
	out.By, out.Lane = addStr(reserved, "by"), addStr(reserved, "lane")
	if reserved["replay"] == true {
		out.Outcome = addStr(reserved, "state")
		out.Changed = stringList(reserved["changed"])
		if out.Outcome == "reserved" {
			out.Outcome = "uncertain"
			out.Note = "an earlier confirm with this operation ID may have sent the plan and never settled; read the project in Linear and tell the owner; nothing was sent again"
		}
		return out, nil
	}
	settle := func(outcome string, changed []string) error {
		_, err := s.teamAddCall(ctx, scope, "project_plan_settle", map[string]any{"operation_id": in.OperationID, "outcome": outcome, "changed": changed})
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
		return out, errors.Join(fmt.Errorf("nothing was sent: %w", e), settle("not_sent", nil))
	}
	sendErr := planner.SaveProjectPlan(ctx, plan.ProjectID, linearprovider.ProjectPlan{Priority: in.Priority, TargetDate: in.TargetDate, StartDate: in.StartDate})
	// The read-back comes before the settle, so the settle states what
	// the read shows: it is final. Linear's reads can lag its write, so a
	// field not as sent yet is read again, unless Linear refused the
	// write outright.
	refused := definiteRefusal(sendErr)
	after, readErr := planner.ProjectSnapshot(ctx, plan.ProjectID)
	for _, wait := range teamAddRereads {
		if refused || (readErr == nil && planSent(in, after)) {
			break
		}
		if e = teamAddWait(ctx, wait); e != nil {
			break
		}
		after, readErr = planner.ProjectSnapshot(ctx, plan.ProjectID)
	}
	uncertain := "; whether Linear set the plan is unknown: read the project in Linear and tell the owner; nothing is retried"
	switch {
	case readErr == nil && refused && planUnsent(in, before, after):
		out.Outcome = "not_sent"
		out.Note = "Linear refused the plan, and the project reads as before"
		return out, errors.Join(sendErr, settle("not_sent", nil))
	case readErr != nil:
		out.Outcome = "uncertain"
		out.Note = "the project couldn't be read back" + uncertain
		return out, errors.Join(sendErr, readErr, settle("uncertain", nil))
	case !planSent(in, after):
		out.Outcome = "uncertain"
		out.Note = "the read-back doesn't show " + planWords(planMissing(in, after)) + " as sent" + uncertain
		return out, errors.Join(sendErr, settle("uncertain", nil))
	}
	changed, other := planChanges(before, after, fields)
	if len(other) > 0 {
		// A plan field this plan didn't send moved too: not a change the
		// settle can name, so the plan is uncertain, loudly.
		out.Outcome = "uncertain"
		out.Note = "Linear also changed " + planWords(other) + ", which this plan didn't send" + uncertain
		return out, errors.Join(sendErr, settle("uncertain", nil))
	}
	out.Changed = changed
	out.Unverified = linearprovider.TeamAddUnverified(before, after)
	out.Outcome = "accepted"
	if len(out.Changed) > 0 {
		out.Outcome = "accepted_with_changes"
		out.Note = "Linear also changed: " + strings.Join(out.Changed, ", ") + "; ClawBridge sent only the plan's fields, and never undoes Linear's changes"
	}
	if len(out.Unverified) > 0 {
		out.Note = strings.TrimPrefix(out.Note+"; ", "; ") + "Linear's read left out " + strings.Join(out.Unverified, " and ") + ", so the plan couldn't prove them unchanged"
	}
	if e = settle("accepted", out.Changed); e != nil {
		return out, fmt.Errorf("the plan was set (%s) but wasn't settled on the server: %w", out.Outcome, e)
	}
	return out, nil
}

// planDates are the start and target dates a plan leaves the project
// with: the planned value where given, otherwise the value read. ok is
// false for a date the read left out; "" is none.
func planDates(in ProjectPlanInput, before linearprovider.ProjectSnapshot) (start string, startOK bool, target string, targetOK bool) {
	start, startOK = before.StartDate()
	target, targetOK = before.TargetDate()
	if in.StartDate.Present {
		start, startOK = derefDate(in.StartDate.Value), true
	}
	if in.TargetDate.Present {
		target, targetOK = derefDate(in.TargetDate.Value), true
	}
	return
}

// checkPlanDates is §2.2's start ≤ target on the merged dates (C3), and
// §2.4: a date Linear holds at another precision than a day is changed
// there first.
func checkPlanDates(in ProjectPlanInput, before linearprovider.ProjectSnapshot) error {
	if !in.StartDate.Present && !in.TargetDate.Present {
		return nil
	}
	for _, d := range []struct {
		field, name string
		planned     bool
	}{{"targetDate", "target date", in.TargetDate.Present}, {"startDate", "start date", in.StartDate.Present}} {
		if !d.planned {
			continue
		}
		if res, ok := before.Resolution(d.field); ok && !dayResolution(res) {
			return fmt.Errorf("this project's %s is a %s in Linear; change its precision there first; nothing was sent", d.name, resolutionWord(res))
		}
	}
	start, startOK, target, targetOK := planDates(in, before)
	switch {
	case in.TargetDate.Present && target != "" && !startOK:
		return errors.New("the project's read doesn't show its start date, so start ≤ target can't be checked; nothing was sent")
	case in.StartDate.Present && start != "" && !targetOK:
		return errors.New("the project's read doesn't show its target date, so start ≤ target can't be checked; nothing was sent")
	case start != "" && target != "" && start > target:
		return fmt.Errorf("the start date %s would come after the target date %s; nothing was sent", start, target)
	}
	return nil
}

func dayResolution(res string) bool { return res == "" || res == "day" }

func resolutionWord(res string) string {
	if res == "halfYear" {
		return "half-year"
	}
	return res
}

// planSent reports whether every planned field reads as planned.
func planSent(in ProjectPlanInput, s linearprovider.ProjectSnapshot) bool {
	return len(planMissing(in, s)) == 0
}

// planMissing names the planned fields that don't read as planned.
func planMissing(in ProjectPlanInput, s linearprovider.ProjectSnapshot) []string {
	out := []string{}
	if in.Priority != nil {
		if got, ok := s.Priority(); !ok || got != *in.Priority {
			out = append(out, "priority")
		}
	}
	if in.StartDate.Present {
		if got, ok := s.StartDate(); !ok || got != derefDate(in.StartDate.Value) {
			out = append(out, "startDate")
		}
	}
	if in.TargetDate.Present {
		if got, ok := s.TargetDate(); !ok || got != derefDate(in.TargetDate.Value) {
			out = append(out, "targetDate")
		}
	}
	return out
}

// planUnsent reports whether every planned field reads as it did before.
func planUnsent(in ProjectPlanInput, before, after linearprovider.ProjectSnapshot) bool {
	if in.Priority != nil {
		b, bOK := before.Priority()
		a, aOK := after.Priority()
		if !bOK || !aOK || a != b {
			return false
		}
	}
	for _, d := range []struct {
		planned bool
		read    func(linearprovider.ProjectSnapshot) (string, bool)
	}{{in.StartDate.Present, linearprovider.ProjectSnapshot.StartDate}, {in.TargetDate.Present, linearprovider.ProjectSnapshot.TargetDate}} {
		if !d.planned {
			continue
		}
		b, bOK := d.read(before)
		a, aOK := d.read(after)
		if !bOK || !aOK || a != b {
			return false
		}
	}
	return true
}

// planChanges names what else differs between the reads: changed are the
// names the settle takes (FR10's list, and a date's resolution unless it
// stayed a day's for a planned date); other are the plan fields this plan
// didn't send. Names only, never values.
func planChanges(before, after linearprovider.ProjectSnapshot, fields []string) (changed, other []string) {
	changed, other = []string{}, []string{}
	for _, name := range linearprovider.TeamAddChanges(before, after, "") {
		switch name {
		case "priority", "startDate", "targetDate":
			if !slices.Contains(fields, name) {
				other = append(other, name)
			}
		case "startDateResolution", "targetDateResolution":
			field := strings.TrimSuffix(name, "Resolution")
			b, bOK := before.Resolution(field)
			a, aOK := after.Resolution(field)
			if slices.Contains(fields, field) && bOK && aOK && dayResolution(b) && dayResolution(a) {
				continue
			}
			changed = append(changed, name)
		default:
			changed = append(changed, name)
		}
	}
	return changed, other
}

// newProjectPlan is the preview: each planned field current → planned,
// what is sent, and the digest over the project, its teams, the planned
// values and the current values read.
func newProjectPlan(in ProjectPlanInput, before linearprovider.ProjectSnapshot) ProjectPlanPreview {
	plan := ProjectPlanPreview{ProjectID: in.ProjectID, Teams: sortedTeams(before.TeamIDs), Changes: []ProjectPlanChange{}, Sends: append([]string{"id"}, in.fields()...), before: before}
	planned := [][]any{}
	if in.Priority != nil {
		cur := "unknown"
		if n, ok := before.Priority(); ok {
			cur = priorityWord(n)
		}
		plan.Changes = append(plan.Changes, ProjectPlanChange{Field: "priority", Current: cur, Planned: priorityWord(*in.Priority)})
		planned = append(planned, []any{"priority", *in.Priority})
	}
	for _, d := range []struct {
		field string
		f     linearprovider.Field[*string]
		read  func(linearprovider.ProjectSnapshot) (string, bool)
	}{{"startDate", in.StartDate, linearprovider.ProjectSnapshot.StartDate}, {"targetDate", in.TargetDate, linearprovider.ProjectSnapshot.TargetDate}} {
		if !d.f.Present {
			continue
		}
		cur := "unknown"
		if v, ok := d.read(before); ok {
			cur = dateWord(v)
		}
		change := ProjectPlanChange{Field: d.field, Current: cur, Planned: dateWord(derefDate(d.f.Value))}
		if res, ok := before.Resolution(d.field); ok && !dayResolution(res) {
			change.Resolution = res
		}
		plan.Changes = append(plan.Changes, change)
		var v any
		if d.f.Value != nil {
			v = *d.f.Value
		}
		planned = append(planned, []any{d.field, v})
	}
	current := map[string]any{}
	if n, ok := before.Priority(); ok {
		current["priority"] = n
	} else {
		current["priority"] = "unknown"
	}
	for field, read := range map[string]func(linearprovider.ProjectSnapshot) (string, bool){"startDate": linearprovider.ProjectSnapshot.StartDate, "targetDate": linearprovider.ProjectSnapshot.TargetDate} {
		current[field] = "unknown"
		if v, ok := read(before); ok {
			current[field] = v
		}
		current[field+"Resolution"] = "unknown"
		if res, ok := before.Resolution(field); ok {
			current[field+"Resolution"] = res
		}
	}
	raw, _ := json.Marshal([]any{plan.ProjectID, plan.Teams, planned, current})
	sum := sha256.Sum256(raw)
	plan.PlanSHA256 = hex.EncodeToString(sum[:])
	return plan
}

var priorityWords = []string{"none", "urgent", "high", "medium", "low"}

func priorityWord(n int) string {
	if n < 0 || n >= len(priorityWords) {
		return "unknown"
	}
	return fmt.Sprintf("%s (%d)", priorityWords[n], n)
}

func dateWord(d string) string {
	if d == "" {
		return "none"
	}
	return d
}

// planWords names plan fields as a person reads them.
func planWords(fields []string) string {
	words := []string{}
	for _, f := range fields {
		switch f {
		case "targetDate":
			words = append(words, "target date")
		case "startDate":
			words = append(words, "start date")
		default:
			words = append(words, f)
		}
	}
	return strings.Join(words, ", ")
}

// planRefusal is the server's refusal of a caller who may not confirm a
// plan (forbidden, outside_lane): the preview shows it and asks.
func planRefusal(err error) (string, bool) {
	var coded interface{ Code() string }
	if errors.As(err, &coded) && coded.Code() == "forbidden" {
		return err.Error(), true
	}
	var rejected rejection
	if errors.As(err, &rejected) {
		if _, code, message := rejected.Rejection(); code == "forbidden" {
			if message == "" {
				message = "the server refused this caller"
			}
			return message, true
		}
	}
	return "", false
}

// planWho says who confirms a preview the server allows.
func planWho(by, lane, team string, proven bool) string {
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
		who += " This server sets no project plan until its operator sets serve --project-plan-proven (project_plan_unproven); the preview works."
	}
	return who
}
