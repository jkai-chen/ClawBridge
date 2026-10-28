package linearkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

// View is a custom view as Linear reads it back.
type View struct {
	ID        string
	Name      string
	Shared    bool
	UpdatedAt string
	// TeamID is "" for a view with no team, which ClawBridge never writes.
	TeamID string
	// CreatorID and CreatorName say who made the view (shown before an
	// adoption replaces its filter). The name is text from Linear.
	CreatorID, CreatorName string
	// Kind is "issue" or "project", from Linear's modelName; "" for any
	// other kind.
	Kind string
	// Filter is the view's filter as Linear holds it (filterData for an
	// issue view, projectFilterData for a project view), re-encoded with
	// sorted keys and its numbers kept exactly, for comparing with what was
	// sent.
	Filter string
}

// filterField is the input and read-back field of a kind's filter.
func filterField(kind string) (string, error) {
	switch kind {
	case "issue":
		return "filterData", nil
	case "project":
		return "projectFilterData", nil
	}
	return "", errors.New("a view's kind is issue or project")
}

var viewID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// errNoTeam is a create without a team: never built, never sent (§14, S3).
var errNoTeam = errors.New("a view is created only on a named team: team must be a Linear team ID")

type viewNode struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Shared    bool   `json:"shared"`
	UpdatedAt string `json:"updatedAt"`
	Team      *struct {
		ID string `json:"id"`
	} `json:"team"`
	Creator *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"creator"`
	ModelName         string          `json:"modelName"`
	FilterData        json.RawMessage `json:"filterData"`
	ProjectFilterData json.RawMessage `json:"projectFilterData"`
}

func (n *viewNode) view() (View, error) {
	if n == nil || !viewID.MatchString(n.ID) {
		return View{}, ErrUnavailable
	}
	v := View{ID: n.ID, Name: n.Name, Shared: n.Shared, UpdatedAt: n.UpdatedAt}
	if n.Team != nil {
		v.TeamID = n.Team.ID
	}
	if n.Creator != nil {
		v.CreatorID, v.CreatorName = n.Creator.ID, n.Creator.Name
	}
	raw := n.FilterData
	switch n.ModelName {
	case "Issue", "":
		v.Kind = "issue"
	case "Project":
		v.Kind, raw = "project", n.ProjectFilterData
	}
	filter, err := Canonical(raw)
	if err != nil {
		return View{}, ErrUnavailable
	}
	v.Filter = filter
	return v, nil
}

// Canonical re-encodes a JSON value with sorted object keys and its
// numbers as written; "" for null or nothing.
func Canonical(raw []byte) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return "", errors.New("not one JSON value")
	}
	out, err := json.Marshal(v)
	return string(out), err
}

// viewErr turns Do's input error into the view's fixed refusal.
func viewErr(err error, input error) error {
	if errors.Is(err, errInput) {
		return input
	}
	return err
}

// CreateView creates a shared view on team, with the filter as filterData.
// team must be a team ID: without one nothing is built or sent.
func (c *Client) CreateView(ctx context.Context, team, name, kind string, filter json.RawMessage) (View, error) {
	if !viewID.MatchString(team) {
		return View{}, errNoTeam
	}
	field, err := filterField(kind)
	if err != nil {
		return View{}, err
	}
	input := map[string]any{"name": name, "teamId": team, "shared": true, field: filter}
	var out struct {
		Result *struct {
			Success    bool      `json:"success"`
			CustomView *viewNode `json:"customView"`
		} `json:"customViewCreate"`
	}
	if err := c.do(ctx, "viewCreate", map[string]any{"input": input}, &out); err != nil {
		return View{}, viewErr(err, ErrViewRefused)
	}
	if out.Result == nil || !out.Result.Success {
		return View{}, ErrUnavailable
	}
	return out.Result.CustomView.view()
}

// UpdateView renames the view and replaces its filter. It never names a
// team or the sharing: the view keeps both.
func (c *Client) UpdateView(ctx context.Context, id, name, kind string, filter json.RawMessage) (View, error) {
	if !viewID.MatchString(id) {
		return View{}, ErrViewNotFound
	}
	var out struct {
		Result *struct {
			Success    bool      `json:"success"`
			CustomView *viewNode `json:"customView"`
		} `json:"customViewUpdate"`
	}
	field, err := filterField(kind)
	if err != nil {
		return View{}, err
	}
	input := map[string]any{"name": name, field: filter}
	if err := c.do(ctx, "viewUpdate", map[string]any{"id": id, "input": input}, &out); err != nil {
		return View{}, viewErr(err, ErrViewRefused)
	}
	if out.Result == nil || !out.Result.Success {
		return View{}, ErrUnavailable
	}
	return out.Result.CustomView.view()
}

// View reads one view back by ID.
func (c *Client) View(ctx context.Context, id string) (View, error) {
	if !viewID.MatchString(id) {
		return View{}, ErrViewNotFound
	}
	var out struct {
		CustomView *viewNode `json:"customView"`
	}
	if err := c.do(ctx, "view", map[string]any{"id": id}, &out); err != nil {
		return View{}, viewErr(err, ErrViewNotFound)
	}
	if out.CustomView == nil {
		return View{}, ErrViewNotFound
	}
	return out.CustomView.view()
}

// DeleteView deletes a view. Linear answering it has no such view is
// ErrViewNotFound: it may already be gone.
func (c *Client) DeleteView(ctx context.Context, id string) error {
	if !viewID.MatchString(id) {
		return ErrViewNotFound
	}
	var out struct {
		Result *struct {
			Success bool `json:"success"`
		} `json:"customViewDelete"`
	}
	if err := c.do(ctx, "viewDelete", map[string]any{"id": id}, &out); err != nil {
		return viewErr(err, ErrViewNotFound)
	}
	if out.Result == nil || !out.Result.Success {
		return ErrUnavailable
	}
	return nil
}

// ProjectLabel is a project label as the key reads it: its team is "" for
// a workspace label.
type ProjectLabel struct {
	ID, Name, TeamID, ParentID string
	// ParentGroupType is the parent group's groupType: singleSelect or
	// multiSelect.
	ParentGroupType string
	Group           bool
	// Archived is true for an archived or a retired label.
	Archived      bool
	ParentIsGroup bool
}

// projectLabelPages bounds the read (D2): past it, the list is refused,
// never used in part.
const projectLabelPages = 4

// ErrTooManyLabels is a label list longer than ClawBridge reads.
var ErrTooManyLabels = errors.New("the workspace has more project labels than ClawBridge reads (1,000); nothing was sent")

// ProjectLabels reads every project label the key can see, workspace and
// team ones, archived ones included (the caller leaves those out).
func (c *Client) ProjectLabels(ctx context.Context) ([]ProjectLabel, error) {
	var out []ProjectLabel
	after := ""
	for page := 0; page < projectLabelPages; page++ {
		var res struct {
			Labels *struct {
				Nodes []struct {
					ID         string  `json:"id"`
					Name       string  `json:"name"`
					IsGroup    bool    `json:"isGroup"`
					ArchivedAt *string `json:"archivedAt"`
					RetiredAt  *string `json:"retiredAt"`
					Parent     *struct {
						ID        string `json:"id"`
						IsGroup   bool   `json:"isGroup"`
						GroupType string `json:"groupType"`
					} `json:"parent"`
					Team *struct {
						ID string `json:"id"`
					} `json:"team"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"projectLabels"`
		}
		vars := map[string]any{}
		if after != "" {
			vars["after"] = after
		}
		if err := c.do(ctx, "projectLabels", vars, &res); err != nil {
			return nil, viewErr(err, ErrUnavailable)
		}
		if res.Labels == nil {
			return nil, ErrUnavailable
		}
		for _, n := range res.Labels.Nodes {
			if !viewID.MatchString(n.ID) {
				return nil, ErrUnavailable
			}
			l := ProjectLabel{ID: n.ID, Name: n.Name, Group: n.IsGroup, Archived: n.ArchivedAt != nil || n.RetiredAt != nil}
			if n.Team != nil {
				l.TeamID = n.Team.ID
			}
			if n.Parent != nil {
				l.ParentID, l.ParentIsGroup, l.ParentGroupType = n.Parent.ID, n.Parent.IsGroup, n.Parent.GroupType
			}
			out = append(out, l)
		}
		if !res.Labels.PageInfo.HasNextPage {
			return out, nil
		}
		if res.Labels.PageInfo.EndCursor == "" || res.Labels.PageInfo.EndCursor == after {
			return nil, ErrUnavailable
		}
		after = res.Labels.PageInfo.EndCursor
	}
	return nil, ErrTooManyLabels
}
