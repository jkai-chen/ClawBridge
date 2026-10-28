package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

const rolesUsage = `clawbridge linear roles show [--names --profile ID]
clawbridge linear roles set --file F --expected-revision N [--confirm] [--profile ID]
clawbridge linear roles person WHO --role lead|engineer|reviewer --team UUID [--team UUID]... [--link EMAIL] --expected-revision N [--confirm] [--profile ID]
clawbridge linear roles person WHO --remove --expected-revision N [--confirm]
clawbridge linear roles lead TEAM_UUID WHO [--remove] --expected-revision N [--confirm] [--profile ID]
  Who leads each bound Linear team, and each person's role. Any member reads
  it. Only the project owner changes it, in their own terminal, never with
  --session. WHO is a Linear name, email or user ID, looked up among the
  Linear workspace's users through your own Linear connection: a role needs
  workspace membership, never a team's, and a guest or inactive user can't
  have one. Without --confirm a change is only previewed.`

type rolesFlags struct {
	file, role, link, profile string
	teams                     multiFlag
	revision                  int64
	confirm, remove, names    bool
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// runLinearRoles is `clawbridge linear roles`.
func runLinearRoles(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(stdout, rolesUsage)
		return err
	}
	action, rest := args[0], args[1:]
	var v rolesFlags
	f := flag.NewFlagSet("linear roles "+action, flag.ContinueOnError)
	f.SetOutput(stderr)
	f.StringVar(&v.profile, "profile", flags.LinearProfile, "your Linear connection, to look people up (clawbridge linear profiles)")
	min, max := 0, 0
	switch action {
	case "show":
		f.BoolVar(&v.names, "names", false, "add each person's Linear name, read through your own connection")
	case "set":
		f.StringVar(&v.file, "file", "", "JSON file {\"teams\": [{\"team_id\", \"leads\"}], \"people\": [{\"linear_user_id\", \"role\", \"teams\", \"principal\"?}]}")
	case "person":
		f.StringVar(&v.role, "role", "", "lead, engineer or reviewer")
		f.Var(&v.teams, "team", "a bound team UUID the person works in (repeatable)")
		// Not --principal: that is the global flag for who you sign in as.
		f.StringVar(&v.link, "link", "", "the person's ClawBridge account, as the owner asserts it (kept when omitted)")
		f.BoolVar(&v.remove, "remove", false, "remove the person, and their leads")
		min, max = 1, 1
	case "lead":
		f.BoolVar(&v.remove, "remove", false, "stop the person leading the team")
		min, max = 2, 2
	default:
		return errors.New("use linear roles show|set|person|lead")
	}
	if action != "show" {
		f.Int64Var(&v.revision, "expected-revision", -1, "the roles' revision you last read (0 for none)")
		f.BoolVar(&v.confirm, "confirm", false, "store the change the preview showed")
	}
	// Flags may come after the positional arguments (person "Ada" --role
	// reviewer): parse, take one positional, and parse the rest.
	var pos []string
	for {
		if err := f.Parse(rest); err != nil {
			return err
		}
		if f.NArg() == 0 {
			break
		}
		pos, rest = append(pos, f.Arg(0)), f.Args()[1:]
	}
	if len(pos) < min || len(pos) > max {
		return fmt.Errorf("usage:\n%s", rolesUsage)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	executor, closeExecutor, scope, err := linearExecutor(bounded, flags)
	if err != nil {
		return err
	}
	defer closeExecutor()
	read := func() (rolesView, error) {
		result, e := executor.Execute(bounded, bus.Request{Operation: "team_roles_get", Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Args: instanceArgs(scope)})
		if e != nil {
			return rolesView{}, e
		}
		return decodeRolesView(result.Data)
	}
	if action == "show" {
		current, e := read()
		if e != nil {
			return e
		}
		out := map[string]any{"revision": current.Revision, "teams": current.Teams, "people": current.People, "roles": current.Roles}
		if v.names {
			users, e := rolesUsers(bounded, flags, executor, scope, v.profile)
			if e != nil {
				return e
			}
			names := map[string]string{}
			for _, u := range users {
				names[u.ID] = terminalSafe(u.Name)
			}
			out["names"] = names
		}
		return emit(ctx, stdout, "linear_roles", bus.Result{Source: "server", Data: out})
	}
	// Setting roles is the owner's own act: never a session's.
	if flags.Session != "" {
		return errors.New("team roles are set without --session; the project owner runs this in their own terminal")
	}
	if v.revision < 0 {
		return errors.New("--expected-revision is required: the revision linear roles show printed (0 for none)")
	}
	current, err := read()
	if err != nil {
		return err
	}
	if current.Revision != v.revision {
		return fmt.Errorf("team roles changed: current revision is %d; read them again", current.Revision)
	}
	users, err := rolesUsers(bounded, flags, executor, scope, v.profile)
	if err != nil {
		return err
	}
	next := bus.TeamRoles{Teams: current.mappingTeams(), People: current.People}
	switch action {
	case "set":
		if v.file == "" {
			return errors.New("roles set requires --file")
		}
		raw, e := readLinearFile(v.file)
		if e != nil {
			return e
		}
		next = bus.TeamRoles{}
		if e = json.Unmarshal([]byte(raw), &next); e != nil {
			return errors.New("the roles file is {\"teams\": [...], \"people\": [...]}")
		}
	case "person":
		who, e := matchRoleUser(users, pos[0])
		if e != nil {
			return e
		}
		// Changing a person keeps what the change doesn't name: their account
		// link unless --link replaces it, and, while they stay a lead, their
		// leads in the teams they keep.
		link, keepLeads := v.link, []string{}
		for _, p := range next.People {
			if p.LinearUserID == who.ID && link == "" {
				link = p.Principal
			}
		}
		for _, t := range next.Teams {
			if v.role == "lead" && containsString(t.Leads, who.ID) && slices.ContainsFunc(v.teams, func(s string) bool { return strings.EqualFold(s, t.TeamID) }) {
				keepLeads = append(keepLeads, t.TeamID)
			}
		}
		next = withoutPerson(next, who.ID)
		if !v.remove {
			if v.role == "" || len(v.teams) == 0 {
				return errors.New("roles person requires --role and at least one --team, or --remove")
			}
			next.People = append(next.People, bus.TeamRolePerson{LinearUserID: who.ID, Role: v.role, Teams: v.teams, Principal: link})
			for _, t := range keepLeads {
				next = withLead(next, t, who.ID, false)
			}
		}
	case "lead":
		team := strings.ToLower(pos[0])
		who, e := matchRoleUser(users, pos[1])
		if e != nil {
			return e
		}
		next = withLead(next, team, who.ID, v.remove)
	}
	if err = checkRoleUsers(next, users); err != nil {
		return err
	}
	scope.Session = ""
	result, err := executor.Execute(bounded, bus.Request{Operation: "team_roles_set", Workspace: scope.Workspace, Project: scope.Project, Args: map[string]any{"expected_revision": v.revision, "confirm": v.confirm, "mapping": rolesArg(next)}})
	if err != nil {
		return err
	}
	data := result.Data
	if m, ok := data.(map[string]any); ok && m["preview"] == true {
		m["next"] = "run the same command with --confirm to store it"
	}
	return emit(ctx, stdout, "linear_roles", bus.Result{Source: result.Source, Data: data})
}

func instanceArgs(scope bus.Request) map[string]any {
	a := map[string]any{}
	if i, ok := scope.Args["instance"]; ok && scope.Session != "" {
		a["instance"] = i
	}
	return a
}

// rolesView is team_roles_get's answer.
type rolesView struct {
	Revision int64                `json:"revision"`
	Teams    []rolesViewTeam      `json:"teams"`
	People   []bus.TeamRolePerson `json:"people"`
	Roles    map[string]int       `json:"roles"`
}

type rolesViewTeam struct {
	TeamID    string   `json:"team_id"`
	Leads     []string `json:"leads"`
	TeamBound bool     `json:"team_bound"`
}

func decodeRolesView(data any) (rolesView, error) {
	var v rolesView
	raw, _ := json.Marshal(data)
	if json.Unmarshal(raw, &v) != nil {
		return v, errors.New("the server's answer had no team roles")
	}
	if v.People == nil {
		v.People = []bus.TeamRolePerson{}
	}
	return v, nil
}

func (v rolesView) mappingTeams() []bus.TeamRoleTeam {
	out := []bus.TeamRoleTeam{}
	for _, t := range v.Teams {
		out = append(out, bus.TeamRoleTeam{TeamID: t.TeamID, Leads: append([]string{}, t.Leads...)})
	}
	return out
}

// withoutPerson drops a person and every lead they hold.
func withoutPerson(m bus.TeamRoles, id string) bus.TeamRoles {
	people := []bus.TeamRolePerson{}
	for _, p := range m.People {
		if p.LinearUserID != id {
			people = append(people, p)
		}
	}
	teams := []bus.TeamRoleTeam{}
	for _, t := range m.Teams {
		leads := []string{}
		for _, l := range t.Leads {
			if l != id {
				leads = append(leads, l)
			}
		}
		teams = append(teams, bus.TeamRoleTeam{TeamID: t.TeamID, Leads: leads})
	}
	return bus.TeamRoles{Teams: teams, People: people}
}

// withLead makes id a lead of team (or stops it): the person becomes a
// lead in that team if they aren't one yet.
func withLead(m bus.TeamRoles, team, id string, remove bool) bus.TeamRoles {
	found := false
	for i := range m.Teams {
		if m.Teams[i].TeamID != team {
			continue
		}
		found = true
		leads := []string{}
		for _, l := range m.Teams[i].Leads {
			if l != id {
				leads = append(leads, l)
			}
		}
		if !remove {
			leads = append(leads, id)
		}
		m.Teams[i].Leads = leads
	}
	if !found && !remove {
		m.Teams = append(m.Teams, bus.TeamRoleTeam{TeamID: team, Leads: []string{id}})
	}
	if remove {
		return m
	}
	for i := range m.People {
		if m.People[i].LinearUserID == id {
			m.People[i].Role = "lead"
			if !containsString(m.People[i].Teams, team) {
				m.People[i].Teams = append(m.People[i].Teams, team)
			}
			return m
		}
	}
	m.People = append(m.People, bus.TeamRolePerson{LinearUserID: id, Role: "lead", Teams: []string{team}})
	return m
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkRoleUsers holds T2' at set: every person is an active, non-guest
// user of the Linear workspace, as Linear states it.
func checkRoleUsers(m bus.TeamRoles, users []linearprovider.WorkspaceUser) error {
	byID := map[string]linearprovider.WorkspaceUser{}
	for _, u := range users {
		byID[strings.ToLower(u.ID)] = u
	}
	for _, p := range m.People {
		u, ok := byID[strings.ToLower(p.LinearUserID)]
		switch {
		case !ok:
			return fmt.Errorf("Linear user %s isn't a user of this Linear workspace", p.LinearUserID)
		case !u.Eligible():
			return fmt.Errorf("%s can't have a role: only an active, non-guest user of the Linear workspace can (as Linear states it)", terminalSafe(u.Name))
		}
	}
	return nil
}

// matchRoleUser finds who among the workspace's eligible users, by user
// ID, name or email, either case.
func matchRoleUser(users []linearprovider.WorkspaceUser, who string) (linearprovider.WorkspaceUser, error) {
	who = strings.TrimSpace(who)
	found := map[string]linearprovider.WorkspaceUser{}
	ineligible := 0
	for _, u := range users {
		if strings.EqualFold(u.ID, who) || strings.EqualFold(strings.TrimSpace(u.Name), who) || (u.Email != "" && strings.EqualFold(u.Email, who)) {
			if !u.Eligible() {
				ineligible++
				continue
			}
			found[u.ID] = u
		}
	}
	switch len(found) {
	case 0:
		if ineligible > 0 {
			return linearprovider.WorkspaceUser{}, fmt.Errorf("%q isn't an active, non-guest user of the Linear workspace, so can't have a role", who)
		}
		return linearprovider.WorkspaceUser{}, fmt.Errorf("no user of the Linear workspace is %q; check the spelling, or give their Linear user ID", who)
	case 1:
		for _, u := range found {
			return u, nil
		}
	}
	names := []string{}
	for _, u := range found {
		names = append(names, terminalSafe(u.Name)+" ("+u.ID+")")
	}
	sort.Strings(names)
	return linearprovider.WorkspaceUser{}, fmt.Errorf("%q matches %d Linear users: %s; give the one you mean by user ID", who, len(found), strings.Join(names, "; "))
}

// rolesArg is the mapping as the bus takes it.
func rolesArg(m bus.TeamRoles) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// rolesUsers lists the Linear workspace's users through this person's own
// connection (--profile, or the only one). Nothing is stored.
var rolesUsers = func(ctx context.Context, flags Config, executor linearboard.Executor, scope bus.Request, profile string) ([]linearprovider.WorkspaceUser, error) {
	directory, err := linearDirectory()
	if err != nil {
		return nil, err
	}
	manager := newLinearManager(directory)
	if profile == "" {
		profiles, e := manager.Profiles()
		if e != nil {
			return nil, e
		}
		if len(profiles) != 1 {
			return nil, errors.New("to look people up, name your Linear connection with --profile (clawbridge linear profiles)")
		}
		profile = profiles[0].ID
	}
	service := newLinearService(directory, profile, manager, executor, scope.Args["linear_authority"].(string))
	return service.WorkspaceUsers(ctx, scope)
}
