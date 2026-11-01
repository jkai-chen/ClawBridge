package guibackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/tui"
)

// ReadDeps is what the Projects, Linear, Teams & People, Dispatcher and
// Account read pages need: the TUI's own inputs.
type ReadDeps struct {
	Exec   doctor.Executor
	Linear tui.LinearSource
	Scope  Scope
	// Hosted is a cloud profile; Account and the Linear write queue exist
	// only there.
	Hosted bool
	// DeviceID is this device's, for the device alerts (hosted).
	DeviceID string
}

// maxOverviews caps the project_overview reads one projects.list makes, as
// the TUI does.
const maxOverviews = 20

// linearTimeout is the TUI's bound on one Linear action.
const linearTimeout = 45 * time.Second

// ReadMethods builds the read methods of the P2-5 pages. None of them
// writes: every executor call passes readOps and every Linear action
// passes readLinearOps, whatever the code below asks (readguard.go).
func ReadMethods(d ReadDeps) map[string]Method {
	r := &reader{scope: d.Scope, hosted: d.Hosted, deviceID: d.DeviceID, exec: ReadOnly(d.Exec)}
	if d.Linear != nil {
		r.linear = ReadOnlyLinear(d.Linear)
	}
	read := func(f func(context.Context, json.RawMessage) (any, error)) Method {
		return Method{Class: Read, Call: f}
	}
	return map[string]Method{
		"projects.list":        read(r.projectsList),
		"project.threads":      read(r.projectThreads),
		"project.thread":       read(r.projectThread),
		"linear.connection":    read(r.linearConnection),
		"linear.binding":       read(r.linearBinding),
		"linear.tasks":         read(r.linearTasks),
		"linear.issue":         read(r.linearIssue),
		"linear.intake":        read(r.linearIntake),
		"linear.cancelKinds":   read(r.linearCancelKinds),
		"linear.projectStatus": read(r.linearProjectStatus),
		"linear.views":         read(r.linearViews),
		"linear.queue":         read(r.linearQueue),
		"teams.roles":          read(r.teamsRoles),
		"teams.members":        read(r.teamsMembers),
		"dispatcher.status":    read(r.dispatcherStatus),
		"account.devices":      read(r.accountDevices),
		"account.invites":      read(r.accountInvites),
		"account.users":        read(r.accountUsers),
	}
}

// reader holds only the guarded paths: the raw executor and Linear source
// of ReadDeps are never kept, so nothing below can reach them.
type reader struct {
	scope    Scope
	hosted   bool
	deviceID string
	exec     doctor.Executor
	linear   tui.LinearSource
}

func badParams(msg string) error { return &Error{Code: CodeParams, Message: msg} }

func decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return badParams("the parameters are not what this method takes")
	}
	return nil
}

// generic is v as plain JSON values, whatever its Go type.
func generic(v any) any {
	switch v.(type) {
	case nil, map[string]any, []any, string, float64, bool:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func objOf(v any) map[string]any {
	m, _ := generic(v).(map[string]any)
	return m
}

func listOf(v any) []any {
	l, _ := generic(v).([]any)
	if l == nil {
		return []any{}
	}
	return l
}

func text(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// person is a request as the person: no session, no instance.
func (r *reader) person(op, project string, args map[string]any) bus.Request {
	if args == nil {
		args = map[string]any{}
	}
	return bus.Request{Operation: op, Workspace: r.scope.Workspace, Project: project, Args: args}
}

// serverRead is one read that must come from the server, not a copy.
func (r *reader) serverRead(ctx context.Context, req bus.Request) (any, error) {
	res, err := r.exec.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.Source == "offline-mirror" {
		return nil, errors.New("the authority is offline; this page needs the server")
	}
	return res.Data, nil
}

// ---- Projects and Threads

// projectsList is the TUI's Projects load: list_projects, then up to 20
// project_overview reads.
func (r *reader) projectsList(ctx context.Context, _ json.RawMessage) (any, error) {
	res, err := r.exec.Execute(ctx, r.person("list_projects", "", nil))
	if err != nil {
		return nil, err
	}
	overviews := map[string]any{}
	for i, p := range listOf(objOf(res.Data)["projects"]) {
		if i == maxOverviews {
			break
		}
		row := objOf(p)
		id := text(row, "id")
		o, e := r.exec.Execute(ctx, r.person("project_overview", firstNonEmpty(text(row, "slug"), id), nil))
		if e != nil {
			overviews[id] = map[string]any{"error": e.Error()}
			continue
		}
		overviews[id] = generic(o.Data)
	}
	return map[string]any{"source": res.Source, "data": generic(res.Data), "overviews": overviews}, nil
}

// projectThreads is project_threads as the person. The server refuses a
// session, an agent and a non-owner, as it does the TUI.
func (r *reader) projectThreads(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Project string `json:"project"`
		Before  string `json:"before"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Project == "" {
		return nil, badParams("project is required")
	}
	args := map[string]any{}
	if p.Before != "" {
		args["before"] = p.Before
	}
	return r.exec.Execute(ctx, r.person("project_threads", p.Project, args))
}

func (r *reader) projectThread(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Project  string `json:"project"`
		ThreadID string `json:"thread_id"`
		After    string `json:"after"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Project == "" || p.ThreadID == "" {
		return nil, badParams("project and thread_id are required")
	}
	args := map[string]any{"thread_id": p.ThreadID}
	if p.After != "" {
		args["after"] = p.After
	}
	return r.exec.Execute(ctx, r.person("project_thread", p.Project, args))
}

// ---- Linear

type profileParams struct {
	Profile       string `json:"profile"`
	LinearProject string `json:"linear_project"`
	IssueID       string `json:"issue_id"`
	Cursor        string `json:"cursor"`
	Limit         int    `json:"limit"`
	Suggest       bool   `json:"suggest"`
}

func (r *reader) do(ctx context.Context, req tui.LinearRequest) (tui.LinearData, error) {
	if r.linear == nil {
		return tui.LinearData{}, errors.New("Native Linear integration unavailable; use clawbridge linear --help")
	}
	ctx, cancel := context.WithTimeout(ctx, linearTimeout)
	defer cancel()
	return r.linear.Do(ctx, req)
}

func (r *reader) linearParams(raw json.RawMessage) (profileParams, error) {
	var p profileParams
	err := decode(raw, &p)
	return p, err
}

func profilesOf(d tui.LinearData) []any {
	out := listOf(d.Profiles)
	return out
}

// linearConnection is the "profiles" action: the connections, with their
// access and the account they are connected as.
func (r *reader) linearConnection(ctx context.Context, _ json.RawMessage) (any, error) {
	d, err := r.do(ctx, tui.LinearRequest{Operation: "profiles"})
	if err != nil && len(d.Profiles) == 0 {
		return nil, err
	}
	out := map[string]any{"profiles": profilesOf(d)}
	if err != nil {
		out["binding_error"] = err.Error()
	}
	return out, nil
}

// linearBinding is the "profiles" action's binding, with the names stored
// beside it (project_overview). It never refreshes the names.
func (r *reader) linearBinding(ctx context.Context, _ json.RawMessage) (any, error) {
	d, err := r.do(ctx, tui.LinearRequest{Operation: "profiles"})
	if err != nil && d.BindingV2 == nil {
		return nil, err
	}
	out := map[string]any{"binding": bindingJSON(d.BindingV2), "without_project": listOf(d.WithoutProject)}
	if err != nil {
		out["binding_error"] = err.Error()
	}
	if r.scope.Project != "" {
		if o, e := r.exec.Execute(ctx, r.person("project_overview", r.scope.Project, nil)); e == nil {
			data := objOf(o.Data)
			out["stored"] = map[string]any{"linear": data["linear"], "role": text(objOf(data["project"]), "role")}
		}
	}
	return out, nil
}

// bindingJSON is the binding v2 with snake_case names, and nothing the
// page does not show.
func bindingJSON(b *bus.TaskboardBindingV2) any {
	if b == nil {
		return nil
	}
	return map[string]any{
		"configured":            b.Configured,
		"revision":              b.Revision,
		"mapping_version":       b.MappingVersion,
		"updated_at":            b.UpdatedAt,
		"provider":              b.Provider,
		"mode":                  b.Mode,
		"team_ids":              nilSlice(b.LinearTeamIDs),
		"default_team_id":       b.LinearDefaultTeamID,
		"project_ids":           nilSlice(b.LinearProjectIDs),
		"other_teams":           b.LinearOtherTeams,
		"parent_team_id":        b.LinearParentTeamID,
		"lanes":                 listOf(b.Lanes),
		"unpinned":              b.Unpinned,
		"teams_without_project": nilSlice(b.TeamsWithoutProject),
	}
}

func nilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (r *reader) linearTasks(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "issues", ProfileID: p.Profile, Limit: limit, Cursor: p.Cursor})
	if err != nil {
		return nil, err
	}
	return map[string]any{"page": generic(d.IssuesV2)}, nil
}

func (r *reader) linearIssue(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	if p.IssueID == "" {
		return nil, badParams("issue_id is required")
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "issue", ProfileID: p.Profile, IssueID: p.IssueID})
	if err != nil {
		return nil, err
	}
	return map[string]any{"issue": generic(d.IssueV2)}, nil
}

// intakeRead is the Intake view's read: the policy, the trusted list and
// the queue through the authority, and names and the label check through
// the viewer's Linear connection, best effort.
func (r *reader) intakeRead(ctx context.Context, profile string) (map[string]any, error) {
	if r.scope.Project == "" {
		return nil, errors.New("intake is set per project; open the UI with a project")
	}
	read := func(op string) (any, error) { return r.serverRead(ctx, r.person(op, r.scope.Project, nil)) }
	policy, err := read("intake_policy_get")
	if err != nil {
		return nil, err
	}
	p := objOf(policy)
	trustedRaw, err := read("intake_trust_list")
	if err != nil {
		return nil, err
	}
	trusted := []any{}
	for _, t := range listOf(trustedRaw) {
		trusted = append(trusted, text(objOf(t), "linear_user_id"))
	}
	queueRaw, err := read("intake_list")
	if err != nil {
		return nil, err
	}
	queue := []any{}
	for _, q := range listOf(queueRaw) {
		switch text(objOf(q), "state") {
		case "queued", "offered", "dismissed":
			queue = append(queue, q)
		}
	}
	out := map[string]any{
		"mode": text(p, "mode"), "revision": p["revision"], "signals": listOf(p["signals"]),
		"trusted": trusted, "queue": queue, "members": []any{}, "labels": []any{}, "names_note": "",
	}
	if r.linear == nil {
		out["names_note"] = "Linear isn't set up here, so trusted users show by ID."
		return out, nil
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "intake", ProfileID: profile})
	if err != nil {
		out["names_note"] = "Names and the label check need your Linear connection: " + err.Error()
		return out, nil
	}
	members := []any{}
	for _, m := range d.Members {
		if m.Active {
			members = append(members, generic(m))
		}
	}
	labels := []any{}
	for _, l := range d.IntakeLabels {
		labels = append(labels, map[string]any{"team_id": l.TeamID, "found": l.Found, "complete": l.Complete})
	}
	out["members"], out["labels"] = members, labels
	return out, nil
}

func (r *reader) linearIntake(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	return r.intakeRead(ctx, p.Profile)
}

// teamsMembers is the bound teams' Linear members, with who intake trusts.
func (r *reader) teamsMembers(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	in, err := r.intakeRead(ctx, p.Profile)
	if err != nil {
		return nil, err
	}
	return map[string]any{"members": in["members"], "trusted": in["trusted"], "names_note": in["names_note"]}, nil
}

func (r *reader) linearCancelKinds(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "cancel-kinds", ProfileID: p.Profile})
	if err != nil {
		return nil, err
	}
	if d.CancelKindsView == nil {
		return nil, errors.New("the server's answer had no who-may-cancel setting")
	}
	return map[string]any{"view": generic(d.CancelKindsView)}, nil
}

func (r *reader) linearProjectStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	if p.LinearProject == "" {
		return nil, badParams("linear_project is required")
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "project-status", ProfileID: p.Profile, LinearProject: p.LinearProject, Suggest: p.Suggest})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": generic(d.ProjectStatus)}, nil
}

func (r *reader) linearViews(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "views", ProfileID: p.Profile})
	if err != nil {
		return nil, err
	}
	teams := []any{}
	for _, t := range d.Views {
		teams = append(teams, map[string]any{
			"team_id": t.TeamID, "views": listOf(t.Views), "other_kinds": t.OtherKinds,
			"workspace_views": t.WorkspaceViews, "more": t.More, "error": t.Err,
		})
	}
	return map[string]any{"teams": teams}, nil
}

// linearQueue is the hosted project's Linear budget and queued writes
// (linear_app_status, linear_write_status).
func (r *reader) linearQueue(ctx context.Context, _ json.RawMessage) (any, error) {
	if !r.hosted {
		return map[string]any{"available": false, "reason": "The write queue is for hosted workspaces."}, nil
	}
	if r.scope.Project == "" {
		return nil, errors.New("the write queue is per project; open the UI with a project")
	}
	w, err := r.exec.Execute(ctx, r.person("linear_write_status", r.scope.Project, map[string]any{"scope": "project"}))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"available": true, "writes": listOf(objOf(w.Data)["writes"])}
	if s, e := r.exec.Execute(ctx, r.person("linear_app_status", r.scope.Project, nil)); e == nil {
		out["status"] = generic(s.Data)
	}
	return out, nil
}

// ---- Teams & People

// teamsRoles is the "roles" action: each bound team's lead and the people
// of the mapping, with who the viewer is and the workspace users' names.
func (r *reader) teamsRoles(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := r.linearParams(raw)
	if err != nil {
		return nil, err
	}
	d, err := r.do(ctx, tui.LinearRequest{Operation: "roles", ProfileID: p.Profile})
	if err != nil {
		return nil, err
	}
	if d.Roles == nil {
		return nil, errors.New("the server's answer had no team roles")
	}
	v := d.Roles
	teams := []any{}
	for _, t := range v.Teams {
		teams = append(teams, map[string]any{"team_id": t.TeamID, "leads": nilSlice(t.Leads), "bound": t.Bound})
	}
	users := []any{}
	for _, u := range v.Users {
		users = append(users, map[string]any{"id": u.ID, "name": u.Name, "active": u.Active, "guest": u.Guest})
	}
	return map[string]any{
		"revision": v.Revision, "teams": teams, "people": listOf(v.People),
		"you_lead": nilSlice(v.YouLead), "owner": v.Owner, "users": users, "names_err": v.NamesErr,
	}, nil
}

// ---- Dispatcher

// dispatcherStatus is the Model screen's load: dispatcher_model_doctor
// (a project owner's; others get the TUI's "denied" sentence) and
// dispatcher_get's policy and model block, with the hosted Linear budget.
func (r *reader) dispatcherStatus(ctx context.Context, _ json.RawMessage) (any, error) {
	if r.scope.Project == "" {
		return nil, errors.New("the dispatcher is set per project; open the UI with a project")
	}
	out := map[string]any{"doctor": nil, "denied": "", "view": nil, "budget": nil}
	doc, err := r.serverRead(ctx, r.person("dispatcher_model_doctor", r.scope.Project, nil))
	switch {
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "owner permission"):
		out["denied"] = "Only a project owner sees and sets up the dispatcher model."
	case err != nil:
		return nil, err
	default:
		out["doctor"] = generic(doc)
	}
	if v, e := r.serverRead(ctx, r.person("dispatcher_get", r.scope.Project, nil)); e == nil {
		out["view"] = generic(v)
	}
	if r.hosted {
		if s, e := r.exec.Execute(ctx, r.person("linear_app_status", r.scope.Project, nil)); e == nil {
			out["budget"] = objOf(s.Data)["budget"]
		}
	}
	return out, nil
}

// ---- Account (hosted only)

func notHosted() map[string]any {
	return map[string]any{"hosted": false, "reason": "Account is for hosted workspaces."}
}

func (r *reader) accountRead(ctx context.Context, op string) (map[string]any, error) {
	if r.scope.Workspace == "" {
		return nil, errors.New("no workspace authority is configured; complete join setup first")
	}
	data, err := r.serverRead(ctx, r.person(op, "", nil))
	return objOf(data), err
}

// accountDevices is the TUI's Account load: whoami, device_list, a pending
// ownership offer and, with a device key, the device alerts.
func (r *reader) accountDevices(ctx context.Context, _ json.RawMessage) (any, error) {
	if !r.hosted {
		return notHosted(), nil
	}
	who, err := r.accountRead(ctx, "whoami")
	if err != nil {
		return nil, err
	}
	principal := text(who, "principal")
	devices, err := r.accountRead(ctx, "device_list")
	if err != nil {
		return nil, err
	}
	transfer, err := r.accountRead(ctx, "owner_transfer_get")
	if err != nil {
		return nil, err
	}
	events := []any{}
	if r.deviceID != "" {
		ev, err := r.accountRead(ctx, "device_events")
		if err != nil {
			return nil, err
		}
		events = listOf(ev["events"])
	}
	return map[string]any{
		"hosted": true, "principal": principal,
		"main_owner": principal != "" && text(objOf(who["workspace"]), "owner_principal") == principal,
		"devices":    listOf(devices["devices"]), "transfer": transfer["pending"], "events": events,
	}, nil
}

// accountInvites is invite_list, with the project slugs that name its
// grants (best effort, as the TUI does).
func (r *reader) accountInvites(ctx context.Context, _ json.RawMessage) (any, error) {
	if !r.hosted {
		return notHosted(), nil
	}
	invites, err := r.accountRead(ctx, "invite_list")
	if err != nil {
		return nil, err
	}
	projects := []any{}
	if p, e := r.accountRead(ctx, "list_projects"); e == nil {
		for _, row := range listOf(p["projects"]) {
			projects = append(projects, map[string]any{"id": text(objOf(row), "id"), "slug": text(objOf(row), "slug")})
		}
	}
	return map[string]any{"hosted": true, "invites": listOf(invites["invites"]), "projects": projects}, nil
}

// accountUsers is user_list, which only the workspace's main owner may read.
func (r *reader) accountUsers(ctx context.Context, _ json.RawMessage) (any, error) {
	if !r.hosted {
		return notHosted(), nil
	}
	who, err := r.accountRead(ctx, "whoami")
	if err != nil {
		return nil, err
	}
	principal := text(who, "principal")
	if principal == "" || text(objOf(who["workspace"]), "owner_principal") != principal {
		return map[string]any{"hosted": true, "main_owner": false, "users": []any{}}, nil
	}
	users, err := r.accountRead(ctx, "user_list")
	if err != nil {
		return nil, err
	}
	return map[string]any{"hosted": true, "main_owner": true, "principal": principal, "users": listOf(users["users"])}, nil
}
