package linearconn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// lockContext preserves the shared index/refresh mutex without parking an
// uncancellable goroutine behind another request's network refresh.
func lockContext(ctx context.Context, mu *sync.Mutex) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			if err := ctx.Err(); err != nil {
				mu.Unlock()
				return err
			}
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// diskLock prevents cross-process credential rotation/profile update races.
// Locks are never stolen: an interrupted process requires explicit operator
// inspection/removal, rather than risking reuse of a consumed refresh token.
func (m *Manager) diskLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, e := m.privateDir()
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path := filepath.Join(d, ".credentials.lock")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			info, se := f.Stat()
			if se == nil {
				se = makePrivate(path)
			}
			if se == nil {
				se = privatePath(path, false)
			}
			if se == nil {
				se = ctx.Err()
			}
			_ = f.Close()
			if se != nil {
				if current, le := os.Lstat(path); le == nil && info != nil && os.SameFile(info, current) {
					_ = os.Remove(path)
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return nil, problem("profile lock failed")
			}
			return func() {
				current, e := os.Lstat(path)
				if e == nil && os.SameFile(info, current) {
					_ = os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(e, os.ErrExist) {
			return nil, problem("profile lock unavailable")
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, problem("profile busy; inspect retained lock before recovery")
		case <-timer.C:
		}
	}
}
