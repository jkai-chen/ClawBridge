package notify

import (
	"context"
	"time"

	"clawbridge/internal/bus"
)

// HeartbeatInterval is how often a process holding a session renews its
// liveness lease. The lease lasts 90 s, so two renewals in a row may fail.
const HeartbeatInterval = 30 * time.Second

// KeepAlive renews the liveness lease of the session request names, every
// interval, until ctx ends. request is read at each beat, so a process whose
// instance changes renews the new one; ok false skips the beat. A failed
// renewal (an older server, a retired session, a network error) waits for
// the next beat.
func KeepAlive(ctx context.Context, e Executor, interval time.Duration, request func() (bus.Request, bool)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r, ok := request()
		if !ok || ctx.Err() != nil {
			continue
		}
		r.Operation = "heartbeat"
		bounded, cancel := context.WithTimeout(ctx, interval/2)
		_, _ = e.Execute(bounded, r)
		cancel()
	}
}
