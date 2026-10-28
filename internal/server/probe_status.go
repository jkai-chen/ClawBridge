package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// The A4 probe (live/PROJECT-FLOW-A4-PROBE.md): the server's app moves the
// operator's throwaway project to started, to completed and back, each move
// sent as project flow sends it (save_project over the app's MCP) and read
// back by status ID. Three accepted moves prove the app's write path.

// probeProjectEnv names the one project the probe may move (P1).
const probeProjectEnv = "CLAWBRIDGE_PROBE_PROJECT"

// statusProbeQuery reads what the guards and the moves need: never a name.
const statusProbeQuery = `query StatusProbe($id: String!) { project(id: $id) { id archivedAt trashed teams(first: 10) { nodes { id } } status { id type } } projectStatuses(first: 50) { nodes { id type archivedAt } pageInfo { hasNextPage } } }`

// statusSetter is the app's MCP connection as the probe uses it;
// *linearprovider.Provider implements it.
type statusSetter interface {
	SetProjectStatus(ctx context.Context, project, status string) error
	Close() error
}

// statusProbeApp is what the probe asks of the app.
type statusProbeApp interface {
	ProbeP8Project(ctx context.Context, project string) (linearapp.P8Project, error)
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
	HasScope(ctx context.Context, scope string) (bool, error)
	StatusSetter(ctx context.Context) (statusSetter, error)
}

type appStatusProbe struct{ *linearapp.App }

func (a appStatusProbe) StatusSetter(ctx context.Context) (statusSetter, error) {
	return a.Provider(ctx, false)
}

// newStatusProbeApp builds the app identity for the probe; tests replace
// it. Like newProbeApp, it never mints.
var newStatusProbeApp = func(store linearapp.Store) (statusProbeApp, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	a, ok := app.(*linearapp.App)
	if !ok {
		return nil, errors.New("the Linear app cannot set a project's status")
	}
	return appStatusProbe{a}, nil
}

// statusProbeStore is the server's store as the probe reads it.
type statusProbeStore interface {
	linearapp.Store
	OperatorProbeBindings(ctx context.Context, linearProject string) (bus.ProbeBindings, error)
}

// probeProjectFacts is one read of the project and the status list.
type probeProjectFacts struct {
	found, archived, trashed bool
	teams                    []string
	status                   ProjectStatus
	statuses                 projectStatusFacts
}

func readProbeProject(ctx context.Context, app statusProbeApp, project string) (probeProjectFacts, error) {
	type status struct {
		ID         string  `json:"id"`
		Type       string  `json:"type"`
		ArchivedAt *string `json:"archivedAt"`
	}
	var out struct {
		Project *struct {
			ID         string  `json:"id"`
			ArchivedAt *string `json:"archivedAt"`
			Trashed    *bool   `json:"trashed"`
			Teams      struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"teams"`
			Status *status `json:"status"`
		} `json:"project"`
		ProjectStatuses *struct {
			Nodes    []status `json:"nodes"`
			PageInfo struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"projectStatuses"`
	}
	if err := app.GraphQL(ctx, statusProbeQuery, map[string]any{"id": project}, &out); err != nil {
		return probeProjectFacts{}, err
	}
	f := probeProjectFacts{}
	if out.Project == nil || !strings.EqualFold(out.Project.ID, project) {
		return f, nil
	}
	if out.Project.Status == nil || out.ProjectStatuses == nil {
		return f, errors.New("unreadable")
	}
	f.found = true
	f.archived = out.Project.ArchivedAt != nil
	f.trashed = out.Project.Trashed != nil && *out.Project.Trashed
	for _, t := range out.Project.Teams.Nodes {
		f.teams = append(f.teams, strings.ToLower(t.ID))
	}
	slices.Sort(f.teams)
	f.status = ProjectStatus{ID: strings.ToLower(out.Project.Status.ID), Type: out.Project.Status.Type}
	f.statuses.More = out.ProjectStatuses.PageInfo.HasNextPage
	for _, n := range out.ProjectStatuses.Nodes {
		if n.ArchivedAt == nil {
			f.statuses.Statuses = append(f.statuses.Statuses, ProjectStatus{ID: strings.ToLower(n.ID), Type: n.Type})
		}
	}
	return f, nil
}

// statusProbeMove is one planned move.
type statusProbeMove struct {
	Step   string `json:"step"`
	ToType string `json:"to_type"`
	ToID   string `json:"to_id"`
}

// linearAppStatusProbe runs the A4 probe on project. It refuses, sending
// nothing, unless project is the one CLAWBRIDGE_PROBE_PROJECT names; it
// exists, unarchived and not trashed, with no issue, member or lead; it is
// in no binding of this server and on a team one of them binds; and the app
// holds the write scope. With dryRun it stops there. Each move is sent once
// and read back by status ID; a move not accepted stops the probe, after
// one read of where the project is. It prints IDs, types and classes only.
func linearAppStatusProbe(ctx context.Context, store statusProbeStore, project, allowMember string, dryRun bool, stdout io.Writer) error {
	project = strings.ToLower(project)
	out := map[string]any{"project": project}
	emit := func(ret error) error {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"project_status_probe": out}); err != nil {
			return err
		}
		return ret
	}
	refuse := func(why string) error {
		return emit(errors.New("linear-app-probe project-status: refused: " + why + "; nothing was sent"))
	}
	if !linearprovider.ValidID(project) {
		return errors.New("--project-status needs a Linear project UUID")
	}
	if strings.ToLower(os.Getenv(probeProjectEnv)) != project {
		return refuse(probeProjectEnv + " must name this project")
	}
	bindings, err := store.OperatorProbeBindings(ctx, project)
	if err != nil {
		return err
	}
	if bindings.ProjectBound {
		return refuse("a task board binding of this server names the project")
	}
	app, err := newStatusProbeApp(store)
	if err != nil {
		return err
	}
	pre, err := app.ProbeP8Project(ctx, project)
	if err != nil {
		return probeFailure("project_status_project", err)
	}
	pre.Allow(allowMember)
	out["shape"] = pre
	if !pre.Empty() {
		return refuse("the project must exist with no issues, members or lead")
	}
	f, err := readProbeProject(ctx, app, project)
	if err != nil {
		return probeFailure("project_status_read", err)
	}
	if !f.found {
		return refuse("the project can't be read")
	}
	out["team_ids"], out["archived"], out["trashed"] = f.teams, f.archived, f.trashed
	if f.archived || f.trashed {
		return refuse("the project is archived or trashed")
	}
	// P2: a pass proves the app's access on a team a binding uses.
	shared := []string{}
	for _, t := range f.teams {
		if slices.Contains(bindings.Teams, t) {
			shared = append(shared, t)
		}
	}
	out["bound_team_ids"] = shared
	if len(shared) == 0 {
		return refuse("the project is on no team a binding of this server uses")
	}
	// P3: the original status, to put back by ID if the last move fails.
	out["original"] = f.status
	moves := []statusProbeMove{}
	for _, typ := range []string{"started", "completed"} {
		target, err := projectStatusTarget(f.statuses, typ, "")
		if err != nil {
			out["plan_error"] = typ
			return refuse("no single unarchived " + typ + " status")
		}
		moves = append(moves, statusProbeMove{"a", typ, target.ID})
	}
	moves[1].Step = "b"
	moves = append(moves, statusProbeMove{"c", f.status.Type, f.status.ID})
	out["plan"] = moves
	granted, err := app.HasScope(ctx, "write")
	if err != nil {
		return probeFailure("project_status_scope", err)
	}
	if !granted {
		return refuse("the app doesn't hold the write scope")
	}
	if dryRun {
		out["dry_run"] = true
		return emit(nil)
	}
	setter, err := app.StatusSetter(ctx)
	if err != nil {
		return probeFailure("project_status_connect", err)
	}
	defer setter.Close()
	results := []map[string]any{}
	for _, m := range moves {
		r := map[string]any{"step": m.Step, "to_type": m.ToType}
		results = append(results, r)
		out["moves"] = results
		outcome, code := "uncertain", ""
		if err := setter.SetProjectStatus(ctx, project, m.ToID); err != nil {
			outcome, code = linearboard.StatusUpdateOutcome("", err)
		}
		if outcome == "uncertain" {
			outcome, code = probeReadBack(ctx, app, project, m.ToID, code)
		}
		r["outcome"] = outcome
		if code != "" {
			r["code"] = code
		}
		if outcome != "accepted" {
			now, err := readProbeProject(ctx, app, project)
			if err != nil || !now.found {
				out["status_now"] = "unreadable"
			} else {
				out["status_now"] = now.status
			}
			out["result"] = "FAIL"
			return emit(errors.New("linear-app-probe project-status: step " + m.Step + " was " + outcome + "; stopped without retrying: put the project back to the original status ID if it moved"))
		}
	}
	out["result"] = "PASS"
	return emit(nil)
}

// probeReadBack reads the project's status by ID after a send, as
// appProjectStatus.set does: accepted only when it shows the target.
func probeReadBack(ctx context.Context, app statusProbeApp, project, to, code string) (string, string) {
	var read struct {
		Project *struct {
			Status *struct {
				ID string `json:"id"`
			} `json:"status"`
		} `json:"project"`
	}
	switch e := app.GraphQL(ctx, projectStatusReadBackQuery, map[string]any{"id": project}, &read); {
	case e != nil || read.Project == nil || read.Project.Status == nil:
		return "uncertain", "read_back_failed"
	case strings.EqualFold(read.Project.Status.ID, to):
		return "accepted", ""
	case code == "":
		code = "read_back_mismatch"
	}
	return "uncertain", code
}
