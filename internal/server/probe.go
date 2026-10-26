package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"clawbridge/internal/linearapp"
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
		return errors.New("ALERT: Linear reports a project update was created for a project that should not exist; tell the coordinator now")
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
