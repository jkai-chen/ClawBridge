package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/watcher"
)

// orderApp is what the health-order probe asks of the app; *linearapp.App
// implements it.
type orderApp interface {
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
}

// newOrderApp builds the app identity for the health-order probe; tests
// replace it. Like newProbeApp, it never mints.
var newOrderApp = func(store linearapp.Store) (orderApp, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	a, ok := app.(*linearapp.App)
	if !ok {
		return nil, errors.New("the Linear app cannot read projects")
	}
	return a, nil
}

var projectUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// orderedUpdate is one project update as the order probe prints it: its ID
// and creation time, never its body or author.
type orderedUpdate struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

// linearAppHealthOrder settles whether the health judgement's read holds a
// project's newest update (R75-M1), the one the judgement acts on. It sends
// that read unchanged and the same read with last: in place of first:, two
// app reads and no write, and prints each answer's IDs and creation times in
// the order Linear gave them, the order of each, how many distinct updates
// the two saw, and agree: the read holds the newest, on a project with more
// updates than one read returns (conclusive).
func linearAppHealthOrder(ctx context.Context, store linearapp.Store, project string, stdout io.Writer) error {
	if !projectUUID.MatchString(project) {
		return errors.New("--health-order needs --project, a Linear project UUID")
	}
	last := strings.Replace(watcher.HealthProjectQuery, "projectUpdates(first: ", "projectUpdates(last: ", 1)
	if last == watcher.HealthProjectQuery {
		return errors.New("linear-app-probe health_order: the health read no longer pages projectUpdates with first:")
	}
	app, err := newOrderApp(store)
	if err != nil {
		return err
	}
	read := func(step, query string) ([]orderedUpdate, error) {
		var raw struct {
			Project *struct {
				ProjectUpdates struct {
					Nodes []struct {
						ID        string `json:"id"`
						CreatedAt string `json:"createdAt"`
					} `json:"nodes"`
				} `json:"projectUpdates"`
			} `json:"project"`
		}
		if err := app.GraphQL(ctx, query, map[string]any{"id": project}, &raw); err != nil {
			return nil, probeFailure(step, err)
		}
		if raw.Project == nil {
			return nil, errors.New("linear-app-probe " + step + ": the app cannot see the project")
		}
		us := []orderedUpdate{}
		for _, n := range raw.Project.ProjectUpdates.Nodes {
			if _, err := time.Parse(time.RFC3339Nano, n.CreatedAt); err != nil {
				return nil, errors.New("linear-app-probe " + step + ": an update has no readable createdAt")
			}
			us = append(us, orderedUpdate{n.ID, n.CreatedAt})
		}
		return us, nil
	}
	first, err := read("health_order_first", watcher.HealthProjectQuery)
	if err != nil {
		return err
	}
	tail, err := read("health_order_last", last)
	if err != nil {
		return err
	}
	newest, seen := "", map[string]bool{}
	for _, u := range append(append([]orderedUpdate{}, first...), tail...) {
		seen[u.ID] = true
		if newest == "" || later(u.CreatedAt, newest) {
			newest = u.CreatedAt
		}
	}
	// The judgement uses the newest update of its read (bus newestUpdate,
	// by createdAt), so the read must hold it, in whatever order. With no
	// more updates than one read returns, both reads hold them all and
	// prove nothing.
	hasNewest := len(first) == 0 && len(tail) == 0
	for _, u := range first {
		hasNewest = hasNewest || !later(newest, u.CreatedAt)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"health_order": map[string]any{
		"first":             first,
		"first_order":       updateOrder(first),
		"last":              tail,
		"last_order":        updateOrder(tail),
		"distinct":          len(seen),
		"conclusive":        len(seen) > bus.HealthUpdatesRead,
		"window_has_newest": hasNewest,
		"agree":             len(seen) > bus.HealthUpdatesRead && hasNewest,
	}})
}

// later reports whether a is after b; both parsed by the caller already.
func later(a, b string) bool {
	ta, _ := time.Parse(time.RFC3339Nano, a)
	tb, _ := time.Parse(time.RFC3339Nano, b)
	return ta.After(tb)
}

// updateOrder names the order of updates as Linear returned them.
func updateOrder(us []orderedUpdate) string {
	if len(us) < 2 {
		return "single"
	}
	down, up := true, true
	for i := 1; i < len(us); i++ {
		if later(us[i].CreatedAt, us[i-1].CreatedAt) {
			down = false
		}
		if later(us[i-1].CreatedAt, us[i].CreatedAt) {
			up = false
		}
	}
	switch {
	case down && !up:
		return "newest_first"
	case up && !down:
		return "oldest_first"
	case up && down:
		return "same_time"
	}
	return "mixed"
}
