package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"clawbridge/internal/principal"
)

// Team roles (live/TEAM-ROLE-MAPPING-DESIGN.md): who leads each bound
// Linear team, and each person's role, held in ClawBridge because Linear's
// own team owner is every workspace admin. Step 1 stores and shows the
// mapping; its consumers (trust by role, subscribers, project leads, the
// dispatcher) come later, each switched on per project.
const teamRolesSchema = `
CREATE TABLE team_role_mapping(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),mapping TEXT NOT NULL,updated_by TEXT NOT NULL,updated_at TEXT NOT NULL);
`

const (
	maxTeamLeads     = 3
	maxRolePeople    = 200
	teamRolesNoBoard = "the task board isn't linked; team roles belong to bound teams"
)

// TeamRoleNames are the roles a person can have. More need a design change.
var TeamRoleNames = []string{"lead", "engineer", "reviewer"}

// TeamRoleTeam is one bound team and its leads (Linear user IDs).
type TeamRoleTeam struct {
	TeamID string   `json:"team_id"`
	Leads  []string `json:"leads"`
}

// TeamRolePerson is one person: their Linear user, role and teams, and the
// ClawBridge principal the owner says is theirs (T3: an assertion).
type TeamRolePerson struct {
	LinearUserID string   `json:"linear_user_id"`
	Role         string   `json:"role"`
	Teams        []string `json:"teams"`
	Principal    string   `json:"principal,omitempty"`
}

// TeamRoles is the mapping.
type TeamRoles struct {
	Teams  []TeamRoleTeam   `json:"teams"`
	People []TeamRolePerson `json:"people"`
}

func teamRolesOperation(op string) bool {
	return op == "team_roles_get" || op == "team_roles_set" || op == "team_roles_doc_get" || op == "team_roles_doc_put"
}

// teamRolesOp reads the mapping (any member or session) or sets it (the
// project owner, or a team's lead within their own teams; without a
// session; on the hosted server only from a person's own device).
func (s *Store) teamRolesOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, errors.New("instance requires session")
	}
	if r.Operation == "team_roles_get" {
		return s.readTeamRolesView(ctx, tx, pid)
	}
	if r.Session != "" {
		return nil, scoped("a session can't set team roles; the project owner runs linear roles set in their own terminal", "forbidden")
	}
	// The generated document is the owner's alone (design §13).
	if r.Operation == "team_roles_doc_get" || r.Operation == "team_roles_doc_put" {
		if projectRole != "owner" {
			return nil, scoped("project owner permission required", "forbidden")
		}
		if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
			return nil, scoped("the team roles document is published only from a person's own signed-in device", "forbidden")
		}
		if r.Operation == "team_roles_doc_get" {
			return s.teamRolesDocGet(ctx, tx, pid)
		}
		return s.teamRolesDocPut(ctx, tx, wid, pid, a)
	}
	if projectRole == "" || projectRole == "reader" {
		return nil, scoped("project owner permission required", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("team roles are set only from a person's own signed-in device", "forbidden")
	}
	return s.setTeamRoles(ctx, tx, wid, pid, a, projectRole != "owner")
}

// setTeamRoles checks the new mapping against the binding and the
// project's members, and stores it at the revision read. Without confirm
// it is a preview: the change, counted, and nothing written. byLead is a
// caller who isn't the owner: their set must stay inside the teams they
// lead (checkLeadEdit).
func (s *Store) setTeamRoles(ctx context.Context, tx *transaction, wid, pid string, a map[string]any, byLead bool) (any, error) {
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("team_roles_set requires expected_revision")
	}
	current, revision, err := readTeamRoles(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if revision != expected {
		return nil, conflict(fmt.Sprintf("team roles changed: current revision is %d; read them again", revision))
	}
	next, err := decodeTeamRoles(a["mapping"])
	if err != nil {
		return nil, err
	}
	b, err := s.kindBinding(ctx, tx, pid)
	if err != nil {
		if strings.Contains(err.Error(), projectKindNoTeam) {
			return nil, invalid(teamRolesNoBoard)
		}
		return nil, err
	}
	if err = checkTeamRoles(&next, b.LinearTeamIDs); err != nil {
		return nil, err
	}
	if byLead {
		led := ledTeams(current, s.principal, b.LinearTeamIDs)
		if len(led) == 0 {
			return nil, scoped("only the project owner or a team's lead sets team roles; a lead's row must be linked to their ClawBridge account by the owner", "forbidden")
		}
		if err = checkLeadEdit(current, next, led); err != nil {
			return nil, err
		}
	}
	for _, p := range next.People {
		if p.Principal == "" {
			continue
		}
		member, e := s.projectMember(ctx, tx, wid, pid, p.Principal)
		if e != nil {
			return nil, e
		}
		if !member {
			return nil, invalid("a person's principal must be a member of this project")
		}
	}
	change := teamRolesChange(current, next)
	if a["confirm"] != true {
		return map[string]any{"preview": true, "revision": revision, "mapping": next, "change": change}, nil
	}
	encoded, _ := json.Marshal(next)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	revision = expected + 1
	// A first set that races another one finds the row there (review L3):
	// the same conflict as an update at a stale revision.
	var result interface{ RowsAffected() (int64, error) }
	if expected == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(project_id) DO NOTHING`, pid, revision, string(encoded), s.principal, now)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE team_role_mapping SET revision=?,mapping=?,updated_by=?,updated_at=? WHERE project_id=? AND revision=?`, revision, string(encoded), s.principal, now, pid, expected)
	}
	if err == nil {
		if n, _ := result.RowsAffected(); n != 1 {
			err = conflict("team roles changed concurrently; read them again")
		}
	}
	if err != nil {
		return nil, err
	}
	// Counts only: never a name, an email or a Linear user ID (T6).
	by := "owner"
	if byLead {
		by = "lead"
	}
	detail := map[string]any{"revision": revision, "by": by, "teams": len(next.Teams), "people": len(next.People), "roles": roleCounts(next), "added": change["added"], "removed": change["removed"], "changed": change["changed"]}
	if err = s.audit(ctx, tx, wid, pid, "", "team_roles", pid, "set", detail); err != nil {
		return nil, err
	}
	return s.readTeamRolesView(ctx, tx, pid)
}

// projectMember is whether principal is a member of the project: on the
// hosted server the workspace owner or a holder of a project role; locally
// only the local principal, who owns everything.
func (s *Store) projectMember(ctx context.Context, tx *transaction, wid, pid, who string) (bool, error) {
	if !s.postgres {
		return samePrincipal(who, s.principal), nil
	}
	w, err := one(ctx, tx, `SELECT owner_principal FROM workspaces WHERE id=?`, wid)
	if err != nil {
		return false, err
	}
	if w["owner_principal"] == who {
		return true, nil
	}
	rows, err := query(ctx, tx, `SELECT 1 FROM project_roles WHERE project_id=? AND principal=?`, pid, who)
	return len(rows) > 0, err
}

// ledTeams are the bound teams that the caller leads: teams whose leads
// include a person with role lead whose row the owner linked to the
// caller's principal (T3). Only the stored mapping counts, never the set.
func ledTeams(m TeamRoles, who string, bound []string) []string {
	mine := map[string]bool{}
	for _, p := range m.People {
		if p.Role == "lead" && p.Principal != "" && samePrincipal(p.Principal, who) {
			mine[p.LinearUserID] = true
		}
	}
	var out []string
	for _, t := range m.Teams {
		if !slices.Contains(bound, t.TeamID) {
			continue
		}
		for _, l := range t.Leads {
			if mine[l] {
				out = append(out, t.TeamID)
				break
			}
		}
	}
	return out
}

// checkLeadEdit holds a lead's set to the owner's ruling (design §11.1 Q1,
// review T2'): the teams and their leads stay as they are, and every row
// added, removed or changed is an engineer or reviewer, before and after,
// working only in teams the lead leads. A lead never changes a lead row,
// their own included, and never links a person to a ClawBridge account:
// that link is the owner's assertion (T3).
func checkLeadEdit(old, next TeamRoles, led []string) error {
	before, _ := json.Marshal(old.Teams)
	after, _ := json.Marshal(next.Teams)
	if string(before) != string(after) {
		return scoped("a lead can't change which teams are mapped or who leads them; the project owner does", "forbidden")
	}
	rows := map[string][2]*TeamRolePerson{}
	for i := range old.People {
		p := &old.People[i]
		r := rows[p.LinearUserID]
		r[0] = p
		rows[p.LinearUserID] = r
	}
	for i := range next.People {
		p := &next.People[i]
		r := rows[p.LinearUserID]
		r[1] = p
		rows[p.LinearUserID] = r
	}
	for _, r := range rows {
		was, now := r[0], r[1]
		if was != nil && now != nil && was.Role == now.Role && was.Principal == now.Principal && slices.Equal(was.Teams, now.Teams) {
			continue
		}
		for _, p := range r {
			if p == nil {
				continue
			}
			if p.Role == "lead" {
				return scoped("a lead sets only engineer and reviewer roles; the project owner sets leads", "forbidden")
			}
			for _, t := range p.Teams {
				if !slices.Contains(led, t) {
					return scoped("a lead sets roles only for people who work only in teams they lead", "forbidden")
				}
			}
		}
		if now != nil && (was == nil && now.Principal != "" || was != nil && was.Principal != now.Principal) {
			return scoped("only the project owner links a person to a ClawBridge account", "forbidden")
		}
	}
	return nil
}

// samePrincipal compares principals as stored: a stored one is normalized
// (lowercased), while the local one keeps the OS user's case (on Windows,
// local:SID:DOMAIN\user).
func samePrincipal(a, b string) bool { return principal.Normal(a) == principal.Normal(b) }

// decodeTeamRoles reads the mapping argument, an object or its JSON text.
func decodeTeamRoles(v any) (TeamRoles, error) {
	var out TeamRoles
	var raw []byte
	switch m := v.(type) {
	case string:
		raw = []byte(m)
	case nil:
		return out, invalid("team_roles_set requires mapping")
	default:
		raw, _ = json.Marshal(m)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, invalid("mapping is {\"teams\": [{\"team_id\", \"leads\"}], \"people\": [{\"linear_user_id\", \"role\", \"teams\", \"principal\"?}]}")
	}
	return out, nil
}

// checkTeamRoles holds the design's rules (§3), and lowers the IDs. Every
// team is bound; a lead is a person with role lead in that team; a person
// has one role and is in at least one bound team.
func checkTeamRoles(m *TeamRoles, bound []string) error {
	if m.Teams == nil {
		m.Teams = []TeamRoleTeam{}
	}
	if m.People == nil {
		m.People = []TeamRolePerson{}
	}
	if len(m.People) > maxRolePeople {
		return invalid(fmt.Sprintf("at most %d people", maxRolePeople))
	}
	people := map[string]*TeamRolePerson{}
	for i := range m.People {
		p := &m.People[i]
		p.LinearUserID = strings.ToLower(strings.TrimSpace(p.LinearUserID))
		if !viewIDPattern.MatchString(p.LinearUserID) {
			return invalid("each person's linear_user_id is a Linear user ID")
		}
		if people[p.LinearUserID] != nil {
			return invalid("a person is listed once, with one role")
		}
		if !slices.Contains(TeamRoleNames, p.Role) {
			return invalid("a role is lead, engineer or reviewer")
		}
		if len(p.Teams) == 0 {
			return invalid("each person is in at least one bound team")
		}
		seen := map[string]bool{}
		for j, t := range p.Teams {
			t = strings.ToLower(t)
			if !slices.Contains(bound, t) {
				return invalid("each team is a team of this project's binding")
			}
			if seen[t] {
				return invalid("a person lists a team once")
			}
			seen[t] = true
			p.Teams[j] = t
		}
		sort.Strings(p.Teams)
		// T3: the owner's word for who this is in ClawBridge, checked here
		// to be a project member and again by every consumer that uses it.
		p.Principal = principal.Normal(p.Principal)
		if principal.CI(p.Principal) {
			return invalid("a person's principal is a person's account, not a CI device")
		}
		people[p.LinearUserID] = p
	}
	teams := map[string]bool{}
	for i := range m.Teams {
		t := &m.Teams[i]
		t.TeamID = strings.ToLower(t.TeamID)
		if !slices.Contains(bound, t.TeamID) {
			return invalid("each team is a team of this project's binding")
		}
		if teams[t.TeamID] {
			return invalid("a team is listed once")
		}
		teams[t.TeamID] = true
		if t.Leads == nil {
			t.Leads = []string{}
		}
		if len(t.Leads) > maxTeamLeads {
			return invalid(fmt.Sprintf("a team has at most %d leads", maxTeamLeads))
		}
		seen := map[string]bool{}
		for j, l := range t.Leads {
			l = strings.ToLower(l)
			p := people[l]
			if p == nil || p.Role != "lead" || !slices.Contains(p.Teams, t.TeamID) {
				return invalid("a team's lead is listed under people with role lead in that team")
			}
			if seen[l] {
				return invalid("a team lists a lead once")
			}
			seen[l] = true
			t.Leads[j] = l
		}
	}
	sort.Slice(m.Teams, func(i, j int) bool { return m.Teams[i].TeamID < m.Teams[j].TeamID })
	sort.Slice(m.People, func(i, j int) bool { return m.People[i].LinearUserID < m.People[j].LinearUserID })
	return nil
}

// teamRolesChange counts what a set changes, by person: added, removed,
// and changed (another role, teams or principal).
func teamRolesChange(old, next TeamRoles) map[string]any {
	before := map[string]TeamRolePerson{}
	for _, p := range old.People {
		before[p.LinearUserID] = p
	}
	added, changed := 0, 0
	for _, p := range next.People {
		was, ok := before[p.LinearUserID]
		switch {
		case !ok:
			added++
		case was.Role != p.Role || was.Principal != p.Principal || !slices.Equal(was.Teams, p.Teams):
			changed++
		}
		delete(before, p.LinearUserID)
	}
	leads := map[string]string{}
	for _, t := range old.Teams {
		leads[t.TeamID] = strings.Join(t.Leads, ",")
	}
	teamsChanged := 0
	for _, t := range next.Teams {
		if was, ok := leads[t.TeamID]; !ok || was != strings.Join(t.Leads, ",") {
			teamsChanged++
		}
		delete(leads, t.TeamID)
	}
	return map[string]any{"added": added, "removed": len(before), "changed": changed, "teams_changed": teamsChanged + len(leads)}
}

func roleCounts(m TeamRoles) map[string]int {
	out := map[string]int{}
	for _, r := range TeamRoleNames {
		out[r] = 0
	}
	for _, p := range m.People {
		out[p.Role]++
	}
	return out
}

// readTeamRoles is the stored mapping and its revision; revision 0 and an
// empty mapping when none is set.
func readTeamRoles(ctx context.Context, tx *transaction, pid string) (TeamRoles, int, error) {
	empty := TeamRoles{Teams: []TeamRoleTeam{}, People: []TeamRolePerson{}}
	row, err := one(ctx, tx, `SELECT revision,mapping FROM team_role_mapping WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return empty, 0, nil
	}
	if err != nil {
		return empty, 0, err
	}
	var m TeamRoles
	if json.Unmarshal([]byte(fmt.Sprint(row["mapping"])), &m) != nil {
		return empty, 0, errors.New("the stored team roles are unreadable")
	}
	return m, asInt(row["revision"]), nil
}

// readTeamRolesView is what team_roles_get shows: the mapping, with each
// team marked bound or not (a later binding change can drop one; every
// consumer then ignores it), who set it when, and you_lead: the bound teams
// the reader leads, so a lead's view knows what it may change. The server
// checks that again at set.
func (s *Store) readTeamRolesView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	m, revision, err := readTeamRoles(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	var bound []string
	if b, e := s.kindBinding(ctx, tx, pid); e == nil {
		bound = b.LinearTeamIDs
	}
	teams := []map[string]any{}
	for _, t := range m.Teams {
		teams = append(teams, map[string]any{"team_id": t.TeamID, "leads": t.Leads, "team_bound": slices.Contains(bound, t.TeamID)})
	}
	youLead := ledTeams(m, s.principal, bound)
	if youLead == nil {
		youLead = []string{}
	}
	out := map[string]any{"revision": revision, "teams": teams, "people": m.People, "roles": roleCounts(m), "you_lead": youLead, "updated_by": nil, "updated_at": nil}
	if revision > 0 {
		row, e := one(ctx, tx, `SELECT updated_by,updated_at FROM team_role_mapping WHERE project_id=?`, pid)
		if e != nil {
			return nil, e
		}
		out["updated_by"], out["updated_at"] = row["updated_by"], row["updated_at"]
	}
	return out, nil
}

// roleTrusted is the mapping's half of trust by role for intake (design
// §6.3, review T2' (a)): whether the stored mapping gives the Linear user
// one of roles in the issue's team, at the current revision, with that
// team still bound. reason says why not, for the caller's audit. It fails
// closed: no mapping, an unreadable one or a dropped team is not trusted.
// T2' (b), an active non-guest workspace user read within 15 minutes, is a
// Linear read and stays with the caller, as does excluding bots by the
// actor class (§12 L1/L2). reviewer never authorises (r3), so roles naming
// it trusts nobody.
func (s *Store) roleTrusted(ctx context.Context, tx *transaction, pid, team, linearUserID string, roles []string) (bool, string, error) {
	if len(roles) == 0 {
		return false, "no roles are trusted", nil
	}
	for _, r := range roles {
		if r != "lead" && r != "engineer" {
			return false, "only lead and engineer can be trusted roles", nil
		}
	}
	m, revision, err := readTeamRoles(ctx, tx, pid)
	if err != nil {
		return false, "the team roles are unreadable", err
	}
	if revision == 0 {
		return false, "no team roles are set", nil
	}
	team, linearUserID = strings.ToLower(team), strings.ToLower(linearUserID)
	b, err := s.kindBinding(ctx, tx, pid)
	if err != nil {
		return false, "the binding is unreadable", err
	}
	if !slices.Contains(b.LinearTeamIDs, team) {
		return false, "the issue's team isn't bound", nil
	}
	for _, p := range m.People {
		if p.LinearUserID != linearUserID {
			continue
		}
		if !slices.Contains(p.Teams, team) {
			return false, "the person doesn't work in the issue's team", nil
		}
		if !slices.Contains(roles, p.Role) {
			return false, "the person's role isn't trusted", nil
		}
		return true, "trusted by role " + p.Role, nil
	}
	return false, "the person has no team role", nil
}
