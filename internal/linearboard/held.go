package linearboard

import (
	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"context"
)

// heldOperation is one short multi-write operation (applying a claim
// marker) that reuses a single native connection, one binding proof, and
// its issue and label-membership reads across its commands, instead of
// paying for them per command. A write to an issue forgets that issue's
// reads.
type heldOperation struct {
	profile  linearconn.Profile
	provider Provider
	proofs   map[string]verifiedV2
	issues   map[string]linearprovider.IssueV2
	members  map[string]membership
}

type membership struct{ found, complete bool }

// hold returns a Service whose commands share one held operation, and the
// function that ends it.
func (s *Service) hold() (*Service, func()) {
	held := *s
	held.held = &heldOperation{proofs: map[string]verifiedV2{}, issues: map[string]linearprovider.IssueV2{}, members: map[string]membership{}}
	return &held, func() {
		if held.held.provider != nil {
			held.held.provider.Close()
		}
	}
}

// proveBinding is verifyBindingV2, once per binding in a held operation.
func (s *Service) proveBinding(ctx context.Context, p Provider, b bus.TaskboardBindingV2) (verifiedV2, error) {
	if s.held == nil {
		return verifyBindingV2(ctx, p, b)
	}
	key := digest(b)
	if v, ok := s.held.proofs[key]; ok {
		return cloneVerifiedV2(v), nil
	}
	v, e := verifyBindingV2(ctx, p, b)
	if e == nil {
		s.held.proofs[key] = cloneVerifiedV2(v)
	}
	return v, e
}

// readIssue is p.IssueV2, once per issue in a held operation.
func (s *Service) readIssue(ctx context.Context, p ProviderV2, id string) (linearprovider.IssueV2, error) {
	if s.held == nil {
		return p.IssueV2(ctx, id)
	}
	if i, ok := s.held.issues[id]; ok {
		return i, nil
	}
	i, e := p.IssueV2(ctx, id)
	if e == nil {
		s.held.issues[id] = i
	}
	return i, e
}

// rereadIssue always reads the issue from Linear, and refreshes a held
// operation's cache with it.
func (s *Service) rereadIssue(ctx context.Context, p ProviderV2, id string) (linearprovider.IssueV2, error) {
	i, e := p.IssueV2(ctx, id)
	if e == nil && s.held != nil {
		s.held.issues[id] = i
	}
	return i, e
}

// wrote forgets a held operation's reads of an issue it just changed.
func (s *Service) wrote(id string) {
	if s.held != nil {
		delete(s.held.issues, id)
		for k := range s.held.members {
			delete(s.held.members, k)
		}
	}
}

// heldMembership answers label membership once per label in a held
// operation; it is what labelDeltaPlanV2 sees.
type heldMembership struct {
	ProviderV2
	m    labelMembershipV2
	seen map[string]membership
}

func (h heldMembership) LabelCatalog(ctx context.Context, team string, o linearprovider.PageOptions) (linearprovider.Page[linearprovider.LabelName], error) {
	return h.m.LabelCatalog(ctx, team, o)
}

func (h heldMembership) IssueHasLabel(ctx context.Context, issue, team, project, label string) (bool, bool, error) {
	key := issue + "|" + label
	if r, ok := h.seen[key]; ok {
		return r.found, r.complete, nil
	}
	found, complete, e := h.m.IssueHasLabel(ctx, issue, team, project, label)
	if e == nil {
		h.seen[key] = membership{found, complete}
	}
	return found, complete, e
}

func (s *Service) planProvider(p ProviderV2) ProviderV2 {
	if m, ok := p.(labelMembershipV2); ok && s.held != nil {
		return heldMembership{ProviderV2: p, m: m, seen: s.held.members}
	}
	return p
}
