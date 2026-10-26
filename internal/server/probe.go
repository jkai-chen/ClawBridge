package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// probeApp is what linear-app-probe asks of the app; *linearapp.App
// implements it.
type probeApp interface {
	Tools(ctx context.Context) (map[string][]string, error)
	ProbeProjectShape(ctx context.Context, project string) (linearapp.ProjectShape, error)
	ProbeProjectUpdateScope(ctx context.Context) (linearapp.ScopeProbe, error)
}

// newProbeApp builds the app identity for linear-app-probe; tests replace
// it. It never mints: the probe uses the token the server published.
var newProbeApp = func(store linearapp.Store) (probeApp, error) {
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
	return app, nil
}

// linearAppProbe answers, as the server's own Linear app: which MCP tools it
// is offered, what one project's health inputs look like, and whether its
// scopes allow a project update, by sending one for a project that does not
// exist. It prints classes, names and value kinds only: never a token,
// secret, ID, message or field value.
func linearAppProbe(ctx context.Context, store linearapp.Store, project string, stdout io.Writer) error {
	app, err := newProbeApp(store)
	if err != nil {
		return err
	}
	out := map[string]any{}
	tools, err := app.Tools(ctx)
	if err != nil {
		return probeFailure("tools", err)
	}
	names := []string{}
	updates := map[string][]string{}
	for name, fields := range tools {
		names = append(names, name)
		if n := strings.ToLower(name); strings.Contains(n, "project_update") || strings.Contains(n, "status_update") || strings.Contains(n, "projectupdate") {
			updates[name] = fields
		}
	}
	sort.Strings(names)
	out["tools"] = map[string]any{"names": names, "project_update_tools": updates}
	if project != "" {
		shape, err := app.ProbeProjectShape(ctx, project)
		if err != nil {
			return probeFailure("project", err)
		}
		out["project"] = shape
	}
	result, err := app.ProbeProjectUpdateScope(ctx)
	if err != nil {
		return probeFailure("project_update", err)
	}
	out["project_update_create"] = result
	if err = json.NewEncoder(stdout).Encode(out); err != nil {
		return err
	}
	if result.Class == "created" {
		id := result.CreatedID
		if id == "" {
			id = "(no ID returned)"
		}
		return errors.New("ALERT: Linear reports project update " + id + " was created for a project that should not exist; tell the coordinator now, and have the owner delete it")
	}
	return nil
}

// probeFailure names the step and keeps only the app error's code.
func probeFailure(step string, err error) error {
	var coded *linearapp.Error
	if errors.As(err, &coded) {
		return errors.New("linear-app-probe " + step + ": " + coded.Code)
	}
	return errors.New("linear-app-probe " + step + ": failed")
}

// docProbeApp is what linear-app-doc-probe asks of the app.
type docProbeApp interface {
	ProbeDocuments(ctx context.Context) (linearapp.DocumentsProbe, error)
}

// newDocProbeApp builds the app identity for linear-app-doc-probe; tests
// replace it. Like newProbeApp, it never mints.
var newDocProbeApp = func(store linearapp.Store) (docProbeApp, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	d, ok := app.(docProbeApp)
	if !ok {
		return nil, errors.New("the Linear app cannot list documents")
	}
	return d, nil
}

// linearAppDocProbe answers whether the server's Linear app may list the
// workspace's documents (LINEAR-DOCS §4): one read, printed as counts,
// parent kinds and order only, never an ID, title or time.
func linearAppDocProbe(ctx context.Context, store linearapp.Store, stdout io.Writer) error {
	app, err := newDocProbeApp(store)
	if err != nil {
		return err
	}
	result, err := app.ProbeDocuments(ctx)
	if err != nil {
		return probeFailure("documents", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"list_documents": result})
}

// P8 (live/PROJECT-HEALTH-DESIGN.md §8, owner's decision 26 Oct): one real
// project status update as the app, on an empty throwaway project, read
// back, deleted and read again, to settle whether the app may post health.
const (
	p8Body   = "ClawBridge probe, will be deleted"
	p8Marker = "ClawBridge probe"
	p8Health = "onTrack"
)

// statusUpdater is the app's MCP connection as P8 uses it;
// *linearprovider.Provider implements it.
type statusUpdater interface {
	StatusUpdateSchemas() map[string]any
	StatusUpdatePreflight(health string) error
	SaveStatusUpdate(ctx context.Context, project, body, health string) (string, error)
	StatusUpdates(ctx context.Context, project, marker string) ([]linearprovider.StatusUpdateRef, error)
	DeleteStatusUpdate(ctx context.Context, id string) error
	Close() error
}

// p8App is what P8 asks of the app.
type p8App interface {
	ProbeP8Project(ctx context.Context, project string) (linearapp.P8Project, error)
	StatusUpdater(ctx context.Context) (statusUpdater, error)
}

type appP8 struct{ *linearapp.App }

func (a appP8) StatusUpdater(ctx context.Context) (statusUpdater, error) {
	return a.Provider(ctx, false)
}

// newP8App builds the app identity for P8; tests replace it. Like
// newProbeApp, it never mints.
var newP8App = func(store linearapp.Store) (p8App, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	a, ok := app.(*linearapp.App)
	if !ok {
		return nil, errors.New("the Linear app cannot post status updates")
	}
	return appP8{a}, nil
}

// probeClass is an error's class: its code, with Linear's own error code
// when the provider kept one. Never a message.
func probeClass(err error) string {
	if err == nil {
		return "ok"
	}
	var provider *linearprovider.Error
	if errors.As(err, &provider) {
		if provider.LinearCode != "" {
			return provider.Kind + ":" + provider.LinearCode
		}
		return provider.Kind
	}
	var app *linearapp.Error
	if errors.As(err, &app) {
		return app.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "failed"
}

// linearAppP8 runs P8 on project. It refuses, sending nothing, unless the
// project exists with no issue, member or lead, and unless the app's MCP
// tools can create, list and delete a status update. With dryRun it stops
// there. It prints classes, the schemas' field names and enums, and the
// update's ID only; an update it cannot prove deleted is an ALERT.
func linearAppP8(ctx context.Context, store linearapp.Store, project string, dryRun bool, stdout io.Writer) error {
	app, err := newP8App(store)
	if err != nil {
		return err
	}
	out := map[string]any{}
	emit := func(ret error) error {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"p8": out}); err != nil {
			return err
		}
		return ret
	}
	pre, err := app.ProbeP8Project(ctx, project)
	if err != nil {
		return probeFailure("p8_project", err)
	}
	out["project"] = pre
	if !pre.Empty() {
		return emit(errors.New("linear-app-probe p8: refused: the project must exist with no issues, members or lead; nothing was sent"))
	}
	u, err := app.StatusUpdater(ctx)
	if err != nil {
		return probeFailure("p8_connect", err)
	}
	defer u.Close()
	out["schemas"] = u.StatusUpdateSchemas()
	if err = u.StatusUpdatePreflight(p8Health); err != nil {
		out["preflight"] = probeClass(err)
		return emit(errors.New("linear-app-probe p8: refused: the app's tools cannot create, list and delete a project status update; nothing was sent"))
	}
	out["preflight"] = "ok"
	if dryRun {
		out["dry_run"] = true
		return emit(nil)
	}
	id, saveErr := u.SaveStatusUpdate(ctx, project, p8Body, p8Health)
	out["save"] = probeClass(saveErr)
	// Read back whatever the save answered: an update may exist even when
	// the answer was lost.
	list, readErr := u.StatusUpdates(ctx, project, p8Marker)
	read := map[string]any{"class": probeClass(readErr)}
	out["read"] = read
	if readErr == nil {
		marked := []linearprovider.StatusUpdateRef{}
		for _, r := range list {
			if r.Marked || r.ID == id {
				marked = append(marked, r)
			}
		}
		read["found"] = len(marked)
		switch {
		case id == "" && len(marked) == 1:
			id = marked[0].ID
			read["id_from"] = "read"
		case id == "" && len(marked) > 1:
			out["update_ids"] = refIDs(marked)
			return emit(errors.New("ALERT: several probe status updates are on the project; the owner deletes every one listed in update_ids"))
		}
		for _, r := range marked {
			if r.ID == id {
				read["health"] = r.Health
			}
		}
	}
	if id == "" {
		if readErr == nil {
			// Nothing was created: the save's class is the answer.
			return emit(nil)
		}
		return emit(errors.New("ALERT: the save's answer and the read-back were both unreadable; an update may exist on the project: the owner checks it and deletes any"))
	}
	out["update_id"] = id
	delErr := u.DeleteStatusUpdate(ctx, id)
	out["delete"] = probeClass(delErr)
	after, againErr := u.StatusUpdates(ctx, project, p8Marker)
	gone := map[string]any{"class": probeClass(againErr)}
	out["read_after_delete"] = gone
	if againErr == nil {
		present := false
		for _, r := range after {
			present = present || r.ID == id
		}
		gone["present"] = present
		if !present {
			return emit(nil)
		}
	}
	return emit(errors.New("ALERT: status update " + id + " may still exist: the owner deletes it"))
}

func refIDs(refs []linearprovider.StatusUpdateRef) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}
