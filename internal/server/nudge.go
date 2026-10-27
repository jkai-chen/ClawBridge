package server

import (
	"context"
	"log"
	"sync"
	"time"

	"clawbridge/internal/watcher"
)

// A nudge applies one project's due claim markers soon after a confirm or
// a release commits, instead of at the next clock tick
// (live/STATUS-FLOW-DESIGN.md §8.3). It is a marker pass only, through the
// ticks' single flight, under the project's lease and budget. At most one
// pass a minute per project: signals in between collapse into one, which
// runs when the minute is up. It runs only beside the in-process clock,
// never in a request.

// nudgeEvery is the least time between two nudged passes of one project.
const nudgeEvery = time.Minute

// nudgeRetry is how soon a nudge that found a tick running tries again.
const nudgeRetry = 5 * time.Second

// markerPasser applies one project's due markers.
type markerPasser interface {
	Markers(ctx context.Context, pid string) any
}

type nudger struct {
	flight  *singleFlight
	markers markerPasser
	// project resolves a request's workspace and project to the bound
	// project's ID, or "".
	project func(ctx context.Context, workspace, project string) (string, error)
	now     func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time
	pending map[string]bool
	asked   []target
	wake    chan struct{}
}

type target struct{ workspace, project string }

func newNudger(flight *singleFlight, markers markerPasser, project func(context.Context, string, string) (string, error)) *nudger {
	return &nudger{flight: flight, markers: markers, project: project, now: time.Now, last: map[string]time.Time{}, pending: map[string]bool{}, wake: make(chan struct{}, 1)}
}

// Nudge asks for a marker pass of the request's project. It never blocks.
func (n *nudger) Nudge(workspace, project string) {
	n.mu.Lock()
	if len(n.asked) < 100 {
		n.asked = append(n.asked, target{workspace, project})
	}
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Ticked marks pending each project whose tick's marker pass stopped with
// rows left, so a nudge pass with its own deadline drains them without
// waiting for a confirm or a release. It never blocks.
func (n *nudger) Ticked(got any) {
	ticks, _ := got.([]watcher.ProjectTick)
	left := false
	n.mu.Lock()
	for _, t := range ticks {
		if t.MarkersPending() {
			n.pending[t.ProjectID], left = true, true
		}
	}
	n.mu.Unlock()
	if left {
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
}

// run serves nudges until ctx ends.
func (n *nudger) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		case <-timer.C:
		}
		wait := n.serve(ctx)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// serve resolves the asked projects, runs every pending project whose
// minute is up, and returns how long until the next is due.
func (n *nudger) serve(ctx context.Context) time.Duration {
	n.mu.Lock()
	asked := n.asked
	n.asked = nil
	n.mu.Unlock()
	for _, t := range asked {
		pid, err := n.project(ctx, t.workspace, t.project)
		if err != nil {
			log.Printf("watcher: nudge: project unresolved")
		}
		if pid != "" {
			n.mu.Lock()
			n.pending[pid] = true
			n.mu.Unlock()
		}
	}
	wait := time.Hour
	for {
		pid, due := n.next()
		if pid == "" {
			if due > 0 && due < wait {
				wait = due
			}
			return wait
		}
		ran, more := false, false
		ok := n.flight.claim()
		if ok {
			func() {
				defer n.flight.running.Store(false)
				pass, cancel := context.WithTimeout(ctx, tickDeadline)
				defer cancel()
				out := n.markers.Markers(pass, pid)
				ran = true
				left, _ := out.(interface{ MarkersPending() bool })
				more = left != nil && left.MarkersPending()
			}()
		}
		n.mu.Lock()
		if ran {
			// A pass that stopped with rows left (its row limit or its
			// clock) comes again in a minute, in its own budget, until
			// the queue drains.
			if !more {
				delete(n.pending, pid)
			}
			n.last[pid] = n.now()
		}
		n.mu.Unlock()
		if !ran {
			// A tick holds the flight: try again shortly, never alongside it.
			return nudgeRetry
		}
	}
}

// next is a pending project whose minute is up, or, with none, how long
// until the soonest pending one is.
func (n *nudger) next() (string, time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	soonest := time.Duration(0)
	for pid := range n.pending {
		left := nudgeEvery - now.Sub(n.last[pid])
		if left <= 0 {
			return pid, 0
		}
		if soonest == 0 || left < soonest {
			soonest = left
		}
	}
	return "", soonest
}
