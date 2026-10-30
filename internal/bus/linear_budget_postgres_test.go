package bus

import (
	"strings"
	"testing"
)

// On the hosted authority a member's own write and an agent session's are
// counted after their calls, a reader charges nothing, no one charges a
// read (the server charges issue history itself), and the overview
// splits the hour, with no cap.
func TestPostgresLinearWriteCharge(t *testing.T) {
	f := newPGIdentityFixture(t)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"member", "reader"} {
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
	if _, err := as("reader", Request{Operation: "linear_write_charge", Args: charge("r-1", "create", 2)}); !refusedWith(err, "member permission") {
		t.Fatal("a reader charged:", err)
	}
	// A read charge is no client's: refused for a reader as any charge is,
	// and for a member as an unknown kind.
	if _, err := as("reader", Request{Operation: "linear_write_charge", Args: charge("r-2", "read", 2)}); !refusedWith(err, "member permission") {
		t.Fatal("a reader charged a read:", err)
	}
	if _, err := as("member", Request{Operation: "linear_write_charge", Args: charge("m-0", "read", 2)}); err == nil || !strings.Contains(err.Error(), "kind must be one of") {
		t.Fatal("a member charged a read:", err)
	}
	if _, err := as("member", Request{Operation: "linear_write_charge", Args: charge("m-1", "comment", 3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := as("member", Request{Operation: "register_session", Session: "member-s", Args: map[string]any{"instance": "member-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	session := func(a map[string]any) (Result, error) {
		a["instance"] = "member-i"
		return as("member", Request{Operation: "linear_write_charge", Session: "member-s", Args: a})
	}
	if _, err := session(charge("s-1", "update", 6)); err != nil {
		t.Fatal(err)
	}
	overview, err := as("member", Request{Operation: "project_overview", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	b := overview.Data.(map[string]any)["linear_budget"].(LinearBudget)
	if b.Used != 9 || b.By.People != 3 || b.By.Agents != 6 || b.By.App != 0 || b.Capped || b.Limit != 0 {
		t.Fatal(b)
	}
}
