package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Applying a view-set file (LINEAR-VIEWS-PLAN D5, §13.1 3d, A1–A5): the
// project owner's per-project issue views in one file, previewed together
// and written one entry at a time, each as its own idempotent operation.
// A name already taken by a view made in Linear is refused unless the owner
// adopts it, by name: then exactly that one view's filter is replaced, and
// it becomes ClawBridge's.

const maxViewSetEntries = 50

// ViewApplyInput is one apply: the file's bytes, the operation ID each
// entry's own ID derives from, the names to adopt, and --confirm.
type ViewApplyInput struct {
	File        []byte
	OperationID string
	Adopt       []string
	Confirm     bool
	// ExpectedPlanSHA is the preview's plan_sha256: a confirm applies only
	// the plan its preview showed (as #201's M3 for a single write).
	ExpectedPlanSHA string
}

// ViewApplyEntry is one entry's plan, and with --confirm its outcome.
type ViewApplyEntry struct {
	Team string `json:"team_id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Action is create, update, unchanged, adopt or refused.
	Action      string   `json:"action"`
	Reason      string   `json:"reason,omitempty"`
	ViewID      string   `json:"view_id,omitempty"`
	OperationID string   `json:"operation_id"`
	Scope       string   `json:"scope,omitempty"`
	AgentFilter string   `json:"agent_filter,omitempty"`
	Uses        []string `json:"uses,omitempty"`
	FilterSHA   string   `json:"filter_sha256,omitempty"`
	// BindingRevision and ExpectedFilterAsOf are what the entry's write
	// preview read; its confirm carries them.
	BindingRevision    int64  `json:"binding_revision,omitempty"`
	ExpectedFilterAsOf string `json:"expected_filter_as_of,omitempty"`
	// Replaces is, for an adoption, the view as it is now (A2, A3).
	Replaces *AdoptedView     `json:"replaces,omitempty"`
	Result   *ViewWriteResult `json:"result,omitempty"`
}

// AdoptedView is what an adoption replaces: the current filter (shown to
// the owner, never stored or audited), its digest, who made the view and
// when it last changed, and the filter keys agents couldn't use (A3).
type AdoptedView struct {
	Filter      string   `json:"filter"`
	FilterSHA   string   `json:"filter_sha256"`
	CreatorID   string   `json:"creator_id,omitempty"`
	CreatorName string   `json:"creator_name,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	GoesAway    []string `json:"keys_going_away,omitempty"`
}

// ViewApplyResult is the whole set's preview or outcome.
type ViewApplyResult struct {
	Preview bool             `json:"preview"`
	Entries []ViewApplyEntry `json:"entries"`
	Refused int              `json:"refused"`
	// PlanSHA digests the plan: every entry as shown, and what it read.
	PlanSHA string `json:"plan_sha256"`
	// Stopped names the entry a confirmed apply stopped at, if one failed.
	Stopped string `json:"stopped,omitempty"`
}

type viewSetEntry struct {
	Team string `json:"team"`
	// Kind is "issue" (the default) or "project".
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Filter json.RawMessage `json:"filter"`
	Adopt  bool            `json:"adopt"`
}

// parseViewSet reads the file strictly: {"views": [...]}, 1 to 50 entries,
// each a bound-team-shaped ID, a name and a filter object, no other keys,
// and no two entries with the same name on a team.
func parseViewSet(raw []byte) ([]viewSetEntry, error) {
	var file struct {
		Views []viewSetEntry `json:"views"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&file); err != nil || d.More() {
		return nil, errors.New(`the view-set file must be {"views": [{"team", "name", "filter", "adopt"}]} with no other keys`)
	}
	if len(file.Views) == 0 || len(file.Views) > maxViewSetEntries {
		return nil, fmt.Errorf("a view-set file holds 1 to %d views", maxViewSetEntries)
	}
	seen := map[string]bool{}
	for i, e := range file.Views {
		if !linearprovider.ValidID(e.Team) {
			return nil, fmt.Errorf("views[%d]: team must be a Linear team ID", i)
		}
		if !bus.LinearNameOK(e.Name) {
			return nil, fmt.Errorf("views[%d]: name must be 1 to 80 characters with no control or direction characters", i)
		}
		if len(bytes.TrimSpace(e.Filter)) == 0 {
			return nil, fmt.Errorf("views[%d]: filter is required", i)
		}
		if e.Kind == "" {
			file.Views[i].Kind, e.Kind = "issue", "issue"
		}
		if e.Kind != "issue" && e.Kind != "project" {
			return nil, fmt.Errorf("views[%d]: kind is issue or project", i)
		}
		key := e.Team + "|" + e.Kind + "|" + strings.ToLower(strings.TrimSpace(e.Name))
		if seen[key] {
			return nil, fmt.Errorf("views[%d]: another entry has this name on this team", i)
		}
		seen[key] = true
	}
	return file.Views, nil
}

// entryOperationID derives an entry's own operation ID from the apply's
// and the entry's team and name, so a re-run sends nothing twice.
func entryOperationID(base string, e viewSetEntry) string {
	return base + "-" + digest([]string{e.Team, e.Kind, strings.ToLower(strings.TrimSpace(e.Name))})[:12]
}

// ViewApply previews or applies a view-set file. Any refused entry refuses
// the whole confirm before anything is sent.
func (s *Service) ViewApply(ctx context.Context, scope bus.Request, in ViewApplyInput, key ViewKey) (ViewApplyResult, error) {
	// A session is refused these: only a person, in their own terminal.
	if !(in.Confirm && scope.Session == "") {
		return s.viewApply(ctx, scope, in, key)
	}
	// A confirm writes through the person's own profile: the project's
	// hourly budget counts it (FR8).
	var out ViewApplyResult
	err := s.budgeted(ctx, scope, "view", in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.viewApply(ctx, scope, in, key)
		return e
	})
	return out, err
}

func (s *Service) viewApply(ctx context.Context, scope bus.Request, in ViewApplyInput, key ViewKey) (ViewApplyResult, error) {
	var none ViewApplyResult
	entries, e := parseViewSet(in.File)
	if e != nil {
		return none, e
	}
	if !operationPattern.MatchString(in.OperationID) || len(in.OperationID) > 100 {
		return none, errors.New("operation_id requires 1–100 ASCII identifier characters (each entry adds a suffix)")
	}
	adopt := map[string]bool{}
	for _, name := range in.Adopt {
		adopt[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for name := range adopt {
		found := false
		for _, entry := range entries {
			found = found || strings.ToLower(strings.TrimSpace(entry.Name)) == name
		}
		if !found {
			return none, fmt.Errorf("--adopt %q names no entry of the file", name)
		}
	}
	if scope.Session != "" {
		return none, errors.New("a session can't apply Linear views; the project owner runs it in their own terminal")
	}
	stored, e := s.storedViews(ctx, scope)
	if e != nil {
		return none, e
	}
	out := ViewApplyResult{Preview: !in.Confirm}
	plans := make([]ViewApplyEntry, len(entries))
	for i, entry := range entries {
		plans[i] = s.planEntry(ctx, scope, entry, in.OperationID, stored, entry.Adopt || adopt[strings.ToLower(strings.TrimSpace(entry.Name))], key)
		if plans[i].Action == "refused" {
			out.Refused++
		}
	}
	out.Entries = plans
	raw, e := json.Marshal(plans)
	if e != nil {
		return none, e
	}
	out.PlanSHA = digestText(string(raw))
	if !in.Confirm || out.Refused > 0 {
		out.Preview = true
		if in.Confirm {
			return out, fmt.Errorf("%d of %d entries are refused; nothing was sent (see the preview)", out.Refused, len(entries))
		}
		return out, nil
	}
	switch in.ExpectedPlanSHA {
	case "":
		return out, errors.New("a confirm carries the preview's plan_sha256; repeat the command the preview printed")
	case out.PlanSHA:
	default:
		out.Preview = true
		return out, errors.New("the plan changed since the preview (see this preview); preview again; nothing was sent")
	}
	for i, entry := range entries {
		p := &out.Entries[i]
		if p.Action == "unchanged" {
			continue
		}
		r, e := s.applyEntry(ctx, scope, entry, *p, key)
		if e != nil {
			p.Reason = e.Error()
			out.Stopped = p.Name
			return out, nil
		}
		p.Result = &r
	}
	return out, nil
}

// storedRow is a view ClawBridge recorded, by team and lower-cased name.
type storedRow struct{ id, sha string }

func (s *Service) storedViews(ctx context.Context, scope bus.Request) (map[string]storedRow, error) {
	scope.Operation, scope.Args = "linear_views_list", map[string]any{}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	raw, _ := json.Marshal(r.Data)
	var list struct {
		Views []struct {
			ViewID    string `json:"view_id"`
			TeamID    string `json:"team_id"`
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			FilterSHA string `json:"filter_sha256"`
		} `json:"views"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil, errors.New("the recorded views are unreadable")
	}
	out := map[string]storedRow{}
	for _, v := range list.Views {
		out[v.TeamID+"|"+v.Kind+"|"+strings.ToLower(strings.TrimSpace(v.Name))] = storedRow{v.ViewID, v.FilterSHA}
	}
	return out, nil
}

// planEntry decides one entry: an update of the view ClawBridge recorded
// under this name, unchanged when its filter is the same, a create when
// the name is free, an adoption when the owner asked for one and exactly
// one view made in Linear has the name (A1), and otherwise a refusal.
func (s *Service) planEntry(ctx context.Context, scope bus.Request, e viewSetEntry, base string, stored map[string]storedRow, adopt bool, key ViewKey) ViewApplyEntry {
	p := ViewApplyEntry{Team: e.Team, Name: strings.TrimSpace(e.Name), OperationID: entryOperationID(base, e)}
	refuse := func(reason string) ViewApplyEntry { p.Action, p.Reason = "refused", reason; return p }
	f, err := parseKindFilter(e.Kind, e.Filter)
	if err != nil {
		return refuse(err.Error())
	}
	p.Kind, p.AgentFilter, p.Uses, p.FilterSHA = e.Kind, f.AgentFilter(), f.Uses, f.SHA256
	if row, ok := stored[e.Team+"|"+e.Kind+"|"+strings.ToLower(p.Name)]; ok {
		p.ViewID = row.id
		if row.sha == f.SHA256 {
			p.Action = "unchanged"
			return p
		}
		p.Action = "update"
		pre, err := s.ViewWrite(ctx, scope, ViewWriteInput{Action: "update", ViewID: row.id, Filter: e.Filter, OperationID: p.OperationID}, key)
		if err != nil {
			return refuse(err.Error())
		}
		p.Scope, p.BindingRevision, p.ExpectedFilterAsOf = pre.Scope, pre.BindingRevision, pre.ExpectedFilterAsOf
		return p
	}
	matches, err := s.namedViews(ctx, e.Team, e.Kind, p.Name)
	if err != nil {
		return refuse(err.Error())
	}
	switch {
	case len(matches) == 0:
		p.Action = "create"
		pre, err := s.ViewWrite(ctx, scope, ViewWriteInput{Action: "create", Kind: e.Kind, Team: e.Team, Name: p.Name, Filter: e.Filter, OperationID: p.OperationID}, key)
		if err != nil {
			return refuse(err.Error())
		}
		p.Scope, p.BindingRevision = pre.Scope, pre.BindingRevision
		return p
	case !adopt:
		return refuse("a view made in Linear already has this name on the team; to take it over, adopt it: --adopt " + quoteName(p.Name) + ` or "adopt": true`)
	case len(matches) > 1:
		return refuse(fmt.Sprintf("%d views made in Linear have this name on the team; an adoption needs exactly one", len(matches)))
	}
	p.Action, p.ViewID = "adopt", matches[0]
	pre, old, err := s.adoptPreview(ctx, scope, e, p, key)
	if err != nil {
		return refuse(err.Error())
	}
	p.Scope, p.Replaces, p.BindingRevision = pre.Scope, old, pre.BindingRevision
	return p
}

// namedViews lists the team's shared views of this kind with this name,
// over the person's MCP connection.
func (s *Service) namedViews(ctx context.Context, team, kind, name string) ([]string, error) {
	_, p, done, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	vp, ok := p.(viewsProvider)
	if !ok {
		return nil, &linearprovider.Error{Kind: "capability_unavailable", NotSent: true}
	}
	var ids []string
	cursor := ""
	for page := 0; page < viewScopePages; page++ {
		got, e := vp.Views(ctx, team, kind, linearprovider.PageOptions{Limit: viewPageSize, Cursor: cursor})
		if e != nil {
			return nil, e
		}
		for _, v := range got.Items {
			if v.TeamID == team && strings.EqualFold(strings.TrimSpace(v.Name), name) {
				ids = append(ids, v.ID)
			}
		}
		if got.Complete {
			return ids, nil
		}
		cursor = got.NextCursor
	}
	return nil, errors.New("the team has more views than ClawBridge reads to check the name; nothing was sent")
}

// adoptPreview checks an adoption as a write of the new filter on the
// view's team, and reads the view as it is now with the owner's key (A2):
// still there, on that team and shared, or refused (A1, A5).
func (s *Service) adoptPreview(ctx context.Context, scope bus.Request, e viewSetEntry, p ViewApplyEntry, key ViewKey) (ViewWriteResult, *AdoptedView, error) {
	if key == nil {
		return ViewWriteResult{}, nil, errors.New("an adoption reads the view's current filter with your Linear API key")
	}
	pre, err := s.ViewWrite(ctx, scope, ViewWriteInput{Action: "create", Kind: e.Kind, Team: e.Team, Name: p.Name, Filter: e.Filter, OperationID: p.OperationID, adoption: true}, key)
	if err != nil {
		return ViewWriteResult{}, nil, err
	}
	now, err := key.View(ctx, p.ViewID)
	switch {
	case errors.Is(err, ErrKeyNotFound):
		return ViewWriteResult{}, nil, errors.New("the view to adopt is gone from Linear; an adoption never creates one")
	case err != nil:
		return ViewWriteResult{}, nil, err
	case now.TeamID != e.Team || !now.Shared || now.Kind != e.Kind:
		return ViewWriteResult{}, nil, errors.New("the view to adopt isn't a shared " + e.Kind + " view of this team")
	}
	old := &AdoptedView{Filter: now.Filter, CreatorID: now.CreatorID, CreatorName: now.CreatorName, UpdatedAt: now.UpdatedAt}
	old.FilterSHA = digestText(now.Filter)
	if now.Filter != "" && e.Kind == "issue" {
		if f, err := ParseViewFilter([]byte(now.Filter)); err == nil {
			old.GoesAway = f.Uses
		} else {
			old.GoesAway = []string{"(unreadable filter)"}
		}
	}
	return pre, old, nil
}

// applyEntry writes one planned entry: a create or update as ViewWrite
// does, or an adoption as an update of the view it takes over.
func (s *Service) applyEntry(ctx context.Context, scope bus.Request, e viewSetEntry, p ViewApplyEntry, key ViewKey) (ViewWriteResult, error) {
	switch p.Action {
	case "create":
		return s.ViewWrite(ctx, scope, ViewWriteInput{Action: "create", Kind: e.Kind, Team: e.Team, Name: p.Name, Filter: e.Filter, OperationID: p.OperationID, Confirm: true, ExpectedRevision: p.BindingRevision, ExpectedFilterSHA: p.FilterSHA}, key)
	case "update":
		return s.ViewWrite(ctx, scope, ViewWriteInput{Action: "update", ViewID: p.ViewID, Filter: e.Filter, OperationID: p.OperationID, Confirm: true, ExpectedRevision: p.BindingRevision, ExpectedFilterSHA: p.FilterSHA, ExpectedFilterAsOf: p.ExpectedFilterAsOf}, key)
	case "adopt":
		return s.adoptView(ctx, scope, e, p, key)
	}
	return ViewWriteResult{}, errors.New("nothing to apply")
}

// adoptView sends an adoption: the checks of a write, the view read again
// (it must still be there: A5), then the ledgered update and its
// read-back (A4), recorded as adopted.
func (s *Service) adoptView(ctx context.Context, scope bus.Request, e viewSetEntry, p ViewApplyEntry, key ViewKey) (ViewWriteResult, error) {
	b, e2 := s.BindingV2(ctx, scope)
	if e2 != nil {
		return ViewWriteResult{}, e2
	}
	access, e2 := s.writeAccessV2(ctx, scope, b)
	if e2 != nil {
		return ViewWriteResult{}, e2
	}
	if access.ProjectRole != "owner" || access.SessionID != "" {
		return ViewWriteResult{}, errors.New("only the project owner, without a session, adopts a Linear view")
	}
	pre, old, err := s.adoptPreview(ctx, scope, e, p, key)
	if err != nil {
		return ViewWriteResult{}, err
	}
	f, err := parseKindFilter(e.Kind, e.Filter)
	if err != nil {
		return ViewWriteResult{}, err
	}
	out := pre
	out.Action, out.ViewID, out.Name, out.OldFilterSHA, out.Changes = "adopt", p.ViewID, p.Name, old.FilterSHA, nil
	in := ViewWriteInput{Action: "adopt", Team: e.Team, ViewID: p.ViewID, Name: p.Name, Filter: e.Filter, OperationID: p.OperationID, Confirm: true}
	// The adopted view isn't a row ClawBridge wrote: what it replaces was
	// read again just now, and the plan digest pinned it at the preview.
	return s.sendView(ctx, scope, b, access.SessionID, in, out, f, key, nil)
}

func quoteName(name string) string { return fmt.Sprintf("%q", name) }

func digestText(text string) string {
	if text == "" {
		return strings.Repeat("0", 64)
	}
	f, err := ParseViewFilter([]byte(text))
	if err == nil {
		return f.SHA256
	}
	return digest(text)
}

// parseKindFilter parses a filter by its view's kind.
func parseKindFilter(kind string, text []byte) (ViewFilter, error) {
	if kind == "project" {
		f, _, err := ParseProjectViewFilter(text)
		return f, err
	}
	return ParseViewFilter(text)
}
