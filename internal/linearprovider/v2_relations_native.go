package linearprovider

import "strings"

// relationTool negotiates before dispatch. A provider error never falls through
// to another mutation. The save_issue vocabulary and semantic signatures are
// curated from the official authenticated catalog, not arbitrary array fields.
func (p *Provider) relationTool(in RelationInput) (string, map[string]any, bool, error) {
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
