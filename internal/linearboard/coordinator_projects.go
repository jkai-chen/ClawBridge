package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// A coordinator's project create (live/FR5-PLAN.md r1): the project's
// designated coordinator session creates a Linear project in its lane's
// default team, with a kind, through its person's own Linear profile. The
// server reserves the create before any Linear call and the settle tells
// the owner; delete and archive stay the owner's, in Linear.

// coordinatorCreateChecks refuses, before anything is recorded or sent, a
// coordinator's create the plan doesn't allow: --closes (the owner's), no
// --kind, a name the agent issue guards refuse (F2), or a team other than
// the lane's default team (Q-A).
func coordinatorCreateChecks(b bus.TaskboardBindingV2, access bus.TaskboardAccess, in AdminInput) error {
	if in.Closes != "" {
		return errors.New("--closes on project create is the project owner's; nothing was sent")
	}
	if in.ProjectKind == "" {
		return errors.New("a coordinator's project create needs --kind NAME, one of the project's kinds (linear kind show); nothing was sent")
	}
	// The name is already 1–80 characters with no control character or
	// @-mention (ValidateAdmin), inside an issue title's limit; the issue
	// guard adds profile links and mention:// URLs.
	name := in.Name
	if e := mentionGuard(&name); e != nil {
		return e
	}
	home := b.LinearDefaultTeamID
	if len(b.Lanes) > 0 {
		if access.LaneWriteTeamIDs == nil {
			return outsideLaneError("outside_lane: this binding has lanes, but the ClawBridge server named no lane for this session; upgrade the server; nothing was sent")
		}
		l, ok := b.Lane(access.Lane)
		if !ok {
			return outsideLaneError("outside_lane: this session has no lane, so it creates no project; the project owner pins it with linear lanes pin; nothing was sent")
		}
		home = l.DefaultTeamID
	}
	if in.TeamID != home {
		return outsideLaneError(fmt.Sprintf("outside_lane: a coordinator creates projects only in its lane's default team %s; nothing was sent", home))
	}
	return nil
}

func nameDigest(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// reserveProjectCreate records the create on the server before any Linear
// call (F1) and returns the kind's label ID, which the server checked:
// seeded, and of the create's team.
func (s *Service) reserveProjectCreate(ctx context.Context, scope bus.Request, in AdminInput) (string, error) {
	scope.Operation = "project_create_reserve"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["operation_id"], scope.Args["team_id"], scope.Args["kind"], scope.Args["name_sha256"] = in.OperationID, in.TeamID, in.ProjectKind, nameDigest(in.Name)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return "", fmt.Errorf("the project create could not be reserved; nothing was sent: %w", e)
	}
	m, _ := r.Data.(map[string]any)
	label, _ := m["kind_label_id"].(string)
	if !linearprovider.ValidID(label) {
		return "", errors.New("the server's reservation named no kind label; nothing was sent")
	}
	return label, nil
}

// ownerKindLabel is the owner's --kind: the setting's seeded label of the
// kind, on the create's team.
func (s *Service) ownerKindLabel(ctx context.Context, scope bus.Request, in AdminInput) (string, error) {
	set, e := s.kindSetting(ctx, scope)
	if e != nil {
		return "", e
	}
	if set.team != in.TeamID {
		return "", errors.New("the project kinds belong to another team; nothing was sent")
	}
	for _, v := range set.values {
		if v.Name != in.ProjectKind {
			continue
		}
		if set.groupLabelID == "" || !linearprovider.ValidID(v.LabelID) {
			return "", errors.New("the kind's label isn't seeded in Linear yet (linear kind seed); nothing was sent")
		}
		return v.LabelID, nil
	}
	return "", errors.New("--kind must name one of the project's kinds (linear kind show); nothing was sent")
}

// settleProjectCreate closes a coordinator's reservation: created, with
// the project's ID, which tells the owner; or not sent, when result is nil
// or Linear already had the name. A failed settle never undoes the create:
// the reservation stays open, and the owner is told it is unconfirmed.
func (s *Service) settleProjectCreate(ctx context.Context, scope bus.Request, in AdminInput, result *AdminResult) {
	scope.Operation = "project_create_settle"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["operation_id"] = in.OperationID
	created := result != nil && result.Outcome != "observed_unchanged" && linearprovider.ValidID(result.ID)
	if created {
		scope.Args["outcome"], scope.Args["linear_project_id"] = "created", result.ID
	} else {
		scope.Args["outcome"] = "not_sent"
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if result == nil {
		return
	}
	result.Lead = &CreateLeadResult{Note: noLeadYet}
	switch {
	case e != nil && created:
		result.OwnerNotice = "created; the owner was not notified (" + e.Error() + "); the overview shows it as unconfirmed"
	case e != nil:
		result.OwnerNotice = "not created; the reservation could not be closed (" + e.Error() + ")"
	case !created:
		result.OwnerNotice = "not created: Linear already has a project of that name"
	default:
		m, _ := r.Data.(map[string]any)
		switch n, _ := policyRevision(m["owner_sessions_told"]); {
		case m["replay"] == true:
			result.OwnerNotice = "the owner was told when it was created"
		case n > 0:
			result.OwnerNotice = fmt.Sprintf("the owner was told (%d session(s))", n)
		default:
			result.OwnerNotice = "the owner has no active session; it shows in the overview"
		}
	}
}

// ProjectCreates lists the coordinator's creates and the unconfirmed ones;
// the read tells the owner, once, of a reservation gone stale.
func (s *Service) ProjectCreates(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "project_create_list"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// AckProjectCreate is the owner's acknowledgement of an unconfirmed create
// they checked in Linear.
func (s *Service) AckProjectCreate(ctx context.Context, scope bus.Request, operationID string) (any, error) {
	if !operationPattern.MatchString(operationID) {
		return nil, errors.New("--ack takes the create's operation ID")
	}
	if scope.Session != "" {
		return nil, errors.New("a session can't acknowledge a Linear write; the project owner, or the project lead who confirmed it, does, in their own terminal, without --session")
	}
	scope.Operation = "project_create_ack"
	scope.Args = map[string]any{"operation_id": operationID}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}
