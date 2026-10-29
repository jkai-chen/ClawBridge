package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Coordinator-only Linear admin writes: plain team labels from the project
// owner's approved list, and team projects. They run from the CLI only,
// for the ClawBridge project owner, through the personal Linear profile,
// and never retire, delete or merge anything.

// AdminInput names one admin write. From is set for a rename only.
// Initiatives belong to the workspace: initiative_create names no team, and
// initiative_add_project names the initiative and a bound project.
type AdminInput struct {
	Kind         string // label_create, label_rename, project_create, initiative_create or initiative_add_project
	OperationID  string
	TeamID       string
	Name         string
	From         string
	Description  string
	InitiativeID string
	ProjectID    string
	// Closes is project create's --closes (A3): agent puts the new project's
	// rule into the proposed link; human is the default and adds nothing.
	Closes string
	// ProjectKind is project create's --kind: the project kind whose
	// seeded label the new project gets. A coordinator's create needs it.
	ProjectKind string
}

// AdminResult is an admin write's receipt. Proposal is the binding preview
// for a created project in a project-set binding; ProposalNote explains a
// missing one.
type AdminResult struct {
	Kind           string   `json:"kind"`
	Outcome        string   `json:"outcome"`
	ID             string   `json:"id,omitempty"`
	ProjectID      string   `json:"project_id,omitempty"`
	Name           string   `json:"name"`
	PreviousName   string   `json:"previous_name,omitempty"`
	TeamID         string   `json:"team_id"`
	PolicyRevision int      `json:"policy_revision,omitempty"`
	Unverified     []string `json:"unverified,omitempty"`
	DuplicateIDs   []string `json:"duplicate_ids,omitempty"`
	Audit          string   `json:"audit,omitempty"`
	Proposal       any      `json:"proposal"`
	ProposalNote   string   `json:"proposal_note,omitempty"`
	ConfirmCommand string   `json:"confirm_command,omitempty"`
	// Lead is what project create did about the new project's lead, with
	// project leads on (step 6.1a); nil otherwise.
	Lead *CreateLeadResult `json:"lead,omitempty"`
	// A project create with a kind: the kind and its label's ID.
	ProjectKind string `json:"project_kind,omitempty"`
	KindLabelID string `json:"kind_label_id,omitempty"`
	// OwnerNotice is how the owner was told of a coordinator's create
	// (FR5).
	OwnerNotice string `json:"owner_notice,omitempty"`
}

// adminProvider is a provider that can run the admin writes.
type adminProvider interface {
	AdminReason() string
	TeamLabelsNamed(ctx context.Context, team, name string) ([]linearprovider.AdminLabel, bool, error)
	CreateTeamLabel(ctx context.Context, team, name string) (linearprovider.AdminReceipt, error)
	RenameTeamLabel(ctx context.Context, team, id, name string) (linearprovider.AdminReceipt, error)
	TeamProjectsNamed(ctx context.Context, team, name string) ([]linearprovider.ProjectListing, bool, error)
	CreateTeamProject(ctx context.Context, team, name string, labels ...string) (linearprovider.AdminReceipt, error)
	InitiativeReason() string
	InitiativesNamed(ctx context.Context, name string) ([]linearprovider.Initiative, bool, error)
	CreateInitiative(ctx context.Context, name, description string) (linearprovider.AdminReceipt, error)
	InitiativeHasProject(ctx context.Context, initiative, project string) (bool, error)
	AddProjectToInitiative(ctx context.Context, initiative, project string) (linearprovider.AdminReceipt, error)
}

var errAdminOwnerOnly = errors.New("Linear label and project creation is for the ClawBridge project owner only; ask the coordinator")

// noLeadYet is a coordinator's create's lead line (FR5 Q-C).
const noLeadYet = "no lead set: a coordinator's create sets none; once the project is in the binding, the project owner can set one with clawbridge linear roles apply-leads"

// ValidateAdmin checks an admin write before any read.
func ValidateAdmin(in AdminInput) error {
	if !operationPattern.MatchString(in.OperationID) {
		return errors.New("admin write requires operation_id")
	}
	if in.Closes != "" && (in.Kind != "project_create" || in.Closes != ClosesAgent && in.Closes != ClosesHuman) {
		return errors.New("--closes is agent or human, on project create only")
	}
	if in.ProjectKind != "" && in.Kind != "project_create" {
		return errors.New("--kind is for project create only")
	}
	if in.ProjectKind != "" {
		if e := bus.ValidLabelName(in.ProjectKind); e != nil {
			return errors.New("--kind must name one of the project's kinds")
		}
	}
	switch in.Kind {
	case "initiative_create":
		if in.TeamID != "" || in.From != "" || in.InitiativeID != "" || in.ProjectID != "" {
			return errors.New("initiative create takes a name and an optional description only")
		}
		if e := bus.ValidLabelName(in.Name); e != nil {
			return e
		}
		if !validText(in.Description, 2000, true) {
			return errors.New("an initiative description is up to 2000 bytes of text")
		}
		return mentionGuard(&in.Description)
	case "initiative_add_project":
		if in.TeamID != "" || in.From != "" || in.Name != "" || in.Description != "" || !linearprovider.ValidID(in.InitiativeID) || !linearprovider.ValidID(in.ProjectID) {
			return errors.New("initiative add-project takes an initiative UUID and a project UUID only")
		}
		return nil
	}
	if in.Description != "" || in.InitiativeID != "" || in.ProjectID != "" {
		return errors.New("only initiative writes take a description, an initiative or a project")
	}
	if !linearprovider.ValidID(in.TeamID) {
		return errors.New("admin write requires a bound team UUID")
	}
	if e := bus.ValidLabelName(in.Name); e != nil {
		return e
	}
	switch in.Kind {
	case "label_create", "project_create":
		if in.From != "" {
			return errors.New("only a rename names the label it renames")
		}
	case "label_rename":
		if e := bus.ValidLabelName(in.From); e != nil {
			return e
		}
		if strings.EqualFold(in.From, in.Name) {
			return errors.New("a rename needs a different name")
		}
	default:
		return errors.New("unsupported admin write")
	}
	return nil
}

// labelPolicy reads the project's approved label list.
func (s *Service) labelPolicy(ctx context.Context, scope bus.Request) (bus.LabelPolicy, int, error) {
	scope.Operation = "label_policy_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return bus.LabelPolicy{}, 0, e
	}
	if r.Source != "server" && r.Source != "local-authority" {
		return bus.LabelPolicy{}, 0, errors.New("the label policy requires authoritative settings")
	}
	m, ok := r.Data.(map[string]any)
	if !ok {
		return bus.LabelPolicy{}, 0, errors.New("invalid label policy")
	}
	policy, e := bus.ParseLabelPolicy(map[string]any{"labels": m["labels"], "renames": m["renames"]})
	if e != nil {
		return bus.LabelPolicy{}, 0, e
	}
	revision, ok := policyRevision(m["revision"])
	if !ok {
		return bus.LabelPolicy{}, 0, errors.New("invalid label policy revision")
	}
	return policy, revision, nil
}

func policyRevision(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n >= 0
	case int64:
		return int(n), n >= 0
	case float64:
		return int(n), n >= 0 && n == float64(int(n))
	case json.Number:
		// A hosted server's answer, decoded with UseNumber.
		i, err := n.Int64()
		return int(i), err == nil && i >= 0
	}
	return 0, false
}

// approved returns the policy's own spelling of name, or "".
func approved(names []string, name string) string {
	for _, n := range names {
		if n == name {
			return n
		}
	}
	return ""
}

// Admin runs one admin write. The project owner only; everything is checked
// before the ledger records anything, and a label write is checked against
// the policy revision it was approved under.
func (s *Service) Admin(ctx context.Context, scope bus.Request, in AdminInput) (AdminResult, error) {
	if e := ValidateAdmin(in); e != nil {
		return AdminResult{}, e
	}
	// Through the person's own profile: the project's hourly budget counts
	// it (FR8).
	kind := "admin"
	if in.Kind == "project_create" {
		kind = "project_create"
	}
	var out AdminResult
	e := s.budgeted(ctx, scope, kind, in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.admin(ctx, scope, in)
		return e
	})
	return out, e
}

func (s *Service) admin(ctx context.Context, scope bus.Request, in AdminInput) (AdminResult, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return AdminResult{}, e
	}
	if !b.Configured {
		return AdminResult{}, errors.New("task-board is not linked")
	}
	if in.Closes != "" && b.Mode != "project_set" {
		return AdminResult{}, errors.New("a team board has no per-project close rule; nothing was sent")
	}
	if s.cfg.Authority == "" {
		return AdminResult{}, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return AdminResult{}, e
	}
	// The project's designated coordinator session (while the dispatcher is on,
	// a domain's) may create a project,
	// in its lane's default team, with a kind (FR5); every other admin
	// write stays the owner's.
	coordinator := access.ProjectRole != "owner"
	if coordinator && (in.Kind != "project_create" || !access.Coordinator) {
		return AdminResult{}, errAdminOwnerOnly
	}
	if coordinator {
		if e := coordinatorCreateChecks(b, access, in); e != nil {
			return AdminResult{}, e
		}
	}
	initiative := strings.HasPrefix(in.Kind, "initiative_")
	if !initiative && !contains(b.LinearTeamIDs, in.TeamID) {
		return AdminResult{}, errors.New("the team is not in this project's binding; nothing was sent")
	}
	revision := 0
	if in.Kind == "label_create" || in.Kind == "label_rename" {
		policy, rev, e := s.labelPolicy(ctx, scope)
		if e != nil {
			return AdminResult{}, e
		}
		revision = rev
		if in.Kind == "label_create" && approved(policy.Labels, in.Name) == "" {
			return AdminResult{}, fmt.Errorf("label %q is not in the approved list (revision %d); the project owner sets it with linear label-policy set; nothing was sent", in.Name, rev)
		}
		if in.Kind == "label_rename" && policy.Renames[in.From] != in.Name {
			return AdminResult{}, fmt.Errorf("renaming %q to %q is not in the approved list (revision %d); nothing was sent", in.From, in.Name, rev)
		}
	}
	dispatching, settled := false, false
	// The kind's label: for a coordinator from its reservation, made
	// before anything reaches Linear (F1); for the owner from the setting.
	label := ""
	if coordinator {
		if label, e = s.reserveProjectCreate(ctx, scope, in); e != nil {
			return AdminResult{}, e
		}
		// Until the write starts, any refusal closes the reservation as
		// not sent; a retry with the same operation_id reopens it.
		defer func() {
			if !dispatching && !settled {
				s.settleProjectCreate(ctx, scope, in, nil)
			}
		}()
	} else if in.ProjectKind != "" {
		if label, e = s.ownerKindLabel(ctx, scope, in); e != nil {
			return AdminResult{}, e
		}
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	payload := struct {
		Version                        int
		Kind                           string
		Profile                        string
		Binding                        bus.TaskboardBindingV2
		Actor, Authority, InstanceHash string
		Input                          AdminInput
		PolicyRevision                 int
		KindLabelID                    string `json:",omitempty"`
	}{1, "admin", s.cfg.ProfileID, b, access.SessionID, s.cfg.Authority, digest(scope.Args["instance"]), in, revision, label}
	l := s.operations(ctx)
	if r, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		// A recorded operation may have reached Linear: its reservation
		// is settled from the receipt, or stays open.
		dispatching = exists
		if e != nil {
			return AdminResult{}, e
		}
		result, e := adminResult(r)
		if e != nil {
			return AdminResult{}, e
		}
		// A retry after a lost answer settles the reservation from the
		// receipt: nothing is created twice (F3).
		if coordinator {
			s.settleProjectCreate(ctx, scope, in, &result)
			settled = true
		}
		// A retry records an audit an earlier run failed to; the authority
		// keeps one row per operation.
		result.Audit = "recorded"
		if _, e = s.auditAdmin(ctx, scope, in, result); e != nil {
			result.Audit = "failed: " + e.Error()
		}
		return result, nil
	}
	profile, p, done, e := s.openForWrite(ctx)
	if e != nil {
		return AdminResult{}, e
	}
	defer done()
	if profile.Access != linearconn.Write {
		return AdminResult{}, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	admin, ok := p.(adminProvider)
	if !ok {
		return AdminResult{}, errors.New("native provider lacks the admin writes")
	}
	if reason := admin.AdminReason(); reason != "" {
		return AdminResult{}, errors.New("Linear admin writes are unavailable: " + reason)
	}
	if reason := admin.InitiativeReason(); initiative && reason != "" {
		return AdminResult{}, errors.New("Linear initiative writes are unavailable: " + reason)
	}
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return AdminResult{}, e
	}
	// A sub-team's issues can use its bound parent's labels, so ClawBridge
	// never makes a sub-team copy of one (CROSS-TEAM-BINDING §3.2).
	if p2, ok := p.(ProviderV2); ok && in.Kind == "label_create" {
		for _, parent := range labelTeamsV2(v, b, in.TeamID)[1:] {
			found, _, e := labelsNamedV2(ctx, p2, parent, in.Name)
			if e != nil {
				return AdminResult{}, e
			}
			if len(found) > 0 {
				return AdminResult{}, fmt.Errorf("the bound parent team already has label %q, and the sub-team's issues can use it; nothing was sent", in.Name)
			}
		}
	}
	// The pre-check decides the outcome before the ledger: an existing name
	// is never written over, and an incomplete walk sends nothing.
	plan, e := adminPlan(ctx, admin, p, b, in)
	if e != nil {
		return AdminResult{}, e
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return AdminResult{}, e
	}
	dispatching = true
	out, e := l.write(namespace, in.OperationID, ledgerMeta{in.Kind, b.ProjectID}, payload, func() (any, error) {
		if e := s.unchangedV2(ctx, scope, b); e != nil {
			return nil, authorityFailure(e)
		}
		fresh, e := s.writeAccessV2(ctx, scope, b)
		if e != nil {
			return nil, authorityFailure(e)
		}
		still := fresh.ProjectRole == "owner"
		if coordinator {
			still = fresh.Coordinator && fresh.Lane == access.Lane
		}
		if fresh.SessionID != access.SessionID || !still {
			return nil, refusedBeforeDispatch(errAdminOwnerOnly)
		}
		// The approved list is read again: a change since the check, even
		// one that keeps this name, refuses the write before it is sent.
		if in.Kind == "label_create" || in.Kind == "label_rename" {
			if _, now, e := s.labelPolicy(ctx, scope); e != nil || now != revision {
				return nil, refusedBeforeDispatch(errors.New("the approved label list changed before the write; read it again and use a new operation_id"))
			}
		}
		again, e := adminPlan(ctx, admin, p, b, in)
		if e != nil {
			return nil, proofFailure(e)
		}
		if again.observed != plan.observed || again.target != plan.target {
			return nil, refusedBeforeDispatch(errors.New("Linear changed before the write; read it again and use a new operation_id"))
		}
		if e = ctx.Err(); e != nil {
			return nil, notDispatched(e)
		}
		result := AdminResult{Kind: in.Kind, Name: in.Name, TeamID: in.TeamID, ProjectID: in.ProjectID, PolicyRevision: revision, ProjectKind: in.ProjectKind, KindLabelID: label}
		if in.Kind == "initiative_add_project" {
			result.ID = in.InitiativeID
		}
		if in.Kind == "label_rename" {
			result.PreviousName = in.From
		}
		if plan.observed {
			result.Outcome = "observed_unchanged"
			if in.Kind != "initiative_add_project" {
				result.ID = plan.existing
			}
			return result, nil
		}
		sent, known := sentCount(p)
		var r linearprovider.AdminReceipt
		switch in.Kind {
		case "label_create":
			r, e = admin.CreateTeamLabel(ctx, in.TeamID, in.Name)
		case "label_rename":
			r, e = admin.RenameTeamLabel(ctx, in.TeamID, plan.target, in.Name)
		case "initiative_create":
			r, e = admin.CreateInitiative(ctx, in.Name, in.Description)
		case "initiative_add_project":
			r, e = admin.AddProjectToInitiative(ctx, in.InitiativeID, in.ProjectID)
		default:
			var labels []string
			if label != "" {
				labels = []string{label}
			}
			r, e = admin.CreateTeamProject(ctx, in.TeamID, in.Name, labels...)
		}
		if e != nil {
			return nil, writeFailure(p, sent, known, e, func(e error) error { return e })
		}
		result.Outcome, result.Unverified = "accepted", r.Unverified
		if in.Kind != "initiative_add_project" {
			result.ID = r.ID
		}
		// A concurrent create of the same name can still make a duplicate;
		// it is reported, never cleaned up.
		if in.Kind == "label_create" {
			after, complete, e := admin.TeamLabelsNamed(ctx, in.TeamID, in.Name)
			switch {
			case e != nil || !complete:
				result.Outcome = "accepted_duplicate_unknown"
			case len(after) > 1:
				result.Outcome = "accepted_duplicate_observed"
				for _, l := range after {
					result.DuplicateIDs = append(result.DuplicateIDs, l.ID)
				}
			}
			// The re-list can prove the team a receipt left out.
			for _, l := range after {
				if l.ID == r.ID && l.TeamKnown && l.TeamID == in.TeamID {
					result.Unverified = nil
				}
			}
		}
		return result, nil
	})
	if e != nil {
		// A refusal sent nothing and is final for this operation_id: the
		// reservation closes as not sent. Any other failure may have
		// reached Linear, so it stays open and shows as unconfirmed.
		var refused refusedBeforeDispatchError
		var never notDispatchedError
		if coordinator && (errors.As(e, &refused) || errors.As(e, &never)) {
			s.settleProjectCreate(ctx, scope, in, nil)
		}
		return AdminResult{}, e
	}
	result, e := adminResult(out)
	if e != nil {
		return AdminResult{}, e
	}
	if coordinator {
		s.settleProjectCreate(ctx, scope, in, &result)
	}
	if _, e = s.auditAdmin(ctx, scope, in, result); e != nil {
		result.Audit = "failed: " + e.Error()
	} else {
		result.Audit = "recorded"
	}
	if in.Kind == "project_create" {
		s.proposeProject(ctx, scope, b, &result, in.Closes)
	}
	return result, nil
}

type adminPlanned struct {
	observed bool
	existing string // the label or project already there
	target   string // the label a rename changes
}

// adminPlan reads Linear to decide what an admin write would do. A label or
// project already named in.Name is not created again; a workspace label
// with that name, or one whose own scope is unknown, refuses the create;
// a rename needs exactly one plain label of the team named in.From.
func adminPlan(ctx context.Context, p adminProvider, board Provider, b bus.TaskboardBindingV2, in AdminInput) (adminPlanned, error) {
	incomplete := errors.New("Linear's list could not be read completely; nothing was sent")
	switch in.Kind {
	case "initiative_create":
		// An existing initiative of that name is returned, never updated.
		found, complete, e := p.InitiativesNamed(ctx, in.Name)
		if e != nil {
			return adminPlanned{}, e
		}
		if !complete {
			return adminPlanned{}, incomplete
		}
		if len(found) > 0 {
			return adminPlanned{observed: true, existing: found[0].ID}, nil
		}
		return adminPlanned{}, nil
	case "initiative_add_project":
		// Only a project of a bound team joins an initiative: one in the
		// binding, or one just created in a bound team.
		project, e := board.GetProject(ctx, in.ProjectID)
		if e != nil {
			return adminPlanned{}, e
		}
		bound := false
		for _, t := range project.TeamIDs {
			bound = bound || contains(b.LinearTeamIDs, t)
		}
		if project.ID != in.ProjectID || !bound {
			return adminPlanned{}, errors.New("the project is not in a team of this project's binding; nothing was sent")
		}
		in, e := p.InitiativeHasProject(ctx, in.InitiativeID, in.ProjectID)
		if e != nil {
			return adminPlanned{}, e
		}
		if in {
			return adminPlanned{observed: true, existing: project.ID}, nil
		}
		return adminPlanned{target: project.ID}, nil
	case "project_create":
		found, complete, e := p.TeamProjectsNamed(ctx, in.TeamID, in.Name)
		if e != nil {
			return adminPlanned{}, e
		}
		if !complete {
			return adminPlanned{}, incomplete
		}
		if len(found) > 0 {
			return adminPlanned{observed: true, existing: found[0].ID}, nil
		}
		return adminPlanned{}, nil
	case "label_create":
		found, complete, e := p.TeamLabelsNamed(ctx, in.TeamID, in.Name)
		if e != nil {
			return adminPlanned{}, e
		}
		if !complete {
			return adminPlanned{}, incomplete
		}
		for _, l := range found {
			switch {
			case l.TeamKnown && l.TeamID == "":
				return adminPlanned{}, fmt.Errorf("exists_at_workspace_level: a workspace label named %q already exists; nothing was sent", l.Name)
			case !l.TeamKnown:
				return adminPlanned{}, fmt.Errorf("exists_scope_unknown: a label named %q is already available to this team (team or workspace label); nothing was sent", l.Name)
			}
		}
		if len(found) > 0 {
			return adminPlanned{observed: true, existing: found[0].ID}, nil
		}
		return adminPlanned{}, nil
	}
	from, complete, e := p.TeamLabelsNamed(ctx, in.TeamID, in.From)
	if e != nil {
		return adminPlanned{}, e
	}
	if !complete {
		return adminPlanned{}, incomplete
	}
	if len(from) != 1 {
		return adminPlanned{}, fmt.Errorf("found %d labels named %q; a rename needs exactly one; nothing was sent", len(from), in.From)
	}
	l := from[0]
	if !l.TeamKnown || l.TeamID != in.TeamID || l.Group || l.Retired {
		return adminPlanned{}, fmt.Errorf("label %q is not a plain, current label of this team; nothing was sent", in.From)
	}
	to, complete, e := p.TeamLabelsNamed(ctx, in.TeamID, in.Name)
	if e != nil {
		return adminPlanned{}, e
	}
	if !complete {
		return adminPlanned{}, incomplete
	}
	if len(to) > 0 {
		return adminPlanned{}, fmt.Errorf("a label named %q already exists; nothing was sent", in.Name)
	}
	return adminPlanned{target: l.ID}, nil
}

var adminKinds = map[string]bool{"label_create": true, "label_rename": true, "project_create": true, "initiative_create": true, "initiative_add_project": true}

// validAdminReceipt checks an admin receipt recorded in the ledger, strictly.
func validAdminReceipt(raw []byte) bool {
	var r AdminResult
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || !adminKinds[r.Kind] || r.Proposal != nil || r.Audit != "" || r.ConfirmCommand != "" || r.ProposalNote != "" {
		return false
	}
	switch r.Outcome {
	case "accepted", "observed_unchanged", "accepted_duplicate_observed", "accepted_duplicate_unknown":
	default:
		return false
	}
	switch r.Kind {
	case "initiative_create":
		if r.TeamID != "" || r.ProjectID != "" || bus.ValidLabelName(r.Name) != nil {
			return false
		}
	case "initiative_add_project":
		if r.TeamID != "" || r.Name != "" || !linearprovider.ValidID(r.ProjectID) {
			return false
		}
	default:
		if !linearprovider.ValidID(r.TeamID) || r.ProjectID != "" || bus.ValidLabelName(r.Name) != nil {
			return false
		}
	}
	if !linearprovider.ValidID(r.ID) || (r.PreviousName != "") != (r.Kind == "label_rename") {
		return false
	}
	for _, f := range r.Unverified {
		if f != "team" && f != "teams" && f != "initiative" {
			return false
		}
	}
	for _, id := range r.DuplicateIDs {
		if !linearprovider.ValidID(id) {
			return false
		}
	}
	return true
}

// adminResult reads a receipt back from the ledger, where it is JSON.
func adminResult(v any) (AdminResult, error) {
	if r, ok := v.(AdminResult); ok {
		return r, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return AdminResult{}, errors.New("invalid admin receipt")
	}
	r := AdminResult{Kind: fmt.Sprint(m["kind"]), Outcome: fmt.Sprint(m["outcome"]), Name: fmt.Sprint(m["name"]), TeamID: fmt.Sprint(m["team_id"])}
	r.ID, _ = m["id"].(string)
	r.PreviousName, _ = m["previous_name"].(string)
	r.ProjectID, _ = m["project_id"].(string)
	if n, ok := policyRevision(m["policy_revision"]); ok {
		r.PolicyRevision = n
	}
	for _, k := range []string{"unverified", "duplicate_ids"} {
		if list, ok := m[k].([]any); ok {
			for _, x := range list {
				if s, ok := x.(string); ok {
					if k == "unverified" {
						r.Unverified = append(r.Unverified, s)
					} else {
						r.DuplicateIDs = append(r.DuplicateIDs, s)
					}
				}
			}
		}
	}
	return r, nil
}

// auditAdmin records the write in the project's history: IDs and names,
// and for a label the policy revision it was checked against. already
// reports a write the history had recorded before.
func (s *Service) auditAdmin(ctx context.Context, scope bus.Request, in AdminInput, r AdminResult) (already bool, err error) {
	scope.Operation = "linear_admin_audit"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["kind"] = in.Kind
	scope.Args["outcome"] = r.Outcome
	scope.Args["operation_id"] = in.OperationID
	for k, v := range map[string]string{"linear_team_id": in.TeamID, "name": in.Name, "linear_id": r.ID, "previous_name": in.From, "linear_project_id": in.ProjectID} {
		if v != "" {
			scope.Args[k] = v
		}
	}
	if in.Kind == "label_create" || in.Kind == "label_rename" {
		scope.Args["policy_revision"] = r.PolicyRevision
	}
	out, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return false, e
	}
	m, _ := out.Data.(map[string]any)
	return m["already"] == true, nil
}

// AdminReplay is the result of AdminAuditReplay: the recorded receipt and
// what the history answered.
type AdminReplay struct {
	OperationID string      `json:"operation_id"`
	Replayed    bool        `json:"replayed"`
	Audit       string      `json:"audit"`
	Receipt     AdminResult `json:"receipt"`
}

// AdminAuditReplay records the history row of an admin write that Linear
// accepted but whose audit failed, from this client's operation ledger. It
// is for writes a retry can no longer reach because the binding or the
// approved list changed since: the record is found whatever payload it was
// written for. Only an accepted admin receipt is replayed; nothing is sent
// to Linear and the ledger is not written. The project owner only, from the
// session the write ran under (none for a sessionless write).
func (s *Service) AdminAuditReplay(ctx context.Context, scope bus.Request, operationID string) (AdminReplay, error) {
	if !operationPattern.MatchString(operationID) {
		return AdminReplay{}, errors.New("audit replay requires operation_id")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return AdminReplay{}, e
	}
	if !b.Configured {
		return AdminReplay{}, errors.New("task-board is not linked")
	}
	if s.cfg.Authority == "" {
		return AdminReplay{}, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return AdminReplay{}, e
	}
	if access.ProjectRole != "owner" {
		return AdminReplay{}, errAdminOwnerOnly
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	recorded, exists, e := s.operations(ctx).lookupAny(namespace, operationID)
	if e != nil {
		return AdminReplay{}, fmt.Errorf("no replayable admin write: %w", e)
	}
	if !exists {
		return AdminReplay{}, errors.New("no admin write with this operation_id in this client's ledger for this project and session; pass the --session it ran under, or none")
	}
	r, e := adminResult(recorded)
	if e != nil || !adminKinds[r.Kind] {
		return AdminReplay{}, errors.New("this operation_id is not an admin write")
	}
	in := AdminInput{Kind: r.Kind, OperationID: operationID, TeamID: r.TeamID, Name: r.Name, From: r.PreviousName, ProjectID: r.ProjectID}
	out := AdminReplay{OperationID: operationID, Replayed: true, Receipt: r, Audit: "recorded"}
	already, e := s.auditAdmin(ctx, scope, in, r)
	switch {
	case e != nil:
		out.Audit = "failed: " + e.Error()
	case already:
		out.Audit = "already"
	}
	return out, nil
}

// proposeProject adds the binding proposal for a created project. Only a
// project-set binding gets one: in a team board a project of a bound team is
// already in scope, and switching mode would narrow the scope.
func (s *Service) proposeProject(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2, r *AdminResult, closes string) {
	if b.Mode != "project_set" {
		r.ProposalNote = "already in scope: the team board covers every project of its teams"
		return
	}
	if r.ID == "" || contains(b.LinearProjectIDs, r.ID) {
		r.ProposalNote = "already in scope: the project is in the binding"
		return
	}
	projects := append(append([]string{}, b.LinearProjectIDs...), r.ID)
	in := LinkInputV2{TeamIDs: b.LinearTeamIDs, DefaultTeamID: b.LinearDefaultTeamID, ProjectIDs: projects, ExpectedRevision: b.Revision, TeamsWithoutProject: b.TeamsWithoutProject}
	if closes == ClosesAgent {
		in.Closes = map[string]string{r.ID: ClosesAgent}
	}
	preview, e := s.LinkV2(ctx, scope, in)
	if e != nil {
		r.ProposalNote = "no proposal: " + e.Error()
		return
	}
	r.Proposal = preview
	args := []string{"clawbridge linear link"}
	for _, t := range SortedTeams(b.LinearTeamIDs) {
		args = append(args, "--team "+t)
	}
	args = append(args, "--default-team "+b.LinearDefaultTeamID)
	if p, ok := preview.(LinkPreviewV2); ok {
		for _, t := range p.TeamsWithoutProject {
			args = append(args, "--team-without-project "+t)
		}
	}
	for _, p := range SortedTeams(projects) {
		args = append(args, "--linear-project "+p)
	}
	if closes == ClosesAgent {
		args = append(args, "--closes "+r.ID+"=agent")
	}
	args = append(args, fmt.Sprintf("--expected-revision %d --confirm", b.Revision))
	r.ConfirmCommand = strings.Join(args, " ")
}

// LabelPolicyV2 reads the project's approved label list.
func (s *Service) LabelPolicyV2(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "label_policy_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// SetLabelPolicyV2 replaces the approved label list at expected, the
// revision the owner last read. The file is decoded strictly here and again
// by the authority.
func (s *Service) SetLabelPolicyV2(ctx context.Context, scope bus.Request, raw []byte, expected int64) (any, error) {
	if e := uniqueKeys(raw); e != nil {
		return nil, e
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("the label policy file must hold one JSON object")
	}
	policy, e := bus.ParseLabelPolicy(v)
	if e != nil {
		return nil, e
	}
	scope.Operation = "label_policy_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = expected
	scope.Args["label_policy"] = map[string]any{"labels": policy.Labels, "renames": policy.Renames}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// uniqueKeys refuses JSON in which an object repeats a key: a decoder would
// silently keep the last one.
func uniqueKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		t, e := dec.Token()
		if e != nil {
			return e
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				key, _ := k.(string)
				if seen[key] {
					return fmt.Errorf("the label policy file repeats the key %q", key)
				}
				seen[key] = true
				if e = walk(); e != nil {
					return e
				}
			}
			_, e = dec.Token()
			return e
		case json.Delim('['):
			for dec.More() {
				if e := walk(); e != nil {
					return e
				}
			}
			_, e = dec.Token()
			return e
		}
		return nil
	}
	if e := walk(); e != nil {
		if strings.Contains(e.Error(), "repeats the key") {
			return e
		}
		return errors.New("the label policy file must hold one JSON object")
	}
	return nil
}
