package bus

import (
	"strings"
	"testing"
)

const rolesDocTwo = "d0c00000-0000-4000-8000-000000000002"

func rolesDocArgs(state, token, expect string, extra ...any) map[string]any {
	a := map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": teamRolesDocSource, "state": state, "publish_token": token, "expect_token": expect, "mapping_revision": 1, "source_sha256": strings.Repeat("5", 64)}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i].(string)] = extra[i+1]
	}
	return a
}

// The generated document's one row: reserved, recorded, by token CAS; read
// back with the revision it was rendered from; owner only and never from a
// session.
func TestTeamRolesDocLedger(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	if got := h.call("team_roles_doc_get", "", nil).(map[string]any); got["published"] != nil || got["revision"] != 0 {
		t.Fatal(got)
	}
	h.call("team_roles_doc_put", "", rolesDocArgs("pending", "aaaaaaaaaaaa", ""))
	if e := h.errorOf("team_roles_doc_put", "", rolesDocArgs("pending", "bbbbbbbbbbbb", "")); !strings.Contains(e, "changed") {
		t.Fatal("a stale reserve won:", e)
	}
	digest := strings.Repeat("e", 64)
	h.call("team_roles_doc_put", "", rolesDocArgs("published", "aaaaaaaaaaaa", "aaaaaaaaaaaa", "document_id", seedDoc, "published_sha256", digest, "mapping_revision", 3))
	got := h.call("team_roles_doc_get", "", nil).(map[string]any)["published"].(map[string]any)
	if got["state"] != "published" || got["document_id"] != seedDoc || got["published_sha256"] != digest || got["revision"] != 3 || got["replaced_document_id"] != "" {
		t.Fatal(got)
	}
	// --recreate under a project: the one row moves and names the old
	// document for archiving by hand (review V2).
	h.call("team_roles_doc_put", "", rolesDocArgs("pending", "cccccccccccc", "aaaaaaaaaaaa", "replaced_document_id", seedDoc))
	got = h.call("team_roles_doc_get", "", nil).(map[string]any)["published"].(map[string]any)
	if got["state"] != "pending" || got["replaced_document_id"] != seedDoc {
		t.Fatal(got)
	}
	h.call("register_session", "s", map[string]any{"instance": "s-i", "role": "worker"})
	for want, c := range map[string]struct {
		op, session string
		args        map[string]any
	}{
		"a session can't":           {"team_roles_doc_get", "s", map[string]any{"instance": "s-i"}},
		"records only generated:":   {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "source_path", "docs/plan.md")},
		"bound team or project":     {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "parent_kind", "initiative")},
		"not bound":                 {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "parent_id", "99999999-0000-4000-8000-000000000099")},
		"mapping revision":          {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "mapping_revision", 0)},
		"a pending row has no":      {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "document_id", seedDoc)},
		"read-back content digest":  {"team_roles_doc_put", "", rolesDocArgs("published", "cccccccccccc", "cccccccccccc")},
		"replaced_document_id is":   {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "replaced_document_id", "x")},
		"rendered content's digest": {"team_roles_doc_put", "", rolesDocArgs("pending", "dddddddddddd", "cccccccccccc", "source_sha256", "x")},
	} {
		if e := h.errorOf(c.op, c.session, c.args); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
}

// T4 both ways, in the bus (review V1): a git publish never addresses the
// generated path or records a generated document, and the generated row
// never takes a git publish's document.
func TestTeamRolesDocKindIsolation(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	digest := strings.Repeat("e", 64)
	for op, a := range map[string]map[string]any{
		"doc_publication_get": {"parent_kind": "team", "parent_id": teamOne, "source_path": teamRolesDocSource},
		"doc_publication_put": pubArgs(1, 1, 1, "pending", "aaaaaaaaaaaa", "", "source_path", teamRolesDocSource),
	} {
		if e := h.errorOf(op, "", a); !strings.Contains(e, "never published from git") {
			t.Fatal(op, e)
		}
	}
	h.call("doc_publication_put", "", pubArgs(1, 1, 1, "published", "aaaaaaaaaaaa", "", "document_id", seedDoc, "published_sha256", digest))
	if e := h.errorOf("team_roles_doc_put", "", rolesDocArgs("published", "bbbbbbbbbbbb", "", "document_id", seedDoc, "published_sha256", digest)); !strings.Contains(e, "published from git") {
		t.Fatal(e)
	}
	h.call("team_roles_doc_put", "", rolesDocArgs("published", "bbbbbbbbbbbb", "", "document_id", rolesDocTwo, "published_sha256", digest))
	if e := h.errorOf("doc_publication_put", "", pubArgs(1, 1, 1, "published", "cccccccccccc", "aaaaaaaaaaaa", "document_id", rolesDocTwo, "published_sha256", digest)); !strings.Contains(e, "generated by ClawBridge") {
		t.Fatal(e)
	}
}

func TestTeamRolesDocMarker(t *testing.T) {
	for in, want := range map[string]struct {
		n int
		r string
	}{
		"team_roles:r4":                            {4, ""},
		"team_roles:r12;replaced:" + seedDoc:       {12, seedDoc},
		"0123456789abcdef0123456789abcdef01234567": {-1, ""},
		"team_roles:rx":                            {-1, ""},
	} {
		if n, r := teamRolesDocMarker(in); n != want.n || r != want.r {
			t.Fatal(in, n, r)
		}
	}
}
