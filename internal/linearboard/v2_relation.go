package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

func normalizeRelationV2(in RelationInputV2) (RelationInputV2, error) {
	validID := operationPattern.MatchString(in.OperationID) || (in.duplicateStep && stepOperationPattern.MatchString(in.OperationID))
	if !validID || (in.Action != "add" && in.Action != "remove") || !linearprovider.ValidID(in.FromID) || !linearprovider.ValidID(in.ToID) || in.FromID == in.ToID {
		return in, errors.New("relation requires one explicit action, operation ID and distinct issue UUIDs")
	}
	in.source = in.FromID
	// A duplicate edge (from the duplicate, to the original) is made only
	// by the duplicate operation, with its checks; never added or removed
	// as a plain relation.
	if in.Type == "duplicate" && in.duplicateStep && in.Action == "add" {
		return in, nil
	}
	if in.Type == "blocked-by" {
		in.FromID, in.ToID = in.ToID, in.FromID
		in.Type = "blocks"
	}
	if in.Type != "blocks" && in.Type != "related" {
		return in, errors.New("unsupported relation type")
	}
	if in.Type == "related" && in.FromID > in.ToID {
		in.FromID, in.ToID = in.ToID, in.FromID
	}
	return in, nil
}
func (s *Service) RelationV2(ctx context.Context, scope bus.Request, in RelationInputV2) (any, error) {
	in, e := normalizeRelationV2(in)
	if e != nil {
		return nil, e
	}
	if app, e := s.appWrites(ctx); e != nil || app {
		if e != nil {
			return nil, e
		}
		return s.appWrite(ctx, scope, "relation_"+in.Action, map[string]any{"operation_id": in.OperationID, "type": in.Type, "from_id": in.FromID, "to_id": in.ToID})
	}
	// The person's own profile writes it: the project's hourly budget
	// counts it (FR8).
	var out any
	e = s.budgeted(ctx, scope, "relation", in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.relationV2Personal(ctx, scope, in)
		return e
	})
	return out, e
}

func (s *Service) relationV2Personal(ctx context.Context, scope bus.Request, in RelationInputV2) (_ any, err error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if s.cfg.Authority == "" {
		return nil, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return nil, e
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	payload := struct {
		Version                        int
		Profile                        string
		Binding                        bus.TaskboardBindingV2
		Actor, Authority, InstanceHash string
		Input                          RelationInputV2
	}{2, s.cfg.ProfileID, b, access.SessionID, s.cfg.Authority, digest(scope.Args["instance"]), in}
	l := s.operations(ctx)
	if r, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		if e != nil {
			return r, e
		}
		return s.postReceiptV2(ctx, scope, b, r)
	}
	profile, p, done, e := s.openForWrite(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	if profile.Access != linearconn.Write {
		return nil, errors.New("personal Linear profile is read-only")
	}
	v, drop, e := s.proveWriteBinding(ctx, p, b, profile)
	if e != nil {
		return nil, e
	}
	defer func() {
		if err != nil {
			drop()
		}
	}()
	p2, ok := p.(ProviderV2)
	if !ok {
		return nil, errors.New("native provider lacks safe one-edge relation capability")
	}
	caps := &commandCapsV2{p: p2}
	var lane func(string) error
	if len(b.Lanes) > 0 {
		lane = func(team string) error { return checkLaneV2(b, access, scope.Session, team) }
	}
	_, _, e = relationEvidenceV2(ctx, p2, b, v, caps, in, lane)
	if e != nil {
		return nil, e
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return nil, e
	}
	return l.write(namespace, in.OperationID, ledgerMeta{Operation: "relation_" + in.Action, ProjectID: b.ProjectID}, payload, func() (any, error) {
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
		// The same command's binding verification is reused; both issues and
		// their edges are re-read here, and each end's team and project
		// checked again, and unchangedV2 re-checks the binding.
		existing, observed, e := relationEvidenceV2(ctx, p2, b, v, caps, in, lane)
		if e != nil {
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
		receipt := ReceiptV2{Kind: "edge", Outcome: "accepted", Action: in.Action, Type: in.Type, FromID: in.FromID, ToID: in.ToID}
		if observed {
			receipt.Outcome = "observed_unchanged"
			if existing != nil {
				receipt.ID = existing.ID
			}
			return s.postReceiptV2(ctx, scope, b, receipt)
		}
		sent, known := sentCount(p2)
		r, e := p2.Relation(ctx, linearprovider.RelationInput{Action: in.Action, Type: in.Type, FromID: in.FromID, ToID: in.ToID})
		if e != nil {
			return nil, writeFailure(p2, sent, known, e, func(e error) error { return s.postFailureV2(ctx, scope, b, e) })
		}
		if r.Action != in.Action || r.Type != in.Type || r.FromID != in.FromID || r.ToID != in.ToID || (r.ID != "" && !linearprovider.ValidID(r.ID)) {
			return nil, errors.New("relation receipt does not match the normalized edge")
		}
		// A response that does not itself name the edge is confirmed by
		// reading it back: an added edge must be visible, and a removed one
		// must have been visible before and be gone now (native relation
		// lists are visible, not complete, so absence alone proves nothing).
		if !r.Confirmed && in.Type == "duplicate" {
			// No issue read shows a duplicate edge, so it cannot be read
			// back: the duplicate operation reads the issue's state after.
			receipt.ID = r.ID
			receipt.Unverified = []string{"relation"}
			return s.postReceiptV2(ctx, scope, b, receipt)
		}
		if !r.Confirmed {
			after, observedAfter, e := relationEvidenceV2(ctx, p2, b, v, caps, in, nil)
			if e != nil || !(observedAfter || (in.Action == "remove" && existing != nil && after == nil)) {
				return nil, s.postFailureV2(ctx, scope, b, &linearprovider.Error{Kind: "receipt_unverified"})
			}
			if r.ID == "" && after != nil {
				r.ID = after.ID
			}
		}
		receipt.ID = r.ID
		return s.postReceiptV2(ctx, scope, b, receipt)
	})
}
