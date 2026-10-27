package linearprovider

import (
	"context"
	"strings"
)

// View is one shared custom view, as list_custom_views returns it. Linear's
// MCP gives no filter: a view's filter is read only with the person's API
// key (LINEAR-VIEWS-PLAN §12.1). The view's description, free text written
// by others, is never kept.
type View struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
	// Kind is "issue" or "project"; any other kind is left out of a page
	// and counted.
	Kind string `json:"kind"`
	// TeamID is empty for a workspace view, which a page leaves out and
	// counts.
	TeamID   string `json:"team_id"`
	TeamKey  string `json:"team_key,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

// ViewKinds are the kinds a view list may ask for.
var ViewKinds = map[string]string{"issue": "Issue", "project": "Project"}

// ViewPage is one page of a team's shared views. Rows of another kind and
// workspace views are left out, only counted.
type ViewPage struct {
	Page[View]
	OtherKinds     int `json:"other_kinds,omitempty"`
	WorkspaceViews int `json:"workspace_views,omitempty"`
}

func (p *Provider) viewsSupported() bool {
	t := p.tools["list_custom_views"]
	if t == nil {
		return false
	}
	props, req, e := schema(t)
	if e != nil || len(req) != 0 {
		return false
	}
	for _, f := range []struct{ name, kind string }{{"team", "string"}, {"shared", "boolean"}, {"type", "string"}, {"limit", "integer"}, {"cursor", "string"}} {
		if k, ok := field(props, []string{f.name}, f.kind); !ok || k != f.name {
			return false
		}
	}
	return true
}

// Views lists one page of a team's shared views, of one kind or both.
// Personal views are never asked for. A row that isn't shared, or names
// another team, refuses the page: the provider ignored a filter.
func (p *Provider) Views(ctx context.Context, team, kind string, o PageOptions) (ViewPage, error) {
	if !ValidID(team) || (kind != "" && ViewKinds[kind] == "") || o.Limit < 0 || o.Limit > 100 || len(o.Cursor) > 2048 {
		return ViewPage{}, fail("invalid")
	}
	if !p.viewsSupported() {
		return ViewPage{}, fail("capability_unavailable")
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
	args := []arg{a(team, "team"), {[]string{"shared"}, true, "boolean"}, {[]string{"limit"}, o.Limit, "integer"}}
	if kind != "" {
		args = append(args, a(ViewKinds[kind], "type"))
	}
	if o.Cursor != "" {
		args = append(args, a(o.Cursor, "cursor"))
	}
	m, e := p.call(ctx, []string{"list_custom_views"}, args...)
	if e != nil {
		return ViewPage{}, e
	}
	rows, next, complete, e := list(m, "customViews")
	if e != nil {
		return ViewPage{}, e
	}
	if len(rows) > o.Limit || (!complete && next == "") || (complete && next != "") {
		return ViewPage{}, fail("invalid_response")
	}
	out := ViewPage{Page: Page[View]{Items: []View{}, NextCursor: next, Complete: complete}}
	seen := map[string]bool{}
	for _, r := range rows {
		shared, ok := r["shared"].(bool)
		if !ok || !shared {
			return ViewPage{}, fail("invalid_response")
		}
		v := View{ID: str(r, "id"), Name: str(r, "name"), URL: str(r, "url")}
		if !ValidID(v.ID) || v.Name == "" || seen[v.ID] {
			return ViewPage{}, fail("invalid_response")
		}
		seen[v.ID] = true
		switch t := r["team"].(type) {
		case nil:
			out.WorkspaceViews++
			continue
		case map[string]any:
			v.TeamID, v.TeamKey, v.TeamName = str(t, "id"), str(t, "key"), str(t, "name")
		default:
			return ViewPage{}, fail("invalid_response")
		}
		if v.TeamID != team {
			return ViewPage{}, fail("invalid_response")
		}
		model, _ := r["modelName"].(string)
		switch strings.ToLower(model) {
		case "issue", "project":
			v.Kind = strings.ToLower(model)
		default:
			out.OtherKinds++
			continue
		}
		if kind != "" && v.Kind != kind {
			return ViewPage{}, fail("invalid_response")
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}
