package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func unreadableCode(err error) bool {
	c, ok := err.(interface{ Code() string })
	return ok && c.Code() == "binding_unreadable"
}

// An unreadable binding is refused for every use and never guessed at; only
// a project owner's explicit repair clear, at the stored revision, replaces it.
func TestUnreadableTaskboardBindingFailsClosedUntilAnOwnerRepairs(t *testing.T) {
	for name, c := range map[string]struct {
		damage string
		// A JSON object this binary cannot read may be a newer binary's
		// mapping after a downgrade: the refusal says to upgrade first.
		newer bool
	}{
		"not json":         {`UPDATE taskboard_bindings SET mapping='{not json'`, false},
		"json array":       {`UPDATE taskboard_bindings SET mapping='[]'`, false},
		"not a mapping":    {`UPDATE taskboard_bindings SET mapping='{"linear_team_ids":"x"}'`, true},
		"legacy not valid": {`UPDATE taskboard_bindings SET mapping='{"linear_team_ids":[]}',mapping_version=1`, true},
	} {
		damage := c.damage
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne))
			if _, e := h.s.db.Exec(damage); e != nil {
				t.Fatal(e)
			}
			var raw string
			if e := h.s.db.QueryRow(`SELECT mapping FROM taskboard_bindings`).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			run := func(op string, a map[string]any) error {
				_, e := h.s.Execute(ctx, h.request(op, "a", a))
				return e
			}
			for op, a := range map[string]map[string]any{
				"taskboard_get":      nil,
				"taskboard_get_v2":   nil,
				"taskboard_set_v2":   taskboardArgsV2(1, projectTwo),
				"taskboard_clear_v2": {"expected_revision": 1},
			} {
				e := run(op, a)
				message, public := PublicMessage(e)
				if !unreadableCode(e) || !public || !strings.Contains(message, "revision 1") || !strings.Contains(message, "--expected-revision 1 --confirm --repair") || strings.Contains(message, "upgrade instead of repairing") != c.newer {
					t.Fatalf("%s: %v", op, e)
				}
			}
			// Repair never rebinds, and is refused for a lead or a stale revision.
			set := taskboardArgsV2(1, projectTwo)
			set["repair"] = true
			if e := run("taskboard_set_v2", set); e == nil {
				t.Fatal("repair rebound to a new mapping")
			}
			var wid, pid string
			if e := h.s.db.QueryRow(`SELECT workspace_id,id FROM projects WHERE slug='p'`).Scan(&wid, &pid); e != nil {
				t.Fatal(e)
			}
			tx, e := h.s.db.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			_, e = h.s.taskboard(ctx, &transaction{Tx: tx}, wid, pid, "lead", "taskboard_clear_v2", map[string]any{"expected_revision": int64(1), "repair": true})
			tx.Rollback()
			if !unreadableCode(e) {
				t.Fatal("a lead repaired an unreadable binding", e)
			}
			// The request path refuses a repair flag on set; the store refuses
			// a repair rebind even from the owner.
			tx, e = h.s.db.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			set["expected_revision"] = int64(1)
			_, e = h.s.taskboard(ctx, &transaction{Tx: tx}, wid, pid, "owner", "taskboard_set_v2", set)
			tx.Rollback()
			if !unreadableCode(e) {
				t.Fatal("an owner repair rebound to a new mapping", e)
			}
			if e = run("taskboard_clear_v2", map[string]any{"expected_revision": 0, "repair": true}); e == nil || !strings.Contains(e.Error(), "conflict") {
				t.Fatal("repair at a stale revision", e)
			}
			if _, e = h.s.Execute(ctx, h.request("taskboard_clear_v2", "a", map[string]any{"expected_revision": 1, "repair": true})); e != nil {
				t.Fatal("owner could not repair", e)
			}
			b, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "a", nil))
			if e != nil || b.Configured || b.Revision != 2 {
				t.Fatal("binding not cleared by repair", b, e)
			}
			sum := sha256.Sum256([]byte(raw))
			var details string
			if e = h.s.db.QueryRow(`SELECT details FROM history WHERE action='taskboard_clear_v2'`).Scan(&details); e != nil || !strings.Contains(details, hex.EncodeToString(sum[:])) {
				t.Fatal("repair not audited with the cleared row's digest", details, e)
			}
			// A readable binding is unlinked normally, never "repaired".
			h.call("taskboard_set_v2", "a", taskboardArgsV2(2, projectOne))
			if e = run("taskboard_clear_v2", map[string]any{"expected_revision": 3, "repair": true}); e == nil || !strings.Contains(e.Error(), "readable") {
				t.Fatal("repair accepted on a readable binding", e)
			}
		})
	}
}

func TestLegacyClearAlsoRepairsAnUnreadableBinding(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set", "a", taskboardArgs(0))
	if _, e := h.s.db.Exec(`UPDATE taskboard_bindings SET mapping='[]'`); e != nil {
		t.Fatal(e)
	}
	h.call("taskboard_clear", "a", map[string]any{"expected_revision": 1, "repair": true})
	if b, e := ParseTaskboardBinding(h.call("taskboard_get", "a", nil)); e != nil || b.Configured {
		t.Fatal(b, e)
	}
}

// On the hosted authority a lead may unlink a readable binding but not repair
// an unreadable one; the project owner may.
func TestPostgresOnlyTheProjectOwnerRepairsAnUnreadableBinding(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner := newID("owner") + "@repair-test.example"
	lead := newID("lead") + "@repair-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("repairws")
	as := func(principal, op string, a map[string]any) (Result, error) {
		return s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Args: a})
	}
	for _, step := range []struct {
		op string
		a  map[string]any
	}{
		{"workspace_init", map[string]any{"mode": "server"}},
		{"project_create", map[string]any{"slug": "p"}},
		{"project_role_set", map[string]any{"principal": lead, "role": "lead"}},
		{"taskboard_set_v2", taskboardArgsV2(0, projectOne)},
	} {
		if _, err = as(owner, step.op, step.a); err != nil {
			t.Fatal(step.op, err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE taskboard_bindings SET mapping='{not json' WHERE project_id=(SELECT p.id FROM projects p JOIN workspaces x ON x.id=p.workspace_id WHERE x.name=$1)`, w); err != nil {
		t.Fatal(err)
	}
	repair := map[string]any{"expected_revision": 1, "repair": true}
	if _, err = as(lead, "taskboard_clear_v2", repair); !unreadableCode(err) {
		t.Fatal("a lead repaired an unreadable binding", err)
	}
	if _, err = as(owner, "taskboard_clear_v2", repair); err != nil {
		t.Fatal("owner could not repair", err)
	}
}
