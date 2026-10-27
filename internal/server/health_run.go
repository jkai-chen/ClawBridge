package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/watcher"
)

// healthRunStore is what `operator health-run` asks of the bus; *bus.Store
// implements it.
type healthRunStore interface {
	linearapp.Store
	OperatorHealthProject(ctx context.Context, linearProject string) (string, error)
	OperatorForcedHealthPost(ctx context.Context, linearProject, updateID string) (bus.HealthPostRef, error)
	OperatorForcedHealthPostDeleted(ctx context.Context, post bus.HealthPostRef) error
}

// healthForcer makes one forced health run; *watcher.Watcher is one.
type healthForcer interface {
	ForceHealth(ctx context.Context, pid, lp string, forceNew bool) (watcher.HealthRun, error)
}

// newHealthForcer builds the server's watcher over the operator's store and
// the app; tests replace it. Like newProbeApp, it never mints: it uses the
// token the server published.
var newHealthForcer = func(store *bus.Store) (healthForcer, error) {
	config, set, err := appConfig(store)
	if err != nil {
		return nil, err
	}
	if !set {
		return nil, errors.New("the Linear app is not configured on this server")
	}
	config.NoMint = true
	app, err := linearapp.New(config)
	if err != nil {
		return nil, err
	}
	// A forced run spends the same hour's budget as the server's ticks.
	budget, err := readWatchBudget(os.Getenv)
	if err != nil {
		return nil, err
	}
	return &watcher.Watcher{Store: store, App: watcherApp{app}, Ledger: linearboard.SharedLedger(store, nil), Budget: budget}, nil
}

// healthRun forces one health run of the Linear project now, through the
// scheduled run's facts, report and post (watcher.ForceHealth), and prints
// what it did: the health, whether it acted, the post's state and update
// ID. A run that failed in Linear is printed and returned as an error.
func healthRun(ctx context.Context, store healthRunStore, forcer healthForcer, project string, forceNew bool, stdout io.Writer) error {
	pid, err := store.OperatorHealthProject(ctx, project)
	if err != nil {
		return err
	}
	run, err := forcer.ForceHealth(ctx, pid, strings.ToLower(project), forceNew)
	if err != nil {
		var coded interface{ Code() string }
		if msg, ok := bus.PublicMessage(err); ok && errors.As(err, &coded) {
			return errors.New("health-run: " + coded.Code() + ": " + msg)
		}
		return errors.New("health-run: " + probeClass(err))
	}
	if err = json.NewEncoder(stdout).Encode(map[string]any{"health_run": run}); err != nil {
		return err
	}
	if run.Error != "" {
		return errors.New("health-run: the run failed (" + run.Error + "); nothing was posted")
	}
	if run.Posted == "pending" {
		return errors.New("health-run: the post's answer was unreadable; it stays pending until a later run finds it, so check the project's updates before deleting")
	}
	return nil
}

// healthPostApp is what --delete asks of the app: its MCP status-update
// tools, and a GraphQL read by ID. appP8 implements it.
type healthPostApp interface {
	StatusUpdater(ctx context.Context) (statusUpdater, error)
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
}

// newHealthPostApp builds the app identity for --delete; tests replace it.
// Like newProbeApp, it never mints.
var newHealthPostApp = func(store linearapp.Store) (healthPostApp, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	a, ok := app.(*linearapp.App)
	if !ok {
		return nil, errors.New("the Linear app cannot delete status updates")
	}
	return appP8{a}, nil
}

// healthUpdateQuery reads one project update by ID: no node means it is
// deleted (or archived), whatever the project's other updates.
const healthUpdateQuery = `query HealthPostByID($id: ID!) { projectUpdates(filter: { id: { eq: $id } }) { nodes { id } } }`

// healthUpdateGone reads update id by ID and reports whether Linear no
// longer has it. Any answer but none or exactly that update is an error.
func healthUpdateGone(ctx context.Context, app healthPostApp, id string) (bool, error) {
	var out struct {
		ProjectUpdates *struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"projectUpdates"`
	}
	if err := app.GraphQL(ctx, healthUpdateQuery, map[string]any{"id": id}, &out); err != nil {
		return false, err
	}
	switch {
	case out.ProjectUpdates == nil:
		return false, errors.New("invalid_response")
	case len(out.ProjectUpdates.Nodes) == 0:
		return true, nil
	case len(out.ProjectUpdates.Nodes) == 1 && strings.EqualFold(out.ProjectUpdates.Nodes[0].ID, id):
		return false, nil
	}
	return false, errors.New("invalid_response")
}

// healthRunDelete deletes from Linear the project update updateID, only
// when a forced health run made it on project, then reads that update by
// ID. It records the delete only when the delete succeeded and the read
// finds no such update; anything else is an ALERT, and nothing is
// recorded (R91-H1).
func healthRunDelete(ctx context.Context, store healthRunStore, project, updateID string, stdout io.Writer) error {
	post, err := store.OperatorForcedHealthPost(ctx, project, updateID)
	if err != nil {
		return err
	}
	app, err := newHealthPostApp(store)
	if err != nil {
		return err
	}
	u, err := app.StatusUpdater(ctx)
	if err != nil {
		return probeFailure("health_run_connect", err)
	}
	defer u.Close()
	out := map[string]any{"update_id": post.UpdateID}
	emit := func(ret error) error {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"health_run_delete": out}); err != nil {
			return err
		}
		return ret
	}
	delErr := u.DeleteStatusUpdate(ctx, post.UpdateID)
	out["delete"] = probeClass(delErr)
	gone, readErr := healthUpdateGone(ctx, app, post.UpdateID)
	read := map[string]any{"class": probeClass(readErr)}
	if readErr == nil {
		read["gone"] = gone
	}
	out["read_after_delete"] = read
	if delErr != nil || readErr != nil || !gone {
		out["recorded"] = false
		return emit(errors.New("ALERT: project update " + post.UpdateID + " may still exist and its delete is not recorded: the owner checks it on Linear and deletes it"))
	}
	if err = store.OperatorForcedHealthPostDeleted(ctx, post); err != nil {
		out["recorded"] = false
		return emit(errors.New("health-run: the update is gone from Linear but its delete could not be recorded: " + err.Error()))
	}
	out["recorded"] = true
	return emit(nil)
}
