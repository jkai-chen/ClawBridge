package linearprovider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The workspace's users, with no team filter: eligible only when Linear
// states both active and not a guest (T2'); a missing flag fails closed,
// and the email is read but never output.
func TestWorkspaceUsersFailClosed(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{nativeUserList()}}
	p, _ := NewWithCaller(context.Background(), f)
	f.result = map[string]any{"users": []any{
		map[string]any{"id": uid, "name": "Ada", "email": "ada@example.test", "active": true, "isGuest": false},
		map[string]any{"id": iid, "name": "Gus", "active": true, "guest": true},
		map[string]any{"id": tid, "name": "Old", "active": false, "guest": false},
	}, "hasNextPage": false}
	users, e := p.WorkspaceUsers(context.Background(), PageOptions{})
	if e != nil || !users.Complete || len(users.Items) != 3 {
		t.Fatal(users, e)
	}
	if _, team := f.args[0]["team"]; team {
		t.Fatal("a team filter was sent", f.args[0])
	}
	if !users.Items[0].Eligible() || users.Items[1].Eligible() || users.Items[2].Eligible() {
		t.Fatal(users.Items)
	}
	if raw, _ := json.Marshal(users); strings.Contains(string(raw), "ada@example.test") || users.Items[0].Email != "ada@example.test" {
		t.Fatal(string(raw))
	}
	// No guest flag, or no active flag: not eligible.
	f.result = map[string]any{"users": []any{map[string]any{"id": uid, "active": true}, map[string]any{"id": iid, "guest": false}}, "hasNextPage": false}
	users, e = p.WorkspaceUsers(context.Background(), PageOptions{})
	if e != nil || users.Items[0].Eligible() || users.Items[1].Eligible() {
		t.Fatal(users, e)
	}
	// Two spellings that disagree are refused.
	f.result = map[string]any{"users": []any{map[string]any{"id": uid, "active": true, "guest": true, "isGuest": false}}, "hasNextPage": false}
	if _, e = p.WorkspaceUsers(context.Background(), PageOptions{}); e == nil {
		t.Fatal("contradicting guest flags accepted")
	}
}

// The shape list_users returns (keys checked live on 28 Oct, values made
// up): isActive, isAdmin and isGuest booleans, and no bot or app marker;
// the app's own user isn't listed.
func TestWorkspaceUsersLiveShape(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{nativeUserList()}}
	p, _ := NewWithCaller(context.Background(), f)
	f.result = map[string]any{"hasNextPage": false, "users": []any{map[string]any{"createdAt": "2026-01-01T00:00:00Z", "displayName": "ada", "email": "ada@example.test", "id": uid, "isActive": true, "isAdmin": false, "isGuest": false, "name": "Ada Example", "status": "", "updatedAt": "2026-01-01T00:00:00Z"}}}
	users, e := p.WorkspaceUsers(context.Background(), PageOptions{})
	if e != nil || len(users.Items) != 1 || !users.Items[0].Eligible() || users.Items[0].Name != "Ada Example" {
		t.Fatal(users, e)
	}
}
