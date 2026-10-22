package linearboard

import (
	"context"
	"sync"
	"time"
)

// ProviderTTL bounds how long a long-lived process (the MCP server) keeps one
// native connection, and so its tools/list, before reopening it.
const ProviderTTL = 5 * time.Minute

// providerPool keeps one open native connection per Service, and so per
// profile, inside one process. Nothing is shared across processes. A kept
// connection is reopened, with a fresh tools/list, when it is older than the
// TTL, when the profile's access changes, when a call on it failed or missed a
// capability, when its session no longer answers, or after any command error.
type providerPool struct {
	ttl     time.Duration
	mu      sync.Mutex
	current *pooledProvider
}
type pooledProvider struct {
	p        Provider
	readonly bool
	opened   time.Time
	users    int
	retired  bool
}

// KeepProviderOpen makes this Service reuse one native connection for up to
// ttl. Only a long-lived process should call it, before its first command.
func (s *Service) KeepProviderOpen(ttl time.Duration) {
	if ttl > 0 {
		s.pool = &providerPool{ttl: ttl}
	}
}

// Close releases a kept-open native connection.
func (s *Service) Close() error {
	s.pool.drop()
	return nil
}

// settle reopens the kept connection after any failed command, so an error
// never leaves a suspect session or tool list in use.
func (s *Service) settle(err error) {
	if err != nil {
		s.pool.drop()
	}
}

func (pool *providerPool) open(ctx context.Context, readonly bool, now time.Time, fresh func() (Provider, error)) (Provider, func(), error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if c := pool.current; c != nil {
		age := now.Sub(c.opened)
		if c.readonly == readonly && age >= 0 && age < pool.ttl && alive(ctx, c.p) {
			c.users++
			return c.p, pool.release(c), nil
		}
		pool.retire(c)
	}
	p, err := fresh()
	if err != nil {
		return p, nil, err
	}
	c := &pooledProvider{p: p, readonly: readonly, opened: now, users: 1}
	pool.current = c
	return p, pool.release(c), nil
}

func alive(ctx context.Context, p Provider) bool {
	if a, ok := p.(interface{ Alive(context.Context) bool }); ok {
		return a.Alive(ctx)
	}
	return true
}

// retire stops handing out c; it is closed once its last user releases it.
func (pool *providerPool) retire(c *pooledProvider) {
	c.retired = true
	if pool.current == c {
		pool.current = nil
	}
	if c.users == 0 {
		c.p.Close()
	}
}
func (pool *providerPool) release(c *pooledProvider) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			pool.mu.Lock()
			defer pool.mu.Unlock()
			c.users--
			if c.retired && c.users == 0 {
				c.p.Close()
			}
		})
	}
}
func (pool *providerPool) drop() {
	if pool == nil {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.current != nil {
		pool.retire(pool.current)
	}
}
