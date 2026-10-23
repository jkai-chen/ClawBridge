package linearboard

import (
	"context"
	"slices"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// BindingProofTTL bounds how long a long-lived process (the MCP server) reuses
// a read command's verified Linear binding.
const BindingProofTTL = 60 * time.Second

// bindingProofCache holds the last verified binding for read commands only.
// Writes and links never consult it and always verify against Linear. An entry
// is keyed by the profile and the full binding digest, so any binding change
// (revision, mode, teams, projects) misses, and any failed command drops it.
type bindingProofCache struct {
	ttl time.Duration
	mu  sync.Mutex
	key string
	at  time.Time
	v   verifiedV2
	ok  bool
}

// CacheReadBindingProof lets read commands of this Service reuse a verified
// binding for up to ttl. Only a long-lived process should call it, before its
// first command.
func (s *Service) CacheReadBindingProof(ttl time.Duration) {
	if ttl > 0 {
		s.proofs = &bindingProofCache{ttl: ttl}
	}
}

// verifyReadBindingV2 is verifyBindingV2 for read commands. Never call it from
// a write path.
func (s *Service) verifyReadBindingV2(ctx context.Context, p Provider, b bus.TaskboardBindingV2) (verifiedV2, error) {
	c := s.proofs
	if c == nil || s.held != nil {
		return s.proveBinding(ctx, p, b)
	}
	key := digest([]any{"binding-proof", s.cfg.ProfileID, b.WorkspaceID, b.ProjectID, b.Revision, digest(b)})
	now := s.cfg.Now()
	if v, ok := c.get(key, now); ok {
		return v, nil
	}
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		c.drop()
		return v, e
	}
	c.put(key, now, v)
	return cloneVerifiedV2(v), nil
}

func (c *bindingProofCache) get(key string, now time.Time) (verifiedV2, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := now.Sub(c.at)
	if !c.ok || c.key != key || age < 0 || age >= c.ttl {
		return verifiedV2{}, false
	}
	return cloneVerifiedV2(c.v), true
}
func (c *bindingProofCache) put(key string, now time.Time, v verifiedV2) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.at, c.v, c.ok = key, now, cloneVerifiedV2(v), true
}
func (c *bindingProofCache) drop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.at, c.v, c.ok = "", time.Time{}, verifiedV2{}, false
}

// cloneVerifiedV2 keeps callers from changing a cached proof.
func cloneVerifiedV2(v verifiedV2) verifiedV2 {
	out := verifiedV2{Pairs: slices.Clone(v.Pairs), Teams: slices.Clone(v.Teams), Projects: make([]linearprovider.Project, len(v.Projects))}
	for n, project := range v.Projects {
		project.TeamIDs = slices.Clone(project.TeamIDs)
		out.Projects[n] = project
	}
	if v.Projects == nil {
		out.Projects = nil
	}
	return out
}
