package linearapp

import (
	"context"
	"sync"
	"time"

	"clawbridge/internal/linearprovider"
)

// SessionIdle is how long the server keeps an MCP session as the app open
// after a write, for the next write to reuse: opening one costs four
// Linear calls (the handshake and tools/list).
const SessionIdle = 120 * time.Second

// maxIdleSessions bounds the sessions kept open between writes. Writes run
// one at a time per session, so concurrent writes open more; the extra
// ones are closed when they are returned.
const maxIdleSessions = 4

// Sessions keeps the app's MCP write sessions open for SessionIdle between
// writes. A session is kept per token identity: the app, its Linear
// organisation and the token itself, so a re-minted token never reuses a
// session the old one opened. A write takes a session for itself (a Lease)
// and gives it back when it ends; one that failed, or saw any call on it
// fail, closes it instead. Sessions is safe for concurrent use.
type Sessions struct {
	app  *App
	idle time.Duration
	now  func() time.Time
	// open opens a new write session; after schedules the idle reaper.
	// Tests replace both.
	open  func(ctx context.Context) (*linearprovider.Provider, error)
	after func(d time.Duration, f func()) (stop func() bool)

	mu     sync.Mutex
	free   []*session
	stop   func() bool
	closed bool
}

type session struct {
	p    *linearprovider.Provider
	key  string
	last time.Time
}

// WriteSessions is the app's one pool of kept write sessions.
func (a *App) WriteSessions() *Sessions {
	a.sessionsOnce.Do(func() {
		a.sessions = &Sessions{
			app:  a,
			idle: SessionIdle,
			now:  a.cfg.Now,
			open: func(ctx context.Context) (*linearprovider.Provider, error) { return a.Provider(ctx, false) },
			after: func(d time.Duration, f func()) func() bool {
				return time.AfterFunc(d, f).Stop
			},
		}
	})
	return a.sessions
}

// Close closes every kept session and keeps none from now on: the
// server's shutdown.
func (s *Sessions) Close() {
	s.mu.Lock()
	s.closed = true
	closing := s.free
	s.free = nil
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	s.mu.Unlock()
	closeAll(closing)
}

// Reap closes the kept sessions idle for SessionIdle or longer.
func (s *Sessions) Reap() {
	s.mu.Lock()
	closing := s.expire("")
	s.mu.Unlock()
	closeAll(closing)
}

// expire takes out of the pool the sessions idle too long and, with a key,
// those of any other key. s.mu is held.
func (s *Sessions) expire(key string) []*session {
	now := s.now()
	var keep, closing []*session
	for _, c := range s.free {
		if now.Sub(c.last) >= s.idle || now.Before(c.last) || key != "" && c.key != key {
			closing = append(closing, c)
		} else {
			keep = append(keep, c)
		}
	}
	s.free = keep
	return closing
}

// reaper runs after the idle time, closes what expired, and runs again
// while sessions are kept.
func (s *Sessions) reaper() {
	s.Reap()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = nil
	s.schedule()
}

// schedule starts the reaper when sessions are kept and none is pending.
// s.mu is held.
func (s *Sessions) schedule() {
	if s.closed || s.stop != nil || len(s.free) == 0 {
		return
	}
	next := s.idle
	now := s.now()
	for _, c := range s.free {
		if d := c.last.Add(s.idle).Sub(now); d < next {
			next = d
		}
	}
	s.stop = s.after(max(next, time.Second), s.reaper)
}

func closeAll(sessions []*session) {
	for _, c := range sessions {
		c.p.Close()
	}
}

// Lease is one write's hold on the pool. Every session it hands out is the
// write's alone until Release.
type Lease struct {
	s    *Sessions
	mu   sync.Mutex
	held []*session
	// spare are this write's sessions the board has finished with; a
	// later open in the same write takes one again.
	spare []*session
}

// Lease starts one write's use of the pool.
func (s *Sessions) Lease() *Lease { return &Lease{s: s} }

// Leased is a write's session: closing it hands it back to the write's
// Lease, which keeps or closes it at Release.
type Leased struct {
	*linearprovider.Provider
	done func()
}

func (l *Leased) Close() error {
	l.done()
	return nil
}

// Provider gives the write a session as the app: one it already used, one
// an earlier write kept for the same token, or a new one. A kept session
// that fails its first call unsent is reopened once (Reused). A read-only
// session is never kept: it opens afresh and closes as before.
func (l *Lease) Provider(ctx context.Context, readonly bool) (*Leased, error) {
	if readonly {
		p, err := l.s.app.Provider(ctx, true)
		if err != nil {
			return nil, err
		}
		var once sync.Once
		return &Leased{Provider: p, done: func() { once.Do(func() { p.Close() }) }}, nil
	}
	l.mu.Lock()
	if n := len(l.spare); n > 0 {
		c := l.spare[n-1]
		l.spare = l.spare[:n-1]
		l.mu.Unlock()
		return l.leased(c), nil
	}
	l.mu.Unlock()
	if _, _, err := l.s.app.Token(ctx); err != nil {
		return nil, err
	}
	key := l.s.app.TokenKey()
	s := l.s
	s.mu.Lock()
	closing := s.expire(key)
	var c *session
	if n := len(s.free); n > 0 {
		c = s.free[n-1]
		s.free = s.free[:n-1]
	}
	s.mu.Unlock()
	closeAll(closing)
	if c != nil {
		c.p.Reused(func(ctx context.Context) (*linearprovider.Provider, error) {
			p, err := s.open(ctx)
			c.key = s.app.TokenKey()
			return p, err
		})
	} else {
		p, err := s.open(ctx)
		if err != nil {
			return nil, err
		}
		c = &session{p: p, key: key}
	}
	l.mu.Lock()
	l.held = append(l.held, c)
	l.mu.Unlock()
	return l.leased(c), nil
}

func (l *Lease) leased(c *session) *Leased {
	var once sync.Once
	return &Leased{Provider: c.p, done: func() {
		once.Do(func() {
			l.mu.Lock()
			l.spare = append(l.spare, c)
			l.mu.Unlock()
		})
	}}
}

// Release ends the write. With ok, every session it took that the board
// finished with, saw no failed call, and still matches the token in use
// is kept for the next write; any other is closed: a failed write, a call
// that failed or missed a capability, a 401 that re-minted the token.
func (l *Lease) Release(ok bool) {
	l.mu.Lock()
	held, spare := l.held, map[*session]bool{}
	for _, c := range l.spare {
		spare[c] = true
	}
	l.held, l.spare = nil, nil
	l.mu.Unlock()
	key := l.s.app.TokenKey()
	s := l.s
	var closing []*session
	s.mu.Lock()
	now := s.now()
	for _, c := range held {
		c.p.Reused(nil)
		if !ok || !spare[c] || c.p.Broken() || key == "" || c.key != key || s.closed || len(s.free) >= maxIdleSessions {
			closing = append(closing, c)
			continue
		}
		c.last = now
		s.free = append(s.free, c)
	}
	s.schedule()
	s.mu.Unlock()
	closeAll(closing)
}
