package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"clawbridge/internal/bus"
)

// The server drain (live/ZERO-LOSS-DEPLOY-DESIGN.md §2): while the
// database's maintenance row is active, this process answers every request
// but a pure read with 503 maintenance, closes its streams, and its workers
// start nothing new. It counts what is in flight and reports itself drained
// when its flag is set and nothing is.

const (
	// maintenancePoll is how often the process re-reads the row and reports
	// itself; a notify on bus.MaintenanceChannel re-reads at once.
	maintenancePoll = 5 * time.Second
	// maintenanceCache is how stale a request's view of the row may be.
	maintenanceCache = time.Second
	// resumeGrace keeps the workers paused and the model API busy after a
	// maintenance ends, so every live session beats (every 30 s at most)
	// before anything judges its presence (E2). It is the lease TTL.
	resumeGrace = 90 * time.Second
	// maxRetryAfter caps the Retry-After a draining server answers.
	maxRetryAfter = 30
)

// drainStore is the hosted store's drain API.
type drainStore interface {
	MaintenanceState(ctx context.Context) (bus.Maintenance, error)
	ReportServerInstance(ctx context.Context, instance string, drainedFor time.Time) error
}

// Drain is one serving process's view of maintenance and its in-flight
// count.
type Drain struct {
	store    drainStore
	instance string
	now      func() time.Time

	mu sync.Mutex
	// state is the last row read, at readAt (this process's clock); a zero
	// readAt is stale.
	state   bus.Maintenance
	readAt  time.Time
	reading bool
	// failing is a read that failed; it is logged once until one works.
	failing bool
	// resumeAt is when the workers and the model API may start again after
	// a maintenance ended.
	resumeAt time.Time
	// active is state.Active, for the hot paths that never read.
	active atomic.Bool

	inflight atomic.Int64
	kick     chan struct{}
	// onStart and onEnd run when this process sees a maintenance begin or
	// end.
	onStart func(retry int)
	onEnd   func()
}

// NewDrain is the drain of the serving process named instance.
func NewDrain(store drainStore, instance string, now func() time.Time) *Drain {
	if now == nil {
		now = time.Now
	}
	return &Drain{store: store, instance: instance, now: now, kick: make(chan struct{}, 1)}
}

// newInstanceName names a serving process for the drain's reports.
func newInstanceName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "clawbridge-server-" + hex.EncodeToString(b[:]), nil
}

// Begin counts one request or worker item in flight until its done runs.
// A request is counted when it starts, before authentication and before
// the maintenance check (D5), so one that read "not in maintenance" just
// before a start still holds the drain until it ends.
func (d *Drain) Begin() (done func()) {
	d.inflight.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			if d.inflight.Add(-1) == 0 && d.active.Load() {
				d.poke()
			}
		})
	}
}

func (d *Drain) poke() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Invalidate is a notify on bus.MaintenanceChannel: the next request reads
// the row, and the loop re-reads it now.
func (d *Drain) Invalidate() {
	d.mu.Lock()
	d.readAt = time.Time{}
	d.mu.Unlock()
	d.poke()
}

// Quiet is a maintenance this process has seen: a pure read writes nothing
// after it commits (bus.Store.SetQuiet).
func (d *Drain) Quiet() bool { return d.active.Load() }

// Paused is a maintenance, or the grace after one: the workers start
// nothing and the model API is busy. It answers, when paused, how long
// until a retry may work.
func (d *Drain) Paused() (int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.Active {
		return d.retryLocked(), true
	}
	if wait := d.resumeAt.Sub(d.now()); wait > 0 {
		return retrySeconds(wait), true
	}
	return 0, false
}

// StartWork counts a worker item in flight unless the drain is paused.
func (d *Drain) StartWork() (done func(), ok bool) {
	done = d.Begin()
	if _, paused := d.Paused(); paused {
		done()
		return nil, false
	}
	return done, true
}

// startWork is StartWork for a process that may have no drain: without
// one, work always starts.
func (d *Drain) startWork() (func(), bool) {
	if d == nil {
		return func() {}, true
	}
	return d.StartWork()
}

// clearAtStart is a serving process's start: it ends a maintenance that
// began before it, since this binary is the swap's new one, and then waits
// the resume grace. A start that cleared nothing (a crash restart) never
// waits, nor does one that couldn't read the row.
func clearAtStart(ctx context.Context, store interface {
	ClearMaintenanceAtStart(context.Context) (bool, error)
}, d *Drain) {
	cleared, err := store.ClearMaintenanceAtStart(ctx)
	if err != nil {
		log.Print("drain: cannot clear an older maintenance at start")
		return
	}
	if cleared {
		log.Printf("drain: ended the maintenance that began before this start; workers resume in %s", resumeGrace)
		d.ResumeAfterGrace()
	}
}

// ResumeAfterGrace is a serving process's start that ended a maintenance:
// its workers wait the grace too. A start that cleared nothing never waits.
func (d *Drain) ResumeAfterGrace() {
	d.mu.Lock()
	d.resumeAt = d.now().Add(resumeGrace)
	d.mu.Unlock()
}

// Check is a request's view of maintenance: whether it is on, and the
// Retry-After to answer. It reads the row when the view is older than
// maintenanceCache; a failed read keeps the last view, so an unreadable
// row never shuts the server.
func (d *Drain) Check(ctx context.Context) (int, bool) {
	d.mu.Lock()
	stale := !d.reading && d.now().Sub(d.readAt) >= maintenanceCache
	if stale {
		d.reading = true
	}
	d.mu.Unlock()
	if stale {
		read, cancel := context.WithTimeout(ctx, 2*time.Second)
		d.refresh(read)
		cancel()
		d.mu.Lock()
		d.reading = false
		d.mu.Unlock()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.state.Active {
		return 0, false
	}
	return d.retryLocked(), true
}

// retryLocked is the Retry-After for the maintenance as last read:
// min(30, what is left), at least 1.
func (d *Drain) retryLocked() int {
	return retrySeconds(d.state.Remaining() - d.now().Sub(d.readAt))
}

func retrySeconds(left time.Duration) int {
	return max(1, min(maxRetryAfter, int(math.Ceil(left.Seconds()))))
}

// refresh reads the row and applies a start or an end.
func (d *Drain) refresh(ctx context.Context) {
	m, err := d.store.MaintenanceState(ctx)
	if err != nil {
		// The last view stands for another maintenanceCache, so an outage
		// costs one read a second, not one a request.
		d.mu.Lock()
		d.readAt = d.now()
		first := !d.failing
		d.failing = true
		d.mu.Unlock()
		if first && ctx.Err() == nil {
			log.Print("drain: maintenance state unreadable; keeping the last one")
		}
		return
	}
	d.mu.Lock()
	d.failing = false
	was := d.state.Active
	d.state, d.readAt = m, d.now()
	if was && !m.Active {
		d.resumeAt = d.now().Add(resumeGrace)
	}
	retry := d.retryLocked()
	d.active.Store(m.Active)
	d.mu.Unlock()
	switch {
	case m.Active && !was:
		log.Printf("drain: maintenance until %s; refusing writes", m.EndsAt.UTC().Format(time.RFC3339))
		if d.onStart != nil {
			d.onStart(retry)
		}
	case was && !m.Active:
		log.Printf("drain: maintenance over; workers resume in %s", resumeGrace)
		if d.onEnd != nil {
			d.onEnd()
		}
	}
}

// report tells the database this process is serving and, with its flag
// set and nothing in flight, that it has drained for this maintenance.
func (d *Drain) report(ctx context.Context) {
	d.mu.Lock()
	var drainedFor time.Time
	if d.state.Active && d.inflight.Load() == 0 {
		drainedFor = d.state.StartedAt
	}
	d.mu.Unlock()
	if err := d.store.ReportServerInstance(ctx, d.instance, drainedFor); err != nil && ctx.Err() == nil {
		log.Print("drain: instance report failed; retrying at the next poll")
	}
}

// Run re-reads the row and reports this process every maintenancePoll,
// and at once on a notify or when the last in-flight item ends.
func (d *Drain) Run(ctx context.Context) {
	tick := time.NewTicker(maintenancePoll)
	defer tick.Stop()
	for {
		step, cancel := context.WithTimeout(ctx, maintenancePoll)
		d.refresh(step)
		d.report(step)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-d.kick:
		}
	}
}

// failMaintenance answers a request a draining server refuses.
func failMaintenance(w http.ResponseWriter, retry int) {
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	fail(w, 503, "maintenance", "the server is in maintenance; retry after "+strconv.Itoa(retry)+" s")
}

// failIntakeMaintenance answers /mcp/intake in JSON-RPC, so the Linear
// agent that called the tool sees why; the watcher's intake cursors pick
// up whatever it asked for after the drain.
func failIntakeMaintenance(w http.ResponseWriter, retry int) {
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(503)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32000, "message": "the server is in maintenance; retry after " + strconv.Itoa(retry) + " s"}})
}

// failModelBusy answers the model API while paused, in its own shape: the
// agent reads retry_after from the body.
func failModelBusy(w http.ResponseWriter, retry int) {
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(503)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "server_busy", "retry_after": retry})
}
