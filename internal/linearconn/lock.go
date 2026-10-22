package linearconn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// refreshTimeout bounds one token refresh made while holding the disk lock;
// lockWait lets a waiter outlast a holder's whole refresh, with a margin.
const refreshTimeout = 20 * time.Second

var lockWait = refreshTimeout + 5*time.Second

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
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	path := filepath.Join(d, ".credentials.lock")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			// Name the holder, so a waiter that times out can say who it is.
			_, se := fmt.Fprintf(f, "pid=%d\n", os.Getpid())
			info, ie := f.Stat()
			if se == nil {
				se = ie
			}
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
			return nil, &Error{Kind: "profile busy", Detail: busyDetail(path, m.config.Now())}
		case <-timer.C:
		}
	}
}

// busyDetail names the process holding the credential lock and how long it
// has held it. A held lock is normally a refresh in progress; removing it is
// only for a holder that no longer runs.
func busyDetail(path string, now time.Time) string {
	holder := "another ClawBridge process"
	if raw, err := os.ReadFile(path); err == nil {
		if pid, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "pid="); ok {
			if _, err = strconv.Atoi(pid); err == nil {
				holder = "ClawBridge process " + pid
			}
		}
	}
	age := ""
	if info, err := os.Lstat(path); err == nil {
		age = fmt.Sprintf(" for %s", now.Sub(info.ModTime()).Round(time.Second))
	}
	return fmt.Sprintf("%s has held the credential lock%s; retry shortly. Remove %s only if that process is no longer running", holder, age, path)
}
