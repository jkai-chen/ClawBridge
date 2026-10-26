package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// dispatcherHarness binds the project to two Linear projects, approves two
// labels, and has sessions a, b and c (from newHarness).
func dispatcherHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	h.call("label_policy_set", "a", map[string]any{"expected_revision": 0, "label_policy": map[string]any{"labels": []any{"payments", "platform"}}})
	return h
}

func paymentsDomain(owner string, trusted ...any) map[string]any {
	if trusted == nil {
		trusted = []any{}
	}
	return map[string]any{"id": "payments", "name": "Payments", "match": map[string]any{"linear_projects": []any{projectOne}, "labels": []any{"payments"}}, "owner_principal": owner, "coordinator_session": map[string]any{"name": "c"}, "trusted_sessions": trusted}
}

// dispatcherSessions creates the project's reserved dispatcher sessions,
// as its first call after an enable does, and leaves the policy as it was.
func (h *harness) dispatcherSessions(pid string) {
	h.t.Helper()
	var revision, enabled int
	err := h.s.db.QueryRow(`SELECT revision,enabled FROM dispatcher_policies WHERE project_id=?`, pid).Scan(&revision, &enabled)
	fresh := err != nil
	if fresh {
		_, err = h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,domains,updated_at,updated_by_principal) VALUES(?,0,1,'{"domains":[]}','2026-01-01T00:00:00Z','x')`, pid)
	} else {
		_, err = h.s.db.Exec(`UPDATE dispatcher_policies SET enabled=1 WHERE project_id=?`, pid)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err = h.s.ExecuteDispatcher(context.Background(), pid, DispatcherSessionName, Request{Operation: "dispatcher_get"}); err != nil {
		h.t.Fatal(err)
	}
	if fresh {
		_, err = h.s.db.Exec(`DELETE FROM dispatcher_policies WHERE project_id=?`, pid)
	} else {
		_, err = h.s.db.Exec(`UPDATE dispatcher_policies SET enabled=? WHERE project_id=?`, enabled, pid)
	}
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) principal() string {
	h.t.Helper()
	return h.call("whoami", "a", nil).(map[string]any)["principal"].(string)
}

// The owner sets the domains by compare-and-swap; sessions are resolved to
// their ID and principal (DP-M1); everything else is checked against the
// project: bound projects, approved labels, a lead or owner as owner.
func TestDispatcherDomainsSetAndRules(t *testing.T) {
	h := dispatcherHarness(t)
	got := h.call("dispatcher_get", "a", map[string]any{}).(map[string]any)
	if got["revision"] != int64(0) || got["enabled"] != false || got["mode"] != "rules" || len(got["domains"].([]DispatcherDomain)) != 0 {
		t.Fatal("default", got)
	}
	me := h.principal()
	set := func(rev int, domains ...any) (map[string]any, string) {
		out, err := h.s.Execute(context.Background(), h.request("dispatcher_domains_set", "a", map[string]any{"expected_revision": rev, "dispatcher_domains": map[string]any{"domains": domains}}))
		if err != nil {
			return nil, err.Error()
		}
		return out.Data.(map[string]any), ""
	}
	out, e := set(0, paymentsDomain(me, map[string]any{"name": "b"}))
	if e != "" {
		t.Fatal(e)
	}
	dom := out["domains"].([]DispatcherDomain)[0]
	if out["revision"] != int64(1) || dom.TrustedSessions[0].ID != h.sessionID("b") || dom.TrustedSessions[0].Principal != me || dom.CoordinatorSession.ID != h.sessionID("c") || dom.StaleAfterHours != 24 {
		t.Fatal(out)
	}
	if _, e = set(0, paymentsDomain(me)); !strings.Contains(e, "revision") {
		t.Fatal("stale revision", e)
	}
	other := func(edit func(d map[string]any)) map[string]any {
		d := paymentsDomain(me)
		d["id"] = "platform"
		d["match"] = map[string]any{"linear_projects": []any{projectTwo}, "labels": []any{}}
		edit(d)
		return d
	}
	for name, d := range map[string]map[string]any{
		"unbound project": other(func(d map[string]any) {
			d["match"] = map[string]any{"linear_projects": []any{"33333333-3333-4333-8333-333333333333"}}
		}),
		"unapproved label": other(func(d map[string]any) { d["match"] = map[string]any{"labels": []any{"secret"}} }),
		"owner not member": other(func(d map[string]any) { d["owner_principal"] = "stranger@example.com" }),
		"unknown session":  other(func(d map[string]any) { d["trusted_sessions"] = []any{map[string]any{"name": "nobody"}} }),
		"wrong principal": other(func(d map[string]any) {
			d["trusted_sessions"] = []any{map[string]any{"name": "b", "principal": "other@example.com"}}
		}),
		"trusted twice": other(func(d map[string]any) {
			d["trusted_sessions"] = []any{map[string]any{"name": "b"}, map[string]any{"name": "b"}}
		}),
		"same project":    other(func(d map[string]any) { d["match"] = map[string]any{"linear_projects": []any{projectOne}} }),
		"no match":        other(func(d map[string]any) { d["match"] = map[string]any{} }),
		"bad stale":       other(func(d map[string]any) { d["stale_after_hours"] = 1 }),
		"bad id":          other(func(d map[string]any) { d["id"] = "Platform" }),
		"mention in name": other(func(d map[string]any) { d["name"] = "@ops" }),
		"unknown key":     other(func(d map[string]any) { d["extra"] = true }),
	} {
		if _, e = set(1, paymentsDomain(me), d); e == "" {
			t.Fatal(name, "accepted")
		}
	}
	// Overlapping labels are allowed, since a label only routes, and reported.
	out, e = set(1, paymentsDomain(me), other(func(d map[string]any) {
		d["match"] = map[string]any{"linear_projects": []any{projectTwo}, "labels": []any{"payments"}}
	}))
	if e != "" || len(out["label_overlaps"].([]string)) != 1 {
		t.Fatal(out, e)
	}
	if acts := h.actions("dispatcher_policy"); len(acts) != 2 {
		t.Fatal("history", acts)
	}
}

// A domain's trust list and coordinator are edited one session at a time,
// by compare-and-swap, and audited with the session's ID.
func TestDispatcherDomainEdit(t *testing.T) {
	h := dispatcherHarness(t)
	h.call("dispatcher_domains_set", "a", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{paymentsDomain(h.principal())}}})
	edit := func(rev int, action, session string) (map[string]any, error) {
		out, err := h.s.Execute(context.Background(), h.request("dispatcher_domain_edit", "a", map[string]any{"expected_revision": rev, "domain": "payments", "action": action, "target_session": session}))
		if err != nil {
			return nil, err
		}
		return out.Data.(map[string]any), nil
	}
	out, err := edit(1, "trust", "b")
	if err != nil || out["domains"].([]DispatcherDomain)[0].TrustedSessions[0].ID != h.sessionID("b") {
		t.Fatal(out, err)
	}
	if _, err = edit(2, "trust", "b"); err == nil {
		t.Fatal("trusted twice")
	}
	if _, err = edit(1, "trust", "c"); err == nil {
		t.Fatal("stale revision")
	}
	if out, err = edit(2, "coordinator", "b"); err != nil || out["domains"].([]DispatcherDomain)[0].CoordinatorSession.Name != "b" {
		t.Fatal(out, err)
	}
	if out, err = edit(3, "untrust", "b"); err != nil || len(out["domains"].([]DispatcherDomain)[0].TrustedSessions) != 0 {
		t.Fatal(out, err)
	}
	for _, bad := range []map[string]any{
		{"expected_revision": 4, "domain": "nope", "action": "trust", "target_session": "b"},
		{"expected_revision": 4, "domain": "payments", "action": "promote", "target_session": "b"},
		{"expected_revision": 4, "domain": "payments", "action": "trust", "target_session": "ghost"},
	} {
		if e := h.errorOf("dispatcher_domain_edit", "a", bad); e == "" {
			t.Fatal("accepted", bad)
		}
	}
}

// People's sessions can't take the dispatcher's names.
func TestDispatcherNamesAreReserved(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"clawbridge-dispatcher", "Clawbridge-Dispatcher-Model"} {
		if e := h.errorOf("register_session", name, map[string]any{"instance": "x"}); !strings.Contains(e, "reserved") {
			t.Fatal(name, e)
		}
	}
}

// Each reserved session runs only its allow-list (DP-M2): every operation
// outside it is refused by the bus, sent directly.
func TestDispatcherAllowLists(t *testing.T) {
	h := dispatcherHarness(t)
	var pid string
	if e := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	forbiddenBoth := []string{
		"dispatcher_domains_set", "dispatcher_domain_edit", "doc_policy_set", "label_policy_set", "status_policy_set",
		"health_set", "taskboard_set_v2", "taskboard_clear_v2", "project_role_set", "project_role_revoke",
		"coordinator_set", "intake_dismiss", "intake_rearm", "intake_policy_set", "intake_token_create",
		"send", "reply", "register_session", "retire_session", "purge_expired", "workspace_set_domains",
		"linear_admin_audit", "doc_publication_put", "doc_publish_override", "comment_watch_lease",
	}
	forbiddenModel := append([]string{"assignment_confirm", "assignment_release", "dispatcher_decision_record", "dispatcher_escalate", "dispatcher_release_notice", "taskboard_get_v2"}, forbiddenBoth...)
	for session, ops := range map[string][]string{DispatcherSessionName: forbiddenBoth, DispatcherModelSessionName: forbiddenModel} {
		for _, op := range ops {
			_, err := h.s.ExecuteDispatcher(context.Background(), pid, session, Request{Operation: op, Args: map[string]any{}})
			var se *serviceError
			if err == nil || !errors.As(err, &se) || se.code != "forbidden" {
				t.Fatalf("%s ran %s: %v", session, op, err)
			}
		}
	}
	if _, err := h.s.ExecuteDispatcher(context.Background(), pid, "clawbridge", Request{Operation: "dispatcher_get"}); err == nil {
		t.Fatal("the watcher's session ran through the dispatcher gate")
	}
	// Allowed reads work, and the sessions an enable creates are hidden
	// from people's listings.
	h.dispatcherSessions(pid)
	for _, session := range []string{DispatcherSessionName, DispatcherModelSessionName} {
		out, err := h.s.ExecuteDispatcher(context.Background(), pid, session, Request{Operation: "dispatcher_get"})
		if err != nil || out.Data.(map[string]any)["revision"] != int64(0) {
			t.Fatal(session, out, err)
		}
	}
	listed := h.call("list_sessions", "a", map[string]any{}).(map[string]any)
	for _, s := range listed["sessions"].([]map[string]any) {
		if strings.HasPrefix(str(s, "name"), "clawbridge-dispatcher") {
			t.Fatal("a dispatcher session is listed", s)
		}
	}
	if e := h.errorOf("send", "a", map[string]any{"to": DispatcherSessionName, "body": "hi", "operation_id": "m1"}); e == "" {
		t.Fatal("a person messaged the dispatcher session")
	}
}

// Schema 17 has every table the dispatcher's later steps use, including
// the agent credential's binding secret (AG-M2a).
func TestDispatcherSchemaColumns(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{
		`SELECT binding_secret_sha256,bound_instance,used_at FROM dispatcher_agent_credentials LIMIT 0`,
		`SELECT state,ciphertext,nonce,config_revision FROM dispatcher_model_keys LIMIT 0`,
		`SELECT token_sha256 FROM dispatcher_launcher_tokens LIMIT 0`,
		`SELECT issue_project_id,issue_labels FROM session_assignments LIMIT 0`,
	} {
		rows, err := h.s.db.Query(q)
		if err != nil {
			t.Fatal(q, err)
		}
		rows.Close()
	}
}
