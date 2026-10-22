package linearprovider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const uid = "940a5670-8981-49b1-9b3c-f9773dc63052"

func issueResult() map[string]any {
	return map[string]any{"id": iid, "teamId": tid, "projectId": nil, "description": "main authorized body"}
}
func v2MutationTool(name string, nullable bool) *mcp.Tool {
	props := map[string]any{}
	for _, key := range []string{"id", "teamId", "projectId", "title", "description", "stateId", "parentId", "assigneeId"} {
		kind := any("string")
		if nullable && (key == "projectId" || key == "parentId" || key == "assigneeId") {
			kind = []any{"string", "null"}
		}
		props[key] = map[string]any{"type": kind}
	}
	props["labelIds"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	required := []string{}
	if name == "create_issue" {
		required = []string{"teamId", "title"}
	} else {
		required = []string{"id"}
	}
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": props, "required": required}}
}
func TestV2KnownnessAndCanonicalContext(t *testing.T) {
	result := issueResult()
	result["parentId"] = nil
	result["assigneeId"] = uid
	result["assignee"] = "display label"
	result["labels"] = []any{map[string]any{"id": tid, "description": "nested secret"}}
	result["issueRelations"] = []any{map[string]any{"type": "blocks", "fromId": iid, "toId": uid}}
	result["relationsComplete"] = true
	f := &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"})}, result: result}
	p, _ := NewWithCaller(context.Background(), f)
	x, e := p.IssueV2(context.Background(), iid)
	if e != nil {
		t.Fatal(e)
	}
	if !x.ProjectKnown || !x.ParentKnown || x.ParentID != "" || !x.AssigneeKnown || x.AssigneeID != uid || !x.LabelsKnown || len(x.LabelIDs) != 1 || !x.RelationsKnown || len(x.Relations) != 1 {
		t.Fatalf("wrong knownness %+v", x)
	}
	encoded, _ := json.Marshal(x)
	if strings.Contains(string(encoded), "nested secret") {
		t.Fatal("nested content leaked")
	}
	f.result = issueResult()
	x, e = p.IssueV2(context.Background(), iid)
	if e != nil || x.LabelsKnown || x.ParentKnown || x.AssigneeKnown || x.RelationsKnown {
		t.Fatal("unknown inferred empty", x, e)
	}
	result = issueResult()
	result["labels"] = []any{"display label"}
	f.result = result
	x, e = p.IssueV2(context.Background(), iid)
	if e != nil || x.LabelsKnown {
		t.Fatal("labelname supplied ID", x, e)
	}
	result["labelIds"] = nil
	if _, e = p.IssueV2(context.Background(), iid); e == nil {
		t.Fatal("null label array accepted")
	}
	result = issueResult()
	result["parentId"] = uid
	result["parent"] = map[string]any{"id": tid}
	f.result = result
	if x, e = p.IssueV2(context.Background(), iid); e == nil || x.Description != "" {
		t.Fatal("conflicting parent exposed body", x, e)
	}
}
func TestV2UnsupportedRelationTypeIsSkippedAsIncomplete(t *testing.T) {
	result := issueResult()
	result["issueRelations"] = []any{
		map[string]any{"type": "duplicate", "fromId": iid, "toId": uid},
		map[string]any{"type": "blocks", "fromId": iid, "toId": uid},
	}
	result["relationsComplete"] = true
	f := &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"})}, result: result}
	p, _ := NewWithCaller(context.Background(), f)
	x, e := p.IssueV2(context.Background(), iid)
	if e != nil || x.Description == "" || x.RelationsKnown || len(x.Relations) != 1 || x.Relations[0].Type != "blocks" {
		t.Fatal("unsupported relation type made the issue unreadable or looked complete", x, e)
	}
	result["issueRelations"] = []any{map[string]any{"type": "duplicate", "fromId": iid, "toId": "not-a-uuid"}}
	if _, e = p.IssueV2(context.Background(), iid); e == nil {
		t.Fatal("malformed unsupported relation accepted")
	}
}
func TestV2PresenceNullAndNoUnknownFieldMutation(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{v2MutationTool("update_issue", true)}, result: issueResult()}
	p, _ := NewWithCaller(context.Background(), f)
	in := UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Labels: Field[[]string]{Present: true, Value: []string{}}, Parent: Field[*string]{Present: true}, Assignee: Field[*string]{Present: true}, Project: Field[*string]{Present: true}}
	if _, e := p.UpdateV2(context.Background(), in); e != nil {
		t.Fatal(e)
	}
	args := f.args[0]
	for _, key := range []string{"projectId", "parentId", "assigneeId"} {
		if value, present := args[key]; !present || value != nil {
			t.Fatal("explicitclear lost", args)
		}
	}
	if ids, ok := args["labelIds"].([]string); !ok || ids == nil || len(ids) != 0 {
		t.Fatal("clear labels lost", args)
	}
	if len(args) != 5 {
		t.Fatal("omitted fields changed", args)
	}
	f.calls = nil
	in.Labels.Value = nil
	if _, e := p.UpdateV2(context.Background(), in); e == nil || len(f.calls) != 0 {
		t.Fatal("null labels dispatched")
	}
	f.tools = []*mcp.Tool{v2MutationTool("update_issue", false)}
	p, _ = NewWithCaller(context.Background(), f)
	if _, e := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Assignee: Field[*string]{Present: true}}); e == nil || len(f.calls) != 0 {
		t.Fatal("unsupported clear dispatched")
	}
	parent := uid
	if _, e := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Parent: Field[*string]{Present: true, Value: &parent}}); e == nil || len(f.calls) != 0 {
		t.Fatal("unproved hierarchy semantics dispatched")
	}
	f.tools = []*mcp.Tool{v2MutationTool("update_issue", true)}
	p, _ = NewWithCaller(context.Background(), f)
	if _, e := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Parent: Field[*string]{Present: true, Value: &parent}}.WithSameDestinationParent()); e != nil || len(f.calls) != 1 {
		t.Fatal("explicit supported parent failed", e, f.calls)
	}
}
func TestV2SchemaCannotAttestParentDestination(t *testing.T) {
	tools := []*mcp.Tool{}
	for _, name := range []string{"create_issue", "update_issue"} {
		tool := v2MutationTool(name, true)
		tool.InputSchema.(map[string]any)["x-clawbridge-parent-preserves-destination"] = true
		tools = append(tools, tool)
	}
	save := officialSaveIssue(t)
	schemaProperty(save.InputSchema)["x-clawbridge-parent-preserves-destination"] = true
	f := &fake{tools: append(tools, save), result: issueResult()}
	p, _ := NewWithCaller(context.Background(), f)
	caps, e := p.Capabilities(context.Background())
	if e != nil || !caps.Parent || caps.ParentSameDestination {
		t.Fatal("schema attested parent destination", caps, e)
	}
	parent := uid
	if e = p.ValidateCreateV2(CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "ok"}, Parent: Field[*string]{Present: true, Value: &parent}}); e == nil {
		t.Fatal("schema-attested parent create preflight succeeded")
	}
	if _, e = p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Parent: Field[*string]{Present: true, Value: &parent}}); e == nil || len(f.calls) != 0 {
		t.Fatal("schema-attested parent dispatched", e, f.calls)
	}
}
func TestV2TeamScopedDiscoveryAndEligibility(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{tool("list_issue_labels", map[string]string{"team": "string", "limit": "number"}), tool("list_users", map[string]string{"team": "string", "limit": "number"})}, result: map[string]any{"labels": []any{map[string]any{"id": tid, "teamId": tid, "name": "Migration", "archived": false, "isGroup": false}}, "hasNextPage": false}}
	p, _ := NewWithCaller(context.Background(), f)
	labels, e := p.Labels(context.Background(), tid, PageOptions{})
	if e != nil || !labels.Complete || !labels.Items[0].EligibilityKnown {
		t.Fatal(labels, e)
	}
	if f.args[0]["team"] != tid {
		t.Fatal("global label search")
	}
	f.result = map[string]any{"users": []any{map[string]any{"id": uid, "active": true}}, "hasNextPage": false}
	members, e := p.Members(context.Background(), tid, PageOptions{})
	if e != nil || !members.Items[0].EligibilityKnown || !members.Items[0].Active || members.Items[0].TeamID != tid {
		t.Fatal(members, e)
	}
	f.result = map[string]any{"users": []any{map[string]any{"id": uid}}, "hasNextPage": false}
	members, e = p.Members(context.Background(), tid, PageOptions{})
	if e != nil || members.Items[0].EligibilityKnown {
		t.Fatal("unknown eligibility assumedactive", members, e)
	}
	f.result = map[string]any{"users": []any{map[string]any{"id": uid, "teamId": iid, "active": true}}, "hasNextPage": false}
	if _, e = p.Members(context.Background(), tid, PageOptions{}); e == nil {
		t.Fatal("foreign member accepted")
	}
	f.tools = []*mcp.Tool{tool("list_users", map[string]string{"query": "string", "limit": "number"})}
	p, _ = NewWithCaller(context.Background(), f)
	before := len(f.calls)
	if _, e = p.Members(context.Background(), tid, PageOptions{}); e == nil || len(f.calls) != before {
		t.Fatal("global member fallback")
	}
}
func TestV2RelationSingleEdgeAndUnprovenCapabilities(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{v2MutationTool("update_issue", true), tool("create_issue_relation", map[string]string{"issueId": "string", "relatedIssueId": "string", "type": "string"}, "issueId", "relatedIssueId", "type"), tool("remove_issue_relation", map[string]string{"issueId": "string", "relatedIssueId": "string", "type": "string"}, "issueId", "relatedIssueId", "type")}, result: map[string]any{"ok": true}}
	p, _ := NewWithCaller(context.Background(), f)
	caps, e := p.Capabilities(context.Background())
	if e != nil || !caps.RelationAdd || !caps.RelationRemove || caps.ParentSameDestination || len(f.calls) != 0 {
		t.Fatal("capability inference/call", caps, e)
	}
	receipt, e := p.Relation(context.Background(), RelationInput{Action: "add", Type: "blocks", FromID: iid, ToID: uid})
	if e != nil || receipt.ID != "" || receipt.FromID != iid || len(f.calls) != 1 || len(f.args[0]) != 3 {
		t.Fatal(receipt, e, f.calls)
	}
	if _, e = p.Relation(context.Background(), RelationInput{Action: "add", Type: "blocks", FromID: iid, ToID: iid}); e == nil || len(f.calls) != 1 {
		t.Fatal("self edge dispatched")
	}
	f.tools = []*mcp.Tool{tool("update_issue", map[string]string{"id": "string", "relations": "array"})}
	p, _ = NewWithCaller(context.Background(), f)
	if _, e = p.Relation(context.Background(), RelationInput{Action: "remove", Type: "blocks", FromID: iid, ToID: uid}); e == nil || len(f.calls) != 1 {
		t.Fatal("full list RMW fallback")
	}
}
func TestV2ProjectionLabelFilterAndDuplicateIdentity(t *testing.T) {
	enum := []any{"id", "uuid", "teamId", "projectId", "description", "labels", "assigneeId", "parentId"}
	schema := map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "team": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}, "label": map[string]any{"type": "string"}, "fields": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": enum}}}}
	row := issueResult()
	row["labelIds"] = []any{tid}
	f := &fake{tools: []*mcp.Tool{{Name: "get_issue", InputSchema: schema}, {Name: "list_issues", InputSchema: schema}}, result: row}
	p, _ := NewWithCaller(context.Background(), f)
	if _, e := p.IssueV2(context.Background(), iid); e != nil {
		t.Fatal(e)
	}
	projection := f.args[0]["fields"].([]string)
	if !strings.Contains(strings.Join(projection, ","), "assigneeId") || !strings.Contains(strings.Join(projection, ","), "labels") {
		t.Fatal("knownness fields omitted", projection)
	}
	f.result = map[string]any{"issues": []any{row}, "hasNextPage": false}
	page, e := p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid}, LabelID: tid})
	if e != nil || len(page.Items) != 1 {
		t.Fatal(page, e)
	}
	if f.args[1]["label"] != tid {
		t.Fatal("label filter omitted")
	}
	other := issueResult()
	other["labelIds"] = []any{tid}
	other["description"] = "different body"
	f.result = map[string]any{"issues": []any{row, other}, "hasNextPage": false}
	if _, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid}}); e == nil {
		t.Fatal("contradictory duplicate selected")
	}
}

func TestV2PreflightAndTypedSchemaConstraints(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{v2MutationTool("create_issue", false), v2MutationTool("update_issue", false)}, result: issueResult()}
	p, _ := NewWithCaller(context.Background(), f)
	if e := p.ValidateCreateV2(CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "ok"}, Assignee: Field[*string]{Present: true}}); e == nil {
		t.Fatal("unsupported clear preflight succeeded")
	}
	if e := p.ValidateUpdateV2(UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Project: Field[*string]{Present: true}}); e == nil {
		t.Fatal("unsupported projectclear succeeded")
	}
	if len(f.calls) != 0 {
		t.Fatal("preflight performedIO")
	}
	tool := v2MutationTool("update_issue", true)
	schema := tool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	props["labelIds"].(map[string]any)["maxItems"] = 1
	f.tools = []*mcp.Tool{tool}
	p, _ = NewWithCaller(context.Background(), f)
	if e := p.ValidateUpdateV2(UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Labels: Field[[]string]{Present: true, Value: []string{tid, uid}}}); e == nil {
		t.Fatal("provider arraybound ignored")
	}
	edge := toolSchemaWithEnum()
	f.tools = []*mcp.Tool{edge}
	p, _ = NewWithCaller(context.Background(), f)
	caps, _ := p.Capabilities(context.Background())
	if caps.RelationAdd {
		t.Fatal("unsupported relationenum advertised")
	}
	if _, e := p.Relation(context.Background(), RelationInput{Action: "add", Type: "blocks", FromID: iid, ToID: uid}); e == nil || len(f.calls) != 0 {
		t.Fatal("unsupported enum dispatched")
	}
}
func toolSchemaWithEnum() *mcp.Tool {
	t := tool("create_issue_relation", map[string]string{"issueId": "string", "relatedIssueId": "string", "type": "string"}, "issueId", "relatedIssueId", "type")
	schema := t.InputSchema.(map[string]any)
	schema["properties"].(map[string]any)["type"].(map[string]any)["enum"] = []any{"duplicate"}
	return t
}
func TestV2LabelNamesCannotBecomeCanonicalUUIDs(t *testing.T) {
	row := issueResult()
	row["labels"] = []any{tid}
	f := &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"})}, result: row}
	p, _ := NewWithCaller(context.Background(), f)
	x, e := p.IssueV2(context.Background(), iid)
	if e != nil || x.LabelsKnown || len(x.LabelIDs) != 0 {
		t.Fatal("UUID-shaped labelname trusted", x, e)
	}
}
