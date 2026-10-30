package linearprovider

import "strings"

// relationTool negotiates before dispatch. A provider error never falls through
// to another mutation. The save_issue vocabulary and semantic signatures are
// curated from the official authenticated catalog, not arbitrary array fields.
func (p *Provider) relationTool(in RelationInput) (string, map[string]any, bool, error) {
	if in.Type == "duplicate" {
		return p.duplicateTool(in)
	}
	names := []string{"create_issue_relation", "add_issue_relation"}
	if in.Action == "remove" {
		names = []string{"remove_issue_relation", "delete_issue_relation"}
	}
	args := []arg{a(in.FromID, "issueId", "fromId"), a(in.ToID, "relatedIssueId", "toId"), a(in.Type, "type")}
	if name, values, err := p.v2Negotiation(names, args); err == nil {
		return name, values, false, nil
	}

	key := "blocks"
	if in.Type == "related" {
		key = "relatedTo"
	}
	if in.Action == "remove" {
		if in.Type == "related" {
			key = "removeRelatedTo"
		} else {
			key = "removeBlocks"
		}
	}
	tool := p.tools["save_issue"]
	if tool == nil {
		return "", nil, false, fail("capability_unavailable")
	}
	props, _, err := schema(tool)
	if err != nil {
		return "", nil, false, err
	}
	property, _ := props[key].(map[string]any)
	description, _ := property["description"].(string)
	description = strings.ToLower(strings.Join(strings.Fields(description), " "))
	if in.Action == "add" {
		if !strings.HasSuffix(description, "append-only; existing relations are never removed") {
			return "", nil, false, fail("capability_unavailable")
		}
	} else {
		want := "issue ids/identifiers to stop blocking"
		if in.Type == "related" {
			want = "related issue ids/identifiers to remove"
		}
		if description != want {
			return "", nil, false, fail("capability_unavailable")
		}
	}
	name, values, err := p.v2Negotiation([]string{"save_issue"}, []arg{
		a(in.FromID, "id"),
		{aliases: []string{key}, value: []string{in.ToID}, kind: "array"},
	})
	if err != nil {
		return "", nil, false, err
	}
	return name, values, true, nil
}

// duplicateTool negotiates a duplicate edge, from the duplicate to the
// original: a relation tool whose type enum names "duplicate", else
// save_issue's duplicateOf. It is only ever added; a duplicate is undone
// by a person in Linear.
func (p *Provider) duplicateTool(in RelationInput) (string, map[string]any, bool, error) {
	if in.Action != "add" {
		return "", nil, false, fail("capability_unavailable")
	}
	for _, name := range []string{"create_issue_relation", "add_issue_relation"} {
		tool := p.tools[name]
		if tool == nil {
			continue
		}
		props, _, err := schema(tool)
		if err != nil {
			continue
		}
		kind, _ := props["type"].(map[string]any)
		choices, _ := kind["enum"].([]any)
		takes := false
		for _, c := range choices {
			takes = takes || c == "duplicate"
		}
		if !takes {
			continue
		}
		args := []arg{a(in.FromID, "issueId", "fromId"), a(in.ToID, "relatedIssueId", "toId"), a(in.Type, "type")}
		if name, values, err := p.v2Negotiation([]string{name}, args); err == nil {
			return name, values, false, nil
		}
	}
	tool := p.tools["save_issue"]
	if tool == nil {
		return "", nil, false, fail("capability_unavailable")
	}
	props, _, err := schema(tool)
	if err != nil {
		return "", nil, false, err
	}
	property, _ := props["duplicateOf"].(map[string]any)
	description, _ := property["description"].(string)
	description = strings.ToLower(strings.Join(strings.Fields(description), " "))
	if !strings.HasPrefix(description, "duplicate of issue id/identifier") {
		return "", nil, false, fail("capability_unavailable")
	}
	name, values, err := p.v2Negotiation([]string{"save_issue"}, []arg{a(in.FromID, "id"), a(in.ToID, "duplicateOf")})
	if err != nil {
		return "", nil, false, err
	}
	return name, values, true, nil
}
