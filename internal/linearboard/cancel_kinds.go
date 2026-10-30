package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
)

// AGENT-CANCEL R2-R3 (live/AGENT-CANCEL-PLAN.md): the project kinds in
// whose Linear projects agents may cancel or mark Duplicate an issue. The
// CLI and the TUI set it through the same op; until the enforcement ships
// it only records the choice.

// CancelKindsView is cancel-kinds show: the list, the status policy's
// revision a set names, and the project's kinds to choose from.
type CancelKindsView struct {
	Revision    int64    `json:"revision"`
	CancelKinds []string `json:"agent_may_cancel_kinds"`
	// Kinds are the values of the project's kind setting; none until the
	// project owner runs linear kind set.
	Kinds []string `json:"kinds"`
	// Line is how doctor and the TUI say it.
	Line string `json:"line"`
}

// CancelKindsV2 reads who may cancel, and the project's kinds.
func (s *Service) CancelKindsV2(ctx context.Context, scope bus.Request) (CancelKindsView, error) {
	data, e := s.StatusPolicyV2(ctx, scope)
	if e != nil {
		return CancelKindsView{}, e
	}
	raw, _ := json.Marshal(data)
	var p struct {
		Revision    int64    `json:"revision"`
		CancelKinds []string `json:"agent_may_cancel_kinds"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return CancelKindsView{}, errors.New("the status policy could not be read")
	}
	kinds, e := s.ProjectKind(ctx, scope)
	if e != nil {
		return CancelKindsView{}, e
	}
	raw, _ = json.Marshal(kinds)
	var k struct {
		Values []bus.ProjectKindValue `json:"values"`
	}
	if json.Unmarshal(raw, &k) != nil {
		return CancelKindsView{}, errors.New("the project kind could not be read")
	}
	out := CancelKindsView{Revision: p.Revision, CancelKinds: p.CancelKinds, Kinds: []string{}, Line: bus.CancelKindsLine(p.CancelKinds)}
	if out.CancelKinds == nil {
		out.CancelKinds = []string{}
	}
	for _, v := range k.Values {
		out.Kinds = append(out.Kinds, v.Name)
	}
	return out, nil
}

// SetCancelKindsV2 sets who may cancel at the policy's revision: the
// project owner or a lead, never a session. via is how the person
// confirmed it: cli for an empty list, cli-tty or tui for kinds typed.
func (s *Service) SetCancelKindsV2(ctx context.Context, scope bus.Request, kinds []string, expected int64, via string) (any, error) {
	if kinds == nil {
		kinds = []string{}
	}
	scope.Operation = "status_cancel_kinds_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["kinds"] = kinds
	scope.Args["expected_revision"] = expected
	scope.Args["via"] = via
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil && strings.Contains(e.Error(), `unsupported operation "status_cancel_kinds_set"`) {
		return nil, errors.New("this server predates cancel-kinds set; upgrade the server first. Nothing was changed")
	}
	return r.Data, e
}

// CancelKindsMatch spells each typed name as the project's kinds do; a
// name no kind has is refused before anything is sent.
func CancelKindsMatch(typed, kinds []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, name := range typed {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		found := ""
		for _, k := range kinds {
			if strings.EqualFold(k, name) {
				found = k
			}
		}
		if found == "" {
			if len(kinds) == 0 {
				return nil, errors.New("this project has no kinds: set the project kinds first: clawbridge linear kind set")
			}
			return nil, fmt.Errorf("%q is not a kind of this project (its kinds: %s)", name, strings.Join(kinds, ", "))
		}
		out = append(out, found)
	}
	return out, nil
}

// cancelKindsAdded are the names next lists that current doesn't,
// ignoring case.
func cancelKindsAdded(current, next []string) []string {
	have := map[string]bool{}
	for _, name := range current {
		have[strings.ToLower(strings.TrimSpace(name))] = true
	}
	var out []string
	for _, name := range next {
		if !have[strings.ToLower(strings.TrimSpace(name))] {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}
