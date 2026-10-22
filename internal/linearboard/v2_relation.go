package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

func normalizeRelationV2(in RelationInputV2) (RelationInputV2, error) {
	if !operationPattern.MatchString(in.OperationID) || (in.Action != "add" && in.Action != "remove") || !linearprovider.ValidID(in.FromID) || !linearprovider.ValidID(in.ToID) || in.FromID == in.ToID {
		return in, errors.New("relation requires one explicit action, operation ID and distinct issue UUIDs")
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
	l := ledger{s.cfg.LedgerDirectory, s.cfg.Now}
	if r, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		if e != nil {
			return r, e
		}
		return s.postReceiptV2(ctx, scope, b, r)
	}
	profile, p, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer p.Close()
	if profile.Access != linearconn.Write {
		return nil, errors.New("personal Linear profile is read-only")
	}
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return nil, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return nil, errors.New("native provider lacks safe one-edge relation capability")
	}
	_, _, e = relationEvidenceV2(ctx, p2, b, v, in)
	if e != nil {
		return nil, e
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return nil, e
	}
	return l.write(namespace, in.OperationID, payload, func() (any, error) {
		if e := s.unchangedV2(ctx, scope, b); e != nil {
			return nil, notDispatched(e)
		}
		fresh, e := s.writeAccessV2(ctx, scope, b)
		if e != nil {
			return nil, notDispatched(e)
		}
		if fresh.SessionID != access.SessionID {
			return nil, notDispatched(errors.New("task-board actor changed before dispatch"))
		}
		freshPairs, e := verifyBindingV2(ctx, p, b)
		if e != nil {
			return nil, notDispatched(e)
		}
		existing, observed, e := relationEvidenceV2(ctx, p2, b, freshPairs, in)
		if e != nil {
			return nil, notDispatched(e)
		}
		if e = s.unchangedV2(ctx, scope, b); e != nil {
			return nil, notDispatched(e)
		}
		fresh, e = s.writeAccessV2(ctx, scope, b)
		if e != nil {
			return nil, notDispatched(e)
		}
		if fresh.SessionID != access.SessionID {
			return nil, notDispatched(errors.New("task-board actor changed before dispatch"))
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
		r, e := p2.Relation(ctx, linearprovider.RelationInput{Action: in.Action, Type: in.Type, FromID: in.FromID, ToID: in.ToID})
		if e != nil {
			return nil, s.postFailureV2(ctx, scope, b, e)
		}
		if r.Action != in.Action || r.Type != in.Type || r.FromID != in.FromID || r.ToID != in.ToID || (r.ID != "" && !linearprovider.ValidID(r.ID)) {
			return nil, errors.New("relation receipt does not match the normalized edge")
		}
		receipt.ID = r.ID
		return s.postReceiptV2(ctx, scope, b, receipt)
	})
}
