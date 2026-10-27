package cli

import (
	"context"
	"encoding/json"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// viewShowPages bounds view show's search through each bound team's views.
const viewShowPages = 10

// filterNote is what view show says of the filter until the person's Linear
// API key arrives (LINEAR-VIEWS-PLAN §12.2).
const filterNote = "a view's filter isn't readable over Linear's MCP; it needs your Linear API key, which comes in a later release"

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
