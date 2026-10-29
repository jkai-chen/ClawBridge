package server

import (
	"context"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// projectTeamsQuery is a Linear project's teams, as the server's app reads
// them (the read #287's project status set trusts).
const projectTeamsQuery = `query ProjectTeams($id: String!) { project(id: $id) { id teams(first: 50) { nodes { id } pageInfo { hasNextPage } } } }`

// planReserveRequest is a project plan reserve as the bus takes it: the
// project's teams a domain coordinator is bound by (project_teams,
// project_teams_more) are only ever the server's own read. A client's are
// dropped; on a session's confirm the server reads them through its app.
// Without an app, or when the read fails, none are set, and the bus refuses
// a domain coordinator's confirm.
func planReserveRequest(ctx context.Context, writer *AppWriter, r bus.Request) bus.Request {
	if r.Operation != "project_plan_reserve" {
		return r
	}
	args := map[string]any{}
	for k, v := range r.Args {
		if k != "project_teams" && k != "project_teams_more" {
			args[k] = v
		}
	}
	r.Args = args
	confirm, _ := args["confirm"].(bool)
	lp, _ := args["linear_project"].(string)
	if !confirm || r.Session == "" || writer == nil || (writer.App == nil && writer.PlanTeams == nil) || !linearprovider.ValidID(lp) {
		return r
	}
	read := writer.projectTeams
	if writer.PlanTeams != nil {
		read = writer.PlanTeams
	}
	teams, more, err := read(ctx, strings.ToLower(lp))
	if err != nil {
		return r
	}
	args["project_teams"], args["project_teams_more"] = teams, more
	return r
}

// projectTeams reads a Linear project's teams through the app. A list
// Linear leaves out, or one with a next page, is partial (more).
func (w *AppWriter) projectTeams(ctx context.Context, lp string) ([]string, bool, error) {
	var out struct {
		Project *struct {
			ID    string `json:"id"`
			Teams *struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool `json:"hasNextPage"`
				} `json:"pageInfo"`
			} `json:"teams"`
		} `json:"project"`
	}
	if err := w.App.GraphQL(ctx, projectTeamsQuery, map[string]any{"id": lp}, &out); err != nil {
		return nil, false, err
	}
	if out.Project == nil || !strings.EqualFold(out.Project.ID, lp) || out.Project.Teams == nil {
		return []string{}, true, nil
	}
	teams := []string{}
	for _, n := range out.Project.Teams.Nodes {
		if linearprovider.ValidID(n.ID) {
			teams = append(teams, strings.ToLower(n.ID))
		}
	}
	return teams, out.Project.Teams.PageInfo.HasNextPage, nil
}
