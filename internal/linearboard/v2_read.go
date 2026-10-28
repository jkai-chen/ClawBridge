package linearboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

type pairV2 struct{ ProjectID, TeamID string }
type verifiedV2 struct {
	Pairs    []pairV2
	Teams    []linearprovider.Team
	Projects []linearprovider.Project
}

func (s *Service) BindingV2(ctx context.Context, scope bus.Request) (bus.TaskboardBindingV2, error) {
	if s.cfg.Executor == nil {
		return bus.TaskboardBindingV2{}, errors.New("task-board authority unavailable")
	}
	scope.Operation = "taskboard_get_v2"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return bus.TaskboardBindingV2{}, e
	}
	if r.Source != "server" && r.Source != "local-authority" {
		return bus.TaskboardBindingV2{}, errors.New("task-board V2 requires authoritative settings")
	}
	b, e := bus.ParseTaskboardBindingV2(r.Data)
	if e != nil || !b.Configured || s.cfg.Workspace == "" {
		return b, e
	}
	// A service bound to one Linear workspace (the app's organisation)
	// serves only bindings pinned to it, on every read of the binding.
	switch b.LinearWorkspaceID {
	case "":
		return bus.TaskboardBindingV2{}, errWorkspaceUnpinned
	case s.cfg.Workspace:
		return b, nil
	}
	return bus.TaskboardBindingV2{}, errWorkspaceMismatch
}

var (
	errWorkspaceUnpinned = errors.New("workspace_unpinned: this binding records no Linear workspace; the project owner links it again to pin it; nothing was sent")
	errWorkspaceMismatch = errors.New("workspace_mismatch: this binding belongs to another Linear workspace than the ClawBridge app's; nothing was sent")
)

func (s *Service) unchangedV2(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2) error {
	current, e := s.BindingV2(ctx, scope)
	if e != nil {
		return e
	}
	if current.WorkspaceID != b.WorkspaceID || current.ProjectID != b.ProjectID || current.Revision != b.Revision || !current.Configured || digest(current) != digest(b) {
		return &ReceiptError{Outcome: "binding_changed"}
	}
	return nil
}
func verifyBindingV2(ctx context.Context, p Provider, b bus.TaskboardBindingV2) (verifiedV2, error) {
	var out verifiedV2
	if !b.Configured {
		return out, errors.New("task-board is not linked")
	}
	teams := SortedTeams(b.LinearTeamIDs)
	for _, id := range teams {
		team, e := p.GetTeam(ctx, id)
		if e != nil {
			return out, e
		}
		if team.ID != id {
			return out, errors.New("Linear team access or identity changed")
		}
		out.Teams = append(out.Teams, team)
	}
	if b.Mode == "team_board" {
		for _, id := range teams {
			out.Pairs = append(out.Pairs, pairV2{TeamID: id})
		}
		return out, nil
	}
	if b.Mode != "project_set" || len(b.LinearProjectIDs) == 0 {
		return out, errors.New("unsupported task-board mapping mode")
	}
	listed, e := listBoundProjects(ctx, p, teams, b.LinearProjectIDs)
	if e != nil {
		return out, e
	}
	covered := map[string]bool{}
	for _, id := range SortedTeams(b.LinearProjectIDs) {
		project, ok := listed[id]
		if !ok {
			// Trashed, missing from the listing, or listed inconsistently:
			// verify individually, exactly as before batching.
			if project, e = p.GetProject(ctx, id); e != nil {
				return out, e
			}
		}
		if project.ID != id {
			return out, errors.New("Linear project identity changed")
		}
		out.Projects = append(out.Projects, project)
		intersects := false
		for _, team := range teams {
			if contains(project.TeamIDs, team) {
				intersects = true
				covered[team] = true
				out.Pairs = append(out.Pairs, pairV2{ProjectID: id, TeamID: team})
			}
		}
		if !intersects {
			return out, errors.New("selected project has no verified allowed team")
		}
	}
	for _, id := range teams {
		if !covered[id] {
			return out, errors.New("selected team is not represented by the allowed projects")
		}
	}
	return out, nil
}

// teamProjectLister lists a team's projects, trashed ones included and marked.
type teamProjectLister interface {
	TeamProjects(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.ProjectListing], error)
}

// maxProjectListPages bounds the batch listing per team; projects beyond it are
// verified individually.
const maxProjectListPages = 10

// listBoundProjects reads each allowed team's project list once and returns
// the bound projects found live in it, matched by UUID only. A bound project
// listed as trashed refuses the binding. One listed differently under two teams,
// or not listed, is left out, so the caller verifies it with get_project. A
// provider without the listing, or a listing Linear answers in an unexpected
// shape, falls back the same way.
func listBoundProjects(ctx context.Context, p Provider, teams, bound []string) (map[string]linearprovider.Project, error) {
	found := map[string]linearprovider.Project{}
	lister, ok := p.(teamProjectLister)
	if !ok {
		return found, nil
	}
	want := map[string]bool{}
	for _, id := range bound {
		want[id] = true
	}
	excluded := map[string]bool{}
	for _, team := range teams {
		cursor := ""
		for page := 0; page < maxProjectListPages; page++ {
			rows, e := lister.TeamProjects(ctx, team, linearprovider.PageOptions{Cursor: cursor})
			if e != nil {
				var pe *linearprovider.Error
				if errors.As(e, &pe) && !pe.NotSent && batchFallbackKinds[pe.Kind] {
					return map[string]linearprovider.Project{}, nil
				}
				return nil, e
			}
			for _, row := range rows.Items {
				if !want[row.ID] || excluded[row.ID] {
					continue
				}
				if row.Trashed {
					// get_project cannot see the trash, so never fall back.
					return nil, errors.New("bound Linear project is in trash; relink the task board")
				}
				if prior, seen := found[row.ID]; seen && !slices.Equal(prior.TeamIDs, row.TeamIDs) {
					delete(found, row.ID)
					excluded[row.ID] = true
					continue
				}
				found[row.ID] = row.Project
			}
			if rows.Complete {
				break
			}
			cursor = rows.NextCursor
		}
	}
	return found, nil
}

// batchFallbackKinds are listing failures that say nothing about the bound
// projects themselves; individual verification still decides.
var batchFallbackKinds = map[string]bool{"capability_unavailable": true, "invalid_response": true, "scope_metadata_missing": true, "pagination_unavailable": true, "result_limit": true}

// walksPairV2: the listing walks the issue's own verified pair, so the
// issue is shown under that pair.
func walksPairV2(pairs []pairV2, b bus.TaskboardBindingV2, issue linearprovider.IssueV2) bool {
	for _, pair := range pairs {
		if pair.TeamID != issue.TeamID {
			continue
		}
		if b.Mode != "project_set" || (issue.ProjectKnown && pair.ProjectID == issue.ProjectID) {
			return true
		}
	}
	return false
}

func checkIssueV2(issue linearprovider.IssueV2, b bus.TaskboardBindingV2, v verifiedV2) error {
	if !linearprovider.ValidID(issue.ID) || !contains(b.LinearTeamIDs, issue.TeamID) {
		return outOfScopeError("issue is outside the selected task-board scope")
	}
	if b.Mode == "project_set" {
		if !issue.ProjectKnown {
			return errors.New("issue project context is unavailable")
		}
		for _, pair := range v.Pairs {
			if pair.ProjectID == issue.ProjectID && pair.TeamID == issue.TeamID {
				return nil
			}
		}
		return outOfScopeError("issue project/team pair is outside the verified scope")
	}
	return nil
}
func (s *Service) ContextV2(ctx context.Context, scope bus.Request) (ContextV2, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return ContextV2{}, e
	}
	profile, p, done, e := s.open(ctx)
	if e != nil {
		return ContextV2{}, e
	}
	defer done()
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return ContextV2{}, e
	}
	out := ContextV2{Binding: b, Profile: profile, WorkspaceIDAvailable: profile.WorkspaceID != ""}
	if p2, ok := p.(ProviderV2); ok {
		// Capability negotiation reads only the advertised tool list.
		if caps, e := p2.Capabilities(ctx); e == nil {
			out.Capabilities = &caps
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return ContextV2{}, e
	}
	return out, nil
}
func (s *Service) LinkV2(ctx context.Context, scope bus.Request, in LinkInputV2) (any, error) {
	if e := ValidateLinkV2(in); e != nil {
		return nil, e
	}
	if e := validateLinkCloses(in); e != nil {
		return nil, e
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if b.Revision != in.ExpectedRevision {
		return nil, errors.New("task-board revision conflict")
	}
	profile, p, done, e := s.openForWrite(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	mapping := b
	mapping.Configured = true
	mapping.Provider = "linear"
	mapping.MappingVersion = 2
	mapping.Mode = "project_set"
	if in.TeamBoard {
		mapping.Mode = "team_board"
	}
	mapping.LinearTeamIDs = SortedTeams(in.TeamIDs)
	mapping.LinearDefaultTeamID = in.DefaultTeamID
	mapping.LinearProjectIDs = SortedTeams(in.ProjectIDs)
	if mapping.LinearProjectIDs == nil {
		mapping.LinearProjectIDs = []string{}
	}
	v, e := verifyBindingV2(ctx, p, mapping)
	if e != nil {
		return nil, e
	}
	// The binding records the Linear workspace it was linked in, read from
	// the connection itself, so a later identity (the ClawBridge app) can be
	// checked against it.
	workspace := profile.WorkspaceID
	if r, ok := p.(interface {
		Workspace(context.Context) (linearprovider.Workspace, error)
	}); ok {
		w, e := r.Workspace(ctx)
		var pe *linearprovider.Error
		switch {
		case e == nil:
			if workspace != "" && workspace != w.ID {
				return nil, errors.New("the personal profile and Linear disagree about the workspace; reconnect the profile")
			}
			workspace = w.ID
		case errors.As(e, &pe) && pe.Kind == "capability_unavailable":
		default:
			return nil, e
		}
	}
	if b.LinearWorkspaceID != "" && workspace == "" {
		return nil, errors.New("this binding is pinned to a Linear workspace, and this connection cannot report its workspace; nothing was changed")
	}
	if b.LinearWorkspaceID != "" && workspace != b.LinearWorkspaceID {
		return nil, errors.New("this binding belongs to another Linear workspace; unlink it before linking this one")
	}
	current, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if digest(current) != digest(b) {
		return nil, errors.New("task-board binding changed during preview")
	}
	var policy closesPolicy
	if !in.TeamBoard {
		if policy, e = s.closesPolicy(ctx, scope); e != nil {
			return nil, e
		}
	}
	if !in.Confirm {
		preview := LinkPreviewV2{Binding: b, DefaultTeamID: in.DefaultTeamID, Teams: v.Teams, Projects: v.Projects, TeamBoard: in.TeamBoard, LinearWorkspaceID: workspace}
		linkClosesPreview(policy, in, &preview)
		return preview, nil
	}
	// A3: every check on who closes runs before the binding is written.
	changes := closesChanges(policy, in.Closes)
	for _, project := range changes {
		if in.Closes[project] != ClosesAgent {
			continue
		}
		if in.Via != ClosesViaTTY && in.Via != ClosesViaTUI {
			return nil, errors.New("letting agents close issues is confirmed at a terminal, by typing the project's name; nothing was changed")
		}
		if policy.Revision == 0 && !in.MinimalPolicy {
			return nil, errors.New(minimalPolicy(policy.FlowProven) + " Run the link again with --minimal-status-policy to create it; nothing was changed.")
		}
	}
	if policy.Revision == 0 && in.MinimalPolicy {
		// §10.8 L1 (R89-L2): the minimal policy is the owner's to create,
		// so a lead is told before the binding is written.
		access, e := s.access(ctx, scope)
		if e != nil {
			return nil, e
		}
		if access.ProjectRole != "owner" {
			return nil, errors.New(minimalOwnerOnly + "; nothing was changed")
		}
		if e = s.minimalPolicyTyped(ctx, scope, mapping.LinearTeamIDs); e != nil {
			return nil, e
		}
	}
	scope.Operation = "taskboard_set_v2"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = in.ExpectedRevision
	scope.Args["mode"] = mapping.Mode
	scope.Args["linear_team_ids"] = mapping.LinearTeamIDs
	scope.Args["linear_default_team_id"] = mapping.LinearDefaultTeamID
	scope.Args["linear_project_ids"] = mapping.LinearProjectIDs
	if workspace != "" {
		scope.Args["linear_workspace_id"] = workspace
	}
	if len(changes) > 0 {
		scope.Args["closes_pending"] = changes
	}
	teamNames, projectNames := linkNames(v)
	scope.Args["linear_team_names"] = teamNames
	scope.Args["linear_project_names"] = projectNames
	r, e := s.cfg.Executor.Execute(ctx, scope)
	for retry := 0; e != nil && retry < 2; retry++ {
		msg := e.Error()
		if strings.Contains(msg, `unsupported argument "linear_team_names"`) || strings.Contains(msg, `unsupported argument "linear_project_names"`) {
			// An older server keeps no names: bind without them.
			delete(scope.Args, "linear_team_names")
			delete(scope.Args, "linear_project_names")
		} else if len(changes) > 0 && strings.Contains(msg, `unsupported argument "closes_pending"`) {
			// An older server: bind without it; closes set then says so.
			delete(scope.Args, "closes_pending")
		} else {
			break
		}
		r, e = s.cfg.Executor.Execute(ctx, scope)
	}
	if e != nil || len(changes) == 0 {
		return r.Data, e
	}
	out := map[string]any{}
	if m, ok := r.Data.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	out["closes"] = s.applyLinkCloses(ctx, scope, in, changes)
	return out, nil
}
func ValidateLinkV2(in LinkInputV2) error {
	if in.ExpectedRevision < 0 || in.ExpectedRevision >= 9007199254740991 || len(in.TeamIDs) < 1 || len(in.TeamIDs) > 20 || !contains(in.TeamIDs, in.DefaultTeamID) || in.TeamBoard != (len(in.ProjectIDs) == 0) || len(in.ProjectIDs) > 50 {
		return errors.New("link requires teams, default, original revision and explicit project set or team board")
	}
	for _, ids := range [][]string{in.TeamIDs, in.ProjectIDs} {
		seen := map[string]bool{}
		for _, id := range ids {
			if !linearprovider.ValidID(id) || seen[id] {
				return errors.New("mapping IDs must be unique canonical UUIDs")
			}
			seen[id] = true
		}
	}
	return nil
}
func (s *Service) UnlinkV2(ctx context.Context, scope bus.Request, revision int64, confirm bool) (any, error) {
	if revision < 0 || revision >= 9007199254740991 {
		return nil, errors.New("invalid original expected revision")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if b.Revision != revision {
		return nil, errors.New("task-board revision conflict")
	}
	if !confirm {
		return map[string]any{"binding": b, "confirmation_required": true}, nil
	}
	scope.Operation = "taskboard_clear_v2"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = revision
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// RepairUnlinkV2 clears a binding the authority reports unreadable. It cannot
// read the binding first, as UnlinkV2 does, because every read of it is
// refused; the authority accepts it only from a project owner, at the stored
// revision, and only while the binding is unreadable.
func (s *Service) RepairUnlinkV2(ctx context.Context, scope bus.Request, revision int64) (any, error) {
	if revision < 0 || revision >= 9007199254740991 {
		return nil, errors.New("invalid original expected revision")
	}
	scope.Operation = "taskboard_clear_v2"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = revision
	scope.Args["repair"] = true
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

type cursorV2 struct {
	Version  int    `json:"v"`
	Scope    string `json:"scope"`
	Pair     int    `json:"pair"`
	Provider string `json:"cursor"`
}

func readCursorV2(opaque, key string, count int) (cursorV2, error) {
	c := cursorV2{Version: 2, Scope: key}
	if opaque == "" {
		return c, nil
	}
	if len(opaque) > 8192 {
		return c, errors.New("cursor exceeds bound")
	}
	raw, e := base64.RawURLEncoding.DecodeString(opaque)
	if e != nil || len(raw) > 6144 {
		return c, errors.New("invalid V2 cursor")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 {
		return c, errors.New("invalid V2 cursor fields")
	}
	for _, k := range []string{"v", "scope", "pair", "cursor"} {
		r := bytes.TrimSpace(fields[k])
		if len(r) == 0 || bytes.Equal(r, []byte("null")) {
			return c, errors.New("incomplete V2 cursor")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 2 || c.Scope != key || c.Pair < 0 || c.Pair >= count || len(c.Provider) > 4096 {
		return c, errors.New("cursor does not belong to current profile, mapping and filters")
	}
	return c, nil
}
func encodeCursorV2(c cursorV2) (string, error) {
	raw, e := json.Marshal(c)
	if e != nil || len(raw) > 6144 {
		return "", errors.New("provider continuation exceeds cursor bound")
	}
	s := base64.RawURLEncoding.EncodeToString(raw)
	if len(s) > 8192 {
		return "", errors.New("cursor exceeds bound")
	}
	return s, nil
}
func issueCursorKeyV2(profile string, b bus.TaskboardBindingV2, label, updatedAfter string, pairs []pairV2) string {
	key := []any{profile, b.WorkspaceID, b.ProjectID, b.MappingVersion, b.Revision, b.Mode, SortedTeams(b.LinearTeamIDs), SortedTeams(b.LinearProjectIDs), label, pairs}
	if updatedAfter != "" {
		// Appended only when set, so unfiltered cursors keep their key.
		key = append(key, updatedAfter)
	}
	return digest(key)
}
func (s *Service) IssuesV2(ctx context.Context, scope bus.Request, in IssuesInputV2) (linearprovider.Page[linearprovider.IssueV2], error) {
	empty := linearprovider.Page[linearprovider.IssueV2]{}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > 8192 || (in.LabelID != "" && !linearprovider.ValidID(in.LabelID)) {
		return empty, errors.New("invalid V2 issue pagination or filter")
	}
	var after time.Time
	if in.UpdatedAfter != "" {
		var ok bool
		if after, ok = linearprovider.UpdatedAfterTime(in.UpdatedAfter); !ok {
			return empty, errors.New("updated_after requires an absolute RFC 3339 time")
		}
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer done()
	v, e := s.verifyReadBindingV2(ctx, p, b)
	if e != nil {
		return empty, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, errors.New("native provider lacks V2 issue capability")
	}
	pairs := v.Pairs
	if in.view != nil {
		pairs = in.view.narrow(pairs)
	}
	if in.LabelID != "" {
		// A team label selects its one team; a workspace label, or a row
		// scoped only by the provider's team filter, spans every allowed team
		// whose listing returns it.
		teams := map[string]bool{}
		global, scoped := false, false
		for _, id := range SortedTeams(b.LinearTeamIDs) {
			label, e := findLabelV2(ctx, p2, id, in.LabelID)
			if e != nil {
				return empty, e
			}
			if label != nil {
				teams[id] = true
				spans := label.Global || label.FilterScoped
				global = global || spans
				scoped = scoped || !spans
			}
		}
		if len(teams) == 0 {
			return empty, errors.New("label is not verified for an allowed team")
		}
		if global && scoped {
			return empty, errors.New("contradictory label scope")
		}
		if !global && len(teams) > 1 {
			return empty, errors.New("ambiguous label team")
		}
		pairs = nil
		for _, pair := range v.Pairs {
			if teams[pair.TeamID] {
				pairs = append(pairs, pair)
			}
		}
	}
	filterKey := in.LabelID
	if in.view != nil {
		// A cursor continues only the same view at the same filter.
		filterKey = "view:" + in.view.id + ":" + in.view.filter.SHA256
	}
	key := issueCursorKeyV2(s.cfg.ProfileID, b, filterKey, in.UpdatedAfter, pairs)
	c, e := readCursorV2(in.Cursor, key, len(pairs))
	if e != nil {
		return empty, e
	}
	if len(pairs) == 0 {
		return empty, errors.New("no verified issue pair")
	}
	pair := pairs[c.Pair]
	page, e := p2.IssuesV2(ctx, linearprovider.IssueQueryV2{IssueQuery: linearprovider.IssueQuery{TeamID: pair.TeamID, ProjectID: pair.ProjectID, Limit: in.Limit, Cursor: c.Provider}, LabelID: in.LabelID, UpdatedAfter: in.UpdatedAfter})
	if e != nil {
		return empty, e
	}
	if len(page.Items) > in.Limit || (page.NextCursor == "" && !page.Complete) || (page.NextCursor != "" && page.Complete) || len(page.NextCursor) > 4096 {
		return empty, errors.New("incomplete or contradictory issue continuation")
	}
	seen := map[string]string{}
	filteredName := ""
	var rows, kept []linearprovider.IssueV2
	for _, issue := range page.Items {
		if linearprovider.ValidID(issue.ID) && issue.TeamID != pair.TeamID {
			// Linear may list a sub-team's issues under its parent team's
			// filter. Such a row is never shown on this pair's page: a
			// verified pair this listing walks shows it there, and anything
			// else is dropped and only counted.
			if !walksPairV2(pairs, b, issue) {
				page.FilteredOtherTeam++
			}
			continue
		}
		if e = checkIssueV2(issue, b, v); e != nil {
			return empty, e
		}
		if issue.TeamID != pair.TeamID || (b.Mode == "project_set" && issue.ProjectID != pair.ProjectID) {
			return empty, errors.New("provider page escaped the selected pair")
		}
		rows = append(rows, issue)
		if in.UpdatedAfter != "" {
			if updated, ok := linearprovider.UpdatedAfterTime(issue.UpdatedAt); !ok || updated.Before(after) {
				return empty, errors.New("provider did not prove the updated_after filter")
			}
		}
		if in.LabelID != "" && !issue.LabelsKnown {
			// Native rows name their labels; one catalog walk per page names
			// the selected label, and each row must carry that name.
			if filteredName == "" {
				if filteredName, e = filteredLabelNameV2(ctx, p2, pair.TeamID, in.LabelID); e != nil {
					return empty, e
				}
			}
			if !contains(issue.LabelNames, filteredName) {
				return empty, errors.New("provider did not prove the selected label filter")
			}
		} else if in.LabelID != "" && !contains(issue.LabelIDs, in.LabelID) {
			return empty, errors.New("provider did not prove the selected label filter")
		}
		h := digest(issue)
		if previous, ok := seen[issue.ID]; ok && previous != h {
			return empty, errors.New("contradictory duplicate issue identity")
		}
		seen[issue.ID] = h
		if in.view != nil {
			ok, e := in.view.match(ctx, p, issue)
			if e != nil {
				return empty, e
			}
			if ok {
				kept = append(kept, issue)
			}
		}
	}
	page.Items = rows
	if in.view != nil {
		page.Items = kept
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, e
	}
	if page.NextCursor != "" {
		if page.NextCursor == c.Provider {
			return empty, errors.New("provider continuation did not advance")
		}
		c.Provider = page.NextCursor
	} else {
		c.Pair++
		c.Provider = ""
	}
	page.NextCursor = ""
	page.Complete = c.Pair >= len(pairs)
	if !page.Complete {
		page.NextCursor, e = encodeCursorV2(c)
		if e != nil {
			return empty, e
		}
	}
	return page, nil
}
func (s *Service) IssueV2(ctx context.Context, scope bus.Request, id string) (linearprovider.IssueV2, error) {
	issue, _, e := s.issueV2(ctx, scope, id, false, nil)
	return issue, e
}

// IssueV2ResolvedLabels is the opt-in read that proves the issue's label IDs
// when native Linear returns label names only; see resolveLabelsV2.
func (s *Service) IssueV2ResolvedLabels(ctx context.Context, scope bus.Request, id string) (linearprovider.IssueV2, error) {
	issue, _, e := s.issueV2(ctx, scope, id, true, nil)
	return issue, e
}

// IssueV2WithComments is the opt-in read that also returns the issue's
// comments. The issue read stays the scope proof: comments are read only
// after it passes, by the proven issue UUID.
func (s *Service) IssueV2WithComments(ctx context.Context, scope bus.Request, id string, in CommentsInputV2) (IssueCommentsV2, error) {
	if !linearprovider.ValidID(id) {
		return IssueCommentsV2{}, errors.New("issue requires immutable UUID")
	}
	since, e := linearprovider.ParseCommentsSince(id, in.Since)
	if e != nil {
		return IssueCommentsV2{}, e
	}
	issue, comments, e := s.issueV2(ctx, scope, id, in.ResolveLabels, &since)
	if e != nil {
		return IssueCommentsV2{}, e
	}
	out := IssueCommentsV2{IssueV2: issue, Comments: comments, CommentsOwnership: "unavailable"}
	out.Items, out.own = s.commentOwners(ctx, scope, id, comments.Items)
	if out.own.available {
		out.CommentsOwnership, out.CommentsYoungPending = "available", out.own.youngPending
	}
	return out, nil
}
func (s *Service) issueV2(ctx context.Context, scope bus.Request, id string, resolve bool, comments *linearprovider.CommentsSince) (issue linearprovider.IssueV2, out linearprovider.Comments, e error) {
	empty := linearprovider.IssueV2{}
	if !linearprovider.ValidID(id) {
		return empty, out, errors.New("issue requires immutable UUID")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, out, e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return empty, out, e
	}
	defer done()
	v, e := s.verifyReadBindingV2(ctx, p, b)
	if e != nil {
		return empty, out, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, out, errors.New("native provider lacks V2 issue capability")
	}
	issue, e = s.readIssue(ctx, p2, id)
	if e != nil {
		return empty, out, e
	}
	if issue.ID != id {
		return empty, out, errors.New("provider returned another issue")
	}
	if e = checkIssueV2(issue, b, v); e != nil {
		return empty, out, e
	}
	if resolve {
		if issue, e = resolveLabelsV2(ctx, p2, issue); e != nil {
			return empty, out, e
		}
	}
	if comments != nil {
		reader, ok := p2.(commentReaderV2)
		if !ok {
			return empty, out, errors.New("native provider lacks issue comment reads")
		}
		// The walk stops short of the caller's deadline, returning an
		// incomplete read (reason deadline) with time left to finish.
		walk, cancel := ctx, context.CancelFunc(func() {})
		if deadline, ok := ctx.Deadline(); ok {
			walk, cancel = context.WithDeadline(ctx, deadline.Add(-commentsReserveV2))
		}
		// An identifier the plain read tolerated but that is not a display
		// identifier is left out, so comments never fail a read that passes.
		identifier := ""
		if linearprovider.ValidIssueIdentifier(issue.Identifier) {
			identifier = issue.Identifier
		}
		out, e = reader.IssueComments(walk, linearprovider.CommentsQuery{IssueID: issue.ID, Identifier: identifier, Since: *comments})
		cancel()
		if e != nil {
			return empty, out, e
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, out, e
	}
	if issue.DescriptionKnown {
		issue.DescriptionSHA256 = descriptionSHA256(issue.Description)
	}
	return issue, out, nil
}

// commentReaderV2 is the optional provider support for reading comments.
type commentReaderV2 interface {
	IssueComments(context.Context, linearprovider.CommentsQuery) (linearprovider.Comments, error)
}

// commentsReserveV2 is the time a comment read leaves before the request's
// deadline for the binding check and the reply.
const commentsReserveV2 = 5 * time.Second

// labelMembershipV2 is the optional provider support for proving label IDs
// from names. Providers without it leave name-only label sets unknown.
type labelMembershipV2 interface {
	LabelCatalog(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.LabelName], error)
	IssueHasLabel(ctx context.Context, issue, team, project, label string) (bool, bool, error)
}

// resolveLabelsV2 proves an issue's label IDs when the read returned names
// only. Each name must match exactly one canonical label in the team's
// complete listing (archived labels and groups included), and list_issues
// filtered by that label's UUID must return the issue. Any ambiguous,
// missing, unconfirmed or unbounded step leaves LabelsKnown false, so a
// replacement is refused exactly as before. Provider read errors are returned.
func resolveLabelsV2(ctx context.Context, p ProviderV2, issue linearprovider.IssueV2) (linearprovider.IssueV2, error) {
	if issue.LabelsKnown || len(issue.LabelNames) == 0 {
		return issue, nil
	}
	m, ok := p.(labelMembershipV2)
	if !ok {
		return issue, nil
	}
	caps, e := p.Capabilities(ctx)
	if e != nil || !caps.LabelMembershipProof {
		return issue, e
	}
	names := map[string]bool{}
	for _, name := range issue.LabelNames {
		if names[name] {
			return issue, nil
		}
		names[name] = true
	}
	byName, complete, e := labelCatalogV2(ctx, m, issue.TeamID)
	if e != nil || !complete {
		return issue, e
	}
	ids := []string{}
	for name := range names {
		candidates := byName[name]
		if len(candidates) != 1 || !linearprovider.ValidID(candidates[0]) {
			return issue, nil
		}
		ids = append(ids, candidates[0])
	}
	for _, id := range ids {
		found, done, e := m.IssueHasLabel(ctx, issue.ID, issue.TeamID, issue.ProjectID, id)
		if e != nil {
			return issue, e
		}
		if !found || !done {
			return issue, nil
		}
	}
	proven, e := sortedUniqueIDs(ids, 100)
	if e != nil {
		return issue, nil
	}
	issue.LabelIDs, issue.LabelsKnown = proven, true
	return issue, nil
}

// labelCatalogV2 walks a team's complete label listing (archived labels and
// groups included) into name -> IDs. complete=false means the walk hit its
// bound, which callers treat as unknown.
func labelCatalogV2(ctx context.Context, m labelMembershipV2, team string) (map[string][]string, bool, error) {
	byName := map[string][]string{}
	cursor, seen := "", map[string]bool{}
	for page := 0; page < 64; page++ {
		rows, e := m.LabelCatalog(ctx, team, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return nil, false, e
		}
		if e = validProviderPageV2(rows.NextCursor, rows.Complete, cursor, len(rows.Items), 100); e != nil {
			return nil, false, e
		}
		for _, row := range rows.Items {
			byName[row.Name] = append(byName[row.Name], row.ID)
		}
		if rows.Complete {
			return byName, true, nil
		}
		if seen[rows.NextCursor] {
			return nil, false, errors.New("label pagination cycle")
		}
		seen[rows.NextCursor] = true
		cursor = rows.NextCursor
	}
	return nil, false, nil
}

// filteredLabelNameV2 names the selected label for rows that carry label names
// only. The provider must prove membership from names, and the label's name
// must be unique in the team's complete listing, as in resolveLabelsV2; a
// names-only row is then accepted only if it carries that name, so a filter
// Linear silently ignored cannot pass rows with other labels.
func filteredLabelNameV2(ctx context.Context, p ProviderV2, team, label string) (string, error) {
	refuse := errors.New("provider did not prove the selected label filter")
	m, ok := p.(labelMembershipV2)
	if !ok {
		return "", refuse
	}
	caps, e := p.Capabilities(ctx)
	if e != nil {
		return "", e
	}
	if !caps.LabelMembershipProof {
		return "", refuse
	}
	byName, complete, e := labelCatalogV2(ctx, m, team)
	if e != nil {
		return "", e
	}
	name := ""
	for candidate, ids := range byName {
		if contains(ids, label) {
			if name != "" || len(ids) != 1 {
				return "", refuse
			}
			name = candidate
		}
	}
	if !complete || name == "" {
		return "", refuse
	}
	return name, nil
}

func sortedUniqueIDs(ids []string, max int) ([]string, error) {
	if ids == nil || len(ids) > max {
		return nil, errors.New("ID array must be present, non-null and bounded")
	}
	out := append([]string{}, ids...)
	sort.Strings(out)
	for i, id := range out {
		if !linearprovider.ValidID(id) || (i > 0 && id == out[i-1]) {
			return nil, errors.New("IDs must be unique canonical UUIDs")
		}
	}
	return out, nil
}

// linkNames are the names Linear gave for a binding's teams and projects,
// as the server stores them (display text only, never a decision): each
// trimmed and cut to 80 characters, and left out when it still breaks the
// rule.
func linkNames(v verifiedV2) (teams, projects map[string]any) {
	teams, projects = map[string]any{}, map[string]any{}
	for _, t := range v.Teams {
		if name, ok := storableName(t.Name); ok {
			teams[t.ID] = name
		}
	}
	for _, p := range v.Projects {
		if name, ok := storableName(p.Name); ok {
			projects[p.ID] = name
		}
	}
	return teams, projects
}

func storableName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 80 {
		name = strings.TrimSpace(string(r[:80]))
	}
	return name, bus.LinearNameOK(name)
}

// RefreshNamesV2 reads the bound teams' and projects' names from the
// caller's Linear connection and stores them with the binding, at its
// current revision, which the refresh leaves as it is.
func (s *Service) RefreshNamesV2(ctx context.Context, scope bus.Request) (any, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return nil, e
	}
	teams, projects := linkNames(v)
	scope.Operation = "taskboard_names_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = b.Revision
	scope.Args["linear_team_names"] = teams
	scope.Args["linear_project_names"] = projects
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}
