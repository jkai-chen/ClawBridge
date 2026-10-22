package linearboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"

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
	return bus.ParseTaskboardBindingV2(r.Data)
}
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
	covered := map[string]bool{}
	for _, id := range SortedTeams(b.LinearProjectIDs) {
		project, e := p.GetProject(ctx, id)
		if e != nil {
			return out, e
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
func checkIssueV2(issue linearprovider.IssueV2, b bus.TaskboardBindingV2, v verifiedV2) error {
	if !linearprovider.ValidID(issue.ID) || !contains(b.LinearTeamIDs, issue.TeamID) {
		return errors.New("issue is outside the selected task-board scope")
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
		return errors.New("issue project/team pair is outside the verified scope")
	}
	return nil
}
func (s *Service) ContextV2(ctx context.Context, scope bus.Request) (ContextV2, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return ContextV2{}, e
	}
	profile, p, e := s.open(ctx)
	if e != nil {
		return ContextV2{}, e
	}
	defer p.Close()
	if _, e = verifyBindingV2(ctx, p, b); e != nil {
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
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if b.Revision != in.ExpectedRevision {
		return nil, errors.New("task-board revision conflict")
	}
	profile, p, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer p.Close()
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
	current, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if digest(current) != digest(b) {
		return nil, errors.New("task-board binding changed during preview")
	}
	if !in.Confirm {
		return LinkPreviewV2{Binding: b, DefaultTeamID: in.DefaultTeamID, Teams: v.Teams, Projects: v.Projects, TeamBoard: in.TeamBoard}, nil
	}
	scope.Operation = "taskboard_set_v2"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = in.ExpectedRevision
	scope.Args["mode"] = mapping.Mode
	scope.Args["linear_team_ids"] = mapping.LinearTeamIDs
	scope.Args["linear_default_team_id"] = mapping.LinearDefaultTeamID
	scope.Args["linear_project_ids"] = mapping.LinearProjectIDs
	if profile.WorkspaceID != "" {
		scope.Args["linear_workspace_id"] = profile.WorkspaceID
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
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
func issueCursorKeyV2(profile string, b bus.TaskboardBindingV2, label string, pairs []pairV2) string {
	return digest([]any{profile, b.WorkspaceID, b.ProjectID, b.MappingVersion, b.Revision, b.Mode, SortedTeams(b.LinearTeamIDs), SortedTeams(b.LinearProjectIDs), label, pairs})
}
func (s *Service) IssuesV2(ctx context.Context, scope bus.Request, in IssuesInputV2) (linearprovider.Page[linearprovider.IssueV2], error) {
	empty := linearprovider.Page[linearprovider.IssueV2]{}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > 8192 || (in.LabelID != "" && !linearprovider.ValidID(in.LabelID)) {
		return empty, errors.New("invalid V2 issue pagination or filter")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	_, p, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer p.Close()
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return empty, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, errors.New("native provider lacks V2 issue capability")
	}
	pairs := v.Pairs
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
	key := issueCursorKeyV2(s.cfg.ProfileID, b, in.LabelID, pairs)
	c, e := readCursorV2(in.Cursor, key, len(pairs))
	if e != nil {
		return empty, e
	}
	if len(pairs) == 0 {
		return empty, errors.New("no verified issue pair")
	}
	pair := pairs[c.Pair]
	page, e := p2.IssuesV2(ctx, linearprovider.IssueQueryV2{IssueQuery: linearprovider.IssueQuery{TeamID: pair.TeamID, ProjectID: pair.ProjectID, Limit: in.Limit, Cursor: c.Provider}, LabelID: in.LabelID})
	if e != nil {
		return empty, e
	}
	if len(page.Items) > in.Limit || (page.NextCursor == "" && !page.Complete) || (page.NextCursor != "" && page.Complete) || len(page.NextCursor) > 4096 {
		return empty, errors.New("incomplete or contradictory issue continuation")
	}
	seen := map[string]string{}
	for _, issue := range page.Items {
		if e = checkIssueV2(issue, b, v); e != nil {
			return empty, e
		}
		if issue.TeamID != pair.TeamID || (b.Mode == "project_set" && issue.ProjectID != pair.ProjectID) {
			return empty, errors.New("provider page escaped the selected pair")
		}
		if in.LabelID != "" && (!issue.LabelsKnown || !contains(issue.LabelIDs, in.LabelID)) {
			return empty, errors.New("provider did not prove the selected label filter")
		}
		h := digest(issue)
		if previous, ok := seen[issue.ID]; ok && previous != h {
			return empty, errors.New("contradictory duplicate issue identity")
		}
		seen[issue.ID] = h
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
	empty := linearprovider.IssueV2{}
	if !linearprovider.ValidID(id) {
		return empty, errors.New("issue requires immutable UUID")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	_, p, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer p.Close()
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return empty, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, errors.New("native provider lacks V2 issue capability")
	}
	issue, e := p2.IssueV2(ctx, id)
	if e != nil {
		return empty, e
	}
	if issue.ID != id {
		return empty, errors.New("provider returned another issue")
	}
	if e = checkIssueV2(issue, b, v); e != nil {
		return empty, e
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, e
	}
	return issue, nil
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
