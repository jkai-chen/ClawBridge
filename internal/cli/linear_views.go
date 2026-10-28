package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearkey"
	"clawbridge/internal/linearprovider"
)

// viewShowPages bounds view show's search through each bound team's views.
const viewShowPages = 10

// filterNote is what view show says of the filter until the person's Linear
// API key arrives (LINEAR-VIEWS-PLAN §12.2).
const filterNote = "a view's filter isn't readable over Linear's MCP; ClawBridge keeps the filter only of the issue views it wrote (linear view create|update)"

// linearViews runs view list and view show. Names and descriptions are other
// people's text, so the answer is made terminal-safe.
func linearViews(ctx context.Context, service *linearboard.Service, scope bus.Request, v linearFlags, pos []string) (any, error) {
	if v.adminAction == "show" {
		found, err := findView(ctx, service, scope, v.team, pos[0])
		if err != nil {
			return nil, err
		}
		return safeData(map[string]any{"view": found, "filter": nil, "filter_note": filterNote})
	}
	team, err := viewTeam(ctx, service, scope, v.team)
	if err != nil {
		return nil, err
	}
	page, err := service.ViewsV2(ctx, scope, team, v.viewKind, v.limit, v.cursor)
	if err != nil {
		return nil, err
	}
	return safeData(page)
}

func viewTeam(ctx context.Context, service *linearboard.Service, scope bus.Request, team string) (string, error) {
	if team != "" {
		return team, nil
	}
	b, err := service.BindingV2(ctx, scope)
	if err != nil {
		return "", err
	}
	if b.LinearDefaultTeamID == "" {
		return "", errors.New("pass --team UUID: the binding has no default team")
	}
	return b.LinearDefaultTeamID, nil
}

// findView looks for a view by ID among one bound team's shared views, or
// every bound team's: Linear's MCP has no read of one view.
func findView(ctx context.Context, service *linearboard.Service, scope bus.Request, team, id string) (linearprovider.View, error) {
	teams := []string{team}
	if team == "" {
		b, err := service.BindingV2(ctx, scope)
		if err != nil {
			return linearprovider.View{}, err
		}
		teams = b.LinearTeamIDs
	}
	for _, t := range teams {
		cursor := ""
		for n := 0; n < viewShowPages; n++ {
			page, err := service.ViewsV2(ctx, scope, t, "", 100, cursor)
			if err != nil {
				return linearprovider.View{}, err
			}
			for _, view := range page.Items {
				if view.ID == id {
					return view, nil
				}
			}
			if page.Complete {
				break
			}
			cursor = page.NextCursor
		}
	}
	return linearprovider.View{}, errors.New("no shared view with that ID in the binding's teams")
}

// safeData decodes a value to plain JSON and strips what could drive a
// terminal from every string.
func safeData(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return terminalSafeAll(out), nil
}

// linearViewWrite runs view create and view update: a preview, then the
// write with --confirm, sent with the person's own Linear API key. The key
// is refused under an agent's environment before anything is read (K2).
func linearViewWrite(ctx context.Context, service *linearboard.Service, scope bus.Request, directory string, v linearFlags, pos []string) (any, error) {
	if err := linearkey.Allowed(); err != nil {
		return nil, err
	}
	in := linearboard.ViewWriteInput{Action: v.adminAction, Team: v.team, Name: v.viewName, OperationID: v.operation, Confirm: v.confirm, ExpectedRevision: v.revision, ExpectedFilterSHA: v.expectedSHA, ExpectedFilterAsOf: v.expectedAsOf}
	if v.adminAction == "update" {
		in.ViewID = pos[0]
	}
	if v.filterFile != "" {
		raw, err := readLinearFile(v.filterFile)
		if err != nil {
			return nil, err
		}
		in.Filter = []byte(raw)
	}
	var key linearboard.ViewKey
	if v.confirm {
		client, err := linearKeys(directory).Open(v.profile)
		if err != nil {
			return nil, err
		}
		key = viewKey{client}
	}
	result, err := service.ViewWrite(ctx, scope, in, key)
	if err != nil {
		return nil, err
	}
	out, err := safeData(result)
	if err != nil {
		return nil, err
	}
	if result.Preview {
		m := out.(map[string]any)
		m["next"] = viewConfirmLine(v, in, result)
	}
	return out, nil
}

// viewConfirmLine is the command that sends what the preview showed.
func viewConfirmLine(v linearFlags, in linearboard.ViewWriteInput, preview linearboard.ViewWriteResult) string {
	parts := []string{"clawbridge linear view", in.Action}
	if in.Action == "create" {
		parts = append(parts, "--team", in.Team)
	} else {
		parts = append(parts, in.ViewID)
	}
	if in.Name != "" {
		parts = append(parts, "--name", strconv.Quote(terminalSafe(in.Name)))
	}
	if v.filterFile != "" {
		parts = append(parts, "--filter-file", strconv.Quote(terminalSafe(v.filterFile)))
	}
	parts = append(parts, "--operation-id", in.OperationID, "--profile", v.profile, "--expected-revision", strconv.FormatInt(preview.BindingRevision, 10), "--expected-filter-sha256", preview.FilterSHA)
	if in.Action == "update" {
		parts = append(parts, "--expected-filter-as-of", strconv.Quote(terminalSafe(preview.ExpectedFilterAsOf)))
	}
	return strings.Join(append(parts, "--confirm"), " ")
}

// viewKey adapts the key client for linearboard, sorting its errors by
// what reached Linear.
type viewKey struct{ c *linearkey.Client }

func (k viewKey) wrap(v linearkey.View, err error) (linearboard.KeyView, error) {
	switch {
	case err == nil:
		return linearboard.KeyView{ID: v.ID, Name: v.Name, UpdatedAt: v.UpdatedAt, TeamID: v.TeamID, Filter: v.Filter, Shared: v.Shared}, nil
	case errors.Is(err, linearkey.ErrForbidden), errors.Is(err, linearkey.ErrRejected), errors.Is(err, linearkey.ErrNoKey):
		return linearboard.KeyView{}, fmt.Errorf("%w: %w", linearboard.ErrKeyNotSent, err)
	case errors.Is(err, linearkey.ErrViewRefused):
		return linearboard.KeyView{}, fmt.Errorf("%w: %w", linearboard.ErrKeyRefused, err)
	case errors.Is(err, linearkey.ErrViewNotFound):
		return linearboard.KeyView{}, fmt.Errorf("%w: %w", linearboard.ErrKeyNotFound, err)
	}
	return linearboard.KeyView{}, err
}

func (k viewKey) CreateView(ctx context.Context, team, name string, filter json.RawMessage) (linearboard.KeyView, error) {
	return k.wrap(k.c.CreateView(ctx, team, name, filter))
}

func (k viewKey) UpdateView(ctx context.Context, id, name string, filter json.RawMessage) (linearboard.KeyView, error) {
	return k.wrap(k.c.UpdateView(ctx, id, name, filter))
}

func (k viewKey) View(ctx context.Context, id string) (linearboard.KeyView, error) {
	return k.wrap(k.c.View(ctx, id))
}
