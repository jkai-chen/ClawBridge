package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

func legacyOnlyV2(in WriteInputV2) bool {
	return in.TeamID == "" && !in.Project.Present && !in.Labels.Present && !in.ExpectedLabels.Present && !in.Parent.Present && !in.Assignee.Present
}
func ValidateWriteV2(in WriteInputV2) error {
	if !operationPattern.MatchString(in.OperationID) {
		return errors.New("write requires operation_id")
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
	if in.TeamID != "" && !linearprovider.ValidID(in.TeamID) {
		return errors.New("team requires UUID")
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
		if !legacyOnlyV2(in) || !linearprovider.ValidID(in.IssueID) || !validText(in.Body, 65536, false) || in.Title != nil || in.Description != nil || in.StateID != nil {
			return errors.New("comment requires scoped issue UUID and bounded body")
		}
	default:
		return errors.New("unsupported task-board V2 mutation")
	}
	return nil
}
func legacyMappingV2(b bus.TaskboardBindingV2) bool {
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
	if in.Labels.Present {
		in.Labels.Value, _ = sortedUniqueIDs(in.Labels.Value, 100)
	}
	if in.ExpectedLabels.Present {
		in.ExpectedLabels.Value, _ = sortedUniqueIDs(in.ExpectedLabels.Value, 100)
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
	l := ledger{s.cfg.LedgerDirectory, s.cfg.Now}
	if receipt, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		if e != nil {
			return receipt, e
		}
		return s.postReceiptV2(ctx, scope, b, receipt)
	}
	profile, p, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer p.Close()
	if profile.Access != linearconn.Write {
		return nil, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	v, e := verifyBindingV2(ctx, p, b)
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
			current, e = p2.IssueV2(ctx, in.IssueID)
			if e != nil {
				return nil, e
			}
		}
		if current.ID != in.IssueID {
			return nil, errors.New("issue identity changed")
		}
		if e = checkIssueV2(current, b, v); e != nil {
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
	if e = checkIssueV2(effective, b, v); e != nil {
		return nil, e
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
		found, e := findLabelsV2(ctx, p2, team, in.Labels.Value)
		if e != nil {
			return nil, e
		}
		if len(found) != len(in.Labels.Value) {
			return nil, errors.New("selected label is outside target team")
		}
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
	if in.Parent.Present {
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
	if in.StateID != nil {
		states, e := p.Statuses(ctx, team)
		if e != nil {
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
				create := linearprovider.CreateInputV2{CreateInput: linearprovider.CreateInput{TeamID: team, ProjectID: project, Title: *in.Title, Description: description, StateID: state}, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee}
				if verifiedParent {
					create = create.WithSameDestinationParent()
				}
				e = validation.ValidateCreateV2(create)
			} else if in.Operation == "update" {
				update := linearprovider.UpdateInputV2{UpdateInput: linearprovider.UpdateInput{ID: in.IssueID, Title: in.Title, Description: in.Description, StateID: in.StateID}, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee}
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
	return l.write(namespace, in.OperationID, payload, func() (any, error) {
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
		if e = refreshWriteProofV2(ctx, p, p2, b, v, in, team, project, current, legacy); e != nil {
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
		if e = ctx.Err(); e != nil {
			return nil, notDispatched(e)
		}
		var receipt any
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
				create := linearprovider.CreateInputV2{CreateInput: base, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee}
				if verifiedParent {
					create = create.WithSameDestinationParent()
				}
				issue, e = p2.CreateV2(ctx, create)
			}
			if e != nil {
				return nil, s.postFailureV2(ctx, scope, b, e)
			}
			if checkIssueV2(issue, b, v) != nil || issue.TeamID != team || issue.ProjectID != project {
				return nil, errors.New("created issue receipt scope mismatch")
			}
			if !legacy && receiptParentV2(issue, in.Parent, parentIssue) != nil {
				return nil, errors.New("created issue receipt parent mismatch")
			}
			if legacy {
				receipt = map[string]any{"id": issue.ID, "identifier": issue.Identifier, "team_id": issue.TeamID, "project_id": issue.ProjectID}
			} else {
				receipt = ReceiptV2{Kind: "issue", Outcome: "accepted", ID: issue.ID, Identifier: issue.Identifier, TeamID: issue.TeamID, ProjectID: projectPointerV2(issue.ProjectID)}
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
				update := linearprovider.UpdateInputV2{UpdateInput: base, Project: in.Project, Labels: in.Labels, Parent: in.Parent, Assignee: in.Assignee}
				if verifiedParent {
					update = update.WithSameDestinationParent()
				}
				issue, e = p2.UpdateV2(ctx, update)
			}
			if e != nil {
				return nil, s.postFailureV2(ctx, scope, b, e)
			}
			if issue.ID != in.IssueID || checkIssueV2(issue, b, v) != nil || issue.TeamID != team || ((in.Project.Present || legacy || current.ProjectKnown) && issue.ProjectID != project) {
				return nil, errors.New("updated issue receipt scope mismatch")
			}
			if !legacy && receiptParentV2(issue, in.Parent, parentIssue) != nil {
				return nil, errors.New("updated issue receipt parent mismatch")
			}
			if legacy {
				receipt = map[string]any{"id": issue.ID, "identifier": issue.Identifier, "team_id": issue.TeamID, "project_id": issue.ProjectID}
			} else {
				receipt = ReceiptV2{Kind: "issue", Outcome: "accepted", ID: issue.ID, Identifier: issue.Identifier, TeamID: issue.TeamID, ProjectID: projectPointerV2(issue.ProjectID)}
			}
		default:
			r, err := p.Comment(ctx, in.IssueID, in.Body)
			if err != nil {
				return nil, s.postFailureV2(ctx, scope, b, err)
			}
			if r.IssueID != in.IssueID || !linearprovider.ValidID(r.ID) {
				return nil, errors.New("comment receipt scope mismatch")
			}
			if legacy {
				receipt = r
			} else {
				receipt = ReceiptV2{Kind: "comment", Outcome: "accepted", ID: r.ID, IssueID: r.IssueID}
			}
		}
		return s.postReceiptV2(ctx, scope, b, receipt)
	})
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
