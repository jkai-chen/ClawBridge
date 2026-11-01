package guibackend

import (
	"context"
	"sync"
	"time"

	"clawbridge/internal/doctor"
)

// DoctorFeed shares one doctor report between doctor.checks and Needs you.
// Doctor makes about a dozen bus calls and a Linear read on the person's
// token, so the 30 s needs cycle must not run it: it asks for the last
// report and gets a fresh one only when that is older than its limit.
type DoctorFeed struct {
	run func(context.Context) (doctor.Report, error)
	now func() time.Time

	mu     sync.Mutex
	report doctor.Report
	err    error
	at     time.Time
	have   bool
}

// NewDoctorFeed wraps run. now is the clock (time.Now when nil).
func NewDoctorFeed(run func(context.Context) (doctor.Report, error), now func() time.Time) *DoctorFeed {
	if now == nil {
		now = time.Now
	}
	return &DoctorFeed{run: run, now: now}
}

// Run runs doctor now and remembers the report: doctor.checks uses it, and
// Needs you reuses the result until it ages out.
func (d *DoctorFeed) Run(ctx context.Context) (doctor.Report, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runLocked(ctx)
}

func (d *DoctorFeed) runLocked(ctx context.Context) (doctor.Report, error) {
	d.report, d.err = d.run(ctx)
	d.at, d.have = d.now(), true
	return d.report, d.err
}

// Cached returns the last report when it is younger than maxAge, else runs
// doctor. A failed run is kept for maxAge too, so a broken doctor is not
// retried every tick.
func (d *DoctorFeed) Cached(maxAge time.Duration) func(context.Context) (doctor.Report, error) {
	return func(ctx context.Context) (doctor.Report, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.have && d.now().Sub(d.at) < maxAge {
			return d.report, d.err
		}
		return d.runLocked(ctx)
	}
}

// Needs-you refresh limits: the background cycle reuses a report for 10
// minutes; opening the page accepts one up to 30 s old.
const (
	DoctorBackgroundAge = 10 * time.Minute
	DoctorPageAge       = 30 * time.Second
)
