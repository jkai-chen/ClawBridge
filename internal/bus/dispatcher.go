package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// The dispatcher (live/DISPATCHER-DESIGN.md r3): an optional coordinator
// for cloud mode. This file is its foundation: schema 17, the owner's
// domains policy with trust keyed on session IDs (DP-M1), and the two
// reserved sessions with their deny-by-default allow-lists (DP-M2). It is
// off until the owner enables it; nothing here confirms, releases or
// routes anything yet.

// Schema 17: every dispatcher table, so the build steps that follow need no
// further migration: per project the policy, decisions, escalations,
// releases and model cases; per owner the sealed model key (one active and
// one pending test at most), the one-time agent credentials, and the
// launcher's tokens (live/DISPATCHER-AGENT-DESIGN.md §11). The same DDL serves SQLite and PostgreSQL.
const dispatcherSchemaV17 = `
CREATE TABLE dispatcher_policies(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 0,mode TEXT NOT NULL DEFAULT 'rules' CHECK(mode IN ('rules','model')),domains TEXT NOT NULL,model TEXT NOT NULL DEFAULT '{}',updated_at TEXT NOT NULL,updated_by_principal TEXT NOT NULL,updated_by_session TEXT);
CREATE TABLE dispatcher_decisions(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),subject_type TEXT NOT NULL,subject_id TEXT NOT NULL,subject_revision TEXT NOT NULL DEFAULT '',domain TEXT NOT NULL DEFAULT '',facts_source TEXT NOT NULL DEFAULT '',engine TEXT NOT NULL CHECK(engine IN ('rules','model')),proposal TEXT NOT NULL,reason_code TEXT NOT NULL,explanation TEXT NOT NULL DEFAULT '',guardrail TEXT NOT NULL,action TEXT NOT NULL,policy_revision INTEGER NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX dispatcher_decisions_project ON dispatcher_decisions(project_id,created_at);
CREATE TABLE dispatcher_escalations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),subject_type TEXT NOT NULL,subject_id TEXT NOT NULL,subject_revision TEXT NOT NULL DEFAULT '',domain TEXT NOT NULL DEFAULT '',reason_code TEXT NOT NULL,recipients TEXT NOT NULL DEFAULT '[]',state TEXT NOT NULL CHECK(state IN ('open','resolved')),delivered INTEGER NOT NULL DEFAULT 0,reminded_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE INDEX dispatcher_escalations_open ON dispatcher_escalations(project_id,state);
CREATE TABLE dispatcher_releases(assignment_id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),notice_at TEXT NOT NULL,release_after TEXT NOT NULL,recipients TEXT NOT NULL DEFAULT '[]',state TEXT NOT NULL CHECK(state IN ('pending','released','cancelled')),updated_at TEXT NOT NULL);
CREATE TABLE dispatcher_model_tokens(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),token_sha256 TEXT NOT NULL UNIQUE,created_by_principal TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,revoked_at TEXT,last_used_at TEXT);
CREATE TABLE dispatcher_cases(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),kind TEXT NOT NULL CHECK(kind IN ('advise','decide','chat')),subject_type TEXT NOT NULL,subject_id TEXT NOT NULL,permitted TEXT NOT NULL DEFAULT '[]',reason_codes TEXT NOT NULL DEFAULT '[]',leased_token TEXT,deadline TEXT NOT NULL,tool_calls INTEGER NOT NULL DEFAULT 0,outcome TEXT NOT NULL DEFAULT '',outcome_body TEXT NOT NULL DEFAULT '',usage TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL,closed_at TEXT);
CREATE INDEX dispatcher_cases_open ON dispatcher_cases(project_id,closed_at);
CREATE TABLE dispatcher_model_keys(owner_principal TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending_test','active')),provider_id TEXT NOT NULL,base_url TEXT NOT NULL DEFAULT '',model TEXT NOT NULL,ciphertext TEXT NOT NULL,nonce TEXT NOT NULL,key_fingerprint TEXT NOT NULL,config_revision INTEGER NOT NULL,caps TEXT NOT NULL DEFAULT '{}',egress_text_version TEXT NOT NULL,egress_confirmed_at TEXT NOT NULL,tested_at TEXT,created_by TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(owner_principal,state));
CREATE TABLE dispatcher_agent_credentials(id TEXT PRIMARY KEY,owner_principal TEXT NOT NULL,credential_sha256 TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,bound_instance TEXT,used_at TEXT,revoked_at TEXT);
CREATE TABLE dispatcher_launcher_tokens(id TEXT PRIMARY KEY,token_sha256 TEXT NOT NULL UNIQUE,created_by TEXT NOT NULL,created_at TEXT NOT NULL,revoked_at TEXT,last_used_at TEXT);
ALTER TABLE session_assignments ADD COLUMN issue_project_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_assignments ADD COLUMN issue_labels TEXT NOT NULL DEFAULT '[]';
`

// The dispatcher's two reserved sessions. Like the watcher's, they are
// role system (so no listing, address or broadcast reaches them) with an
// owner principal no caller can hold, and each has its own audit
// principal. What each may run is its allow-list below.
const (
	DispatcherSessionName      = "clawbridge-dispatcher"
	DispatcherModelSessionName = "clawbridge-dispatcher-model"
	dispatcherPrincipal        = "system:dispatcher"
	dispatcherModelPrincipal   = "system:dispatcher-model"
)

// dispatcherAllowed is each reserved session's allow-list (DP-M2): an
// operation not listed is refused by the bus, however it arrives. The
// dispatcher (Layer 1) confirms and releases only as coordinator, within
// the guardrails, and records its own rows; the model's session reads and
// proposes. Neither ever touches a policy, the binding, labels, roles,
// intake or Linear administration.
var dispatcherAllowed = map[string]map[string]bool{
	DispatcherSessionName:      set("dispatcher_get", "list_sessions", "taskboard_get_v2", "assignment_confirm", "assignment_release", "dispatcher_decision_record", "dispatcher_escalate", "dispatcher_route", "dispatcher_release_notice"),
	DispatcherModelSessionName: set("dispatcher_get", "list_sessions", "dispatcher_case_read", "dispatcher_case_outcome", "dispatcher_route", "dispatcher_ask_session"),
}

func set(ops ...string) map[string]bool {
	m := map[string]bool{}
	for _, op := range ops {
		m[op] = true
	}
	return m
}

// DispatcherMay reports whether a reserved dispatcher session may run op.
func DispatcherMay(session, op string) bool { return dispatcherAllowed[session][op] }

// reservedDispatcherName refuses the dispatcher's names to people's
// sessions.
func reservedDispatcherName(name string) bool {
	n := strings.ToLower(name)
	return n == DispatcherSessionName || n == DispatcherModelSessionName
}

const (
	maxDispatcherDomains  = 20
	maxDispatcherTrusted  = 50
	maxDispatcherMatches  = 100
	defaultStaleAfterHour = 24
)

var dispatcherDomainID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// DispatcherSessionRef is a session as a domain names it: trust is keyed on
// its ID, and the principal is recorded with it (DP-M1). A file may name a
// session by name only; the bus resolves it when the policy is set.
type DispatcherSessionRef struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Principal string `json:"principal,omitempty"`
}

// DispatcherMatch is what puts an issue in a domain. Only a project match
// can ever grant; a label match routes (DP-H1).
type DispatcherMatch struct {
	LinearProjects []string `json:"linear_projects"`
	Labels         []string `json:"labels"`
}

// DispatcherDomain is one owner-defined domain.
type DispatcherDomain struct {
	ID                 string                 `json:"id"`
	Name               string                 `json:"name"`
	Match              DispatcherMatch        `json:"match"`
	OwnerPrincipal     string                 `json:"owner_principal"`
	CoordinatorSession *DispatcherSessionRef  `json:"coordinator_session,omitempty"`
	TrustedSessions    []DispatcherSessionRef `json:"trusted_sessions"`
	StaleAfterHours    int                    `json:"stale_after_hours"`
}

// DispatcherDomains is the owner's domains document.
type DispatcherDomains struct {
	Domains []DispatcherDomain `json:"domains"`
}

// ParseDispatcherDomains decodes a domains document strictly and checks
// its shape; checks against the project (bound projects, approved labels,
// roles, sessions) are dispatcherResolve's.
func ParseDispatcherDomains(v any) (DispatcherDomains, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 256<<10 {
		return DispatcherDomains{}, invalid("the domains document must be a JSON object under 256 KiB")
	}
	var d DispatcherDomains
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.Decode(new(any)) != io.EOF {
		return DispatcherDomains{}, invalid(`the domains document must be {"domains": [{"id", "name", "match": {"linear_projects", "labels"}, "owner_principal", "coordinator_session", "trusted_sessions", "stale_after_hours"}]}`)
	}
	if d.Domains == nil {
		d.Domains = []DispatcherDomain{}
	}
	if len(d.Domains) > maxDispatcherDomains {
		return DispatcherDomains{}, invalid(fmt.Sprintf("at most %d domains", maxDispatcherDomains))
	}
	ids := map[string]bool{}
	claimed := map[string]string{}
	for i := range d.Domains {
		dom := &d.Domains[i]
		if !dispatcherDomainID.MatchString(dom.ID) || ids[dom.ID] {
			return DispatcherDomains{}, invalid("domain IDs are unique, lower case, 1–32 of a-z, 0-9, _ and -")
		}
		ids[dom.ID] = true
		if dom.Name == "" || len(dom.Name) > 64 || policyName(dom.Name) != nil {
			return DispatcherDomains{}, invalid(fmt.Sprintf("domain %s needs a name of 1–64 characters, with no mention", dom.ID))
		}
		if dom.Match.LinearProjects == nil {
			dom.Match.LinearProjects = []string{}
		}
		if dom.Match.Labels == nil {
			dom.Match.Labels = []string{}
		}
		if n := len(dom.Match.LinearProjects) + len(dom.Match.Labels); n == 0 || n > maxDispatcherMatches {
			return DispatcherDomains{}, invalid(fmt.Sprintf("domain %s matches 1–%d Linear projects and labels", dom.ID, maxDispatcherMatches))
		}
		for _, p := range dom.Match.LinearProjects {
			if !lowerUUIDPattern.MatchString(p) {
				return DispatcherDomains{}, invalid("a domain's linear_projects are canonical project UUIDs")
			}
			// Projects are disjoint, so a project match names one domain.
			if other, taken := claimed[p]; taken {
				return DispatcherDomains{}, invalid(fmt.Sprintf("Linear project %s is in both domain %s and domain %s", p, other, dom.ID))
			}
			claimed[p] = dom.ID
		}
		if dom.OwnerPrincipal == "" || len(dom.OwnerPrincipal) > 320 {
			return DispatcherDomains{}, invalid(fmt.Sprintf("domain %s needs an owner_principal", dom.ID))
		}
		if dom.TrustedSessions == nil {
			dom.TrustedSessions = []DispatcherSessionRef{}
		}
		if len(dom.TrustedSessions) > maxDispatcherTrusted {
			return DispatcherDomains{}, invalid(fmt.Sprintf("a domain trusts at most %d sessions", maxDispatcherTrusted))
		}
		if dom.StaleAfterHours == 0 {
			dom.StaleAfterHours = defaultStaleAfterHour
		}
		if dom.StaleAfterHours < 2 || dom.StaleAfterHours > 168 {
			return DispatcherDomains{}, invalid("stale_after_hours is 2–168")
		}
	}
	return d, nil
}

// labelOverlaps names the labels more than one domain matches: allowed,
// since a label only routes, but worth a warning.
func (d DispatcherDomains) labelOverlaps() []string {
	seen := map[string]int{}
	for _, dom := range d.Domains {
		for _, l := range dom.Match.Labels {
			seen[strings.ToLower(l)]++
		}
	}
	var out []string
	for l, n := range seen {
		if n > 1 {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func dispatcherOperation(op string) bool {
	return op == "dispatcher_domains_set" || op == "dispatcher_domain_edit"
}

// principalRole is a principal's project role, for a domain owner check.
func (s *Store) principalRole(ctx context.Context, tx *transaction, w map[string]any, pid, principal string) (string, error) {
	if w["owner_principal"] == principal {
		return "owner", nil
	}
	rows, err := query(ctx, tx, `SELECT role FROM project_roles WHERE project_id=? AND principal=?`, pid, principal)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return str(rows[0], "role"), nil
}

// resolveSession resolves a domain's session reference to an active,
// person-owned session of the project, by ID or by name, and returns it
// with its ID, name and principal recorded (DP-M1).
func resolveSession(ctx context.Context, tx *transaction, pid string, ref DispatcherSessionRef) (DispatcherSessionRef, error) {
	var rows []map[string]any
	var err error
	switch {
	case ref.ID != "":
		rows, err = query(ctx, tx, `SELECT id,name,owner_principal FROM sessions WHERE project_id=? AND id=? AND role<>'system' AND status='active'`, pid, ref.ID)
	case ref.Name != "":
		rows, err = query(ctx, tx, `SELECT id,name,owner_principal FROM sessions WHERE project_id=? AND name=? AND role<>'system' AND status='active'`, pid, ref.Name)
	default:
		return ref, invalid("a session is named by its id or its name")
	}
	if err != nil {
		return ref, err
	}
	if len(rows) != 1 {
		return ref, invalid(fmt.Sprintf("no active session %s%s in this project", ref.ID, ref.Name))
	}
	got := DispatcherSessionRef{ID: str(rows[0], "id"), Name: str(rows[0], "name"), Principal: str(rows[0], "owner_principal")}
	if ref.Principal != "" && ref.Principal != got.Principal {
		return ref, invalid(fmt.Sprintf("session %s belongs to another principal now", got.Name))
	}
	return got, nil
}

// dispatcherResolve checks a domains document against the project: every
// project bound, every label approved, every owner a lead or owner, every
// session resolved to its ID and principal.
func (s *Store) dispatcherResolve(ctx context.Context, tx *transaction, w map[string]any, wid, pid string, d DispatcherDomains) (DispatcherDomains, error) {
	raw, err := s.taskboard(ctx, tx, wid, pid, "", "taskboard_get_v2", map[string]any{})
	if err != nil {
		return d, err
	}
	b, err := ParseTaskboardBindingV2(raw)
	if err != nil {
		return d, err
	}
	if !b.Configured && len(d.Domains) > 0 {
		return d, invalid("link the task board before setting dispatcher domains")
	}
	labels, err := readLabelPolicy(ctx, tx, pid)
	if err != nil {
		return d, err
	}
	approved := map[string]bool{}
	if list, ok := labels["labels"].([]string); ok {
		for _, l := range list {
			approved[strings.ToLower(l)] = true
		}
	}
	for i := range d.Domains {
		dom := &d.Domains[i]
		for _, p := range dom.Match.LinearProjects {
			if !containsString(b.LinearProjectIDs, p) {
				return d, invalid(fmt.Sprintf("domain %s: Linear project %s is not in this project's binding", dom.ID, p))
			}
		}
		for _, l := range dom.Match.Labels {
			if !approved[strings.ToLower(l)] {
				return d, invalid(fmt.Sprintf("domain %s: label %q is not on the approved label list", dom.ID, l))
			}
		}
		role, err := s.principalRole(ctx, tx, w, pid, dom.OwnerPrincipal)
		if err != nil {
			return d, err
		}
		if role != "owner" && role != "lead" {
			return d, invalid(fmt.Sprintf("domain %s: its owner must be a project lead or owner", dom.ID))
		}
		if dom.CoordinatorSession != nil {
			ref, err := resolveSession(ctx, tx, pid, *dom.CoordinatorSession)
			if err != nil {
				return d, err
			}
			dom.CoordinatorSession = &ref
		}
		seen := map[string]bool{}
		for j, t := range dom.TrustedSessions {
			ref, err := resolveSession(ctx, tx, pid, t)
			if err != nil {
				return d, err
			}
			if seen[ref.ID] {
				return d, invalid(fmt.Sprintf("domain %s trusts session %s twice", dom.ID, ref.Name))
			}
			seen[ref.ID] = true
			dom.TrustedSessions[j] = ref
		}
	}
	return d, nil
}

// readDispatcher is the project's dispatcher policy row; with none, off,
// rules mode, no domains, revision 0.
func readDispatcher(ctx context.Context, tx *transaction, pid string) (DispatcherDomains, map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT revision,enabled,mode,domains,updated_at,updated_by_principal FROM dispatcher_policies WHERE project_id=?`, pid)
	if err != nil {
		return DispatcherDomains{}, nil, err
	}
	if len(rows) == 0 {
		return DispatcherDomains{Domains: []DispatcherDomain{}}, map[string]any{"revision": int64(0), "enabled": false, "mode": "rules", "updated_at": nil, "updated_by": nil}, nil
	}
	var stored any
	if json.Unmarshal([]byte(str(rows[0], "domains")), &stored) != nil {
		return DispatcherDomains{}, nil, errors.New("stored dispatcher domains are unreadable")
	}
	d, err := ParseDispatcherDomains(stored)
	if err != nil {
		return DispatcherDomains{}, nil, errors.New("stored dispatcher domains are unreadable")
	}
	return d, map[string]any{"revision": int64(asInt(rows[0]["revision"])), "enabled": asInt(rows[0]["enabled"]) == 1, "mode": str(rows[0], "mode"), "updated_at": rows[0]["updated_at"], "updated_by": rows[0]["updated_by_principal"]}, nil
}

func dispatcherView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	d, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	meta["domains"] = d.Domains
	meta["label_overlaps"] = d.labelOverlaps()
	return meta, nil
}

// writeDispatcher stores the domains document at expected+1, by compare-
// and-swap on the revision.
func (s *Store) writeDispatcher(ctx context.Context, tx *transaction, pid string, expected int64, d DispatcherDomains, actor map[string]any) (int64, error) {
	encoded, _ := json.Marshal(d)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return 0, err
	}
	var session any
	if actor != nil {
		session = actor["id"]
	}
	revision := expected + 1
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_policies(project_id,revision,domains,updated_at,updated_by_principal,updated_by_session) VALUES(?,?,?,?,?,?)`, pid, revision, string(encoded), now, s.principal, session)
	} else {
		var result interface{ RowsAffected() (int64, error) }
		result, err = tx.ExecContext(ctx, `UPDATE dispatcher_policies SET revision=?,domains=?,updated_at=?,updated_by_principal=?,updated_by_session=? WHERE project_id=? AND revision=?`, revision, string(encoded), now, s.principal, session, pid, expected)
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				err = conflict("the dispatcher policy changed concurrently; read it again")
			}
		}
	}
	return revision, err
}

// dispatcherOp sets the domains (the project owner) or edits one domain's
// trust list and coordinator (its owner, or the project owner). Reading the
// policy (dispatcher_get) is open to every member and handled before
// sessions.
func (s *Store) dispatcherOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid, a := w["id"].(string), p["id"].(string), r.Args
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	current, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid(r.Operation + " requires expected_revision")
	}
	if meta["revision"].(int64) != int64(expected) {
		return nil, conflict(fmt.Sprintf("the dispatcher policy changed: current revision is %d", meta["revision"]))
	}
	var next DispatcherDomains
	details := map[string]any{}
	switch r.Operation {
	case "dispatcher_domains_set":
		if projectRole != "owner" {
			return nil, errors.New("project owner permission required")
		}
		if next, err = ParseDispatcherDomains(a["dispatcher_domains"]); err != nil {
			return nil, err
		}
		if next, err = s.dispatcherResolve(ctx, tx, w, wid, pid, next); err != nil {
			return nil, err
		}
		ids := []string{}
		for _, d := range next.Domains {
			ids = append(ids, d.ID)
		}
		details["domains"] = ids
	case "dispatcher_domain_edit":
		domain, action := str(a, "domain"), str(a, "action")
		idx := -1
		for i, d := range current.Domains {
			if d.ID == domain {
				idx = i
			}
		}
		if idx < 0 {
			return nil, invalid("no such domain")
		}
		dom := &current.Domains[idx]
		// A domain's owner edits their own domain's trust and coordinator
		// only (DP-L10); the project owner may edit any.
		if projectRole != "owner" && !(projectRole == "lead" && dom.OwnerPrincipal == s.principal) {
			return nil, errors.New("only the domain's owner or the project owner may edit it")
		}
		ref, err := resolveSession(ctx, tx, pid, DispatcherSessionRef{Name: str(a, "target_session")})
		if action == "untrust" {
			// A session may be untrusted after it is retired: match the
			// recorded name or ID without requiring it active.
			err = nil
			ref = DispatcherSessionRef{Name: str(a, "target_session")}
		}
		if err != nil {
			return nil, err
		}
		switch action {
		case "trust":
			for _, t := range dom.TrustedSessions {
				if t.ID == ref.ID {
					return nil, invalid("that session is already trusted in this domain")
				}
			}
			if len(dom.TrustedSessions) >= maxDispatcherTrusted {
				return nil, invalid(fmt.Sprintf("a domain trusts at most %d sessions", maxDispatcherTrusted))
			}
			dom.TrustedSessions = append(dom.TrustedSessions, ref)
		case "untrust":
			kept := []DispatcherSessionRef{}
			for _, t := range dom.TrustedSessions {
				if t.Name != ref.Name && t.ID != ref.Name {
					kept = append(kept, t)
				}
			}
			if len(kept) == len(dom.TrustedSessions) {
				return nil, invalid("that session is not trusted in this domain")
			}
			dom.TrustedSessions = kept
		case "coordinator":
			dom.CoordinatorSession = &ref
		default:
			return nil, invalid("a domain edit is trust, untrust or coordinator")
		}
		next = current
		details["domain"], details["action"], details["session"] = domain, action, ref.Name
		if ref.ID != "" {
			details["session_id"] = ref.ID
		}
	}
	revision, err := s.writeDispatcher(ctx, tx, pid, int64(expected), next, actor)
	if err != nil {
		return nil, err
	}
	details["revision"] = revision
	if err = s.audit(ctx, tx, wid, pid, "", "dispatcher_policy", pid, r.Operation, details); err != nil {
		return nil, err
	}
	return dispatcherView(ctx, tx, pid)
}

// ensureDispatcherSessions creates the project's two reserved dispatcher
// sessions if they are missing: role system, an owner principal no caller
// can hold, and a reserved name.
func (s *Store) ensureDispatcherSessions(ctx context.Context, tx *transaction, pid string) error {
	for _, d := range []struct{ name, prefix, notes string }{
		{DispatcherSessionName, "dsp_", "ClawBridge dispatcher (rules)"},
		{DispatcherModelSessionName, "dsm_", "ClawBridge dispatcher model agent"},
	} {
		rows, err := query(ctx, tx, `SELECT 1 FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, d.name)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			continue
		}
		now := stamp()
		if _, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,project_id,name,instance,owner_principal,role,agent_kind,model,host,account,transport,notes,status,last_seen,created_at,updated_at) VALUES(?,?,?,?,'',?,'system','','','','{}',?,'active',?,?,?)`,
			d.prefix+strings.TrimPrefix(pid, "prj_"), pid, d.name, newID("inst_"), systemRole, d.notes, now, now, now); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteDispatcher runs one bus operation as one of a project's reserved
// dispatcher sessions. It is a Go call for the server's dispatcher and the
// dispatcher-model API only; no request path reaches it. An operation not
// on the session's allow-list is refused before anything is read (DP-M2).
func (s *Store) ExecuteDispatcher(ctx context.Context, projectID, session string, r Request) (Result, error) {
	if _, reserved := dispatcherAllowed[session]; !reserved {
		return Result{}, classify(scoped("not a dispatcher session", "forbidden"))
	}
	if !DispatcherMay(session, r.Operation) {
		return Result{}, classify(scoped(fmt.Sprintf("operation %s is not allowed for %s", r.Operation, session), "forbidden"))
	}
	if r.Args == nil {
		r.Args = map[string]any{}
	}
	if _, known := operationArgs[r.Operation]; known {
		if err := validateArgs(r.Operation, r.Args); err != nil {
			return Result{}, classify(invalid(err.Error()))
		}
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	p, err := one(ctx, tx, `SELECT * FROM projects WHERE id=?`, projectID)
	if err != nil {
		return Result{}, classify(scopedError(err, "project not found", "not_found"))
	}
	wid := str(p, "workspace_id")
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+wid); err != nil {
			return Result{}, errors.New("authority unavailable")
		}
	}
	scopedStore := *s
	scopedStore.principal = dispatcherPrincipal
	if session == DispatcherModelSessionName {
		scopedStore.principal = dispatcherModelPrincipal
	}
	if err = scopedStore.ensureDispatcherSessions(ctx, tx, projectID); err != nil {
		return Result{}, err
	}
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND role='system' AND name=?`, projectID, session)
	if err != nil {
		return Result{}, classify(scopedError(err, "the project has no dispatcher session", "not_found"))
	}
	tx.actorID = str(actor, "id")
	var out any
	switch r.Operation {
	case "dispatcher_get":
		out, err = dispatcherView(ctx, tx, projectID)
	case "taskboard_get_v2":
		out, err = scopedStore.taskboard(ctx, tx, wid, projectID, "owner", r.Operation, r.Args)
	default:
		// Allowed, and built in a later step of the dispatcher.
		err = scoped(fmt.Sprintf("operation %s is not available in this release", r.Operation), "unsupported")
	}
	if err != nil {
		return Result{}, classify(err)
	}
	if err = tx.Commit(); err != nil {
		return Result{}, errors.New("authority commit failed")
	}
	return Result{Source: "server", Data: out}, nil
}
