package integration

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"clawbridge/internal/linearprovider"
)

// Synthetic structural examples only: no captured account/team/issue data and
// no authenticated transport, OAuth or credential store are used in this file.
func l1NativeIdentityRow(issue bool) map[string]any {
	if issue {
		return map[string]any{"id": "SYN-17", "uuid": l1IssueID, "title": "Synthetic issue", "description": "synthetic scoped body", "team": "Example team", "teamId": l1TeamA, "project": nil, "projectId": nil, "status": "Backlog", "statusType": "backlog"}
	}
	return map[string]any{"id": "P-SYN-7", "uuid": l1Project, "name": "Synthetic project", "teams": []any{map[string]any{"id": l1TeamA, "name": "Example team"}}}
}

func TestL1NativeIssueAdvertisedFieldProjection(t *testing.T) {
	const foreign = "99999999-9999-4999-8999-999999999999"
	for _, operation := range []string{"get_issue", "list_issues"} {
		for _, required := range []bool{false, true} {
			for _, response := range []string{"explicit-null-project", "omitted-project", "conflicting-canonical-id", "conflicting-scope"} {
				t.Run(fmt.Sprintf("%s/fields-required-%v/%s", operation, required, response), func(t *testing.T) {
					peer := newL1ProviderPeer()
					allowed := map[string]bool{}
					for _, name := range []string{"id", "uuid", "title", "description", "team", "teamId", "project", "projectId", "status", "internalInstructions"} {
						allowed[name] = true
					}
					for _, tool := range peer.tools {
						if tool.Name != operation {
							continue
						}
						schema := tool.InputSchema.(map[string]any)
						properties := schema["properties"].(map[string]any)
						properties["fields"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"id", "uuid", "title", "description", "team", "teamId", "project", "projectId", "status", "internalInstructions"}}}
						if required {
							schema["required"] = append(schema["required"].([]string), "fields")
						}
					}
					peer.result = func(name string, args map[string]any) (any, error) {
						if name != operation {
							return nil, fmt.Errorf("unexpected projection fallback %s", name)
						}
						fields, ok := args["fields"].([]any)
						if !ok || len(fields) == 0 {
							return nil, fmt.Errorf("advertised field projection not requested")
						}
						original, projected, selected := l1NativeIdentityRow(true), map[string]any{}, map[string]bool{}
						for _, value := range fields {
							field, ok := value.(string)
							if !ok || !allowed[field] || selected[field] || field == "internalInstructions" {
								return nil, fmt.Errorf("non-curated or duplicate projected field")
							}
							selected[field] = true
							projected[field] = original[field]
						}
						for _, field := range []string{"id", "uuid", "teamId", "projectId"} {
							if !selected[field] {
								return nil, fmt.Errorf("canonical identity/scope field %s omitted", field)
							}
						}
						switch response {
						case "omitted-project":
							delete(projected, "project")
							delete(projected, "projectId")
						case "conflicting-canonical-id":
							projected["id"] = foreign
						case "conflicting-scope":
							projected["team"] = map[string]any{"id": "TEAM-SYN", "uuid": foreign}
						}
						if operation == "list_issues" {
							return map[string]any{"issues": []any{projected}, "hasNextPage": false}, nil
						}
						return projected, nil
					}
					p, err := linearprovider.NewWithCaller(context.Background(), peer)
					if err != nil {
						t.Fatal(err)
					}
					got, err := l1NativeIdentityRead(p, operation)
					if response != "explicit-null-project" {
						if err == nil || !l1NativeIdentityIsZero(got) {
							t.Fatalf("unknown/conflicting projected scope exposed body: %+v %v", got, err)
						}
						return
					}
					if err != nil || len(peer.calls) != 1 {
						t.Fatalf("projected explicit-null scope rejected or repaired: %v", err)
					}
					switch v := got.(type) {
					case linearprovider.Issue:
						l1AssertNativeCanonicalIssue(t, v)
					case linearprovider.Page[linearprovider.Issue]:
						if len(v.Items) != 1 || !v.Complete {
							t.Fatalf("projected issue page invalid: %+v", v)
						}
						l1AssertNativeCanonicalIssue(t, v.Items[0])
					}
				})
			}
		}
	}
}

func TestL1NativeUnknownProjectIsNotInferredAbsent(t *testing.T) {
	for _, operation := range []string{"get_issue", "list_issues"} {
		t.Run(operation, func(t *testing.T) {
			row := l1NativeIdentityRow(true)
			delete(row, "project")
			delete(row, "projectId")
			p, _ := l1NativeIdentityProvider(t, operation, row)
			got, err := l1NativeIdentityRead(p, operation)
			if err == nil || !l1NativeIdentityIsZero(got) {
				t.Fatalf("omitted unknown project became proven absence with issue body: %+v %v", got, err)
			}
		})
	}
}

func TestL1NativeIssueUnsupportedProjectionRefusesBeforeRPC(t *testing.T) {
	for _, operation := range []string{"get_issue", "list_issues"} {
		for _, invalid := range []string{"missing-canonical-scope-field", "wrong-item-type"} {
			t.Run(operation+"/"+invalid, func(t *testing.T) {
				peer := newL1ProviderPeer()
				for _, tool := range peer.tools {
					if tool.Name != operation {
						continue
					}
					items := map[string]any{"type": "string", "enum": []string{"id", "teamId", "projectId", "description"}}
					if invalid == "wrong-item-type" {
						items["type"] = "number"
						items["enum"] = []string{"id", "uuid", "teamId", "projectId"}
					}
					tool.InputSchema.(map[string]any)["properties"].(map[string]any)["fields"] = map[string]any{"type": "array", "items": items}
				}
				peer.result = func(string, map[string]any) (any, error) {
					return nil, fmt.Errorf("unsupported schema reached RPC")
				}
				p, err := linearprovider.NewWithCaller(context.Background(), peer)
				if err != nil {
					t.Fatal(err)
				}
				got, err := l1NativeIdentityRead(p, operation)
				if err == nil || !l1NativeIdentityIsZero(got) || len(peer.calls) != 0 {
					t.Fatalf("unsupported projection reached RPC or exposed data: %+v %v calls=%d", got, err, len(peer.calls))
				}
			})
		}
	}
}

func l1NativeIdentityProvider(t *testing.T, operation string, row map[string]any) (*linearprovider.Provider, *l1ProviderPeer) {
	t.Helper()
	peer := newL1ProviderPeer()
	peer.tools = append(peer.tools, l1SchemaTool("list_projects", map[string]string{"limit": "integer", "cursor": "string"}, "limit"))
	peer.result = func(name string, args map[string]any) (any, error) {
		if name != operation {
			return nil, fmt.Errorf("unexpected identity-repair/fallback call %s", name)
		}
		switch operation {
		case "list_projects":
			return map[string]any{"projects": []any{row}, "hasNextPage": false}, nil
		case "list_issues":
			return map[string]any{"issues": []any{row}, "hasNextPage": false}, nil
		default:
			return row, nil
		}
	}
	p, err := linearprovider.NewWithCaller(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	return p, peer
}

func l1NativeIdentityRead(p *linearprovider.Provider, operation string) (any, error) {
	ctx := context.Background()
	switch operation {
	case "list_projects":
		return p.Projects(ctx, linearprovider.PageOptions{Limit: 10})
	case "get_project":
		return p.GetProject(ctx, l1Project)
	case "list_issues":
		return p.Issues(ctx, linearprovider.IssueQuery{TeamID: l1TeamA, Limit: 10})
	default:
		return p.GetIssue(ctx, l1IssueID)
	}
}

func l1NativeIdentityIsZero(v any) bool {
	switch value := v.(type) {
	case linearprovider.Issue:
		return reflect.DeepEqual(value, linearprovider.Issue{})
	case linearprovider.Project:
		return reflect.DeepEqual(value, linearprovider.Project{})
	case linearprovider.Page[linearprovider.Issue]:
		return len(value.Items) == 0
	case linearprovider.Page[linearprovider.Project]:
		return len(value.Items) == 0
	}
	return false
}

func TestL1NativeDisplayIDAndUUIDCanonicalReads(t *testing.T) {
	for _, operation := range []string{"list_projects", "get_project", "list_issues", "get_issue"} {
		for _, representation := range []string{"display-id-canonical-uuid", "legacy-canonical-id", "equal-canonical-aliases", "uuid-only"} {
			t.Run(operation+"/"+representation, func(t *testing.T) {
				issue := operation == "list_issues" || operation == "get_issue"
				row := l1NativeIdentityRow(issue)
				canonical, alias := l1Project, "projectId"
				if issue {
					canonical, alias = l1IssueID, "issueId"
				}
				if representation == "uuid-only" {
					delete(row, "id")
					if issue {
						row["identifier"] = "SYN-17"
					}
				} else if representation != "display-id-canonical-uuid" {
					row["id"] = canonical
					if issue {
						row["identifier"] = "SYN-17"
					}
					if representation == "legacy-canonical-id" {
						delete(row, "uuid")
					} else {
						row[alias] = canonical
					}
				}
				p, peer := l1NativeIdentityProvider(t, operation, row)
				got, err := l1NativeIdentityRead(p, operation)
				if err != nil {
					t.Fatalf("canonical structural response rejected: %v", err)
				}
				if len(peer.calls) != 1 {
					t.Fatal("complete identity/scope triggered extra resolution")
				}
				switch v := got.(type) {
				case linearprovider.Page[linearprovider.Project]:
					if len(v.Items) != 1 || v.Items[0].ID != canonical || !reflect.DeepEqual(v.Items[0].TeamIDs, []string{l1TeamA}) || !v.Complete {
						t.Fatalf("project list lost immutable scope: %+v", v)
					}
				case linearprovider.Project:
					if v.ID != canonical || !reflect.DeepEqual(v.TeamIDs, []string{l1TeamA}) {
						t.Fatalf("project get lost immutable scope: %+v", v)
					}
				case linearprovider.Page[linearprovider.Issue]:
					if len(v.Items) != 1 || !v.Complete {
						t.Fatalf("issue list incomplete: %+v", v)
					}
					l1AssertNativeCanonicalIssue(t, v.Items[0])
				case linearprovider.Issue:
					l1AssertNativeCanonicalIssue(t, v)
				}
			})
		}
	}
}

func l1AssertNativeCanonicalIssue(t *testing.T, v linearprovider.Issue) {
	t.Helper()
	if v.ID != l1IssueID || v.Identifier != "SYN-17" || v.TeamID != l1TeamA || v.ProjectID != "" || v.State != "Backlog" || v.Description != "synthetic scoped body" || v.ContextComplete {
		t.Fatalf("canonical issue identity/display/scope/body changed: %+v", v)
	}
}

func TestL1NativeDisplayIdentityAmbiguityFailsClosed(t *testing.T) {
	const foreign = "99999999-9999-4999-8999-999999999999"
	for _, operation := range []string{"list_projects", "get_project", "list_issues", "get_issue"} {
		for _, invalid := range []string{"missing-uuid", "null-uuid", "numeric-uuid", "bool-uuid", "array-uuid", "display-uuid", "null-id", "numeric-id", "bool-id", "array-id", "unsafe-display", "uuid-id-conflict", "alias-conflict", "nested-scope-conflict", "nested-id-uuid-conflict"} {
			t.Run(operation+"/"+invalid, func(t *testing.T) {
				issue := operation == "list_issues" || operation == "get_issue"
				row := l1NativeIdentityRow(issue)
				switch invalid {
				case "missing-uuid":
					delete(row, "uuid")
				case "null-uuid":
					row["uuid"] = nil
				case "numeric-uuid":
					row["uuid"] = 42
				case "bool-uuid":
					row["uuid"] = true
				case "array-uuid":
					row["uuid"] = []any{l1IssueID}
				case "display-uuid":
					row["uuid"] = "SYN-17"
				case "null-id":
					row["id"] = nil
				case "numeric-id":
					row["id"] = 42
				case "bool-id":
					row["id"] = false
				case "array-id":
					row["id"] = []any{"SYN-17"}
				case "unsafe-display":
					row["id"] = "SYN-17\x1b[31m"
				case "uuid-id-conflict":
					row["id"] = foreign
				case "alias-conflict":
					if issue {
						row["issueId"] = foreign
					} else {
						row["projectId"] = foreign
					}
				case "nested-scope-conflict":
					if issue {
						row["team"] = map[string]any{"id": "TEAM-SYN", "uuid": foreign}
					} else {
						row["teams"] = []any{map[string]any{"id": "TEAM-SYN", "uuid": l1TeamA, "teamId": foreign}}
					}
				case "nested-id-uuid-conflict":
					if issue {
						row["team"] = map[string]any{"id": foreign, "uuid": l1TeamA}
					} else {
						row["teams"] = []any{map[string]any{"id": foreign, "uuid": l1TeamA}}
					}
				}
				p, peer := l1NativeIdentityProvider(t, operation, row)
				got, err := l1NativeIdentityRead(p, operation)
				if err == nil || !l1NativeIdentityIsZero(got) {
					t.Fatalf("ambiguous identity/scope exposed result: value=%+v err=%v", got, err)
				}
				if len(peer.calls) != 1 {
					t.Fatal("malformed identity/scope was repaired via fallback")
				}
			})
		}
	}
	for _, operation := range []string{"get_project", "get_issue"} {
		t.Run(operation+"/requested-uuid-mismatch", func(t *testing.T) {
			row := l1NativeIdentityRow(operation == "get_issue")
			row["uuid"] = foreign
			p, _ := l1NativeIdentityProvider(t, operation, row)
			got, err := l1NativeIdentityRead(p, operation)
			if err == nil || !l1NativeIdentityIsZero(got) {
				t.Fatalf("requested immutable ID mismatch exposed data: %+v %v", got, err)
			}
		})
	}
}
