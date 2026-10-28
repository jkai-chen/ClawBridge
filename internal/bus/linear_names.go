package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Linear team and project names stored with a binding (owner's Q5,
// live/PROJECT-OVERVIEW-PLAN.md §3): display text for the overview only.
// No code reads a name to decide anything; every decision reads the IDs.
// A link sets them, taskboard_names_set refreshes them without moving the
// binding's revision, and unlinking clears them.
const linearNamesSchemaV33 = `ALTER TABLE taskboard_bindings ADD COLUMN names TEXT;`

const (
	maxLinearNames    = 100
	maxLinearNameRune = 80
)

type linearNames struct {
	Teams     map[string]string `json:"teams"`
	Projects  map[string]string `json:"projects"`
	UpdatedAt string            `json:"updated_at"`
}

// parseNameMap reads one of linear_team_names or linear_project_names: an
// object from bound IDs to names. given is false when the key is absent.
func parseNameMap(a map[string]any, key string, bound []string) (names map[string]string, given bool, err error) {
	raw, ok := a[key]
	if !ok || raw == nil {
		return nil, false, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false, invalid(key + " must be an object of IDs to names")
	}
	if len(m) > maxLinearNames {
		return nil, false, invalid(key + " names at most 100 IDs")
	}
	names = map[string]string{}
	for id, v := range m {
		if !containsString(bound, id) {
			return nil, false, invalid(key + " names only IDs of this link")
		}
		name, ok := v.(string)
		if !ok {
			return nil, false, invalid(key + " values must be strings")
		}
		name = strings.TrimSpace(name)
		if !LinearNameOK(name) {
			return nil, false, invalid(key + " names must be 1 to 80 characters with no control or direction characters")
		}
		names[id] = name
	}
	return names, true, nil
}

// LinearNameOK is the rule a stored name meets: 1 to 80 characters, with
// no control or format characters (the bidi controls are format
// characters).
func LinearNameOK(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxLinearNameRune {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// nextNames are the names a write stores for binding next: a map the
// request gives replaces that map; one it leaves out keeps the names of
// the IDs still bound (PROJECT-OVERVIEW-PLAN-REVIEW L2).
func nextNames(a map[string]any, prev linearNames, next TaskboardBindingV2, at string) (linearNames, error) {
	teams, teamsGiven, err := parseNameMap(a, "linear_team_names", next.ListedTeamIDs())
	if err != nil {
		return linearNames{}, err
	}
	projects, projectsGiven, err := parseNameMap(a, "linear_project_names", next.LinearProjectIDs)
	if err != nil {
		return linearNames{}, err
	}
	if !teamsGiven {
		teams = keptNames(prev.Teams, next.ListedTeamIDs())
	}
	if !projectsGiven {
		projects = keptNames(prev.Projects, next.LinearProjectIDs)
	}
	updated := prev.UpdatedAt
	if teamsGiven || projectsGiven {
		updated = at
	}
	return linearNames{Teams: teams, Projects: projects, UpdatedAt: updated}, nil
}

func keptNames(prev map[string]string, bound []string) map[string]string {
	kept := map[string]string{}
	for _, id := range bound {
		if name, ok := prev[id]; ok {
			kept[id] = name
		}
	}
	return kept
}

// storedNames reads the names column; NULL, or a value that doesn't
// parse, is no names.
func storedNames(raw any) linearNames {
	n := linearNames{}
	if s, ok := raw.(string); ok {
		_ = json.Unmarshal([]byte(s), &n)
	}
	if n.Teams == nil {
		n.Teams = map[string]string{}
	}
	if n.Projects == nil {
		n.Projects = map[string]string{}
	}
	return n
}

// namesColumn is what a write stores: NULL when no name is left.
func namesColumn(n linearNames) any {
	if len(n.Teams) == 0 && len(n.Projects) == 0 {
		return nil
	}
	return asJSON(n)
}

// bindingNames are the names stored with the project's binding and when
// they were last set; namesAt is nil when none was ever stored.
func bindingNames(ctx context.Context, tx *transaction, pid string) (linearNames, any, error) {
	row, err := one(ctx, tx, `SELECT names FROM taskboard_bindings WHERE project_id=?`, pid)
	if err != nil && !errors.Is(err, errNotFound) {
		return linearNames{}, nil, err
	}
	n := storedNames(row["names"])
	var at any
	if n.UpdatedAt != "" {
		at = n.UpdatedAt
	}
	return n, at, nil
}

// setNames is taskboard_names_set: it replaces the given maps of a linked
// binding at its current revision, and leaves the revision as it is, so a
// refresh never conflicts with a link. The audit records counts, never
// the names (PROJECT-OVERVIEW-PLAN-REVIEW L1).
func (s *Store) setNames(ctx context.Context, tx *transaction, wid, pid string, b TaskboardBindingV2, prev linearNames, a map[string]any) (any, error) {
	if !b.Configured {
		return nil, invalid("link the task board first; names belong to a link")
	}
	if _, ok := a["linear_team_names"]; !ok {
		if _, ok := a["linear_project_names"]; !ok {
			return nil, invalid("name linear_team_names, linear_project_names or both")
		}
	}
	at := stamp()
	n, err := nextNames(a, prev, b, at)
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE taskboard_bindings SET names=? WHERE project_id=? AND revision=?`, namesColumn(n), pid, b.Revision)
	if err != nil {
		return nil, err
	}
	if affected, _ := r.RowsAffected(); affected != 1 {
		return nil, &serviceError{code: "conflict", cause: errors.New("taskboard revision conflict; refresh binding")}
	}
	if err = s.audit(ctx, tx, wid, pid, "", "taskboard", pid, "taskboard_names_set", map[string]any{"revision": b.Revision, "teams": len(n.Teams), "projects": len(n.Projects)}); err != nil {
		return nil, err
	}
	return map[string]any{"revision": b.Revision, "names_updated_at": at, "teams": len(n.Teams), "projects": len(n.Projects)}, nil
}
