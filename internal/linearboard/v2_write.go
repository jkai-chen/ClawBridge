package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

var sha256Pattern = regexp.MustCompile(`\A[0-9a-f]{64}\z`)

func legacyOnlyV2(in WriteInputV2) bool {
	return in.TeamID == "" && !in.Project.Present && !in.Labels.Present && !in.ExpectedLabels.Present && !in.Parent.Present && !in.Assignee.Present && in.Priority == nil && in.ExpectedTitle == nil && in.ExpectedDescriptionSHA256 == nil && in.AddLabels == nil && in.RemoveLabels == nil && in.InResponseTo == ""
}

// labelPlanV2 is the part of the requested label deltas that would change the
// issue: adds of labels not proven present, removes of labels proven present.
type labelPlanV2 struct{ Add, Remove []string }

func (p labelPlanV2) empty() bool { return len(p.Add) == 0 && len(p.Remove) == 0 }

// labelDeltaPlanV2 decides each delta from the issue's label membership: the
// known label IDs, or else the server-side list filter by label UUID. An add
// whose membership is unknown is still sent (native adds are append-only); a
// remove is sent only when the label is proven present, and an unknown remove
// is refused because absence cannot be claimed.
func labelDeltaPlanV2(ctx context.Context, p ProviderV2, issue linearprovider.IssueV2, adds, removes []string) (labelPlanV2, error) {
	plan := labelPlanV2{}
	m, membership := p.(labelMembershipV2)
	if !issue.LabelsKnown && membership {
		caps, e := p.Capabilities(ctx)
		if e != nil {
			return plan, e
		}
		membership = caps.LabelMembershipProof
	}
	has := func(label string) (present, known bool, err error) {
		if issue.LabelsKnown {
			return contains(issue.LabelIDs, label), true, nil
		}
		if !membership {
			return false, false, nil
		}
		found, complete, e := m.IssueHasLabel(ctx, issue.ID, issue.TeamID, issue.ProjectID, label)
		return found, found || complete, e
	}
	for _, label := range adds {
		present, known, e := has(label)
		if e != nil {
			return plan, e
		}
		if !known || !present {
			plan.Add = append(plan.Add, label)
		}
	}
	for _, label := range removes {
		present, known, e := has(label)
		if e != nil {
			return plan, e
		}
		if !known {
			return plan, errors.New("label membership is unknown, so its removal is refused; nothing was sent")
		}
		if present {
			plan.Remove = append(plan.Remove, label)
		}
	}
	return plan, nil
}

// writePlanV2 is the label delta plan for a write. A marker's add is sent
// even when the label reads as present: a removal racing it may still land
// (R2-H2), and native adds append.
func writePlanV2(ctx context.Context, p ProviderV2, issue linearprovider.IssueV2, in WriteInputV2) (labelPlanV2, error) {
	if !in.marker {
		return labelDeltaPlanV2(ctx, p, issue, in.AddLabels, in.RemoveLabels)
	}
	plan, e := labelDeltaPlanV2(ctx, p, issue, nil, in.RemoveLabels)
	plan.Add = in.AddLabels
	return plan, e
}

// descriptionSHA256 is the hex SHA-256 of the exact description bytes a read
// returned; callers echo it back as expected_description_sha256.
// writeScopeV2 is the scope a write needs: an agent's comment may go on a
// comment team's issue too; everything else, a marker's comment included,
// needs a write team.
func writeScopeV2(in WriteInputV2) func(linearprovider.IssueV2, bus.TaskboardBindingV2, verifiedV2) error {
	if in.Operation == "comment" && !in.marker {
		return checkCommentIssueV2
	}
	return checkIssueV2
}

func descriptionSHA256(description string) string {
	h := sha256.Sum256([]byte(description))
	return hex.EncodeToString(h[:])
}

// textGuardV2 compares the optional title and description guards with a
// fresh read of the issue.
func textGuardV2(in WriteInputV2, current linearprovider.IssueV2) error {
	if in.ExpectedTitle != nil && current.Title != *in.ExpectedTitle {
		return errors.New("issue title differs from expected_title; inspect the current issue")
	}
	if in.ExpectedDescriptionSHA256 != nil && !current.DescriptionKnown {
		return errors.New("the current description could not be read, so expected_description_sha256 cannot be checked")
	}
	if in.ExpectedDescriptionSHA256 != nil && descriptionSHA256(current.Description) != *in.ExpectedDescriptionSHA256 {
		return errors.New("issue description differs from expected_description_sha256; inspect the current issue")
	}
	return nil
}
func ValidateWriteV2(in WriteInputV2) error {
	if !operationPattern.MatchString(in.OperationID) {
		return errors.New("write requires operation_id")
	}
	if e := mentionGuard(in.Title, in.Description, &in.Body); e != nil {
		return e
	}
	if in.Title != nil && !validText(*in.Title, 512, false) {
		return errors.New("invalid issue title")
	}
	if in.Description != nil && !validText(*in.Description, 65536, true) {
		return errors.New("invalid issue description")
	}
	if in.StateID != nil && !linearprovider.ValidID(*in.StateID) {
		return errors.New("state requires UUID")
	}
	for _, f := range []linearprovider.Field[*string]{in.Project, in.Parent, in.Assignee} {
		if (!f.Present && f.Value != nil) || (f.Present && f.Value != nil && !linearprovider.ValidID(*f.Value)) {
			return errors.New("nullable intent requires explicit presence and canonical UUID or null")
		}
	}
	if in.Labels.Present {
		if _, e := sortedUniqueIDs(in.Labels.Value, 100); e != nil {
			return e
		}
	} else if in.Labels.Value != nil {
		return errors.New("label values require explicit replacement intent")
	}
	if in.ExpectedLabels.Present {
		if in.Operation != "update" || !in.Labels.Present {
			return errors.New("expected_label_ids applies only to an update that replaces labels")
		}
		if _, e := sortedUniqueIDs(in.ExpectedLabels.Value, 100); e != nil {
			return e
		}
	} else if in.ExpectedLabels.Value != nil {
		return errors.New("expected label values require explicit presence")
	} else if in.Operation == "update" && in.Labels.Present {
		return errors.New("label replacement requires expected_label_ids: the complete label set you last read")
	}
	if in.AddLabels != nil || in.RemoveLabels != nil {
		if in.Operation != "update" || in.Labels.Present || in.ExpectedLabels.Present {
			return errors.New("add_label_ids and remove_label_ids apply only to an update that does not replace labels")
		}
		seen := map[string]bool{}
		for _, ids := range [][]string{in.AddLabels, in.RemoveLabels} {
			if ids == nil {
				continue
			}
			sorted, e := sortedUniqueIDs(ids, 20)
			if e != nil || len(sorted) == 0 {
				return errors.New("label deltas name 1–20 unique label UUIDs")
			}
			for _, id := range sorted {
				if seen[id] {
					return errors.New("a label cannot be both added and removed")
				}
				seen[id] = true
			}
		}
	}
	if in.ExpectedTitle != nil && (in.Operation != "update" || in.Title == nil || len(*in.ExpectedTitle) > 1024) {
		return errors.New("expected_title applies only to an update that replaces the title, up to 1024 bytes")
	}
	if in.ExpectedDescriptionSHA256 != nil && (in.Operation != "update" || in.Description == nil || !sha256Pattern.MatchString(*in.ExpectedDescriptionSHA256)) {
		return errors.New("expected_description_sha256 applies only to an update that replaces the description, as 64 lowercase hex characters")
	}
	if in.TeamID != "" && !linearprovider.ValidID(in.TeamID) {
		return errors.New("team requires UUID")
	}
	if in.InResponseTo != "" && (in.Operation != "comment" || !in.Parent.Present || !linearprovider.ValidID(in.InResponseTo)) {
		return errors.New("in_response_to applies only to a reply (parent_id), as the UUID of the notified comment it answers")
	}
	switch in.Operation {
	case "create":
		if in.Title == nil || in.IssueID != "" || in.Body != "" {
			return errors.New("create requires title and no issue ID")
		}
	case "update":
		if in.TeamID != "" || in.Body != "" || !linearprovider.ValidID(in.IssueID) || (in.Title == nil && in.Description == nil && in.StateID == nil && legacyOnlyV2(in)) {
			return errors.New("update requires issue UUID and explicit changed fields; team moves are unsupported")
		}
	case "comment":
		// On a comment, Parent is the comment being answered (reply_to).
		reply := in.Parent
		in.Parent, in.InResponseTo = linearprovider.Field[*string]{}, ""
		if !legacyOnlyV2(in) || !linearprovider.ValidID(in.IssueID) || !validText(in.Body, linearprovider.MaxBodyBytes-linearprovider.CommentMarkerReserve, false) || in.Title != nil || in.Description != nil || in.StateID != nil {
			return errors.New("comment requires scoped issue UUID and bounded body")
		}
		if reply.Present && reply.Value == nil {
			return errors.New("a reply requires the parent comment UUID")
		}
	default:
		return errors.New("unsupported task-board V2 mutation")
	}
	return nil
}

// legacyMappingV2: the binding can be served by the legacy path, which
// knows no access levels, so a version 3 binding never is.
func legacyMappingV2(b bus.TaskboardBindingV2) bool {
	if b.CrossTeam() {
		return false
	}
	return b.Mode == "team_board" || (b.Mode == "project_set" && len(b.LinearProjectIDs) == 1)
}
func sameLegacyMappingV2(old bus.TaskboardBinding, b bus.TaskboardBindingV2) bool {
	project := ""
	if len(b.LinearProjectIDs) == 1 {
		project = b.LinearProjectIDs[0]
	}
	return old.Configured == b.Configured && old.WorkspaceID == b.WorkspaceID && old.ProjectID == b.ProjectID && old.Revision == b.Revision && old.UpdatedAt == b.UpdatedAt && old.LinearProjectID == project && old.LinearDefaultTeamID == b.LinearDefaultTeamID && old.LinearWorkspaceID == b.LinearWorkspaceID && digest(SortedTeams(old.LinearTeamIDs)) == digest(SortedTeams(b.LinearTeamIDs))
}
func legacyPayloadV2(profile string, b bus.TaskboardBinding, actor, authority string, scope bus.Request, in WriteInput) any {
	// This exact field/type/order shape is the historical L1 digest. It MUST NOT
	// gain a version field or any of the optional L2 intent fields.
	return struct {
		Profile      string
		Binding      bus.TaskboardBinding
		Actor        string
		Authority    string
		InstanceHash string
		Input        WriteInput
	}{profile, b, actor, authority, digest(scope.Args["instance"]), in}
}
func (s *Service) postReceiptV2(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2, receipt any) (any, error) {
	if e := s.unchangedV2(ctx, scope, b); e != nil {
		var changed *ReceiptError
		if errors.As(e, &changed) {
			return receipt, changed
		}
		return receipt, &ReceiptError{Outcome: "accepted_context_unverified"}
	}
	return receipt, nil
}
func (s *Service) writeAccessV2(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2) (bus.TaskboardAccess, error) {
	return s.checkWriteAccess(ctx, scope, bus.TaskboardBinding{WorkspaceID: b.WorkspaceID, ProjectID: b.ProjectID})
}
func (s *Service) postFailureV2(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2, providerError error) error {
	var changed *ReceiptError
	if e := s.unchangedV2(ctx, scope, b); errors.As(e, &changed) {
		return errors.Join(providerError, errBindingChangedAfterProviderError)
	}
	return providerError
}

func (s *Service) WriteV2(ctx context.Context, scope bus.Request, in WriteInputV2) (any, error) {
	if e := ValidateWriteV2(in); e != nil {
		return nil, e
	}
	if !in.marker {
		if app, e := s.appWrites(ctx); e != nil || app {
			if e != nil {
				return nil, e
			}
			return s.appWrite(ctx, scope, in.Operation, writeArgsV2(in))
		}
		// Through the person's own profile: the project's hourly budget
		// counts it (FR8).
		var out any
		err := s.budgeted(ctx, scope, in.Operation, in.OperationID, func(ctx context.Context) error {
			var e error
			out, e = s.writeV2Personal(ctx, scope, in)
			return e
		})
		return out, err
	}
	return s.writeV2Personal(ctx, scope, in)
}

// writeV2Personal is WriteV2 through the person's own Linear profile.
func (s *Service) writeV2Personal(ctx context.Context, scope bus.Request, in WriteInputV2) (any, error) {
	if in.Labels.Present {
		in.Labels.Value, _ = sortedUniqueIDs(in.Labels.Value, 100)
	}
	if in.ExpectedLabels.Present {
		in.ExpectedLabels.Value, _ = sortedUniqueIDs(in.ExpectedLabels.Value, 100)
	}
	if in.AddLabels != nil {
		in.AddLabels, _ = sortedUniqueIDs(in.AddLabels, 20)
	}
	if in.RemoveLabels != nil {
		in.RemoveLabels, _ = sortedUniqueIDs(in.RemoveLabels, 20)
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if !b.Configured {
		return nil, errors.New("task-board is not linked")
	}
	if s.cfg.Authority == "" {
		return nil, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return nil, e
	}
	// A create without a state lands in the status policy's create state
	// for its team, on every route (SF-L4).
	if in.Operation == "create" && in.StateID == nil {
		team := b.LinearDefaultTeamID
		if in.TeamID != "" {
			team = in.TeamID
		}
		state, e := s.createState(ctx, scope, team)
		if e != nil {
			return nil, e
		}
		if state != "" {
			in.StateID = &state
		}
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	legacy := legacyOnlyV2(in) && legacyMappingV2(b)
	var payload any
	if legacy {
		// V2 was required first. This authorized V1 metadata read recovers the
		// original team ordering for byte-exact historical digest compatibility;
		// it is never a fallback from an unsupported V2 server or mirror.
		old, e := s.binding(ctx, scope)
		if e != nil {
			return nil, e
		}
		if !sameLegacyMappingV2(old, b) {
			return nil, &ReceiptError{Outcome: "binding_changed"}
		}
		payload = legacyPayloadV2(s.cfg.ProfileID, old, access.SessionID, s.cfg.Authority, scope, in.WriteInput)
	} else {
		payload = struct {
			Version                        int
			Profile                        string
			Binding                        bus.TaskboardBindingV2
			Actor, Authority, InstanceHash string
			Input                          WriteInputV2
		}{2, s.cfg.ProfileID, b, access.SessionID, s.cfg.Authority, digest(scope.Args["instance"]), in}
	}
	l := s.operations(ctx)
	if receipt, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		if e != nil {
			return receipt, e
		}
		return s.postReceiptV2(ctx, scope, b, receipt)
	}
	if in.Operation == "comment" && !in.marker {
		s.sweepComments(ctx, scope, l, namespace)
		if e = s.precheckComment(ctx, scope, in.IssueID, in.OperationID, in.InResponseTo); e != nil {
			return nil, e
		}
	}
	profile, p, done, e := s.openForWrite(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	if profile.Access != linearconn.Write {
		return nil, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	v, e := s.proveBinding(ctx, p, b)
	if e != nil {
		return nil, e
	}
	var p2 ProviderV2
	var caps linearprovider.Capabilities
	if !legacy {
		var ok bool
		p2, ok = p.(ProviderV2)
		if !ok {
			return nil, errors.New("native provider lacks V2 mutation capability")
		}
		caps, e = p2.Capabilities(ctx)
		if e != nil {
			return nil, e
		}
	}
	team := b.LinearDefaultTeamID
	project := ""
	var current linearprovider.IssueV2
	if in.Operation != "create" {
		if legacy {
			issue, e := p.GetIssue(ctx, in.IssueID)
			if e != nil {
				return nil, e
			}
			current = linearprovider.IssueV2{Issue: issue, ProjectKnown: true}
		} else {
			current, e = s.readIssue(ctx, p2, in.IssueID)
			if e != nil {
				return nil, e
			}
		}
		if current.ID != in.IssueID {
			return nil, errors.New("issue identity changed")
		}
		if e = writeScopeV2(in)(current, b, v); e != nil {
			return nil, e
		}
		if e = textGuardV2(in, current); e != nil {
			return nil, e
		}
		if !legacy && in.Labels.Present {
			if current, e = resolveLabelsV2(ctx, p2, current); e != nil {
				return nil, e
			}
		}
		team = current.TeamID
		project = current.ProjectID
	} else {
		if in.TeamID != "" {
			team = in.TeamID
		}
		if b.Mode == "project_set" {
			if in.Project.Present && in.Project.Value != nil {
				project = *in.Project.Value
			} else if !in.Project.Present && len(b.LinearProjectIDs) == 1 {
				project = b.LinearProjectIDs[0]
			} else {
				return nil, errors.New("creation requires an explicit selected Linear project")
			}
		}
	}
	if in.Project.Present {
		if in.Project.Value == nil {
			if b.Mode != "team_board" {
				return nil, errors.New("project clear is permitted only on a team board")
			}
			project = ""
		} else {
			if b.Mode != "project_set" {
				return nil, errors.New("non-null destination requires a project-set mapping")
			}
			project = *in.Project.Value
		}
		if in.Operation == "update" && !caps.UpdateProject {
			return nil, errors.New("provider cannot express project update intent")
		}
		if in.Operation == "create" && in.Project.Value != nil && !caps.CreateProject {
			return nil, errors.New("provider cannot express project destination")
		}
	}
	if !legacy && in.Operation == "create" && project != "" && !caps.CreateProject {
		return nil, errors.New("provider cannot express the verified create destination")
	}
	effective := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: in.IssueID, TeamID: team, ProjectID: project}, ProjectKnown: true}
	if in.Operation == "create" {
		effective.ID = "00000000-0000-0000-0000-000000000000"
	}
	if e = writeScopeV2(in)(effective, b, v); e != nil {
		return nil, e
	}
	// A session writes only its lane's teams; a marker is ClawBridge's own
	// bookkeeping on a claim, not the session's write.
	if !in.marker {
		if e = checkLaneV2(b, access, team); e != nil {
			return nil, e
		}
	}
	if in.Labels.Present {
		if !caps.Labels || !caps.ReplaceLabels {
			return nil, errors.New("provider cannot safely replace issue labels")
		}
		if in.Operation == "update" && !current.LabelsKnown {
			return nil, errors.New("existing canonical label context is unavailable; replacement refused")
		}
		if in.Operation == "update" {
			existing := current.LabelIDs
			if existing == nil {
				existing = []string{}
			}
			if existing, e = sortedUniqueIDs(existing, 100); e != nil {
				return nil, errors.New("existing canonical label context is invalid")
			}
			if digest(existing) != digest(in.ExpectedLabels.Value) {
				return nil, errors.New("issue labels differ from expected_label_ids; inspect current labels")
			}
		}
		found, e := findTeamLabelsV2(ctx, p2, labelTeamsV2(v, b, team), in.Labels.Value)
		if e != nil {
			return nil, e
		}
		if len(found) != len(in.Labels.Value) {
			return nil, errors.New("selected label is outside target team")
		}
	}
	var plan labelPlanV2
	deltas := in.AddLabels != nil || in.RemoveLabels != nil
	if deltas {
		if !caps.LabelDeltas {
			return nil, errors.New("provider cannot add or remove single labels")
		}
		// A marker's label was found by name in this team's eligible
		// labels (markerLabel), so its eligibility is already proven.
		if in.AddLabels != nil && !in.marker {
			if !caps.Labels {
				return nil, errors.New("provider cannot prove label eligibility")
			}
			found, e := findTeamLabelsV2(ctx, p2, labelTeamsV2(v, b, team), in.AddLabels)
			if e != nil {
				return nil, e
			}
			if len(found) != len(in.AddLabels) {
				return nil, errors.New("selected label is outside target team")
			}
		}
		if plan, e = writePlanV2(ctx, s.planProvider(p2), current, in); e != nil {
			return nil, e
		}
	}
	// A request whose only intent is label deltas that are already in effect
	// is recorded as observed_unchanged and sends nothing.
	unchanged := deltas && plan.empty() && in.Title == nil && in.Description == nil && in.StateID == nil && !in.Project.Present && !in.Parent.Present && !in.Assignee.Present && in.Priority == nil
	if in.Priority != nil && !caps.Priority {
		return nil, errors.New("provider cannot set an issue's priority; nothing was sent")
	}
	if in.Assignee.Present {
		if !caps.Assignee {
			return nil, errors.New("provider cannot express assignee set/clear")
		}
		if in.Assignee.Value != nil {
			if !caps.Members {
				return nil, errors.New("provider cannot prove team membership")
			}
			if e = findMemberV2(ctx, p2, team, *in.Assignee.Value); e != nil {
				return nil, e
			}
		}
	}
	var parentIssue linearprovider.IssueV2
	reply := in.Operation == "comment" && in.Parent.Present
	// replyTo is the comment the reply is sent to: the requested parent, or
	// the root of its thread when the parent is itself a reply.
	replyTo := ""
	if reply {
		// The parent must be a comment on this issue, proven by a bounded
		// read before anything is sent.
		replier, ok := p2.(commentReplierV2)
		if !ok || !caps.CommentReply {
			return nil, errors.New("provider cannot express a threaded reply; nothing was sent")
		}
		found, repliesTo, e := replier.IssueHasComment(ctx, in.IssueID, provenIdentifierV2(current), *in.Parent.Value)
		if e != nil {
			return nil, e
		}
		if !found {
			return nil, errors.New("the reply's parent is not a comment on this issue (searched its 200 most recently updated comments); nothing was sent")
		}
		// Linear threads are one level deep and Linear re-parents a reply to
		// a reply onto the thread's root (live probe, 2026-10-23), so the
		// reply is sent to that root and verified against it.
		replyTo = *in.Parent.Value
		if repliesTo == "?" {
			return nil, errors.New("the reply's parent names a thread root that is not a UUID; nothing was sent")
		}
		if repliesTo != "" {
			replyTo = repliesTo
		}
	}
	if in.Parent.Present && !reply {
		if !caps.Parent || !caps.ParentSameDestination {
			return nil, errors.New("provider cannot express a verified same-destination parent")
		}
		if in.Operation == "update" && !in.Project.Present && !current.ProjectKnown {
			return nil, errors.New("current project is unavailable; parent mutation refused")
		}
		if in.Parent.Value == nil && !parentClearAllowed {
			return nil, errors.New("parent clear is not enabled")
		}
		if in.Parent.Value != nil {
			if parentIssue, e = verifyParentV2(ctx, p2, b, v, in.IssueID, *in.Parent.Value); e != nil {
				return nil, e
			}
			if e = sameDestinationParentV2(parentIssue, team, project); e != nil {
				return nil, e
			}
		}
	}
	verifiedParent := in.Parent.Present && in.Parent.Value != nil
	var states []linearprovider.Status
	if in.StateID != nil {
		if states, e = p.Statuses(ctx, team); e != nil {
			return nil, e
		}
		found := false
		for _, state := range states {
			if state.ID == *in.StateID && state.TeamID == team {
				found = true
			}
		}
		if !found {
			return nil, errors.New("state is not verified for target team")
		}
	}
	// Who closes (A2): an agent's write, a session's; a marker's moves are
	// the status policy's own (stateMove).
	if !in.marker && access.SessionID != "" {
		if e = s.closeGuard(ctx, scope, in, states, team, current, project); e != nil {
			return nil, e
		}
	}
	if !legacy {
		if validation, ok := p2.(interface {
			ValidateCreateV2(linearprovider.CreateInputV2) error
			ValidateUpdateV2(linearprovider.UpdateInputV2) error
		}); ok {
			if in.Operation == "create" {
				description, state := "", ""
				if in.Description != nil {
					description = *in.Description
				}
				if in.StateID != nil {
					state = *in.StateID
				}
				create := linearprovider.CreateInputV2{CreateInput: linearprovider.CreateInput{TeamID: team, ProjectID: project, Title: *in.Title, Description: description, StateID: state}, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee, Priority: in.Priority}
				if verifiedParent {
					create = create.WithSameDestinationParent()
				}
				e = validation.ValidateCreateV2(create)
			} else if in.Operation == "update" && !unchanged {
				update := linearprovider.UpdateInputV2{UpdateInput: linearprovider.UpdateInput{ID: in.IssueID, Title: in.Title, Description: in.Description, StateID: in.StateID}, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee, Priority: in.Priority, AddLabels: nonEmptyV2(plan.Add), RemoveLabels: nonEmptyV2(plan.Remove)}
				if verifiedParent {
					update = update.WithSameDestinationParent()
				}
				e = validation.ValidateUpdateV2(update)
			}
			if e != nil {
				return nil, e
			}
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return nil, e
	}
	return l.write(namespace, in.OperationID, ledgerMeta{in.Operation, b.ProjectID}, payload, func() (any, error) {
		if e := s.unchangedV2(ctx, scope, b); e != nil {
			return nil, authorityFailure(e)
		}
		fresh, e := s.writeAccessV2(ctx, scope, b)
		if e != nil {
			return nil, authorityFailure(e)
		}
		if fresh.SessionID != access.SessionID {
			return nil, refusedBeforeDispatch(errActorChanged)
		}
		if !sameLaneV2(b, access, fresh) {
			return nil, refusedBeforeDispatch(errLaneChanged)
		}
		if e = s.refreshWriteProofV2(ctx, p, p2, b, v, in, team, project, current, legacy, plan); e != nil {
			return nil, proofFailure(e)
		}
		if e = s.unchangedV2(ctx, scope, b); e != nil {
			return nil, authorityFailure(e)
		}
		fresh, e = s.writeAccessV2(ctx, scope, b)
		if e != nil {
			return nil, authorityFailure(e)
		}
		if fresh.SessionID != access.SessionID {
			return nil, refusedBeforeDispatch(errActorChanged)
		}
		if !sameLaneV2(b, access, fresh) {
			return nil, refusedBeforeDispatch(errLaneChanged)
		}
		if e = ctx.Err(); e != nil {
			return nil, notDispatched(e)
		}
		if unchanged {
			return s.postReceiptV2(ctx, scope, b, ReceiptV2{Kind: "issue", Outcome: "observed_unchanged", ID: in.IssueID, Identifier: current.Identifier, TeamID: team, ProjectID: projectPointerV2(project)})
		}
		post := func(e error) error { return s.postFailureV2(ctx, scope, b, e) }
		sent, known := sentCount(p)
		var receipt any
		var unverified []string
		switch in.Operation {
		case "create":
			description, state := "", ""
			if in.Description != nil {
				description = *in.Description
			}
			if in.StateID != nil {
				state = *in.StateID
			}
			base := linearprovider.CreateInput{TeamID: team, ProjectID: project, Title: *in.Title, Description: description, StateID: state}
			var issue linearprovider.IssueV2
			if legacy {
				i, err := p.Create(ctx, base)
				e = err
				issue.Issue = i
				issue.ProjectKnown = true
			} else {
				create := linearprovider.CreateInputV2{CreateInput: base, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee, Priority: in.Priority}
				if verifiedParent {
					create = create.WithSameDestinationParent()
				}
				issue, e = p2.CreateV2(ctx, create)
			}
			if e != nil {
				return nil, writeFailure(p, sent, known, e, post)
			}
			if checkIssueV2(issue, b, v) != nil || issue.TeamID != team || issue.ProjectID != project {
				return nil, errors.New("created issue receipt scope mismatch")
			}
			if !legacy && receiptParentV2(issue, in.Parent, parentIssue) != nil {
				return nil, errors.New("created issue receipt parent mismatch")
			}
			issue = s.withStateID(ctx, scope, issue, in, legacy)
			if unverified, e = receiptFieldsV2(issue, in, legacy); e != nil {
				return nil, post(e)
			}
			if legacy {
				receipt = map[string]any{"id": issue.ID, "identifier": issue.Identifier, "team_id": issue.TeamID, "project_id": issue.ProjectID}
			} else {
				receipt = ReceiptV2{Kind: "issue", Outcome: "accepted", ID: issue.ID, Identifier: issue.Identifier, TeamID: issue.TeamID, ProjectID: projectPointerV2(issue.ProjectID), Unverified: unverified}
			}
		case "update":
			base := linearprovider.UpdateInput{ID: in.IssueID, Title: in.Title, Description: in.Description, StateID: in.StateID}
			var issue linearprovider.IssueV2
			if legacy {
				i, err := p.Update(ctx, base)
				e = err
				issue.Issue = i
				issue.ProjectKnown = true
			} else {
				update := linearprovider.UpdateInputV2{UpdateInput: base, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee, Priority: in.Priority, AddLabels: nonEmptyV2(plan.Add), RemoveLabels: nonEmptyV2(plan.Remove)}
				if verifiedParent {
					update = update.WithSameDestinationParent()
				}
				issue, e = p2.UpdateV2(ctx, update)
			}
			s.wrote(in.IssueID)
			if e != nil {
				return nil, writeFailure(p, sent, known, e, post)
			}
			if issue.ID != in.IssueID || checkIssueV2(issue, b, v) != nil || issue.TeamID != team || ((in.Project.Present || legacy || current.ProjectKnown) && issue.ProjectID != project) {
				return nil, errors.New("updated issue receipt scope mismatch")
			}
			if !legacy && receiptParentV2(issue, in.Parent, parentIssue) != nil {
				return nil, errors.New("updated issue receipt parent mismatch")
			}
			issue = s.withStateID(ctx, scope, issue, in, legacy)
			if unverified, e = receiptFieldsV2(issue, in, legacy); e != nil {
				return nil, post(e)
			}
			// Native receipts name labels only; Linear's list filtered by
			// each delta label's UUID then shows whether it took effect. A
			// sent removal answered with an empty label list is checked so
			// too: native reads have reported an empty list as known when it
			// was not, so it does not prove the label gone.
			emptyAfterRemove := len(plan.Remove) > 0 && issue.LabelsKnown && len(issue.LabelIDs) == 0
			if (in.AddLabels != nil || in.RemoveLabels != nil) && (contains(unverified, "labels") || emptyAfterRemove) {
				confirmed, e := deltaReadBackV2(ctx, p2, in, issue.ID, team, project)
				if e != nil {
					return nil, post(e)
				}
				switch {
				case confirmed:
					unverified = slices.DeleteFunc(unverified, func(f string) bool { return f == "labels" })
					if len(unverified) == 0 {
						unverified = nil
					}
				case !contains(unverified, "labels"):
					unverified = append(unverified, "labels")
				}
			}
			if legacy {
				receipt = map[string]any{"id": issue.ID, "identifier": issue.Identifier, "team_id": issue.TeamID, "project_id": issue.ProjectID}
			} else {
				receipt = ReceiptV2{Kind: "issue", Outcome: "accepted", ID: issue.ID, Identifier: issue.Identifier, TeamID: issue.TeamID, ProjectID: projectPointerV2(issue.ProjectID), Unverified: unverified, LabelsAdded: plan.Add, LabelsRemoved: plan.Remove}
			}
		default:
			// The proven identifier lets the readback accept a row that names
			// the issue by it.
			// Every posted comment ends with the agent marker; the ledger
			// keeps the caller's body, so a retry matches as before.
			posted := linearprovider.MarkComment(in.Body, markerSession(access), access.Engineer)
			if in.marker {
				posted = in.Body
			}
			send := func() (linearprovider.Receipt, error) {
				var r linearprovider.Receipt
				var err error
				if reply {
					r, err = p2.(commentReplierV2).ReplyOnIssue(ctx, in.IssueID, provenIdentifierV2(current), replyTo, posted)
				} else {
					r, err = commentV2(ctx, p, in.IssueID, current.Identifier, posted)
				}
				if err != nil {
					return r, writeFailure(p, sent, known, err, post)
				}
				if r.IssueID != in.IssueID || !linearprovider.ValidID(r.ID) {
					return r, errors.New("comment receipt scope mismatch")
				}
				if reply && r.ParentID != replyTo {
					return r, errors.New("comment receipt parent mismatch")
				}
				return r, nil
			}
			var r linearprovider.Receipt
			var answers string
			var err error
			if in.marker {
				r, err = send()
			} else {
				intent := commentIntent{Issue: in.IssueID, OperationID: in.OperationID, Body: posted, Parent: replyTo, InResponseTo: in.InResponseTo}
				r, answers, err = s.sendComment(ctx, scope, l, namespace, intent, send)
			}
			if err != nil {
				return nil, err
			}
			if legacy {
				receipt = r
			} else {
				written := ReceiptV2{Kind: "comment", Outcome: "accepted", ID: r.ID, IssueID: r.IssueID, ParentID: r.ParentID, BodySHA256: r.BodySHA256, InResponseTo: answers}
				if reply && replyTo != *in.Parent.Value {
					written.RequestedParentID = *in.Parent.Value
				}
				receipt = written
			}
		}
		return s.postReceiptV2(ctx, scope, b, receipt)
	})
}
func nonEmptyV2(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return ids
}
func projectPointerV2(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parentClearAllowed enables explicit parent removal. An owner-approved live
// probe showed that clearing a parent preserves the child's team and project.
const parentClearAllowed = true

// verifyParentV2 walks the requested parent's ancestry and returns the
// immediate parent. Native Linear reports parentId as a display identifier,
// so each ancestor is read by the reference its child exposes and matched on
// UUID or identifier; cycles are detected on both.
func verifyParentV2(ctx context.Context, p ProviderV2, b bus.TaskboardBindingV2, v verifiedV2, child, parent string) (linearprovider.IssueV2, error) {
	var first linearprovider.IssueV2
	seen := map[string]bool{}
	if child != "" {
		seen[child] = true
	}
	for depth := 0; depth < 64; depth++ {
		if seen[parent] {
			return first, errors.New("parent hierarchy contains a self-reference or cycle")
		}
		issue, e := p.IssueV2(ctx, parent)
		if e != nil {
			return first, e
		}
		if issue.ID != parent && (issue.Identifier == "" || issue.Identifier != parent) {
			return first, errors.New("parent identity changed")
		}
		if seen[issue.ID] || (issue.Identifier != "" && seen[issue.Identifier]) {
			return first, errors.New("parent hierarchy contains a self-reference or cycle")
		}
		seen[parent], seen[issue.ID] = true, true
		if issue.Identifier != "" {
			seen[issue.Identifier] = true
		}
		if depth == 0 {
			first = issue
		}
		if e = checkIssueV2(issue, b, v); e != nil {
			return first, e
		}
		if !issue.ParentKnown {
			return first, errors.New("required parent ancestry is incomplete")
		}
		next := issue.ParentID
		if next == "" {
			next = issue.ParentIdentifier
		}
		if next == "" {
			return first, nil
		}
		if !linearprovider.ValidID(next) && !linearprovider.ValidIssueIdentifier(next) {
			return first, errors.New("invalid parent ancestry")
		}
		parent = next
	}
	return first, errors.New("parent ancestry exceeds 64 ancestors")
}

// sameDestinationParentV2 is the preservation proof: whatever team or project
// a parent passes on, it already equals the child's effective destination.
func sameDestinationParentV2(parent linearprovider.IssueV2, team, project string) error {
	if parent.TeamID != team || !parent.ProjectKnown || parent.ProjectID != project {
		return errors.New("parent must share the child's team and project; cross-team and cross-project parents are not supported")
	}
	return nil
}

// receiptParentV2 checks a mutation receipt's parent against the request when
// the receipt reports it; native Linear reports the parent's identifier.
// receiptFieldsV2 compares the labels, assignee, parent and state a create or
// update set with the issue Linear returned. A field the receipt reports and
// that differs makes the write uncertain; a field it does not report is named
// as unverified rather than assumed. The parent's value is checked by
// receiptParentV2; here it is only named when the receipt cannot show it.
// withStateID gives a receipt that names its state without the ID (as
// Linear's MCP answer does) the ID issueStateID resolves, so a requested
// state is checked rather than left unverified. A state that cannot be
// resolved stays unverified; a resolved one that differs is a mismatch.
func (s *Service) withStateID(ctx context.Context, scope bus.Request, issue linearprovider.IssueV2, in WriteInputV2, legacy bool) linearprovider.IssueV2 {
	if legacy || in.StateID == nil || issue.StateID != "" {
		return issue
	}
	if id, e := s.issueStateID(ctx, scope, issue); e == nil {
		issue.StateID = id
	}
	return issue
}

func receiptFieldsV2(issue linearprovider.IssueV2, in WriteInputV2, legacy bool) ([]string, error) {
	if legacy {
		return nil, nil
	}
	mismatch := &linearprovider.Error{Kind: "receipt_mismatch"}
	var unverified []string
	if in.Labels.Present {
		if !issue.LabelsKnown {
			unverified = append(unverified, "labels")
		} else {
			got := issue.LabelIDs
			if got == nil {
				got = []string{}
			}
			got, e := sortedUniqueIDs(got, 100)
			if e != nil || digest(got) != digest(in.Labels.Value) {
				return nil, mismatch
			}
		}
	}
	if in.AddLabels != nil || in.RemoveLabels != nil {
		if !issue.LabelsKnown {
			unverified = append(unverified, "labels")
		} else {
			for _, id := range in.AddLabels {
				if !contains(issue.LabelIDs, id) {
					return nil, mismatch
				}
			}
			for _, id := range in.RemoveLabels {
				if contains(issue.LabelIDs, id) {
					return nil, mismatch
				}
			}
		}
	}
	if in.Assignee.Present {
		want := ""
		if in.Assignee.Value != nil {
			want = *in.Assignee.Value
		}
		switch {
		case !issue.AssigneeKnown:
			unverified = append(unverified, "assignee")
		case issue.AssigneeID != want:
			return nil, mismatch
		}
	}
	if in.Priority != nil {
		switch {
		case !issue.PriorityKnown || issue.Priority == nil:
			unverified = append(unverified, "priority")
		case *issue.Priority != *in.Priority:
			return nil, mismatch
		}
	}
	if in.Parent.Present && !issue.ParentKnown {
		unverified = append(unverified, "parent")
	}
	if in.StateID != nil {
		switch {
		case issue.StateID == "":
			unverified = append(unverified, "state")
		case issue.StateID != *in.StateID:
			return nil, mismatch
		}
	}
	return unverified, nil
}

// deltaReadBackV2 asks Linear's label-filtered issue list, for each requested
// delta label, whether the issue now carries it: an added label must be found
// and a removed one proven absent. The list lags a write by some seconds (seen
// on an issue labelled 14 s after its create), so a contradiction is asked
// again after each of deltaRecheckDelays; one that holds through them is a
// receipt_mismatch error. A check that cannot run or reach an answer leaves
// the labels unconfirmed (false) rather than assumed.
func deltaReadBackV2(ctx context.Context, p ProviderV2, in WriteInputV2, issue, team, project string) (bool, error) {
	m, ok := p.(labelMembershipV2)
	if !ok {
		return false, nil
	}
	if caps, e := p.Capabilities(ctx); e != nil || !caps.LabelMembershipProof {
		return false, nil
	}
	confirmed := true
	for want, labels := range map[bool][]string{true: in.AddLabels, false: in.RemoveLabels} {
		for _, label := range labels {
			found, complete, e := m.IssueHasLabel(ctx, issue, team, project, label)
			for _, wait := range deltaRecheckDelays {
				if e != nil || !complete || found == want {
					break
				}
				select {
				case <-ctx.Done():
					return false, nil
				case <-time.After(wait):
				}
				found, complete, e = m.IssueHasLabel(ctx, issue, team, project, label)
			}
			switch {
			case e != nil || (!found && !complete):
				confirmed = false
			case found != want:
				return false, &linearprovider.Error{Kind: "receipt_mismatch"}
			}
		}
	}
	return confirmed, nil
}

// deltaRecheckDelays are the waits before a contradicting label check is
// asked again; tests shorten them.
var deltaRecheckDelays = []time.Duration{2 * time.Second, 4 * time.Second}

func receiptParentV2(issue linearprovider.IssueV2, parent linearprovider.Field[*string], verified linearprovider.IssueV2) error {
	if !parent.Present || !issue.ParentKnown {
		return nil
	}
	if parent.Value == nil {
		if issue.ParentID != "" || issue.ParentIdentifier != "" {
			return errors.New("receipt parent was not cleared")
		}
		return nil
	}
	if issue.ParentID == *parent.Value || (issue.ParentID == "" && issue.ParentIdentifier != "" && issue.ParentIdentifier == verified.Identifier) {
		return nil
	}
	return errors.New("receipt parent does not match the requested parent")
}

// markerSession is the session name the agent marker shows: the name the
// bus resolved for the writing session, however the caller addressed it.
// Session IDs never go to Linear.
func markerSession(access bus.TaskboardAccess) string {
	return access.SessionName
}

// commentReplierV2 is a provider that can prove a reply's parent and post
// the reply in its thread.
type commentReplierV2 interface {
	IssueHasComment(ctx context.Context, issue, identifier, comment string) (found bool, repliesTo string, err error)
	ReplyOnIssue(ctx context.Context, issue, identifier, parent, body string) (linearprovider.Receipt, error)
}

// provenIdentifierV2 is the display identifier the issue read proved, or
// empty when it is not a valid identifier.
func provenIdentifierV2(issue linearprovider.IssueV2) string {
	if linearprovider.ValidIssueIdentifier(issue.Identifier) {
		return issue.Identifier
	}
	return ""
}

// commentV2 comments through CommentOnIssue when the provider has it and the
// read proved a display identifier, and through Comment otherwise.
func commentV2(ctx context.Context, p Provider, issue, identifier, body string) (linearprovider.Receipt, error) {
	if c, ok := p.(interface {
		CommentOnIssue(context.Context, string, string, string) (linearprovider.Receipt, error)
	}); ok && linearprovider.ValidIssueIdentifier(identifier) {
		return c.CommentOnIssue(ctx, issue, identifier, body)
	}
	return p.Comment(ctx, issue, body)
}
