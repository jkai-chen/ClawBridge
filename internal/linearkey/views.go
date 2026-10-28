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
	// Filter is filterData as Linear holds it, re-encoded with sorted keys
	// and its numbers kept exactly, for comparing with what was sent.
	Filter string
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
	FilterData json.RawMessage `json:"filterData"`
}

func (n *viewNode) view() (View, error) {
	if n == nil || !viewID.MatchString(n.ID) {
		return View{}, ErrUnavailable
	}
	v := View{ID: n.ID, Name: n.Name, Shared: n.Shared, UpdatedAt: n.UpdatedAt}
	if n.Team != nil {
		v.TeamID = n.Team.ID
	}
	filter, err := Canonical(n.FilterData)
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
func (c *Client) CreateView(ctx context.Context, team, name string, filter json.RawMessage) (View, error) {
	if !viewID.MatchString(team) {
		return View{}, errNoTeam
	}
	input := map[string]any{"name": name, "teamId": team, "shared": true, "filterData": filter}
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
func (c *Client) UpdateView(ctx context.Context, id, name string, filter json.RawMessage) (View, error) {
	if !viewID.MatchString(id) {
		return View{}, ErrViewNotFound
	}
	var out struct {
		Result *struct {
			Success    bool      `json:"success"`
			CustomView *viewNode `json:"customView"`
		} `json:"customViewUpdate"`
	}
	input := map[string]any{"name": name, "filterData": filter}
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
