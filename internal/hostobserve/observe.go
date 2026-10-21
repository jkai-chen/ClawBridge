// Package hostobserve reads metadata from an explicitly selected existing host.
// It never starts a thread, requests a model turn or proves process exit.
package hostobserve

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"

	"clawbridge/internal/notify"
)

type Binding struct{ Adapter, Endpoint, Thread string }
type Observation struct {
	Adapter     string   `json:"adapter"`
	ThreadID    string   `json:"thread_id"`
	State       string   `json:"state"`
	ActiveFlags []string `json:"active_flags"`
	ObservedAt  string   `json:"observed_at"`
	QueueState  string   `json:"queue_state"`
}
type Result struct {
	Outcome         string       `json:"outcome"`
	LastObservation *Observation `json:"last_observation,omitempty"`
}
type Observer interface {
	Observe(context.Context) (Observation, error)
	Close() error
}
type DialFunc func(context.Context, Binding) (Observer, error)
type Config struct {
	Binding           Binding
	For               string // empty: one status observation; idle or unloaded: level-triggered wait.
	Timeout, Interval time.Duration
	Dial              DialFunc
	Now               func() time.Time
	Wait              func(context.Context, time.Duration) error
}
type observationError struct{ kind string }

func (e *observationError) Error() string { return "host observation " + e.kind }
func Unsupported() error                  { return &observationError{"unsupported"} }
func Unavailable() error                  { return &observationError{"unavailable"} }
func NormalizeBinding(b Binding) (Binding, error) {
	if b.Adapter != "codex" || !notify.ValidThread(b.Thread) {
		return b, errors.New("host observation requires codex adapter and canonical thread UUID")
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(b.Endpoint, "#") || u.Opaque != "" || !path.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) {
		return b, errors.New("host endpoint requires an absolute Unix socket URL without authority, query or fragment")
	}
	b.Endpoint = (&url.URL{Scheme: "unix", Path: path.Clean(u.Path)}).String()
	return b, nil
}
func (c Config) Validate() error {
	if _, err := NormalizeBinding(c.Binding); err != nil {
		return err
	}
	if c.For != "" && c.For != "idle" && c.For != "unloaded" {
		return errors.New("host wait supports idle or unloaded; process exit cannot be observed")
	}
	if c.Timeout < time.Second || c.Timeout > time.Hour {
		return errors.New("host timeout must be 1–3600 seconds")
	}
	if c.Interval < 250*time.Millisecond || c.Interval > 30*time.Second {
		return errors.New("host interval must be 250ms–30s")
	}
	return nil
}

// Run makes one bounded connection, never reconnects, and returns exactly one
// terminal result. The caller writes the source envelope and maps outcome to exit.
func Run(ctx context.Context, c Config) (Result, error) {
	var result Result
	if err := c.Validate(); err != nil {
		return result, err
	}
	c.Binding, _ = NormalizeBinding(c.Binding)
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Wait == nil {
		c.Wait = notify.Wait
	}
	if c.Dial == nil {
		c.Dial = Open
	}
	overall, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	finish := func(err error) Result {
		switch {
		case errors.Is(overall.Err(), context.DeadlineExceeded):
			result.Outcome = "timeout"
		case ctx.Err() != nil:
			result.Outcome = "cancelled"
		default:
			var e *observationError
			if errors.As(err, &e) {
				result.Outcome = e.kind
			} else {
				result.Outcome = "unavailable"
			}
		}
		return result
	}
	if overall.Err() != nil {
		return finish(overall.Err()), nil
	}
	bounded, stop := context.WithTimeout(overall, 5*time.Second)
	observer, err := c.Dial(bounded, c.Binding)
	stop()
	if err != nil {
		return finish(err), nil
	}
	defer observer.Close()
	for {
		bounded, stop = context.WithTimeout(overall, 5*time.Second)
		observation, err := observer.Observe(bounded)
		stop()
		if err != nil {
			return finish(err), nil
		}
		if err = validateObservation(observation, c.Binding); err != nil {
			return finish(err), nil
		}
		if overall.Err() != nil {
			return finish(overall.Err()), nil
		}
		observation.ObservedAt = c.Now().UTC().Format(time.RFC3339Nano)
		result.LastObservation = &observation
		if observation.State == "systemError" {
			return finish(Unavailable()), nil
		}
		if c.For == "" || (c.For == "idle" && observation.State == "idle") || (c.For == "unloaded" && observation.State == "notLoaded") {
			result.Outcome = "matched"
			return result, nil
		}
		if err = c.Wait(overall, c.Interval); err != nil {
			return finish(err), nil
		}
	}
}
func validateObservation(o Observation, b Binding) error {
	if o.Adapter != "codex" || o.ThreadID != b.Thread || o.QueueState != "unknown" || o.ActiveFlags == nil {
		return Unsupported()
	}
	switch o.State {
	case "active":
	case "idle", "notLoaded", "systemError":
		if len(o.ActiveFlags) != 0 {
			return Unsupported()
		}
	default:
		return Unsupported()
	}
	seen := map[string]bool{}
	for _, f := range o.ActiveFlags {
		if (f != "waitingOnApproval" && f != "waitingOnUserInput") || seen[f] {
			return Unsupported()
		}
		seen[f] = true
	}
	return nil
}
