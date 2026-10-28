package linearprovider

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
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
	// UpdatedAfter is an absolute RFC 3339 time; rows updated earlier refuse
	// the page, so a filter Linear ignored cannot pass.
	UpdatedAfter string
}

// UpdatedAfterTime parses an absolute RFC 3339 time, never a duration.
func UpdatedAfterTime(value string) (time.Time, bool) {
	t, e := time.Parse(time.RFC3339Nano, value)
	return t, e == nil && len(value) <= 64
}

type IssueV2 struct {
	Issue
	ProjectKnown bool     `json:"project_known"`
	LabelIDs     []string `json:"label_ids,omitempty"`
	LabelsKnown  bool     `json:"labels_known"`
	// LabelNames holds the display names native Linear returns instead of
	// label IDs. Names never become IDs here; see LabelCatalog and
	// IssueHasLabel for the server-proved resolution.
	LabelNames []string `json:"label_names,omitempty"`
	ParentID   string   `json:"parent_id,omitempty"`
	// ParentIdentifier holds a parent reported only by its display identifier
	// (native Linear returns parentId as e.g. "TEAM-4"). It is never turned
	// into a UUID here; a reader resolves it with IssueV2 when needed.
	ParentIdentifier string `json:"parent_identifier,omitempty"`
	ParentKnown      bool   `json:"parent_known"`
	AssigneeID       string `json:"assignee_id,omitempty"`
	AssigneeKnown    bool   `json:"assignee_known"`
	// Priority is Linear's 0-4 (0 none, 1 urgent ... 4 low), set only when
	// PriorityKnown: a value of any other shape is left unknown, never guessed.
	Priority       *int       `json:"priority,omitempty"`
	PriorityKnown  bool       `json:"priority_known"`
	Relations      []Relation `json:"relations,omitempty"`
	RelationsKnown bool       `json:"relations_known"`
	// DescriptionSHA256 is set only by a single-issue board read; a caller
	// sends it back as expected_description_sha256.
	DescriptionSHA256 string `json:"description_sha256,omitempty"`
	// DescriptionKnown is true when the read returned the description as a
	// string or an explicit null, rather than omitting it.
	DescriptionKnown bool `json:"-"`
	// UpdatedAt is Linear's last-update time when the read returned it.
	UpdatedAt string `json:"updated_at,omitempty"`
}
type Label struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TeamID   string `json:"team_id,omitempty"`
	Archived bool   `json:"archived"`
	Group    bool   `json:"group"`
	Global   bool   `json:"global"`
	// FilterScoped marks a row whose scope comes from the provider's pinned
	// team filter: it is available to the queried team (team or workspace
	// label; the row itself carries no team). TeamID stays empty.
	FilterScoped     bool `json:"team_filter_scoped,omitempty"`
	EligibilityKnown bool `json:"eligibility_known"`
}

// MarshalJSON omits global for a filter-scoped row, whose own fields cannot
// tell a team label from a workspace label.
func (l Label) MarshalJSON() ([]byte, error) {
	type plain Label
	if !l.FilterScoped {
		return json.Marshal(plain(l))
	}
	return json.Marshal(struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Archived         bool   `json:"archived"`
		Group            bool   `json:"group"`
		FilterScoped     bool   `json:"team_filter_scoped"`
		EligibilityKnown bool   `json:"eligibility_known"`
	}{l.ID, l.Name, l.Archived, l.Group, true, l.EligibilityKnown})
}

type Member struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Email is the member's, when the listing gives it: for finding a
	// person by email (intake trust). It's never stored, and never in any
	// output, an agent's included.
	Email            string `json:"-"`
	TeamID           string `json:"team_id"`
	Active           bool   `json:"active"`
	EligibilityKnown bool   `json:"eligibility_known"`
}
type Capabilities struct {
	Labels                   bool `json:"labels"`
	Members                  bool `json:"members"`
	CreateProject            bool `json:"create_project"`
	UpdateProject            bool `json:"update_project"`
	ReplaceLabels            bool `json:"replace_labels"`
	Parent                   bool `json:"parent"`
	Assignee                 bool `json:"assignee"`
	ParentSameDestination    bool `json:"parent_same_destination"`
	RelationAdd              bool `json:"relation_add"`
	RelationRemove           bool `json:"relation_remove"`
	LabelEligibilityByFilter bool `json:"label_eligibility_by_filter"`
	// LabelMembershipProof: issue label IDs can be proven when reads return
	// only names (full label listing plus list_issues filtered by label ID).
	LabelMembershipProof bool `json:"label_membership_proof"`
	// UpdatedAfterFilter: list_issues filters by "updated after" and returns
	// each row's updatedAt, so the filter can be checked row by row.
	UpdatedAfterFilter bool `json:"updated_after_filter"`
	// CommentsRead: list_comments pins the issue parent, page size,
	// creation-time order and cursor, and documents onBehalfOf.
	CommentsRead bool `json:"comments_read"`
	// CommentReply: the comment write accepts parentId, and comments can be
	// read to prove the parent is on the same issue and to confirm a reply.
	CommentReply bool `json:"comment_reply"`
	// MemberTeamFilter and StatusTeamFilter: the member and workflow-state
	// listings document their team filter, so rows that carry no team of
	// their own belong to the team asked for.
	MemberTeamFilter bool `json:"member_team_filter"`
	StatusTeamFilter bool `json:"status_team_filter"`
	// LabelDeltas: an issue update can add or remove single labels by ID
	// (native addLabels/removeLabels) without replacing the set.
	LabelDeltas bool `json:"label_deltas"`
	// Reasons explains, per capability key, why a capability is off, so a
	// silent switch-off (for example after a provider schema change) is
	// visible in status and doctor output.
	Reasons map[string]string `json:"reasons,omitempty"`
}
type CreateInputV2 struct {
	CreateInput
	Project          Field[*string]
	Labels           Field[[]string]
	Parent, Assignee Field[*string]
	// parentVerified is set only through WithSameDestinationParent. No wire
	// argument, schema, flag or environment maps to it, and it is never
	// serialized.
	parentVerified bool
}
type UpdateInputV2 struct {
	UpdateInput
	Project          Field[*string]
	Labels           Field[[]string]
	Parent, Assignee Field[*string]
	// AddLabels and RemoveLabels are label UUIDs added to or removed from
	// the current set (native addLabels/removeLabels); never with Labels.
	AddLabels, RemoveLabels []string
	parentVerified          bool
}

// WithSameDestinationParent marks a parent set that the caller has verified,
// from its own reads, to share the child's team and project. Only the task
// board calls it, after that verification.
func (in CreateInputV2) WithSameDestinationParent() CreateInputV2 {
	in.parentVerified = true
	return in
}

// WithSameDestinationParent is the update form of CreateInputV2's marker.
func (in UpdateInputV2) WithSameDestinationParent() UpdateInputV2 {
	in.parentVerified = true
	return in
}

type Relation struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"`
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
	// Native Linear may name an endpoint only by its display identifier; the
	// matching UUID field is then empty. Readers resolve it against issues
	// they have read and never treat it as scope.
	FromIdentifier string `json:"from_identifier,omitempty"`
	ToIdentifier   string `json:"to_identifier,omitempty"`
}
type RelationInput struct{ Action, Type, FromID, ToID string }
type RelationReceipt struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
	ID     string `json:"id,omitempty"`
	// Confirmed is true only when Linear's response itself names this edge's
	// from, to and type; otherwise the caller must confirm it by a read.
	Confirmed bool `json:"-"`
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
	case bool:
		kind = "boolean"
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
		// Native get_issue returns relations only when asked: without
		// includeRelations no edge is ever read back, so a relation write
		// can never be verified.
		if name == "get_issue" {
			if prop, ok := props["includeRelations"].(map[string]any); ok && prop["type"] == "boolean" {
				values["includeRelations"] = true
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
	// Field shapes, descriptions and schema extensions never prove that a
	// parent change preserves the destination. This capability only says the
	// provider can express a parent beside a project and read issues back;
	// the board proves preservation by allowing same-destination parents only.
	shape := false
	for _, name := range []string{"create_issue", "update_issue", "save_issue"} {
		shape = shape || p.parentShape(name)
	}
	capabilities.ParentSameDestination = shape && p.tools["get_issue"] != nil
	reason := p.labelFilterReason()
	if !capabilities.Labels {
		reason = "label listing is unavailable"
	}
	capabilities.LabelEligibilityByFilter = reason == ""
	capabilities.Reasons = map[string]string{}
	if reason != "" {
		capabilities.Reasons["label_eligibility_by_filter"] = reason
	}
	membership := p.labelMembershipReason()
	capabilities.LabelMembershipProof = membership == ""
	if membership != "" {
		capabilities.Reasons["label_membership_proof"] = membership
	}
	for key, reason := range map[string]string{"member_team_filter": p.memberTeamFilterReason(), "status_team_filter": p.statusTeamFilterReason()} {
		if reason != "" {
			capabilities.Reasons[key] = reason
		}
	}
	capabilities.MemberTeamFilter = capabilities.Reasons["member_team_filter"] == ""
	capabilities.StatusTeamFilter = capabilities.Reasons["status_team_filter"] == ""
	deltas := p.labelDeltasReason()
	capabilities.LabelDeltas = deltas == ""
	if deltas != "" {
		capabilities.Reasons["label_deltas"] = deltas
	}
	updatedAfter := p.updatedAfterReason()
	capabilities.UpdatedAfterFilter = updatedAfter == ""
	if updatedAfter != "" {
		capabilities.Reasons["updated_after_filter"] = updatedAfter
	}
	comments := p.commentsReason()
	capabilities.CommentsRead = comments == ""
	if comments != "" {
		capabilities.Reasons["comments_read"] = comments
	}
	reply := p.commentReplyReason()
	capabilities.CommentReply = reply == ""
	if reply != "" {
		capabilities.Reasons["comment_reply"] = reply
	}
	switch {
	case !shape:
		capabilities.Reasons["parent_same_destination"] = "no issue mutation tool accepts parentId beside project"
	case p.tools["get_issue"] == nil:
		capabilities.Reasons["parent_same_destination"] = "get_issue is not advertised"
	}
	if len(capabilities.Reasons) == 0 {
		capabilities.Reasons = nil
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
	switch m["description"].(type) {
	case string, nil:
		_, x.DescriptionKnown = m["description"]
	}
	x.LabelIDs, x.LabelsKnown, e = parseLabelIDs(m)
	if e == nil && !x.LabelsKnown {
		x.LabelNames = parseLabelNames(m)
	}
	if e != nil {
		return IssueV2{}, e
	}
	x.UpdatedAt = str(m, "updatedAt")
	x.ParentID, x.ParentIdentifier, x.ParentKnown, e = parseParentV2(m)
	if e != nil {
		return IssueV2{}, e
	}
	x.AssigneeID, x.AssigneeKnown, e = optionalCanonical(m, "assigneeId", "assignee")
	if e != nil {
		return IssueV2{}, e
	}
	x.Priority, x.PriorityKnown = parsePriority(m["priority"])
	// Only an explicitly complete typed edge list proves absence. Unrecognized
	// legacy linked-context forms retain RelationsKnown=false.
	if raw, present := m["issueRelations"]; present {
		rows, ok := raw.([]any)
		if !ok || len(rows) > 250 {
			return IssueV2{}, fail("invalid_response")
		}
		skipped := false
		for _, row := range rows {
			node, ok := row.(map[string]any)
			if !ok {
				return IssueV2{}, fail("invalid_response")
			}
			relation := Relation{Type: str(node, "type"), FromID: str(node, "fromId"), ToID: str(node, "toId"), ID: str(node, "id")}
			if relation.ID != "" && !ValidID(relation.ID) {
				return IssueV2{}, fail("invalid_response")
			}
			if relation.Type != "" && relation.Type != "blocks" && relation.Type != "related" && validRelation("related", relation.FromID, relation.ToID) {
				// A well-formed edge of an unsupported type (e.g. duplicate)
				// is not presented, so the list can no longer prove absence.
				skipped = true
				continue
			}
			if !validRelation(relation.Type, relation.FromID, relation.ToID) {
				return IssueV2{}, fail("invalid_response")
			}
			x.Relations = append(x.Relations, relation)
		}
		complete, ok := m["relationsComplete"].(bool)
		x.RelationsKnown = ok && complete && !skipped
	}
	// Native get_issue returns relations as an object of per-kind arrays. It
	// proves the listed edges exist but carries no completeness flag, so it
	// never makes RelationsKnown true: empty arrays are not an absence proof.
	if native, ok := m["relations"].(map[string]any); ok {
		x.Relations = append(x.Relations, nativeRelationsV2(x.Issue, native)...)
	}
	return x, nil
}

// nativeRelationsV2 reads blocks, blockedBy and relatedTo. An endpoint may be
// a UUID, a display identifier or an object carrying either; an element in any
// other form, and duplicateOf, are skipped rather than failing the issue read.
func nativeRelationsV2(self Issue, native map[string]any) []Relation {
	ref := func(v any) (string, string) {
		switch item := v.(type) {
		case string:
			if ValidID(item) {
				return item, ""
			}
			if ValidIssueIdentifier(item) {
				return "", item
			}
		case map[string]any:
			id, identifier := str(item, "uuid"), str(item, "identifier")
			if raw := str(item, "id"); ValidID(raw) && id == "" {
				id = raw
			} else if ValidIssueIdentifier(raw) && identifier == "" {
				identifier = raw
			}
			if id != "" && !ValidID(id) {
				return "", ""
			}
			if identifier != "" && !ValidIssueIdentifier(identifier) {
				identifier = ""
			}
			return id, identifier
		}
		return "", ""
	}
	var out []Relation
	for _, kind := range []struct {
		key, relation string
		outgoing      bool
	}{{"blocks", "blocks", true}, {"blockedBy", "blocks", false}, {"relatedTo", "related", true}} {
		rows, _ := native[kind.key].([]any)
		if len(rows) > 250 {
			rows = rows[:0]
		}
		for _, row := range rows {
			id, identifier := ref(row)
			if (id == "" && identifier == "") || (id != "" && id == self.ID) || (identifier != "" && identifier == self.Identifier) {
				continue
			}
			edge := Relation{Type: kind.relation, FromID: self.ID, FromIdentifier: self.Identifier, ToID: id, ToIdentifier: identifier}
			if !kind.outgoing {
				edge = Relation{Type: kind.relation, FromID: id, FromIdentifier: identifier, ToID: self.ID, ToIdentifier: self.Identifier}
			}
			out = append(out, edge)
		}
	}
	return out
}

// IssueV2 reads one issue by UUID or by display identifier. An identifier read
// must return that exact identifier together with a canonical UUID.
func (p *Provider) IssueV2(ctx context.Context, id string) (IssueV2, error) {
	byIdentifier := !ValidID(id)
	if byIdentifier && !ValidIssueIdentifier(id) {
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
	if (byIdentifier && x.Identifier != id) || (!byIdentifier && x.ID != id) {
		return IssueV2{}, fail("scope_mismatch")
	}
	return x, nil
}

var issueIdentifier = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-[1-9][0-9]{0,9}$`)

// ValidIssueIdentifier reports whether s is a Linear issue display identifier
// such as "TEAM-123". It names an issue but never supplies scope.
func ValidIssueIdentifier(s string) bool { return issueIdentifier.MatchString(s) }

// parseParentV2 reads parentId/parent. A canonical UUID, a display identifier
// and explicit null are all known; a descriptive string parent without
// parentId stays unknown; anything else is invalid.
func parseParentV2(m map[string]any) (string, string, bool, error) {
	var uuidRef, identifierRef string
	known := false
	take := func(v any) error {
		if v == nil {
			if uuidRef != "" || identifierRef != "" {
				return fail("scope_mismatch")
			}
			known = true
			return nil
		}
		s, ok := v.(string)
		switch {
		case ok && ValidID(s):
			if uuidRef != "" && uuidRef != s {
				return fail("scope_mismatch")
			}
			uuidRef = s
		case ok && ValidIssueIdentifier(s):
			if identifierRef != "" && identifierRef != s {
				return fail("scope_mismatch")
			}
			identifierRef = s
		default:
			return fail("invalid_response")
		}
		known = true
		return nil
	}
	flat, hasFlat := m["parentId"]
	if hasFlat {
		if e := take(flat); e != nil {
			return "", "", false, e
		}
	}
	switch nested := m["parent"].(type) {
	case nil:
		if _, present := m["parent"]; present {
			if e := take(nil); e != nil {
				return "", "", false, e
			}
		}
	case string:
		// A descriptive label carries no identity and cannot supply one.
	case map[string]any:
		for _, key := range []string{"id", "uuid", "identifier"} {
			if v, present := nested[key]; present {
				if e := take(v); e != nil {
					return "", "", false, e
				}
			}
		}
	default:
		return "", "", false, fail("invalid_response")
	}
	if !hasFlat {
		if _, label := m["parent"].(string); label {
			return "", "", false, nil
		}
	}
	return uuidRef, identifierRef, known, nil
}
func (p *Provider) IssuesV2(ctx context.Context, q IssueQueryV2) (Page[IssueV2], error) {
	if !ValidID(q.TeamID) || (q.ProjectID != "" && !ValidID(q.ProjectID)) || (q.LabelID != "" && !ValidID(q.LabelID)) {
		return Page[IssueV2]{}, fail("invalid")
	}
	var after time.Time
	if q.UpdatedAfter != "" {
		var ok bool
		if after, ok = UpdatedAfterTime(q.UpdatedAfter); !ok {
			return Page[IssueV2]{}, fail("invalid")
		}
		if p.updatedAfterReason() != "" {
			return Page[IssueV2]{}, fail("capability_unavailable")
		}
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
	if q.UpdatedAfter != "" {
		args = append(args, a(after.UTC().Format(time.RFC3339Nano), "updatedAt"))
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
		if pe, ok := e.(*Error); ok && pe.Kind == "scope_metadata_missing" {
			// Native rows carry a display id beside the uuid.
			if uuid, idErr := id(row); idErr == nil {
				x, e = p.IssueV2(ctx, uuid)
			}
		}
		if e != nil {
			return Page[IssueV2]{}, e
		}
		if x.TeamID != q.TeamID || (q.ProjectID != "" && x.ProjectID != q.ProjectID) {
			return Page[IssueV2]{}, fail("scope_mismatch")
		}
		if q.UpdatedAfter != "" {
			if updated, ok := UpdatedAfterTime(x.UpdatedAt); !ok || updated.Before(after) {
				return Page[IssueV2]{}, fail("scope_mismatch")
			}
		}
		if q.LabelID != "" && !x.LabelsKnown {
			// Native rows carry label names only; the pinned server filter,
			// sent with the UUID, is the membership proof for this page.
			if p.labelFilterProofReason() != "" {
				return Page[IssueV2]{}, fail("capability_unavailable")
			}
		} else if q.LabelID != "" {
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

// labelFilterProof reports whether list_issue_labels advertises exactly the
// documented filters that make eligibility provable without per-row team or
// group fields: a team scope, and includeArchived/includeGroups booleans that
// default to false. Any drift in names, types, defaults or the documented
// meaning disables it, and labels then stay unproven as before. This is an
// inference from Linear's documented list filters, pinned by schema.
func (p *Provider) labelFilterProof() bool { return p.labelFilterReason() == "" }

// labelFilterReason returns "" when the pinned contract holds, otherwise a
// short, credential-free description of the first difference.
func (p *Provider) labelFilterReason() string {
	t := p.tools["list_issue_labels"]
	if t == nil {
		return "list_issue_labels is not advertised"
	}
	if !strings.Contains(t.Description, "retiredAt") || !strings.Contains(t.Description, "cannot be applied") {
		return "list_issue_labels schema changed: the retiredAt rule is no longer documented"
	}
	props, _, e := schema(t)
	if e != nil {
		return "list_issue_labels schema is unreadable"
	}
	if team, ok := props["team"].(map[string]any); !ok || team["type"] != "string" {
		return "list_issue_labels schema changed: team filter missing or not a string"
	}
	meanings := map[string][]string{"includeArchived": {"Include archived"}, "includeGroups": {"Include label groups", "isGroup", "cannot be applied directly"}}
	for _, name := range []string{"includeArchived", "includeGroups"} {
		filter, ok := props[name].(map[string]any)
		if !ok || filter["type"] != "boolean" || filter["default"] != false {
			return "list_issue_labels schema changed: " + name + " missing or no longer a boolean defaulting to false"
		}
		text, _ := filter["description"].(string)
		for _, want := range meanings[name] {
			if !strings.Contains(text, want) {
				return "list_issue_labels schema changed: " + name + " no longer has its documented meaning"
			}
		}
	}
	return ""
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
	byFilter := p.labelFilterProof()
	if byFilter {
		args = append(args, arg{[]string{"includeArchived"}, false, "boolean"}, arg{[]string{"includeGroups"}, false, "boolean"})
	}
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
		if byFilter {
			// includeGroups=false and includeArchived=false were sent
			// explicitly. A group row contradicts the filter; a row without
			// its own team is available to the queried team by the filter.
			if group {
				return Page[Label]{}, fail("scope_mismatch")
			}
			groupKnown, archivedKnown = true, true
			if !scopeKnown {
				label.FilterScoped = true
				scopeKnown = true
			}
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
		teamKnown := flat || nested
		if teamKnown {
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
		// A row without its own team belongs to the team only through the
		// documented filter; without it the member is listed, never eligible.
		if !teamKnown && p.memberTeamFilterReason() != "" {
			known = false
		}
		out.Items = append(out.Items, Member{ID: i, Name: str(row, "name"), Email: str(row, "email"), TeamID: team, Active: active, EligibilityKnown: known})
	}
	return out, nil
}

func (p *Provider) parentShape(name string) bool {
	t := p.tools[name]
	if t == nil {
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

// parentSafe allows a parent set only when the caller marked it as verified
// same-destination and the tool can express parent beside project.
func (p *Provider) parentSafe(name string, verified bool) bool {
	return verified && p.parentShape(name)
}
func (p *Provider) mutateV2(ctx context.Context, names []string, args []arg, parent Field[*string], verified bool) (map[string]any, error) {
	name, values, e := p.v2Negotiation(names, args)
	if e != nil {
		return nil, e
	}
	if parent.Present && parent.Value != nil && !p.parentSafe(name, verified) {
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
	return p.validateMutationV2([]string{"create_issue", "save_issue"}, args, in.Parent, in.parentVerified)
}
func (p *Provider) ValidateUpdateV2(in UpdateInputV2) error {
	args, e := updateArgsV2(in)
	if e != nil {
		return e
	}
	if (in.AddLabels != nil || in.RemoveLabels != nil) && p.labelDeltasReason() != "" {
		return fail("capability_unavailable")
	}
	return p.validateMutationV2([]string{"update_issue", "save_issue"}, args, in.Parent, in.parentVerified)
}
func (p *Provider) validateMutationV2(names []string, args []arg, parent Field[*string], verified bool) error {
	name, _, e := p.v2Negotiation(names, args)
	if e != nil {
		return e
	}
	if parent.Present && parent.Value != nil && !p.parentSafe(name, verified) {
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
	m, e := p.mutateV2(ctx, []string{"create_issue", "save_issue"}, args, in.Parent, in.parentVerified)
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
	if !ValidID(in.ID) || (in.Title == nil && in.Description == nil && in.StateID == nil && !in.Project.Present && !in.Labels.Present && !in.Parent.Present && !in.Assignee.Present && in.AddLabels == nil && in.RemoveLabels == nil) {
		return nil, fail("invalid")
	}
	if in.Labels.Present && (in.AddLabels != nil || in.RemoveLabels != nil) {
		return nil, fail("invalid")
	}
	args := []arg{a(in.ID, "id", "issueId")}
	for _, delta := range []struct {
		ids []string
		key string
	}{{in.AddLabels, "addLabels"}, {in.RemoveLabels, "removeLabels"}} {
		if delta.ids == nil {
			continue
		}
		ids, e := canonicalIDs(delta.ids, 20)
		if e != nil || len(ids) == 0 {
			return nil, fail("invalid")
		}
		args = append(args, arg{[]string{delta.key}, ids, "array"})
	}
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
	if (in.AddLabels != nil || in.RemoveLabels != nil) && p.labelDeltasReason() != "" {
		return IssueV2{}, fail("capability_unavailable")
	}
	m, e := p.mutateV2(ctx, []string{"update_issue", "save_issue"}, args, in.Parent, in.parentVerified)
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
	named := map[string]bool{}
	for key, want := range map[string]string{"fromId": in.FromID, "issueId": in.FromID, "toId": in.ToID, "relatedIssueId": in.ToID, "type": in.Type, "action": in.Action} {
		if raw, present := m[key]; present {
			value, ok := raw.(string)
			if !ok || value != want {
				return RelationReceipt{}, fail("scope_mismatch")
			}
			named[key] = true
		}
	}
	receipt.Confirmed = (named["fromId"] || named["issueId"]) && (named["toId"] || named["relatedIssueId"]) && named["type"]
	return receipt, nil
}

// parseLabelNames returns the label display names of a native issue whose
// labels field is a plain string array; any other shape yields nil.
func parseLabelNames(m map[string]any) []string {
	rows, ok := m["labels"].([]any)
	if !ok || len(rows) == 0 || len(rows) > 100 {
		return nil
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		name, ok := row.(string)
		if !ok || name == "" || len(name) > 512 {
			return nil
		}
		names = append(names, name)
	}
	return names
}

// labelMembershipReason returns "" when label IDs can be proven from names:
// list_issue_labels can include archived labels and groups, and list_issues
// filters by a label ID ("Label name or ID") within a team and project. A
// name is never sent; the board sends only canonical label UUIDs.
func (p *Provider) labelMembershipReason() string {
	catalog, issues := p.tools["list_issue_labels"], p.tools["list_issues"]
	if catalog == nil || issues == nil {
		return "list_issue_labels or list_issues is not advertised"
	}
	props, _, e := schema(catalog)
	if e != nil {
		return "list_issue_labels schema is unreadable"
	}
	for _, name := range []string{"includeArchived", "includeGroups"} {
		if filter, ok := props[name].(map[string]any); !ok || filter["type"] != "boolean" {
			return "list_issue_labels schema changed: " + name + " is not a boolean"
		}
	}
	return p.labelFilterProofReason()
}

// memberTeamFilterReason is empty when the member listing filters by team:
// list_team_members by its name, or list_users by a team filter documented
// as returning only that team's members.
func (p *Provider) memberTeamFilterReason() string {
	name := "list_users"
	if p.tools[name] == nil {
		name = "list_team_members"
	}
	if p.tools[name] == nil {
		return "no member listing tool is advertised"
	}
	props, _, e := schema(p.tools[name])
	if e != nil {
		return name + " schema is unreadable"
	}
	team, ok := props["team"].(map[string]any)
	if !ok {
		team, ok = props["teamId"].(map[string]any)
	}
	text, _ := team["description"].(string)
	if !ok || team["type"] != "string" || (name == "list_users" && !strings.Contains(text, "Only return members of this team")) {
		return name + " schema changed: team filter missing or no longer documented as returning only that team's members"
	}
	return ""
}

// statusTeamFilterReason is empty when list_issue_statuses is documented as
// listing one team's statuses, selected by a string team filter.
func (p *Provider) statusTeamFilterReason() string {
	t := p.tools["list_issue_statuses"]
	if t == nil {
		return "list_issue_statuses is not advertised"
	}
	props, _, e := schema(t)
	if e != nil {
		return "list_issue_statuses schema is unreadable"
	}
	team, ok := props["team"].(map[string]any)
	if !ok {
		team, ok = props["teamId"].(map[string]any)
	}
	if !ok || team["type"] != "string" || !strings.Contains(t.Description, "in a Linear team") {
		return "list_issue_statuses schema changed: no longer documented as one team's statuses"
	}
	return ""
}

// labelDeltasReason is empty when the issue update tool documents addLabels
// as an append-only array of label strings and removeLabels as an array that
// removes them, so one label can change without replacing the set.
func (p *Provider) labelDeltasReason() string {
	for _, name := range []string{"update_issue", "save_issue"} {
		if p.tools[name] == nil {
			continue
		}
		props, _, e := schema(p.tools[name])
		if e != nil {
			continue
		}
		add, addOK := props["addLabels"].(map[string]any)
		remove, removeOK := props["removeLabels"].(map[string]any)
		if !addOK && !removeOK {
			continue
		}
		addText, _ := add["description"].(string)
		removeText, _ := remove["description"].(string)
		if !addOK || !removeOK || add["type"] != "array" || remove["type"] != "array" || !allowsArrayStrings(add) || !allowsArrayStrings(remove) || !strings.Contains(addText, "Append-only") || !strings.Contains(removeText, "to remove") {
			return name + " schema changed: addLabels/removeLabels missing or no longer documented as add-only and remove arrays"
		}
		return ""
	}
	return "no issue update tool accepts addLabels and removeLabels"
}

// updatedAfterReason is empty when list_issues pins the documented
// "Updated after" string filter and can return each row's updatedAt.
func (p *Provider) updatedAfterReason() string {
	issues := p.tools["list_issues"]
	if issues == nil {
		return "list_issues is not advertised"
	}
	props, _, e := schema(issues)
	if e != nil {
		return "list_issues schema is unreadable"
	}
	filter, ok := props["updatedAt"].(map[string]any)
	if text, _ := filter["description"].(string); !ok || filter["type"] != "string" || !strings.Contains(text, "Updated after") {
		return "list_issues schema changed: updatedAt filter missing or no longer documented as updated after"
	}
	return ""
}

// labelFilterProofReason is empty when list_issues pins the documented
// "Label name or ID" filter, so a list filtered by a label UUID proves that
// every returned issue carries that label.
func (p *Provider) labelFilterProofReason() string {
	issues := p.tools["list_issues"]
	if issues == nil {
		return "list_issues is not advertised"
	}
	props, _, e := schema(issues)
	if e != nil {
		return "list_issues schema is unreadable"
	}
	label, ok := props["label"].(map[string]any)
	if text, _ := label["description"].(string); !ok || label["type"] != "string" || !strings.Contains(text, "Label name or ID") {
		return "list_issues schema changed: label filter missing or no longer documented as name or ID"
	}
	for _, name := range []string{"team", "project"} {
		if filter, ok := props[name].(map[string]any); !ok || filter["type"] != "string" {
			return "list_issues schema changed: " + name + " filter missing"
		}
	}
	if filter, ok := props["includeArchived"].(map[string]any); !ok || filter["type"] != "boolean" {
		return "list_issues schema changed: includeArchived is not a boolean"
	}
	return ""
}

// LabelName is one row of a team's complete label listing.
type LabelName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LabelCatalog reads one page of every label available to the team,
// including archived labels and groups, so name uniqueness is checked over
// everything an issue could carry. It never proves eligibility.
func (p *Provider) LabelCatalog(ctx context.Context, team string, o PageOptions) (Page[LabelName], error) {
	if !ValidID(team) || p.labelMembershipReason() != "" {
		return Page[LabelName]{}, fail("capability_unavailable")
	}
	args, e := pageArgs(o)
	if e != nil {
		return Page[LabelName]{}, e
	}
	args = append(args, a(team, "team", "teamId"), arg{[]string{"includeArchived"}, true, "boolean"}, arg{[]string{"includeGroups"}, true, "boolean"})
	m, e := p.v2Call(ctx, []string{"list_issue_labels"}, args...)
	if e != nil {
		return Page[LabelName]{}, e
	}
	rows, cursor, complete, e := list(m, "labels")
	if e != nil {
		return Page[LabelName]{}, e
	}
	out := Page[LabelName]{Items: []LabelName{}, NextCursor: cursor, Complete: complete}
	for _, row := range rows {
		i, e := entityID(row, "labelId")
		if e != nil {
			return Page[LabelName]{}, e
		}
		out.Items = append(out.Items, LabelName{ID: i, Name: str(row, "name")})
	}
	return out, nil
}

// labelMembershipPages bounds one membership check to 4 pages of 250 issues.
const labelMembershipPages = 4

// IssueHasLabel asks list_issues, filtered by the label's canonical UUID in
// the issue's team (and project, when it has one), whether the issue is
// returned. complete=false means the bound was reached without an answer,
// which callers must treat as unknown.
func (p *Provider) IssueHasLabel(ctx context.Context, issue, team, project, label string) (found, complete bool, err error) {
	if !ValidID(issue) || !ValidID(team) || !ValidID(label) || (project != "" && !ValidID(project)) {
		return false, false, fail("invalid")
	}
	if p.labelMembershipReason() != "" {
		return false, false, fail("capability_unavailable")
	}
	cursor := ""
	for page := 0; page < labelMembershipPages; page++ {
		args := []arg{{[]string{"limit"}, 250, "integer"}, a(team, "team"), a(label, "label"), arg{[]string{"includeArchived"}, true, "boolean"}}
		if project != "" {
			args = append(args, a(project, "project"))
		}
		if cursor != "" {
			args = append(args, a(cursor, "cursor"))
		}
		m, e := p.v2Call(ctx, []string{"list_issues"}, args...)
		if e != nil {
			return false, false, e
		}
		rows, next, done, e := list(m, "issues")
		if e != nil {
			return false, false, e
		}
		for _, row := range rows {
			i, e := id(row)
			if e != nil {
				return false, false, e
			}
			if i == issue {
				return true, true, nil
			}
		}
		if done {
			return false, true, nil
		}
		if next == "" || next == cursor {
			return false, false, fail("pagination_unavailable")
		}
		cursor = next
	}
	return false, false, nil
}

// parsePriority reads Linear's issue priority: a whole number 0-4, bare or
// as {value}. Anything else, null included, is unknown.
func parsePriority(raw any) (*int, bool) {
	if node, ok := raw.(map[string]any); ok {
		raw = node["value"]
	}
	n, ok := raw.(float64)
	if !ok || n != float64(int(n)) || n < 0 || n > 4 {
		return nil, false
	}
	p := int(n)
	return &p, true
}
