package bus

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// ProbeBindings is what the project-status probe's guards read of this
// server's task board bindings: whether a Linear project is in any of them,
// and every bound Linear team.
type ProbeBindings struct {
	ProjectBound bool
	Teams        []string
}

// OperatorProbeBindings reads every binding once. A mapping that names
// linearProject anywhere counts as binding it: the probe never moves a
// project a binding might hold.
func (s *Store) OperatorProbeBindings(ctx context.Context, linearProject string) (ProbeBindings, error) {
	lp, err := lowerUUID(map[string]any{"linear_project": linearProject}, "linear_project", true)
	if err != nil {
		return ProbeBindings{}, err
	}
	tx, err := s.operatorTx(ctx)
	if err != nil {
		return ProbeBindings{}, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT mapping FROM taskboard_bindings WHERE mapping IS NOT NULL ORDER BY project_id`)
	if err != nil {
		return ProbeBindings{}, errors.New("authority unavailable")
	}
	out := ProbeBindings{}
	teams := map[string]bool{}
	for _, row := range rows {
		mapping := strings.ToLower(str(row, "mapping"))
		if strings.Contains(mapping, lp) {
			out.ProjectBound = true
		}
		var m struct {
			Teams []string `json:"linear_team_ids"`
		}
		if json.Unmarshal([]byte(mapping), &m) != nil {
			return ProbeBindings{}, errors.New("a task board binding is unreadable")
		}
		for _, t := range m.Teams {
			teams[t] = true
		}
	}
	for t := range teams {
		out.Teams = append(out.Teams, t)
	}
	sort.Strings(out.Teams)
	return out, nil
}
