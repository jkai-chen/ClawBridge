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
// (live/STATUS-FLOW-DESIGN.md §8.3). A model case that ended without a
// decision asks for a fallback nudge instead, which also runs the
// dispatcher so its rules decide (d5, D5-M3); no other nudge does
// (R106-M1). It is a marker pass, or a marker and dispatcher pass, through
// the ticks' single flight, under the project's lease and budget. At most one
// pass a minute per project: signals in between collapse into one, which
// runs when the minute is up. A Linear webhook's nudge (FR14) asks for a
// webhook pass instead, the comment watch and intake of the project whose
// cursors a delivery marked due, and coalesces the same way (Q10). So does
// an intake source's own due time inside the clock's interval, which the
// nudger reads from the bus after each tick and pass (FR41): a webhook
// pass at that time, never the dispatcher or markers. A new claim's
// proposal asks for a fallback pass, so the dispatcher decides it within
// the minute. It runs only beside the in-process clock, never in a request.

// nudgeEvery is the least time between two nudged passes of one project.
const nudgeEvery = time.Minute

// nudgeRetry is how soon a nudge that found a tick running tries again.
const nudgeRetry = 5 * time.Second

// markerPasser applies one project's due markers, and for a fallback nudge
// also runs its dispatcher; Webhook is a webhook nudge's pass.
type markerPasser interface {
	Markers(ctx context.Context, pid string) any
	Fallback(ctx context.Context, pid string) any
	Webhook(ctx context.Context, pid string) error
}

// nudgeKinds are the passes a pending project asks for.
type nudgeKinds uint8

const (
	// nudgeMarkers is a marker pass.
	nudgeMarkers nudgeKinds = 1 << iota
	// nudgeFallback is markers, then the dispatcher.
	nudgeFallback
	// nudgeWebhook is the comment watch and intake (FR14).
	nudgeWebhook
)

type nudger struct {
	flight  *singleFlight
	markers markerPasser
	// project resolves a request's workspace and project to the bound
	// project's ID, or "".
	project func(ctx context.Context, workspace, project string) (string, error)
	now     func() time.Time
	// dueAt reads each project's earliest intake due time inside the
	// clock's interval from the bus; nil asks for nothing.
	dueAt func(ctx context.Context) (map[string]time.Time, error)

	mu   sync.Mutex
	last map[string]time.Time
	// due is dueAt's last answer: when each project's intake next wants a
	// webhook pass.
	due map[string]time.Time
	// pending is each project waiting for a pass, and the kinds it asks
	// for.
	pending map[string]nudgeKinds
	asked   []target
	wake    chan struct{}
}

type target struct {
	workspace, project string
	kind               nudgeKinds
}

func newNudger(flight *singleFlight, markers markerPasser, project func(context.Context, string, string) (string, error)) *nudger {
	return &nudger{flight: flight, markers: markers, project: project, now: time.Now, last: map[string]time.Time{}, due: map[string]time.Time{}, pending: map[string]nudgeKinds{}, wake: make(chan struct{}, 1)}
}

// Nudge asks for a marker pass of the request's project. It never blocks.
func (n *nudger) Nudge(workspace, project string) { n.askRequest(workspace, project, nudgeMarkers) }

// NudgeDispatch asks for a fallback pass, markers then dispatcher, of the
// request's project: a new claim's proposal, which the dispatcher decides
// within the minute instead of at the next tick. It never blocks.
func (n *nudger) NudgeDispatch(workspace, project string) {
	n.askRequest(workspace, project, nudgeFallback)
}

func (n *nudger) askRequest(workspace, project string, kind nudgeKinds) {
	n.mu.Lock()
	if len(n.asked) < 100 {
		n.asked = append(n.asked, target{workspace, project, kind})
	}
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// NudgeProject asks for a fallback pass of a project by its ID, markers
// then dispatcher: the dispatcher-model API's, after a model case ended
// without a decision. It never blocks.
func (n *nudger) NudgeProject(pid string) { n.ask(pid, nudgeFallback) }

// NudgeWebhook asks for a webhook pass of a project by its ID: a verified
// Linear delivery marked its cursors due. Many in a minute make one pass
// (Q10). It never blocks.
func (n *nudger) NudgeWebhook(pid string) { n.ask(pid, nudgeWebhook) }

func (n *nudger) ask(pid string, kind nudgeKinds) {
	n.mu.Lock()
	n.pending[pid] |= kind
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// markersPending adds pid for a marker pass, keeping a fallback already
// asked for: a marker signal never runs the dispatcher, and never cancels
// a fallback (R106-M1). Called with n.mu held.
func (n *nudger) markersPending(pid string) {
	n.pending[pid] |= nudgeMarkers
}

// refreshDue replaces the due times with the bus's current answer. A read
// that fails keeps the old ones; the clock still reads every due source.
func (n *nudger) refreshDue(ctx context.Context) {
	if n.dueAt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	due, err := n.dueAt(ctx)
	if err != nil {
		log.Printf("watcher: nudge: intake due times unavailable")
		return
	}
	n.mu.Lock()
	n.due = due
	n.mu.Unlock()
}

// nextDue is the earliest due time of any project, or the zero time.
func (n *nudger) nextDue() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	var first time.Time
	for _, t := range n.due {
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first
}

// Ticked marks pending each project whose tick's marker pass stopped with
// rows left, so a nudge pass with its own deadline drains them without
// waiting for a confirm or a release. It reads the intake due times (at most
// 10 seconds) and never blocks on the flight.
func (n *nudger) Ticked(got any) {
	ticks, isTicks := got.([]watcher.ProjectTick)
	// The tick may have moved sources' due times, and the wait of the
	// nudger's timer follows them.
	n.refreshDue(context.Background())
	failed := false
	for _, t := range ticks {
		failed = failed || t.Error != ""
	}
	if isTicks && !failed {
		// One line per tick, so an idle watcher reads as healthy.
		next := "none"
		if due := n.nextDue(); !due.IsZero() {
			next = due.UTC().Format("15:04:05Z")
		}
		log.Printf("watcher: tick: %d project(s) ok, next intake due %s", len(ticks), next)
	}
	left := false
	n.mu.Lock()
	for _, t := range ticks {
		if t.MarkersPending() {
			n.markersPending(t.ProjectID)
			left = true
		}
	}
	hasDue := len(n.due) > 0
	n.mu.Unlock()
	if left || hasDue {
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
			n.pending[pid] |= t.kind
			n.mu.Unlock()
		}
	}
	wait := time.Hour
	for {
		pid, due, kinds := n.next()
		if pid == "" {
			if due > 0 && due < wait {
				wait = due
			}
			return wait
		}
		ran, more := false, false
		ok := n.flight.claim()
		if ok {
			// The pass takes the fallback and the webhook: one asked for
			// again while it runs sets it anew.
			n.mu.Lock()
			n.pending[pid] &^= nudgeFallback | nudgeWebhook
			n.mu.Unlock()
			func() {
				defer n.flight.release()
				pass, cancel := context.WithTimeout(ctx, tickDeadline)
				defer cancel()
				// The webhook's comment watch and intake first, then the
				// markers asked for, in the same pass and minute.
				if kinds&nudgeWebhook != 0 {
					_ = n.markers.Webhook(pass, pid)
				}
				var out any
				switch {
				case kinds&nudgeFallback != 0:
					out = n.markers.Fallback(pass, pid)
				case kinds&nudgeMarkers != 0:
					out = n.markers.Markers(pass, pid)
				}
				ran = true
				left, _ := out.(interface{ MarkersPending() bool })
				more = left != nil && left.MarkersPending()
			}()
		}
		n.mu.Lock()
		if ran {
			// A pass that stopped with rows left (its row limit or its
			// clock) comes again in a minute, in its own budget, until
			// the queue drains: as a marker pass, with a fallback or a
			// webhook asked for while this one ran. A pass that ran
			// markers and drained them clears the marker ask; a webhook
			// pass alone leaves one made meanwhile.
			switch {
			case more:
				n.pending[pid] |= nudgeMarkers
			case kinds&(nudgeMarkers|nudgeFallback) != 0:
				n.pending[pid] &^= nudgeMarkers
			}
			if n.pending[pid] == 0 {
				delete(n.pending, pid)
			}
			n.last[pid] = n.now()
		}
		n.mu.Unlock()
		if ran {
			// The pass read the sources that were due and set their next
			// times.
			n.refreshDue(ctx)
		}
		if !ran {
			// A tick holds the flight: try again shortly, never alongside it.
			return nudgeRetry
		}
	}
}

// next is a pending project whose minute is up, and the kinds it asks
// for, or, with none, how long until the soonest pending one is, or the
// soonest intake due time. A due time that has come asks for a webhook
// pass only, and is spent: the pass's refresh brings the sources' next.
func (n *nudger) next() (string, time.Duration, nudgeKinds) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	soonest := time.Duration(0)
	for pid, t := range n.due {
		if left := t.Sub(now); left <= 0 {
			n.pending[pid] |= nudgeWebhook
			delete(n.due, pid)
		} else if soonest == 0 || left < soonest {
			soonest = left
		}
	}
	for pid, kinds := range n.pending {
		left := nudgeEvery - now.Sub(n.last[pid])
		if left <= 0 {
			return pid, 0, kinds
		}
		if soonest == 0 || left < soonest {
			soonest = left
		}
	}
	return "", soonest, 0
}
