package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// ExecuteV2 is the closed native adapter used by additive, scope-pinned MCP tools.
func (s *Service) ExecuteV2(ctx context.Context, scope bus.Request, operation string, args map[string]any) (any, error) {
	allowed := map[string][]string{"binding": {}, "context": {}, "issues": {"limit", "cursor", "label_id"}, "issue": {"id", "resolve_labels"}, "labels": {"team_id", "limit", "cursor"}, "members": {"team_id", "limit", "cursor"}, "create": {"title", "description", "state_id", "operation_id", "team_id", "project_id", "label_ids", "parent_id", "assignee_id"}, "update": {"id", "title", "description", "state_id", "operation_id", "project_id", "label_ids", "expected_label_ids", "parent_id", "assignee_id"}, "comment": {"id", "body", "operation_id"}, "relation_add": {"from_id", "to_id", "type", "operation_id"}, "relation_remove": {"from_id", "to_id", "type", "operation_id"}}
	keys, ok := allowed[operation]
	if !ok {
		return nil, errors.New("unsupported task-board V2 operation")
	}
	for key, value := range args {
		if !contains(keys, key) {
			return nil, errors.New("unknown task-board V2 argument")
		}
		if key == "project_id" || key == "parent_id" || key == "assignee_id" {
			if value == nil {
				continue
			}
		}
		if key == "limit" {
			if _, e := pageLimitV2(value); e != nil {
				return nil, e
			}
			continue
		}
		if key == "resolve_labels" {
			if _, ok := value.(bool); !ok {
				return nil, errors.New("resolve_labels requires a boolean")
			}
			continue
		}
		if key == "label_ids" || key == "expected_label_ids" {
			if _, e := labelArgumentV2(value); e != nil {
				return nil, e
			}
			continue
		}
		if _, ok := value.(string); !ok {
			return nil, errors.New("task-board V2 fields require exact types")
		}
	}
	text := func(key string) string { v, _ := args[key].(string); return v }
	limit := 50
	if v, ok := args["limit"]; ok {
		limit, _ = pageLimitV2(v)
	}
	switch operation {
	case "binding":
		return s.BindingV2(ctx, scope)
	case "context":
		return s.ContextV2(ctx, scope)
	case "issues":
		return s.IssuesV2(ctx, scope, IssuesInputV2{Limit: limit, Cursor: text("cursor"), LabelID: text("label_id")})
	case "issue":
		if resolve, _ := args["resolve_labels"].(bool); resolve {
			return s.IssueV2ResolvedLabels(ctx, scope, text("id"))
		}
		return s.IssueV2(ctx, scope, text("id"))
	case "labels":
		return s.LabelsV2(ctx, scope, text("team_id"), limit, text("cursor"))
	case "members":
		return s.MembersV2(ctx, scope, text("team_id"), limit, text("cursor"))
	case "relation_add", "relation_remove":
		action := "add"
		if operation == "relation_remove" {
			action = "remove"
		}
		return s.RelationV2(ctx, scope, RelationInputV2{OperationID: text("operation_id"), Action: action, Type: text("type"), FromID: text("from_id"), ToID: text("to_id")})
	default:
		in := WriteInputV2{WriteInput: WriteInput{Operation: operation, OperationID: text("operation_id"), IssueID: text("id"), Body: text("body")}, TeamID: text("team_id")}
		for key, target := range map[string]**string{"title": &in.Title, "description": &in.Description, "state_id": &in.StateID} {
			if _, ok := args[key]; ok {
				v := text(key)
				*target = &v
			}
		}
		for key, target := range map[string]*linearprovider.Field[*string]{"project_id": &in.Project, "parent_id": &in.Parent, "assignee_id": &in.Assignee} {
			if v, ok := args[key]; ok {
				target.Present = true
				if v != nil {
					value := v.(string)
					target.Value = &value
				}
			}
		}
		if v, ok := args["label_ids"]; ok {
			ids, e := labelArgumentV2(v)
			if e != nil {
				return nil, e
			}
			in.Labels = linearprovider.Field[[]string]{Present: true, Value: ids}
		}
		if v, ok := args["expected_label_ids"]; ok {
			ids, e := labelArgumentV2(v)
			if e != nil {
				return nil, e
			}
			in.ExpectedLabels = linearprovider.Field[[]string]{Present: true, Value: ids}
		}
		return s.WriteV2(ctx, scope, in)
	}
}
func pageLimitV2(v any) (int, error) {
	var n int
	switch v := v.(type) {
	case int:
		n = v
	case float64:
		if !(v >= 1 && v <= 100) || v != float64(int(v)) {
			return 0, errors.New("invalid page limit")
		}
		n = int(v)
	default:
		return 0, errors.New("page limit requires an integer")
	}
	if n < 1 || n > 100 {
		return 0, errors.New("page limit requires 1–100")
	}
	return n, nil
}
func labelArgumentV2(v any) ([]string, error) {
	var ids []string
	switch value := v.(type) {
	case []string:
		if value == nil {
			return nil, errors.New("labels cannot be null")
		}
		ids = value
	case []any:
		if value == nil {
			return nil, errors.New("labels cannot be null")
		}
		ids = []string{}
		for _, v := range value {
			s, ok := v.(string)
			if !ok {
				return nil, errors.New("label IDs require strings")
			}
			ids = append(ids, s)
		}
	default:
		return nil, errors.New("labels require an explicit array")
	}
	return sortedUniqueIDs(ids, 100)
}

func (s *Service) StatesV2(ctx context.Context, scope bus.Request, team string) ([]linearprovider.Status, error) {
	if !linearprovider.ValidID(team) {
		return nil, errors.New("workflow states require canonical team UUID")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if !contains(b.LinearTeamIDs, team) {
		return nil, errors.New("workflow team is outside task-board scope")
	}
	_, p, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer p.Close()
	if _, e = verifyBindingV2(ctx, p, b); e != nil {
		return nil, e
	}
	states, e := p.Statuses(ctx, team)
	if e != nil {
		return nil, e
	}
	for _, state := range states {
		if !linearprovider.ValidID(state.ID) || state.TeamID != team {
			return nil, errors.New("workflow state team is not verified")
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return nil, e
	}
	return states, nil
}
