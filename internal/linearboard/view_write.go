package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Issue view writes (LINEAR-VIEWS-PLAN §13.1 3b, §14): the project owner,
// without a session, creates or updates a shared view on one named bound
// team with their own Linear API key. Everything else is read through the
// person's MCP connection: the binding proof, the IDs a filter may name,
// the team's name and member count for the preview's scope line (S1), and
// the views already on the team. The key's client is the CLI's: this
// package never imports it, so the MCP server can't reach the key.

// ViewKey is the person's key client, as the CLI provides it.
type ViewKey interface {
	CreateView(ctx context.Context, team, name string, filter json.RawMessage) (KeyView, error)
	UpdateView(ctx context.Context, id, name string, filter json.RawMessage) (KeyView, error)
	View(ctx context.Context, id string) (KeyView, error)
	DeleteView(ctx context.Context, id string) error
}

// KeyView is a view as Linear reads it back through the key.
type KeyView struct {
	ID, Name, UpdatedAt, TeamID, Filter string
	CreatorID, CreatorName              string
	Shared                              bool
}

// A ViewKey wraps its errors in one of these, so the ledger knows what
// reached Linear.
var (
	// ErrKeyNotSent is a refusal before anything was sent: no key, an
	// agent's environment, a key Linear rejected.
	ErrKeyNotSent = errors.New("nothing was sent")
	// ErrKeyRefused is Linear refusing the write: nothing changed.
	ErrKeyRefused = errors.New("Linear refused the view")
	// ErrKeyNotFound is a view Linear doesn't have.
	ErrKeyNotFound = errors.New("Linear has no such view")
)

// ViewWriteInput is one create or update. Name and Filter are optional on
// an update, which keeps what isn't given.
type ViewWriteInput struct {
	Action      string // create or update
	Team        string // create only
	ViewID      string // update only
	Name        string
	Filter      []byte
	OperationID string
	Confirm     bool
	// A confirm carries what its preview showed (M3): the binding
	// revision, the filter's SHA-256 and, for an update, the stored row's
	// filter_as_of. Anything changed since is refused, nothing sent.
	ExpectedRevision   int64
	ExpectedFilterSHA  string
	ExpectedFilterAsOf string
	// adoption checks an adoption's write: the name is the adopted view's
	// own, so the name check is skipped. Never sent.
	adoption bool
}

// ViewWriteResult is a preview, or the outcome of a confirmed write.
type ViewWriteResult struct {
	// Kind is view_create or view_update on a confirmed write's receipt.
	Kind    string `json:"kind,omitempty"`
	Preview bool   `json:"preview"`
	Action  string `json:"action"`
	// Scope is the line stating where the view is visible (§14.1).
	Scope        string   `json:"scope"`
	TeamID       string   `json:"team_id"`
	TeamName     string   `json:"team_name,omitempty"`
	Members      int      `json:"members,omitempty"`
	MembersKnown bool     `json:"members_known"`
	ViewID       string   `json:"view_id,omitempty"`
	Name         string   `json:"name"`
	AgentFilter  string   `json:"agent_filter"`
	Uses         []string `json:"uses,omitempty"`
	FilterSHA    string   `json:"filter_sha256"`
	// Changes names what an update changes: name, filter.
	Changes []string `json:"changes,omitempty"`
	// BindingRevision and ExpectedFilterAsOf are what the confirm must
	// still find.
	BindingRevision    int64  `json:"binding_revision"`
	ExpectedFilterAsOf string `json:"expected_filter_as_of,omitempty"`
	// Outcome is accepted, uncertain or refused; Mismatch names the
	// read-back fields that differ when uncertain.
	Outcome  string   `json:"outcome,omitempty"`
	Mismatch []string `json:"mismatch,omitempty"`
	// OldFilterSHA is, for an adoption, the digest of the filter it
	// replaced (A2).
	OldFilterSHA string `json:"old_filter_sha256,omitempty"`
	// FilterAsOf is Linear's updatedAt from an accepted read-back.
	FilterAsOf string `json:"filter_as_of,omitempty"`
	// Recorded says the accepted view is stored for agents; RecordError
	// says why not, and a retry with the same operation ID records it.
	Recorded    bool   `json:"recorded"`
	RecordError string `json:"record_error,omitempty"`
}

const (
	viewScopePages = 10
	viewPageSize   = 100
)

// ScopeLine is the preview's statement of where a team view is seen.
func ScopeLine(teamID, teamName string, members int, known bool) string {
	name := teamID
	if teamName != "" {
		name = teamName + " (" + teamID + ")"
	}
	count := "members: unknown"
	if known {
		count = strconv.Itoa(members) + " members"
	}
	return "scope: team " + name + ", a shared team view: visible to everyone in that team (" + count + ")"
}

// ViewWrite previews or makes one issue view write.
func (s *Service) ViewWrite(ctx context.Context, scope bus.Request, in ViewWriteInput, key ViewKey) (ViewWriteResult, error) {
	var none ViewWriteResult
	in.Name = strings.TrimSpace(in.Name)
	if e := validateViewWrite(in); e != nil {
		return none, e
	}
	if scope.Session != "" {
		return none, errors.New("a session can't write a Linear view; the project owner runs it in their own terminal")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return none, e
	}
	if !b.Configured {
		return none, errors.New("task-board is not linked")
	}
	if s.cfg.Authority == "" {
		return none, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return none, e
	}
	if access.ProjectRole != "owner" || access.SessionID != "" {
		return none, errors.New("only the project owner, without a session, writes a Linear view")
	}
	out := ViewWriteResult{Action: in.Action, BindingRevision: b.Revision}
	name, filterText := in.Name, in.Filter
	storedSHA := ""
	var was *storedViewRow
	if in.Action == "create" {
		out.TeamID = in.Team
	} else {
		stored, e := s.storedView(ctx, scope, in.ViewID)
		if e != nil {
			return none, e
		}
		out.TeamID, out.ViewID, out.ExpectedFilterAsOf = stored.team, in.ViewID, stored.asOf
		if !stored.bound {
			return none, errors.New("this view's team left the binding; nothing was sent")
		}
		if name == "" {
			name = stored.name
		} else if name != stored.name {
			out.Changes = append(out.Changes, "name")
		}
		if filterText == nil {
			filterText = []byte(stored.filter)
		}
		storedSHA = stored.sha
		was = &stored
	}
	if !contains(b.LinearTeamIDs, out.TeamID) {
		return none, errors.New("the team is not in this project's binding; nothing was sent")
	}
	f, e := ParseViewFilter(filterText)
	if e != nil {
		return none, e
	}
	if in.Action == "update" && f.SHA256 != storedSHA {
		out.Changes = append(out.Changes, "filter")
	}
	out.Name, out.AgentFilter, out.Uses, out.FilterSHA = name, f.AgentFilter(), f.Uses, f.SHA256

	_, p, done, e := s.open(ctx)
	if e != nil {
		return none, e
	}
	defer done()
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return none, e
	}
	v2, ok := p.(ProviderV2)
	if !ok {
		return none, &linearprovider.Error{Kind: "capability_unavailable", NotSent: true}
	}
	vs, e := viewScope(ctx, p, v2, b, out.TeamID)
	if e != nil {
		return none, e
	}
	if e = f.CheckIDs(vs.ViewScope); e != nil {
		return none, e
	}
	out.TeamName, out.Members, out.MembersKnown = vs.teamName, vs.members, vs.membersKnown
	out.Scope = ScopeLine(out.TeamID, out.TeamName, out.Members, out.MembersKnown)
	// An adoption keeps the adopted view's own name; the apply checked it (A1).
	if !in.adoption {
		if e = s.viewNameFree(ctx, scope, p, out.TeamID, name, in.ViewID); e != nil {
			return none, e
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return none, e
	}
	if !in.Confirm {
		out.Preview = true
		return out, nil
	}
	if e = previewHolds(in, out); e != nil {
		return none, e
	}
	return s.sendView(ctx, scope, b, access.SessionID, in, out, f, key, was)
}

// previewHolds is M3: a confirm writes only what its preview showed.
func previewHolds(in ViewWriteInput, out ViewWriteResult) error {
	if in.ExpectedRevision < 0 || in.ExpectedFilterSHA == "" || in.Action == "update" && in.ExpectedFilterAsOf == "" {
		return errors.New("a confirm carries the preview's binding revision and filter SHA-256 (and, for an update, filter_as_of); repeat the command the preview printed")
	}
	if in.ExpectedRevision != out.BindingRevision || in.ExpectedFilterSHA != out.FilterSHA || in.ExpectedFilterAsOf != out.ExpectedFilterAsOf {
		return errors.New("the binding, the filter or the stored view changed since the preview; preview again; nothing was sent")
	}
	return nil
}

func validateViewWrite(in ViewWriteInput) error {
	switch in.Action {
	case "create":
		if !linearprovider.ValidID(in.Team) || in.ViewID != "" {
			return errors.New("a create names its team with --team (a Linear team ID), and no view")
		}
		if in.Name == "" || in.Filter == nil {
			return errors.New("a create needs a name and a filter")
		}
	case "update":
		if !linearprovider.ValidID(in.ViewID) || in.Team != "" {
			return errors.New("an update names its view by ID, and keeps its team: --team isn't accepted")
		}
		if in.Name == "" && in.Filter == nil {
			return errors.New("an update changes the name, the filter or both")
		}
	default:
		return errors.New("view write must be create or update")
	}
	if in.Name != "" && !bus.LinearNameOK(in.Name) {
		return errors.New("view name must be 1 to 80 characters with no control or direction characters")
	}
	if !operationPattern.MatchString(in.OperationID) {
		return errors.New("operation_id requires 1–128 ASCII identifier characters")
	}
	return nil
}

type storedViewRow struct {
	team, name, filter, sha, asOf string
	bound                         bool
}

// storedView reads a view ClawBridge recorded.
func (s *Service) storedView(ctx context.Context, scope bus.Request, id string) (storedViewRow, error) {
	// A session's read keeps its instance: the bus checks it.
	scope.Operation, scope.Args = "linear_view_get", instanceArgs(scope.Args)
	scope.Args["view_id"] = id
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return storedViewRow{}, e
	}
	m, _ := r.Data.(map[string]any)
	row := storedViewRow{}
	row.team, _ = m["team_id"].(string)
	row.name, _ = m["name"].(string)
	row.filter, _ = m["filter"].(string)
	row.sha, _ = m["filter_sha256"].(string)
	row.asOf, _ = m["filter_as_of"].(string)
	row.bound, _ = m["team_bound"].(bool)
	if row.team == "" || row.filter == "" || row.asOf == "" {
		return storedViewRow{}, errors.New("the recorded view is unreadable")
	}
	return row, nil
}

type viewScopeRead struct {
	ViewScope
	teamName     string
	members      int
	membersKnown bool
}

// viewScope reads what a filter may name: the view's own team, the bound
// projects, and every bound team's labels, states and members. A list
// longer than ClawBridge reads refuses the write, since an ID in it can't
// be proved. The view team's member count may stay unknown only for the
// scope line, never for the check.
func viewScope(ctx context.Context, p Provider, v2 ProviderV2, b bus.TaskboardBindingV2, team string) (viewScopeRead, error) {
	out := viewScopeRead{ViewScope: ViewScope{Team: team, Projects: map[string]bool{}, Labels: map[string]bool{}, States: map[string]bool{}, Members: map[string]bool{}}}
	for _, id := range b.LinearProjectIDs {
		out.Projects[id] = true
	}
	tooLong := errors.New("a bound team's labels or members are more than ClawBridge reads to check a filter; nothing was sent")
	for _, t := range b.LinearTeamIDs {
		cursor := ""
		complete := false
		for page := 0; page < viewScopePages && !complete; page++ {
			labels, e := v2.Labels(ctx, t, linearprovider.PageOptions{Limit: viewPageSize, Cursor: cursor})
			if e != nil {
				return out, e
			}
			for _, l := range labels.Items {
				out.Labels[l.ID] = true
			}
			cursor, complete = labels.NextCursor, labels.Complete
		}
		if !complete {
			return out, tooLong
		}
		states, e := p.Statuses(ctx, t)
		if e != nil {
			return out, e
		}
		for _, st := range states {
			out.States[st.ID] = true
		}
		cursor, complete = "", false
		count := 0
		for page := 0; page < viewScopePages && !complete; page++ {
			members, e := v2.Members(ctx, t, linearprovider.PageOptions{Limit: viewPageSize, Cursor: cursor})
			if e != nil {
				return out, e
			}
			for _, m := range members.Items {
				out.Members[m.ID] = true
			}
			count += len(members.Items)
			cursor, complete = members.NextCursor, members.Complete
		}
		if !complete {
			return out, tooLong
		}
		if t == team {
			out.members, out.membersKnown = count, true
		}
	}
	if got, e := p.GetTeam(ctx, team); e == nil && got.ID == team {
		out.teamName = got.Name
	}
	return out, nil
}

// viewNameFree refuses a name another issue view on the team already has.
func (s *Service) viewNameFree(ctx context.Context, scope bus.Request, p Provider, team, name, self string) error {
	vp, ok := p.(viewsProvider)
	if !ok {
		return &linearprovider.Error{Kind: "capability_unavailable", NotSent: true}
	}
	cursor := ""
	for page := 0; page < viewScopePages; page++ {
		got, e := vp.Views(ctx, team, "issue", linearprovider.PageOptions{Limit: viewPageSize, Cursor: cursor})
		if e != nil {
			return e
		}
		for _, v := range got.Items {
			if v.ID != self && strings.EqualFold(strings.TrimSpace(v.Name), strings.TrimSpace(name)) {
				return fmt.Errorf("the team already has an issue view %s with that name; choose another name; nothing was sent", v.ID)
			}
		}
		if got.Complete {
			return nil
		}
		cursor = got.NextCursor
	}
	return errors.New("the team has more views than ClawBridge reads to check the name; nothing was sent")
}

// sendView sends the write under the operation ledger, reads it back, and
// records an accepted view in the bus.
func (s *Service) sendView(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2, actor string, in ViewWriteInput, out ViewWriteResult, f ViewFilter, key ViewKey, was *storedViewRow) (ViewWriteResult, error) {
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, actor})
	payload := struct {
		Version         int
		Kind, Profile   string
		BindingRevision int64
		Team, View      string
		Name, FilterSHA string
		ExpectedAsOf    string
		OldFilterSHA    string `json:",omitempty"`
	}{1, "view_" + in.Action, s.cfg.ProfileID, b.Revision, out.TeamID, out.ViewID, out.Name, f.SHA256, out.ExpectedFilterAsOf, out.OldFilterSHA}
	l := s.operations(ctx)
	if r, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		if e != nil {
			return ViewWriteResult{}, e
		}
		replay, e := viewReceipt(r)
		if e != nil {
			return ViewWriteResult{}, e
		}
		return s.recordAccepted(ctx, scope, in.Action, replay, f), nil
	}
	receipt, e := l.write(namespace, in.OperationID, payload, func() (any, error) {
		if e := s.unchangedV2(ctx, scope, b); e != nil {
			return nil, authorityFailure(e)
		}
		var sent KeyView
		var e error
		if was != nil {
			// M2: an update overwrites only the view ClawBridge wrote; a
			// change someone made in Linear since is never overwritten.
			cur, e := key.View(ctx, out.ViewID)
			switch {
			case errors.Is(e, ErrKeyNotFound), errors.Is(e, ErrKeyRefused):
				return nil, refusedBeforeDispatch(e)
			case e != nil:
				return nil, notDispatched(e)
			}
			if drift := viewMismatch(cur, out.ViewID, was.team, was.name, was.filter); len(drift) > 0 {
				return nil, refusedBeforeDispatch(fmt.Errorf("the view was changed in Linear since ClawBridge wrote it (%s); nothing was sent: adopt it with view apply --adopt, or leave it", strings.Join(drift, ", ")))
			}
		}
		if in.Action == "create" {
			sent, e = key.CreateView(ctx, out.TeamID, out.Name, json.RawMessage(f.Canonical))
		} else {
			sent, e = key.UpdateView(ctx, out.ViewID, out.Name, json.RawMessage(f.Canonical))
		}
		switch {
		case errors.Is(e, ErrKeyNotSent):
			return nil, notDispatched(e)
		case in.Action == "adopt" && errors.Is(e, ErrKeyNotFound):
			// A5: an adoption never creates the view it meant to take over.
			return nil, refusedBeforeDispatch(errors.New("the view to adopt is gone from Linear; an adoption never creates one, so nothing was made"))
		case errors.Is(e, ErrKeyRefused), errors.Is(e, ErrKeyNotFound):
			return nil, refusedBeforeDispatch(e)
		case e != nil:
			return nil, e
		}
		r := out
		r.Kind, r.Preview, r.ViewID = "view_"+in.Action, false, sent.ID
		// S2: the read-back must show the view as sent, on the team asked
		// for; anything else is uncertain, never accepted.
		back, e := key.View(ctx, sent.ID)
		if e != nil {
			r.Outcome, r.Mismatch = "uncertain", []string{"read_back"}
			return r, nil
		}
		r.Mismatch = viewMismatch(back, sent.ID, out.TeamID, out.Name, f.Canonical)
		if len(r.Mismatch) > 0 {
			r.Outcome = "uncertain"
			return r, nil
		}
		r.Outcome, r.FilterAsOf = "accepted", back.UpdatedAt
		return r, nil
	})
	if e != nil {
		return ViewWriteResult{}, e
	}
	r, e := viewReceipt(receipt)
	if e != nil {
		return ViewWriteResult{}, e
	}
	return s.recordAccepted(ctx, scope, in.Action, r, f), nil
}

// recordAccepted stores an accepted view for agents; a replay records it
// again, which the bus answers from the stored row.
func (s *Service) recordAccepted(ctx context.Context, scope bus.Request, action string, r ViewWriteResult, f ViewFilter) ViewWriteResult {
	if r.Outcome != "accepted" {
		return r
	}
	if e := s.recordView(ctx, scope, action, r, f); e != nil {
		r.Recorded, r.RecordError = false, e.Error()
		return r
	}
	r.Recorded, r.RecordError = true, ""
	return r
}

// viewMismatch names the read-back fields that differ from what was sent.
func viewMismatch(back KeyView, id, team, name, filter string) []string {
	var bad []string
	if !linearprovider.ValidID(id) || !strings.EqualFold(back.ID, id) {
		bad = append(bad, "id")
	}
	if back.TeamID != team {
		bad = append(bad, "team")
	}
	if back.Name != name {
		bad = append(bad, "name")
	}
	if !back.Shared {
		bad = append(bad, "shared")
	}
	if back.Filter != filter {
		bad = append(bad, "filter")
	}
	return bad
}

// validViewReceipt is the ledger's check of a recorded view receipt.
func validViewReceipt(raw []byte) bool {
	var r ViewWriteResult
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Kind != "view_"+r.Action || r.Preview {
		return false
	}
	return (r.Outcome == "accepted" || r.Outcome == "uncertain") && linearprovider.ValidID(r.ViewID) && linearprovider.ValidID(r.TeamID)
}

func viewReceipt(v any) (ViewWriteResult, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return ViewWriteResult{}, errors.New("invalid view receipt")
	}
	var r ViewWriteResult
	if json.Unmarshal(raw, &r) != nil || (r.Outcome != "accepted" && r.Outcome != "uncertain") {
		return ViewWriteResult{}, errors.New("invalid view receipt")
	}
	return r, nil
}

// recordView stores an accepted view in the bus: an update replaces the
// row the preview read (r.ExpectedFilterAsOf).
func (s *Service) recordView(ctx context.Context, scope bus.Request, action string, r ViewWriteResult, f ViewFilter) error {
	uses := make([]any, 0, len(f.Uses))
	for _, u := range f.Uses {
		uses = append(uses, u)
	}
	args := map[string]any{"action": action, "view_id": r.ViewID, "team_id": r.TeamID, "name": r.Name, "filter": f.Canonical, "filter_sha256": f.SHA256, "filter_as_of": r.FilterAsOf, "agent_filter": f.AgentFilter(), "uses": uses, "expected_binding_revision": r.BindingRevision}
	if action == "update" {
		args["expected_filter_as_of"] = r.ExpectedFilterAsOf
	}
	if action == "adopt" {
		args["old_filter_sha256"] = r.OldFilterSHA
	}
	scope.Operation, scope.Args = "linear_view_record", args
	_, e := s.cfg.Executor.Execute(ctx, scope)
	return e
}
