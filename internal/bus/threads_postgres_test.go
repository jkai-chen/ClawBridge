package bus

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// On the hosted authority only the project owner reads the threads, from a
// person's own device key: a Google sign-in, a session, a CI device and
// every lesser role are refused. Nothing is stamped, and each read writes
// one audit row.
func TestPostgresProjectThreadsAccess(t *testing.T) {
	f := newPGIdentityFixture(t)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "lead", "member", "reader"} {
		p := newID(role) + "@identity-test.example"
		code := f.invite(f.owner, p, role)["code"].(string)
		key := newKey(t)
		if _, err := f.join(p, code, key); err != nil {
			t.Fatal(err)
		}
		people[role], device[role] = p, keyID(t, key)
	}
	as := func(role string, r Request) (Result, error) {
		r.Workspace, r.Project = f.workspace, "p"
		return f.asDevice(people[role], device[role], r)
	}
	for _, name := range []string{"a", "b"} {
		f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: name, Args: map[string]any{"instance": name + "-i", "role": "worker"}})
	}
	sent := f.must(f.owner, "sub-owner", Request{Operation: "send", Workspace: f.workspace, Project: "p", Session: "a", Args: map[string]any{"instance": "a-i", "to": "b", "body": "hello", "operation_id": "q"}}).Data.(map[string]any)
	id := sent["id"].(string)
	stamps := func() string {
		var out string
		if err := f.s.db.QueryRow(`SELECT COALESCE(string_agg(COALESCE(d.delivered_at,'-')||COALESCE(d.read_at,'-'),','),'')||(SELECT string_agg(last_seen,',' ORDER BY id) FROM sessions WHERE project_id=m.project_id) FROM messages m LEFT JOIN deliveries d ON d.message_id=m.id WHERE m.id=$1 GROUP BY m.project_id`, id).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := stamps()

	list, err := as("owner", Request{Operation: "project_threads", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	rows := list.Data.(map[string]any)["threads"].([]map[string]any)
	if len(rows) != 1 || rows[0]["thread_id"] != id || !reflect.DeepEqual(rows[0]["participants"], []string{"a", "b"}) || rows[0]["messages"] != int64(1) {
		t.Fatal(rows)
	}
	one, err := as("owner", Request{Operation: "project_thread", Args: map[string]any{"thread_id": id}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := one.Data.(map[string]any)["messages"].([]map[string]any)
	if len(msgs) != 1 || msgs[0]["body"] != "hello" || len(msgs[0]["recipients"].([]map[string]any)) != 1 {
		t.Fatal(msgs)
	}
	for _, k := range []string{"instance", "transport", "created_by_principal", "payload_hash"} {
		if strings.Contains(asJSON(one.Data), `"`+k+`"`) || strings.Contains(asJSON(list.Data), `"`+k+`"`) {
			t.Fatal("output carries", k)
		}
	}
	if stamps() != before {
		t.Fatal("a thread read moved a stamp")
	}
	if f.audited("project_threads", f.projectID()) != 1 || f.audited("project_thread", id) != 1 {
		t.Fatal("audit rows")
	}

	// A cursor that isn't a string is refused, not a panic.
	for _, bad := range []any{5, map[string]any{}} {
		if _, err = as("owner", Request{Operation: "project_threads", Args: map[string]any{"before": bad}}); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	// The limit is per principal: another owner's reads don't count.
	other := newID("owner2") + "@identity-test.example"
	key2 := newKey(t)
	if _, err = f.join(other, f.invite(f.owner, other, "owner")["code"].(string), key2); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < threadsPerMinute; i++ {
		if _, err = as("owner", Request{Operation: "project_threads", Args: map[string]any{}}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err = as("owner", Request{Operation: "project_threads", Args: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "30 a minute") {
		t.Fatal("over the limit:", err)
	}
	if _, err = f.asDevice(other, keyID(t, key2), Request{Operation: "project_threads", Workspace: f.workspace, Project: "p", Args: map[string]any{}}); err != nil {
		t.Fatal("another owner was limited:", err)
	}
	refused := func(what string, want string, run func(op string, a map[string]any) (Result, error)) {
		t.Helper()
		for op, a := range map[string]map[string]any{"project_threads": {}, "project_thread": {"thread_id": id}} {
			if _, err := run(op, a); !refusedWith(err, want) {
				t.Fatal(what, op, err)
			}
		}
	}
	for _, role := range []string{"lead", "member", "reader"} {
		refused(role, "owner", func(op string, a map[string]any) (Result, error) {
			return as(role, Request{Operation: op, Args: a})
		})
	}
	// The workspace owner too, signed in with Google rather than a device.
	refused("google", "device", func(op string, a map[string]any) (Result, error) {
		return f.as(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a})
	})
	// A request with no verified account at all.
	refused("no identity", "device", func(op string, a map[string]any) (Result, error) {
		return f.s.ExecuteAs(context.Background(), f.owner, Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a})
	})
	// The owner's own device, naming a session.
	if _, err = as("owner", Request{Operation: "register_session", Session: "mine", Args: map[string]any{"instance": "mine-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	refused("session", "session", func(op string, a map[string]any) (Result, error) {
		a["instance"] = "mine-i"
		return as("owner", Request{Operation: op, Session: "mine", Args: a})
	})
	// A CI device, even one made an owner behind the API's back.
	key := newKey(t)
	if _, err = f.auth(f.owner, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 30, "public_key": key}); err != nil {
		t.Fatal(err)
	}
	ci := "ci:deploy@" + strings.ToLower(f.workspace)
	f.exec(`UPDATE project_roles SET role='owner' WHERE principal=?`, ci)
	refused("ci", "device", func(op string, a map[string]any) (Result, error) {
		return f.asDevice(ci, keyID(t, key), Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a})
	})
	if f.audited("project_threads", f.projectID()) != threadsPerMinute || f.audited("project_thread", id) != 1 {
		t.Fatal("a refused read was audited")
	}
}

func (f *pgIdentityFixture) projectID() string {
	f.t.Helper()
	var id string
	if err := f.s.db.QueryRow(`SELECT id FROM projects WHERE workspace_id=(SELECT id FROM workspaces WHERE name=$1) AND slug='p'`, f.workspace).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}
