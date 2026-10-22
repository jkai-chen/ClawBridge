package linearprovider

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"unicode/utf8"
)

// Field distinguishes omission from an explicitly supplied value, including null.
type Field[T any] struct {
	Present bool `json:"present"`
	Value   T    `json:"value"`
}
type IssueQueryV2 struct {
	IssueQuery
	LabelID string
}
type IssueV2 struct {
	Issue
	ProjectKnown   bool       `json:"project_known"`
	LabelIDs       []string   `json:"label_ids,omitempty"`
	LabelsKnown    bool       `json:"labels_known"`
	ParentID       string     `json:"parent_id,omitempty"`
	ParentKnown    bool       `json:"parent_known"`
	AssigneeID     string     `json:"assignee_id,omitempty"`
	AssigneeKnown  bool       `json:"assignee_known"`
	Relations      []Relation `json:"relations,omitempty"`
	RelationsKnown bool       `json:"relations_known"`
}
type Label struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	TeamID           string `json:"team_id,omitempty"`
	Archived         bool   `json:"archived"`
	Group            bool   `json:"group"`
	Global           bool   `json:"global"`
	EligibilityKnown bool   `json:"eligibility_known"`
}
type Member struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	TeamID           string `json:"team_id"`
	Active           bool   `json:"active"`
	EligibilityKnown bool   `json:"eligibility_known"`
}
type Capabilities struct {
	Labels                     bool `json:"labels"`
	Members                    bool `json:"members"`
	CreateProject              bool `json:"create_project"`
	UpdateProject              bool `json:"update_project"`
	ReplaceLabels              bool `json:"replace_labels"`
	Parent                     bool `json:"parent"`
	Assignee                   bool `json:"assignee"`
	ParentPreservesDestination bool `json:"parent_preserves_destination"`
	RelationAdd                bool `json:"relation_add"`
	RelationRemove             bool `json:"relation_remove"`
}
type CreateInputV2 struct {
	CreateInput
	Project          Field[*string]
	Labels           Field[[]string]
	Parent, Assignee Field[*string]
}
type UpdateInputV2 struct {
	UpdateInput
	Project          Field[*string]
	Labels           Field[[]string]
	Parent, Assignee Field[*string]
}
type Relation struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"`
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
}
type RelationInput struct{ Action, Type, FromID, ToID string }
type RelationReceipt struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
	ID     string `json:"id,omitempty"`
}

func canonicalIDs(ids []string, limit int) ([]string, error) {
	if ids == nil || len(ids) > limit {
		return nil, fail("invalid")
	}
	out := append([]string{}, ids...)
	sort.Strings(out)
	for i, id := range out {
		if !ValidID(id) || (i > 0 && id == out[i-1]) {
			return nil, fail("invalid")
		}
	}
	return out, nil
}
func nullableArg(f Field[*string], aliases ...string) (arg, bool, error) {
	if !f.Present {
		return arg{}, false, nil
	}
	if f.Value == nil {
		return arg{aliases, nil, "null"}, true, nil
	}
	if !ValidID(*f.Value) {
		return arg{}, false, fail("invalid")
	}
	return a(*f.Value, aliases...), true, nil
}
func extraArgs(project Field[*string], labels Field[[]string], parent, assignee Field[*string]) ([]arg, error) {
	out := []arg{}
	for _, f := range []struct {
		value Field[*string]
		keys  []string
	}{{project, []string{"projectId", "project"}}, {parent, []string{"parentId"}}, {assignee, []string{"assigneeId", "assignee"}}} {
		v, p, e := nullableArg(f.value, f.keys...)
		if e != nil {
			return nil, e
		}
		if p {
			out = append(out, v)
		}
	}
	if labels.Present {
		ids, e := canonicalIDs(labels.Value, 100)
		if e != nil {
			return nil, e
		}
		out = append(out, arg{[]string{"labelIds", "labels"}, ids, "array"})
	}
	return out, nil
}

func schemaProperty(t any) map[string]any {
	b, e := json.Marshal(t)
	if e != nil {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil {
		return nil
	}
	return obj
}
func typedVariant(raw any, kind string) map[string]any {
	m, _ := raw.(map[string]any)
	if m == nil {
		return nil
	}
	if m["type"] == kind {
		return m
	}
	if kinds, ok := m["type"].([]any); ok {
		for _, t := range kinds {
			if t == kind {
				return m
			}
		}
	}
	if choices, ok := m["anyOf"].([]any); ok {
		for _, choice := range choices {
			if selected := typedVariant(choice, kind); selected != nil {
				return selected
			}
		}
	}
	return nil
}
func allowsArrayStrings(raw any) bool {
	m := typedVariant(raw, "array")
	if m == nil {
		return false
	}
	items, _ := m["items"].(map[string]any)
	return items != nil && items["type"] == "string"
}

// constrainedValue checks the small schema vocabulary needed by curated inputs.
// Unknown compositional validation is refused instead of guessing or fetching refs.
func constrainedValue(raw any, value any) bool {
	m, _ := raw.(map[string]any)
	if m == nil {
		return false
	}
	for _, key := range []string{"$ref", "allOf", "oneOf", "not", "if", "then", "else"} {
		if _, ok := m[key]; ok {
			return false
		}
	}
	kind := "null"
	switch value.(type) {
	case string:
		kind = "string"
	case int:
		kind = "integer"
	case []string:
		kind = "array"
	}
	selected := typedVariant(raw, kind)
	if selected == nil && kind == "integer" {
		selected = typedVariant(raw, "number")
	}
	if selected == nil {
		return false
	}
	if branches, ok := m["anyOf"].([]any); ok {
		valid := false
		for _, branch := range branches {
			if constrainedValue(branch, value) {
				valid = true
				break
			}
		}
		if !valid {
			return false
		}
	}
	for _, source := range []map[string]any{m, selected} {
		if choices, exists := source["enum"]; exists {
			list, ok := choices.([]any)
			if !ok {
				return false
			}
			candidate, _ := json.Marshal(value)
			found := false
			for _, choice := range list {
				encoded, _ := json.Marshal(choice)
				if string(candidate) == string(encoded) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		if fixed, exists := source["const"]; exists {
			a, _ := json.Marshal(fixed)
			b, _ := json.Marshal(value)
			if string(a) != string(b) {
				return false
			}
		}
		bounds := func(size int, minKey, maxKey string) bool {
			for _, key := range []string{minKey, maxKey} {
				if raw, exists := source[key]; exists {
					number, ok := raw.(float64)
					if !ok || number < 0 {
						return false
					}
					if key == minKey && float64(size) < number {
						return false
					}
					if key == maxKey && float64(size) > number {
						return false
					}
				}
			}
			return true
		}
		switch v := value.(type) {
		case string:
			if !bounds(utf8.RuneCountInString(v), "minLength", "maxLength") {
				return false
			}
			if raw, exists := source["pattern"]; exists {
				pattern, ok := raw.(string)
				if !ok || len(pattern) > 4096 {
					return false
				}
				compiled, e := regexp.Compile(pattern)
				if e != nil || !compiled.MatchString(v) {
					return false
				}
			}
		case []string:
			if !bounds(len(v), "minItems", "maxItems") {
				return false
			}
			if item, exists := selected["items"]; exists {
				for _, value := range v {
					if !constrainedValue(item, value) {
						return false
					}
				}
			} else {
				return false
			}
		case int:
			if !bounds(v, "minimum", "maximum") {
				return false
			}
		}
	}
	return true
}

// v2Negotiation remains curated: it proves typed array and null support rather
// than sending an arbitrary extra argument through the generic MCP transport.
func (p *Provider) v2Negotiation(names []string, args []arg) (string, map[string]any, error) {
	for _, name := range names {
		name, values, e := p.negotiate([]string{name}, args...)
		if e != nil {
			continue
		}
		props, _, e := schema(p.tools[name])
		if e != nil {
			continue
		}
		safe := true
		for _, x := range args {
			key, ok := field(props, x.aliases, x.kind)
			if !ok {
				safe = false
				break
			}
			if x.kind == "array" && !allowsArrayStrings(props[key]) {
				safe = false
				break
			}
		}
		if !safe {
			continue
		}
		if name == "get_issue" || name == "list_issues" {
			if projection, ok := values["fields"].([]string); ok {
				prop, _ := props["fields"].(map[string]any)
				if branches, ok := prop["anyOf"].([]any); ok {
					for _, branch := range branches {
						candidate, _ := branch.(map[string]any)
						if candidate["type"] == "array" {
							prop = candidate
							break
						}
					}
				}
				items, _ := prop["items"].(map[string]any)
				choices, _ := items["enum"].([]any)
				allowed := map[string]bool{}
				for _, v := range choices {
					if text, ok := v.(string); ok {
						allowed[text] = true
					}
				}
				for _, key := range []string{"labels", "labelIds", "assigneeId", "parentId"} {
					if allowed[key] {
						present := false
						for _, old := range projection {
							if old == key {
								present = true
							}
						}
						if !present {
							projection = append(projection, key)
						}
					}
				}
				values["fields"] = projection
			}
		}
		for key, value := range values {
			if !constrainedValue(props[key], value) {
				safe = false
				break
			}
		}
		if !safe {
			continue
		}
		return name, values, nil
	}
	return "", nil, fail("capability_unavailable")
}
func (p *Provider) v2Call(ctx context.Context, names []string, args ...arg) (map[string]any, error) {
	name, values, e := p.v2Negotiation(names, args)
	if e != nil {
		return nil, e
	}
	return p.invoke(ctx, name, values)
}

func (p *Provider) Capabilities(context.Context) (Capabilities, error) {
	has := func(names []string, args ...arg) bool { _, _, e := p.v2Negotiation(names, args); return e == nil }
	sample := "00000000-0000-4000-8000-000000000001"
	create := []arg{a(sample, "team", "teamId"), a("title", "title"), a("", "description")}
	update := []arg{a(sample, "id", "issueId")}
	labels := has([]string{"list_issue_labels"}, a(sample, "team", "teamId"), arg{[]string{"limit", "first"}, 1, "integer"})
	members := has([]string{"list_users", "list_team_members"}, a(sample, "team", "teamId"), arg{[]string{"limit", "first"}, 1, "integer"})
	capabilities := Capabilities{Labels: labels, Members: members, CreateProject: has([]string{"create_issue", "save_issue"}, append(create, a(sample, "projectId", "project"))...), UpdateProject: has([]string{"update_issue", "save_issue"}, append(update, a(sample, "projectId", "project"))...), ReplaceLabels: has([]string{"update_issue", "save_issue"}, append(update, arg{[]string{"labelIds", "labels"}, []string{}, "array"})...), Parent: has([]string{"update_issue", "save_issue"}, append(update, a(sample, "parentId"))...), Assignee: has([]string{"update_issue", "save_issue"}, append(update, a(sample, "assigneeId", "assignee"))...)}
	// This explicit extension is a testable semantic attestation, not inferred
	// from descriptions or the mere existence of parent/project fields. The
	// official parent/project field shapes alone provide no such semantic proof.
	for _, name := range []string{"create_issue", "update_issue", "save_issue"} {
		if t := p.tools[name]; t != nil {
			root := schemaProperty(t.InputSchema)
			if root["x-clawbridge-parent-preserves-destination"] == true {
				props, _, e := schema(t)
				if e == nil {
					_, parent := field(props, []string{"parentId"}, "string")
					_, project := field(props, []string{"projectId", "project"}, "string")
					if parent && project {
						capabilities.ParentPreservesDestination = true
					}
				}
			}
		}
	}
	for _, action := range []string{"add", "remove"} {
		available := true
		for _, kind := range []string{"blocks", "related"} {
			_, _, _, err := p.relationTool(RelationInput{Action: action, Type: kind, FromID: sample, ToID: "00000000-0000-4000-8000-000000000002"})
			available = available && err == nil
		}
		if action == "add" {
			capabilities.RelationAdd = available
		} else {
			capabilities.RelationRemove = available
		}
	}
	return capabilities, nil
}

func optionalCanonical(m map[string]any, flat, nested string) (string, bool, error) {
	_, a := m[flat]
	v, b := m[nested]
	if !a && !b {
		return "", false, nil
	}
	if _, label := v.(string); label {
		if !a {
			return "", false, nil
		}
		copy := map[string]any{}
		for key, value := range m {
			if key != nested {
				copy[key] = value
			}
		}
		m = copy
	}
	i, e := scopedID(m, flat, nested, true)
	return i, e == nil, e
}
func parseLabelIDs(m map[string]any) ([]string, bool, error) {
	var result []string
	known := false
	for _, key := range []string{"labelIds", "labels"} {
		raw, present := m[key]
		if !present {
			continue
		}
		rows, ok := raw.([]any)
		if !ok {
			return nil, false, fail("invalid_response")
		}
		if len(rows) > 100 {
			return nil, false, fail("result_limit")
		}
		ids := []string{}
		canonical := true
		for _, v := range rows {
			switch item := v.(type) {
			case string:
				// The descriptive labels field may contain UUID-shaped names.
				// Only the explicitly canonical labelIds field supplies IDs.
				if key != "labelIds" {
					canonical = false
					continue
				}
				if !ValidID(item) {
					return nil, false, fail("invalid_response")
				}
				ids = append(ids, item)
			case map[string]any:
				i, e := id(item)
				if e != nil {
					return nil, false, e
				}
				ids = append(ids, i)
			default:
				return nil, false, fail("invalid_response")
			}
		}
		if !canonical {
			continue
		}
		ids, e := canonicalIDs(ids, 100)
		if e != nil {
			return nil, false, fail("invalid_response")
		}
		if known {
			if len(ids) != len(result) {
				return nil, false, fail("scope_mismatch")
			}
			for i := range ids {
				if ids[i] != result[i] {
					return nil, false, fail("scope_mismatch")
				}
			}
		}
		result = ids
		known = true
	}
	return result, known, nil
}
func (p *Provider) parseIssueV2(ctx context.Context, m map[string]any) (IssueV2, error) {
	base, e := p.parseIssue(ctx, m)
	if e != nil {
		return IssueV2{}, e
	}
	x := IssueV2{Issue: base, ProjectKnown: true}
	x.LabelIDs, x.LabelsKnown, e = parseLabelIDs(m)
	if e != nil {
		return IssueV2{}, e
	}
	x.ParentID, x.ParentKnown, e = optionalCanonical(m, "parentId", "parent")
	if e != nil {
		return IssueV2{}, e
	}
	x.AssigneeID, x.AssigneeKnown, e = optionalCanonical(m, "assigneeId", "assignee")
	if e != nil {
		return IssueV2{}, e
	}
	// Only an explicitly complete typed edge list proves absence. Unrecognized
	// legacy linked-context forms retain RelationsKnown=false.
	if raw, present := m["issueRelations"]; present {
		rows, ok := raw.([]any)
		if !ok || len(rows) > 250 {
			return IssueV2{}, fail("invalid_response")
		}
		for _, row := range rows {
			node, ok := row.(map[string]any)
			if !ok {
				return IssueV2{}, fail("invalid_response")
			}
			relation := Relation{Type: str(node, "type"), FromID: str(node, "fromId"), ToID: str(node, "toId"), ID: str(node, "id")}
			if !validRelation(relation.Type, relation.FromID, relation.ToID) || (relation.ID != "" && !ValidID(relation.ID)) {
				return IssueV2{}, fail("invalid_response")
			}
			x.Relations = append(x.Relations, relation)
		}
		complete, ok := m["relationsComplete"].(bool)
		x.RelationsKnown = ok && complete
	}
	return x, nil
}
func (p *Provider) IssueV2(ctx context.Context, id string) (IssueV2, error) {
	if !ValidID(id) {
		return IssueV2{}, fail("invalid")
	}
	m, e := p.v2Call(ctx, []string{"get_issue"}, a(id, "id", "query", "issueId"))
	if e != nil {
		return IssueV2{}, e
	}
	x, e := p.parseIssueV2(ctx, unwrap(m, "issue"))
	if e != nil {
		return IssueV2{}, e
	}
	if x.ID != id {
		return IssueV2{}, fail("scope_mismatch")
	}
	return x, nil
}
func (p *Provider) IssuesV2(ctx context.Context, q IssueQueryV2) (Page[IssueV2], error) {
	if !ValidID(q.TeamID) || (q.ProjectID != "" && !ValidID(q.ProjectID)) || (q.LabelID != "" && !ValidID(q.LabelID)) {
		return Page[IssueV2]{}, fail("invalid")
	}
	args, e := pageArgs(PageOptions{Limit: q.Limit, Cursor: q.Cursor})
	if e != nil {
		return Page[IssueV2]{}, e
	}
	args = append(args, a(q.TeamID, "team", "teamId"))
	if q.ProjectID != "" {
		args = append(args, a(q.ProjectID, "project", "projectId"))
	}
	if q.LabelID != "" {
		args = append(args, a(q.LabelID, "label", "labelId"))
	}
	m, e := p.v2Call(ctx, []string{"list_issues"}, args...)
	if e != nil {
		return Page[IssueV2]{}, e
	}
	rows, cursor, complete, e := list(m, "issues")
	if e != nil {
		return Page[IssueV2]{}, e
	}
	out := Page[IssueV2]{Items: []IssueV2{}, NextCursor: cursor, Complete: complete}
	seen := map[string]string{}
	for _, row := range rows {
		x, e := p.parseIssueV2(ctx, row)
		if pe, ok := e.(*Error); ok && pe.Kind == "scope_metadata_missing" && ValidID(str(row, "id")) {
			x, e = p.IssueV2(ctx, str(row, "id"))
		}
		if e != nil {
			return Page[IssueV2]{}, e
		}
		if x.TeamID != q.TeamID || (q.ProjectID != "" && x.ProjectID != q.ProjectID) {
			return Page[IssueV2]{}, fail("scope_mismatch")
		}
		if q.LabelID != "" {
			if !x.LabelsKnown {
				return Page[IssueV2]{}, fail("capability_unavailable")
			}
			found := false
			for _, label := range x.LabelIDs {
				if label == q.LabelID {
					found = true
				}
			}
			if !found {
				return Page[IssueV2]{}, fail("scope_mismatch")
			}
		}
		encoded, _ := json.Marshal(x)
		if old, exists := seen[x.ID]; exists && old != string(encoded) {
			return Page[IssueV2]{}, fail("invalid_response")
		}
		seen[x.ID] = string(encoded)
		out.Items = append(out.Items, x)
	}
	return out, nil
}

func explicitBool(m map[string]any, keys ...string) (bool, bool, error) {
	var result bool
	known := false
	for _, key := range keys {
		if raw, present := m[key]; present {
			value, ok := raw.(bool)
			if !ok {
				return false, false, fail("invalid_response")
			}
			if known && result != value {
				return false, false, fail("scope_mismatch")
			}
			result = value
			known = true
		}
	}
	return result, known, nil
}
func (p *Provider) Labels(ctx context.Context, team string, o PageOptions) (Page[Label], error) {
	if !ValidID(team) {
		return Page[Label]{}, fail("invalid")
	}
	args, e := pageArgs(o)
	if e != nil {
		return Page[Label]{}, e
	}
	args = append(args, a(team, "team", "teamId"))
	m, e := p.v2Call(ctx, []string{"list_issue_labels"}, args...)
	if e != nil {
		return Page[Label]{}, e
	}
	rows, cursor, complete, e := list(m, "labels")
	if e != nil {
		return Page[Label]{}, e
	}
	out := Page[Label]{Items: []Label{}, NextCursor: cursor, Complete: complete}
	for _, row := range rows {
		i, e := entityID(row, "labelId")
		if e != nil {
			return Page[Label]{}, e
		}
		label := Label{ID: i, Name: str(row, "name")}
		_, flat := row["teamId"]
		_, nested := row["team"]
		scopeKnown := false
		if flat || nested {
			label.TeamID, e = scopedID(row, "teamId", "team", true)
			if e != nil {
				return Page[Label]{}, e
			}
			scopeKnown = true
			label.Global = label.TeamID == ""
			if !label.Global && label.TeamID != team {
				return Page[Label]{}, fail("scope_mismatch")
			}
		}
		archived, archivedKnown, e := explicitBool(row, "archived", "isArchived")
		if e != nil {
			return Page[Label]{}, e
		}
		if raw, present := row["archivedAt"]; present {
			fromTime := false
			if raw != nil {
				stamp, ok := raw.(string)
				if !ok || stamp == "" {
					return Page[Label]{}, fail("invalid_response")
				}
				fromTime = true
			}
			if archivedKnown && archived != fromTime {
				return Page[Label]{}, fail("scope_mismatch")
			}
			archived = fromTime
			archivedKnown = true
		}
		// Retirement independently disqualifies a label even when the provider
		// reports archived=false or archivedAt=null. A null retirement timestamp
		// does not establish that the separate archive state is known.
		if raw, present := row["retiredAt"]; present && raw != nil {
			stamp, ok := raw.(string)
			if !ok || stamp == "" {
				return Page[Label]{}, fail("invalid_response")
			}
			archived = true
			archivedKnown = true
		}
		group, groupKnown, e := explicitBool(row, "isGroup", "group")
		if e != nil {
			return Page[Label]{}, e
		}
		label.Archived = archived
		label.Group = group
		label.EligibilityKnown = scopeKnown && archivedKnown && groupKnown
		out.Items = append(out.Items, label)
	}
	return out, nil
}
func (p *Provider) Members(ctx context.Context, team string, o PageOptions) (Page[Member], error) {
	if !ValidID(team) {
		return Page[Member]{}, fail("invalid")
	}
	args, e := pageArgs(o)
	if e != nil {
		return Page[Member]{}, e
	}
	args = append(args, a(team, "team", "teamId"))
	m, e := p.v2Call(ctx, []string{"list_users", "list_team_members"}, args...)
	if e != nil {
		return Page[Member]{}, e
	}
	key := "users"
	if _, ok := m["members"]; ok {
		key = "members"
	}
	rows, cursor, complete, e := list(m, key)
	if e != nil {
		return Page[Member]{}, e
	}
	out := Page[Member]{Items: []Member{}, NextCursor: cursor, Complete: complete}
	for _, row := range rows {
		i, e := entityID(row, "userId")
		if e != nil {
			return Page[Member]{}, e
		}
		_, flat := row["teamId"]
		_, nested := row["team"]
		if flat || nested {
			scope, e := scopedID(row, "teamId", "team", false)
			if e != nil {
				return Page[Member]{}, e
			}
			if scope != team {
				return Page[Member]{}, fail("scope_mismatch")
			}
		}
		active, known, e := explicitBool(row, "active", "isActive")
		if e != nil {
			return Page[Member]{}, e
		}
		if raw, present := row["disabled"]; present {
			disabled, ok := raw.(bool)
			if !ok {
				return Page[Member]{}, fail("invalid_response")
			}
			if known && active == disabled {
				return Page[Member]{}, fail("scope_mismatch")
			}
			active = !disabled
			known = true
		}
		out.Items = append(out.Items, Member{ID: i, Name: str(row, "name"), TeamID: team, Active: active, EligibilityKnown: known})
	}
	return out, nil
}

func (p *Provider) parentSafe(name string) bool {
	t := p.tools[name]
	if t == nil {
		return false
	}
	root := schemaProperty(t.InputSchema)
	if root["x-clawbridge-parent-preserves-destination"] != true {
		return false
	}
	props, _, e := schema(t)
	if e != nil {
		return false
	}
	_, parent := field(props, []string{"parentId"}, "string")
	_, project := field(props, []string{"projectId", "project"}, "string")
	return parent && project
}
func (p *Provider) mutateV2(ctx context.Context, names []string, args []arg, parent Field[*string]) (map[string]any, error) {
	name, values, e := p.v2Negotiation(names, args)
	if e != nil {
		return nil, e
	}
	if parent.Present && parent.Value != nil && !p.parentSafe(name) {
		return nil, fail("capability_unavailable")
	}
	return p.invoke(ctx, name, values)
}

// ValidateCreateV2 and ValidateUpdateV2 negotiate the exact intent without any
// provider call, allowing the durable ledger to reject unsupported intent before
// recording a pending mutation. They are optional additive service seams.
func (p *Provider) ValidateCreateV2(in CreateInputV2) error {
	args, e := createArgsV2(in)
	if e != nil {
		return e
	}
	return p.validateMutationV2([]string{"create_issue", "save_issue"}, args, in.Parent)
}
func (p *Provider) ValidateUpdateV2(in UpdateInputV2) error {
	args, e := updateArgsV2(in)
	if e != nil {
		return e
	}
	return p.validateMutationV2([]string{"update_issue", "save_issue"}, args, in.Parent)
}
func (p *Provider) validateMutationV2(names []string, args []arg, parent Field[*string]) error {
	name, _, e := p.v2Negotiation(names, args)
	if e != nil {
		return e
	}
	if parent.Present && parent.Value != nil && !p.parentSafe(name) {
		return fail("capability_unavailable")
	}
	return nil
}

func createArgsV2(in CreateInputV2) ([]arg, error) {
	if !ValidID(in.TeamID) || in.Title == "" || !textOK(in.Title, 1024) || !textOK(in.Description, MaxBodyBytes) || (in.StateID != "" && !ValidID(in.StateID)) || (in.ProjectID != "" && !ValidID(in.ProjectID)) {
		return nil, fail("invalid")
	}
	if in.Project.Present && in.ProjectID != "" && (in.Project.Value == nil || *in.Project.Value != in.ProjectID) {
		return nil, fail("invalid")
	}
	args := []arg{a(in.TeamID, "team", "teamId"), a(in.Title, "title"), a(in.Description, "description")}
	if in.ProjectID != "" && !in.Project.Present {
		args = append(args, a(in.ProjectID, "projectId", "project"))
	}
	if in.StateID != "" {
		args = append(args, a(in.StateID, "stateId", "state"))
	}
	extra, e := extraArgs(in.Project, in.Labels, in.Parent, in.Assignee)
	if e != nil {
		return nil, e
	}
	args = append(args, extra...)

	return args, nil
}
func (p *Provider) CreateV2(ctx context.Context, in CreateInputV2) (IssueV2, error) {
	args, e := createArgsV2(in)
	if e != nil {
		return IssueV2{}, e
	}
	m, e := p.mutateV2(ctx, []string{"create_issue", "save_issue"}, args, in.Parent)
	if e != nil {
		return IssueV2{}, e
	}
	x, e := p.parseIssueV2(ctx, unwrap(m, "issue"))
	if e != nil {
		return IssueV2{}, e
	}
	project := in.ProjectID
	if in.Project.Present {
		project = ""
		if in.Project.Value != nil {
			project = *in.Project.Value
		}
	}
	if x.TeamID != in.TeamID || x.ProjectID != project {
		return IssueV2{}, fail("scope_mismatch")
	}
	return x, nil
}
func updateArgsV2(in UpdateInputV2) ([]arg, error) {
	if !ValidID(in.ID) || (in.Title == nil && in.Description == nil && in.StateID == nil && !in.Project.Present && !in.Labels.Present && !in.Parent.Present && !in.Assignee.Present) {
		return nil, fail("invalid")
	}
	args := []arg{a(in.ID, "id", "issueId")}
	for _, f := range []struct {
		value *string
		max   int
		keys  []string
	}{{in.Title, 1024, []string{"title"}}, {in.Description, MaxBodyBytes, []string{"description"}}, {in.StateID, 128, []string{"stateId", "state"}}} {
		if f.value != nil {
			if !textOK(*f.value, f.max) {
				return nil, fail("invalid")
			}
			args = append(args, a(*f.value, f.keys...))
		}
	}
	if in.StateID != nil && !ValidID(*in.StateID) {
		return nil, fail("invalid")
	}
	extra, e := extraArgs(in.Project, in.Labels, in.Parent, in.Assignee)
	if e != nil {
		return nil, e
	}
	args = append(args, extra...)

	return args, nil
}
func (p *Provider) UpdateV2(ctx context.Context, in UpdateInputV2) (IssueV2, error) {
	args, e := updateArgsV2(in)
	if e != nil {
		return IssueV2{}, e
	}
	m, e := p.mutateV2(ctx, []string{"update_issue", "save_issue"}, args, in.Parent)
	if e != nil {
		return IssueV2{}, e
	}
	x, e := p.parseIssueV2(ctx, unwrap(m, "issue"))
	if e != nil {
		return IssueV2{}, e
	}
	if x.ID != in.ID {
		return IssueV2{}, fail("scope_mismatch")
	}
	return x, nil
}
func validRelation(kind, from, to string) bool {
	return (kind == "blocks" || kind == "related") && ValidID(from) && ValidID(to) && from != to
}
func (p *Provider) Relation(ctx context.Context, in RelationInput) (RelationReceipt, error) {
	if !validRelation(in.Type, in.FromID, in.ToID) || (in.Action != "add" && in.Action != "remove") {
		return RelationReceipt{}, fail("invalid")
	}
	if in.Type == "related" && in.FromID > in.ToID {
		return RelationReceipt{}, fail("invalid")
	}
	name, args, issueResult, e := p.relationTool(in)
	if e != nil {
		return RelationReceipt{}, e
	}
	m, e := p.invoke(ctx, name, args)
	if e != nil {
		return RelationReceipt{}, e
	}
	receipt := RelationReceipt{Action: in.Action, Type: in.Type, FromID: in.FromID, ToID: in.ToID}
	if issueResult {
		// save_issue returns the updated issue, not a provider edge ID. Validate
		// its canonical identity without copying its body or inventing an edge ID.
		issueID, err := entityID(unwrap(m, "issue"), "issueId")
		if err != nil {
			return RelationReceipt{}, err
		}
		if issueID != in.FromID {
			return RelationReceipt{}, fail("scope_mismatch")
		}
		return receipt, nil
	}
	m = unwrap(m, "relation")
	if raw, present := m["id"]; present {
		value, ok := raw.(string)
		if !ok || !ValidID(value) {
			return RelationReceipt{}, fail("invalid_response")
		}
		receipt.ID = value
	}
	for key, want := range map[string]string{"fromId": in.FromID, "issueId": in.FromID, "toId": in.ToID, "relatedIssueId": in.ToID, "type": in.Type, "action": in.Action} {
		if raw, present := m[key]; present {
			value, ok := raw.(string)
			if !ok || value != want {
				return RelationReceipt{}, fail("scope_mismatch")
			}
		}
	}
	return receipt, nil
}
