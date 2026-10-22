// Package linearboard binds native provider actions to shared authority settings.
package linearboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

type Executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}
type Connections interface {
	Status(context.Context, string) (linearconn.Profile, error)
	HTTPClient(context.Context, string) (*http.Client, error)
}
type Provider interface {
	Close() error
	GetTeam(context.Context, string) (linearprovider.Team, error)
	GetProject(context.Context, string) (linearprovider.Project, error)
	Issues(context.Context, linearprovider.IssueQuery) (linearprovider.Page[linearprovider.Issue], error)
	GetIssue(context.Context, string) (linearprovider.Issue, error)
	Statuses(context.Context, string) ([]linearprovider.Status, error)
	Create(context.Context, linearprovider.CreateInput) (linearprovider.Issue, error)
	Update(context.Context, linearprovider.UpdateInput) (linearprovider.Issue, error)
	Comment(context.Context, string, string) (linearprovider.Receipt, error)
}
type Config struct {
	Authority       string
	Executor        Executor
	Connections     Connections
	ProfileID       string
	LedgerDirectory string
	Open            func(context.Context, *http.Client, bool) (Provider, error)
	Now             func() time.Time
}
type Service struct {
	cfg  Config
	pool *providerPool
}

func New(c Config) *Service {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Open == nil {
		c.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) {
			return linearprovider.New(ctx, h, ro)
		}
	}
	return &Service{cfg: c}
}

type Context struct {
	Binding              bus.TaskboardBinding `json:"binding"`
	Profile              linearconn.Profile   `json:"profile"`
	WorkspaceIDAvailable bool                 `json:"linear_workspace_id_available"`
}

func (s *Service) binding(ctx context.Context, scope bus.Request) (bus.TaskboardBinding, error) {
	if s.cfg.Executor == nil {
		return bus.TaskboardBinding{}, errors.New("task-board authority unavailable")
	}
	scope.Operation = "taskboard_get"
	scope.Args = instanceArgs(scope.Args)
	result, err := s.cfg.Executor.Execute(ctx, scope)
	if err != nil {
		return bus.TaskboardBinding{}, err
	}
	if result.Source != "server" && result.Source != "local-authority" {
		return bus.TaskboardBinding{}, errors.New("task-board context requires authoritative settings")
	}
	return bus.ParseTaskboardBinding(result.Data)
}
func instanceArgs(args map[string]any) map[string]any {
	out := map[string]any{}
	if token, ok := args["instance"].(string); ok && token != "" {
		out["instance"] = token
	}
	return out
}
func (s *Service) Binding(ctx context.Context, scope bus.Request) (bus.TaskboardBinding, error) {
	return s.binding(ctx, scope)
}

// open returns the native connection for one read command and the function
// that releases it. The profile is checked on every command, even when a
// kept-open connection is reused.
func (s *Service) open(ctx context.Context) (linearconn.Profile, Provider, func(), error) {
	return s.connect(ctx, false)
}

// openForWrite always opens a fresh connection, so a write negotiates against
// Linear's current tool schemas rather than a kept tools/list.
func (s *Service) openForWrite(ctx context.Context) (linearconn.Profile, Provider, func(), error) {
	return s.connect(ctx, true)
}
func (s *Service) connect(ctx context.Context, write bool) (linearconn.Profile, Provider, func(), error) {
	if s.cfg.Connections == nil || s.cfg.ProfileID == "" {
		return linearconn.Profile{}, nil, nil, errors.New("select a personal native Linear profile explicitly")
	}
	profile, err := s.cfg.Connections.Status(ctx, s.cfg.ProfileID)
	if err != nil {
		s.pool.drop()
		return profile, nil, nil, err
	}
	if profile.ID != s.cfg.ProfileID {
		s.pool.drop()
		return profile, nil, nil, errors.New("personal profile identity changed")
	}
	fresh := func() (Provider, error) {
		client, err := s.cfg.Connections.HTTPClient(ctx, s.cfg.ProfileID)
		if err != nil {
			return nil, err
		}
		return s.cfg.Open(ctx, client, profile.Access == linearconn.Read)
	}
	if s.pool != nil {
		provider, done, err := s.pool.open(ctx, profile.Access == linearconn.Read, write, s.cfg.Now(), fresh)
		return profile, provider, done, err
	}
	provider, err := fresh()
	if err != nil {
		return profile, provider, nil, err
	}
	return profile, provider, func() { provider.Close() }, nil
}
func verifyBinding(ctx context.Context, p Provider, b bus.TaskboardBinding) error {
	if !b.Configured {
		return errors.New("task-board is not linked; an owner/lead must select teams and a project or team board")
	}
	for _, id := range b.LinearTeamIDs {
		t, err := p.GetTeam(ctx, id)
		if err != nil {
			return err
		}
		if t.ID != id {
			return errors.New("Linear team access or identity changed")
		}
	}
	if b.LinearProjectID != "" {
		project, err := p.GetProject(ctx, b.LinearProjectID)
		if err != nil {
			return err
		}
		if project.ID != b.LinearProjectID {
			return errors.New("Linear project identity changed")
		}
		for _, id := range b.LinearTeamIDs {
			if !contains(project.TeamIDs, id) {
				return errors.New("selected project does not include every allowed team")
			}
		}
	}
	return nil
}
func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func (s *Service) unchanged(ctx context.Context, scope bus.Request, b bus.TaskboardBinding) error {
	current, err := s.binding(ctx, scope)
	if err != nil {
		return err
	}
	if current.WorkspaceID != b.WorkspaceID || current.ProjectID != b.ProjectID || current.Revision != b.Revision || !current.Configured {
		return &ReceiptError{Outcome: "binding_changed"}
	}
	return nil
}

type ReceiptError struct{ Outcome string }

func (e *ReceiptError) Error() string {
	return "Linear write result: " + e.Outcome + "; inspect the recorded receipt before further work"
}
func (e *ReceiptError) Code() string { return e.Outcome }
func (s *Service) access(ctx context.Context, scope bus.Request) (bus.TaskboardAccess, error) {
	scope.Operation = "taskboard_access"
	scope.Args = instanceArgs(scope.Args)
	result, err := s.cfg.Executor.Execute(ctx, scope)
	if err != nil {
		return bus.TaskboardAccess{}, err
	}
	if result.Source != "server" && result.Source != "local-authority" {
		return bus.TaskboardAccess{}, errors.New("task-board write access requires authority")
	}
	return bus.ParseTaskboardAccess(result.Data)
}
func (s *Service) checkWriteAccess(ctx context.Context, scope bus.Request, b bus.TaskboardBinding) (bus.TaskboardAccess, error) {
	access, err := s.access(ctx, scope)
	if err != nil {
		return access, err
	}
	if access.WorkspaceID != b.WorkspaceID || access.ProjectID != b.ProjectID || !access.CanWrite {
		return access, errWriteAccessDenied
	}
	return access, nil
}
func (s *Service) postReceipt(ctx context.Context, scope bus.Request, b bus.TaskboardBinding, receipt any) (any, error) {
	if err := s.unchanged(ctx, scope, b); err != nil {
		var changed *ReceiptError
		if errors.As(err, &changed) {
			return receipt, changed
		}
		return receipt, &ReceiptError{Outcome: "accepted_context_unverified"}
	}
	return receipt, nil
}
func (s *Service) Context(ctx context.Context, scope bus.Request) (Context, error) {
	b, err := s.binding(ctx, scope)
	if err != nil {
		return Context{}, err
	}
	profile, p, done, err := s.open(ctx)
	if err != nil {
		return Context{}, err
	}
	defer done()
	if err = verifyBinding(ctx, p, b); err != nil {
		return Context{}, err
	}
	if err = s.unchanged(ctx, scope, b); err != nil {
		return Context{}, err
	}
	return Context{b, profile, profile.WorkspaceID != ""}, nil
}

type LinkInput struct {
	TeamIDs                  []string
	DefaultTeamID, ProjectID string
	ExpectedRevision         int64
	TeamBoard, Confirm       bool
}
type LinkPreview struct {
	DefaultTeamID string                  `json:"linear_default_team_id"`
	Binding       bus.TaskboardBinding    `json:"current"`
	Teams         []linearprovider.Team   `json:"teams"`
	Project       *linearprovider.Project `json:"project,omitempty"`
	TeamBoard     bool                    `json:"team_board"`
	Confirmed     bool                    `json:"confirmed"`
}

func (s *Service) Link(ctx context.Context, scope bus.Request, in LinkInput) (any, error) {
	if len(in.TeamIDs) < 1 || len(in.TeamIDs) > 20 || !contains(in.TeamIDs, in.DefaultTeamID) || in.ExpectedRevision < 0 || in.ExpectedRevision >= 9007199254740991 || in.TeamBoard == (in.ProjectID != "") {
		return nil, errors.New("link requires unique teams, default team, expected revision and explicit project or team board")
	}
	seen := map[string]bool{}
	for _, id := range in.TeamIDs {
		if !linearprovider.ValidID(id) || seen[id] {
			return nil, errors.New("invalid or duplicate team UUID")
		}
		seen[id] = true
	}
	if in.ProjectID != "" && !linearprovider.ValidID(in.ProjectID) {
		return nil, errors.New("invalid project UUID")
	}
	b, err := s.binding(ctx, scope)
	if err != nil {
		return nil, err
	}
	if b.Revision != in.ExpectedRevision {
		return nil, errors.New("task-board revision conflict")
	}
	profile, p, done, err := s.openForWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	mapping := b
	mapping.Configured = true
	mapping.Provider = "linear"
	mapping.LinearTeamIDs = append([]string(nil), in.TeamIDs...)
	mapping.LinearDefaultTeamID = in.DefaultTeamID
	mapping.LinearProjectID = in.ProjectID
	if err = verifyBinding(ctx, p, mapping); err != nil {
		return nil, err
	}
	preview := LinkPreview{DefaultTeamID: in.DefaultTeamID, Binding: b, Teams: []linearprovider.Team{}, TeamBoard: in.TeamBoard}
	for _, id := range in.TeamIDs {
		team, e := p.GetTeam(ctx, id)
		if e != nil {
			return nil, e
		}
		preview.Teams = append(preview.Teams, team)
	}
	if in.ProjectID != "" {
		project, e := p.GetProject(ctx, in.ProjectID)
		if e != nil {
			return nil, e
		}
		preview.Project = &project
	}
	current, e := s.binding(ctx, scope)
	if e != nil {
		return nil, e
	}
	if current.Revision != b.Revision {
		return nil, errors.New("task-board binding changed during preview")
	}
	if !in.Confirm {
		return preview, nil
	}
	scope.Operation = "taskboard_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = in.ExpectedRevision
	scope.Args["linear_team_ids"] = in.TeamIDs
	scope.Args["linear_project_id"] = nil
	scope.Args["linear_default_team_id"] = in.DefaultTeamID
	if in.ProjectID != "" {
		scope.Args["linear_project_id"] = in.ProjectID
	}
	if profile.WorkspaceID != "" {
		scope.Args["linear_workspace_id"] = profile.WorkspaceID
	}
	result, err := s.cfg.Executor.Execute(ctx, scope)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}
func (s *Service) Unlink(ctx context.Context, scope bus.Request, revision int64, confirm bool) (any, error) {
	b, err := s.binding(ctx, scope)
	if err != nil {
		return nil, err
	}
	if b.Revision != revision {
		return nil, errors.New("task-board revision conflict")
	}
	if !confirm {
		return map[string]any{"binding": b, "confirmation_required": true}, nil
	}
	scope.Operation = "taskboard_clear"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = revision
	r, err := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, err
}

type cursor struct {
	Version  int    `json:"v"`
	Scope    string `json:"scope"`
	Team     int    `json:"team"`
	Provider string `json:"cursor"`
}

func cursorKey(profile string, b bus.TaskboardBinding) string {
	return digest([]any{profile, b.WorkspaceID, b.ProjectID, b.Revision, b.LinearTeamIDs, b.LinearProjectID})
}
func (s *Service) Issues(ctx context.Context, scope bus.Request, limit int, opaque string) (linearprovider.Page[linearprovider.Issue], error) {
	empty := linearprovider.Page[linearprovider.Issue]{}
	if limit < 1 || limit > 100 || len(opaque) > 8192 {
		return empty, errors.New("issue page requires limit 1–100 and bounded cursor")
	}
	b, err := s.binding(ctx, scope)
	if err != nil {
		return empty, err
	}
	cur := cursor{Version: 1, Scope: cursorKey(s.cfg.ProfileID, b)}
	if opaque != "" {
		raw, e := base64.RawURLEncoding.DecodeString(opaque)
		if e != nil || len(raw) > 4096 || json.Unmarshal(raw, &cur) != nil || cur.Version != 1 || cur.Scope != cursorKey(s.cfg.ProfileID, b) || cur.Team < 0 || cur.Team >= len(b.LinearTeamIDs) {
			return empty, errors.New("cursor does not belong to current profile and binding revision")
		}
	}
	_, p, done, err := s.open(ctx)
	if err != nil {
		return empty, err
	}
	defer done()
	if err = verifyBinding(ctx, p, b); err != nil {
		return empty, err
	}
	page, err := p.Issues(ctx, linearprovider.IssueQuery{TeamID: b.LinearTeamIDs[cur.Team], ProjectID: b.LinearProjectID, Limit: limit, Cursor: cur.Provider})
	if err != nil {
		return empty, err
	}
	if len(page.Items) > limit {
		return empty, errors.New("provider issue page exceeds requested bound")
	}
	if page.NextCursor == "" && !page.Complete {
		return empty, errors.New("Linear issue page is incomplete without a continuation cursor; cannot establish board exhaustion")
	}
	if page.NextCursor != "" && page.Complete {
		return empty, errors.New("Linear issue page has contradictory completeness and continuation metadata")
	}
	for _, issue := range page.Items {
		if err = checkIssue(issue, b); err != nil {
			return empty, err
		}
	}
	if err = s.unchanged(ctx, scope, b); err != nil {
		return empty, err
	}
	if page.NextCursor != "" {
		cur.Provider = page.NextCursor
	} else {
		cur.Team++
		cur.Provider = ""
	}
	page.NextCursor = ""
	page.Complete = cur.Team >= len(b.LinearTeamIDs)
	if !page.Complete {
		raw, _ := json.Marshal(cur)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
func checkIssue(issue linearprovider.Issue, b bus.TaskboardBinding) error {
	if !linearprovider.ValidID(issue.ID) || !contains(b.LinearTeamIDs, issue.TeamID) || (b.LinearProjectID != "" && issue.ProjectID != b.LinearProjectID) {
		return errors.New("issue is outside the selected task-board scope")
	}
	return nil
}
func (s *Service) Issue(ctx context.Context, scope bus.Request, id string) (linearprovider.Issue, error) {
	if !linearprovider.ValidID(id) {
		return linearprovider.Issue{}, errors.New("issue requires immutable UUID")
	}
	b, err := s.binding(ctx, scope)
	if err != nil {
		return linearprovider.Issue{}, err
	}
	_, p, done, err := s.open(ctx)
	if err != nil {
		return linearprovider.Issue{}, err
	}
	defer done()
	if err = verifyBinding(ctx, p, b); err != nil {
		return linearprovider.Issue{}, err
	}
	issue, err := p.GetIssue(ctx, id)
	if err != nil {
		return issue, err
	}
	if issue.ID != id {
		return linearprovider.Issue{}, errors.New("provider returned another issue")
	}
	if err = checkIssue(issue, b); err != nil {
		return linearprovider.Issue{}, err
	}
	if err = s.unchanged(ctx, scope, b); err != nil {
		return linearprovider.Issue{}, err
	}
	return issue, nil
}
func (s *Service) States(ctx context.Context, scope bus.Request, issueID string) ([]linearprovider.Status, error) {
	issue, err := s.Issue(ctx, scope, issueID)
	if err != nil {
		return nil, err
	}
	b, err := s.binding(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err = checkIssue(issue, b); err != nil {
		return nil, err
	}
	_, p, done, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if err = verifyBinding(ctx, p, b); err != nil {
		return nil, err
	}
	states, err := p.Statuses(ctx, issue.TeamID)
	if err != nil {
		return nil, err
	}
	for _, state := range states {
		if state.TeamID != issue.TeamID {
			return nil, errors.New("workflow state team is ambiguous")
		}
	}
	if err = s.unchanged(ctx, scope, b); err != nil {
		return nil, err
	}
	return states, nil
}

type WriteInput struct {
	Operation, OperationID, IssueID string
	Title, Description, StateID     *string
	Body                            string
}

func validText(s string, max int, empty bool) bool {
	return utf8.ValidString(s) && len(s) <= max && (empty || strings.TrimSpace(s) != "") && !strings.ContainsFunc(s, func(r rune) bool {
		return r == 0 || r == 0x1b || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t')
	})
}
func (s *Service) Write(ctx context.Context, scope bus.Request, in WriteInput) (any, error) {
	if err := ValidateWrite(in); err != nil {
		return nil, err
	}
	b, err := s.binding(ctx, scope)
	if err != nil {
		return nil, err
	}
	if s.cfg.Authority == "" {
		return nil, errors.New("canonical authority identity required for operation ledger")
	}
	access, err := s.checkWriteAccess(ctx, scope, b)
	if err != nil {
		return nil, err
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	ledgerPayload := struct {
		Profile      string
		Binding      bus.TaskboardBinding
		Actor        string
		Authority    string
		InstanceHash string
		Input        WriteInput
	}{s.cfg.ProfileID, b, access.SessionID, s.cfg.Authority, digest(scope.Args["instance"]), in}
	if receipt, exists, e := (ledger{s.cfg.LedgerDirectory, s.cfg.Now}).lookup(namespace, in.OperationID, ledgerPayload); exists || e != nil {
		if e != nil {
			return receipt, e
		}
		return s.postReceipt(ctx, scope, b, receipt)
	}
	profile, p, done, err := s.openForWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if profile.Access != linearconn.Write {
		return nil, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	if err = verifyBinding(ctx, p, b); err != nil {
		return nil, err
	}
	team := b.LinearDefaultTeamID
	if in.Operation != "create" {
		issue, e := p.GetIssue(ctx, in.IssueID)
		if e != nil {
			return nil, e
		}
		if issue.ID != in.IssueID {
			return nil, errors.New("issue identity changed")
		}
		if err = checkIssue(issue, b); err != nil {
			return nil, err
		}
		team = issue.TeamID
	}
	if in.StateID != nil {
		states, e := p.Statuses(ctx, team)
		if e != nil {
			return nil, e
		}
		found := false
		for _, state := range states {
			if state.ID == *in.StateID && state.TeamID == team {
				found = true
			}
		}
		if !found {
			return nil, errors.New("state is not verified for the issue team")
		}
	}
	if err = s.unchanged(ctx, scope, b); err != nil {
		return nil, err
	}

	return (ledger{s.cfg.LedgerDirectory, s.cfg.Now}).write(namespace, in.OperationID, ledgerPayload, func() (any, error) {
		// Last revision check occurs after obtaining the private per-operation lock.
		if e := s.unchanged(ctx, scope, b); e != nil {
			return nil, authorityFailure(e)
		}
		fresh, e := s.checkWriteAccess(ctx, scope, b)
		if e != nil {
			return nil, authorityFailure(e)
		}
		if fresh.SessionID != access.SessionID {
			return nil, refusedBeforeDispatch(errActorChanged)
		}
		switch in.Operation {
		case "create":
			description := ""
			if in.Description != nil {
				description = *in.Description
			}
			state := ""
			if in.StateID != nil {
				state = *in.StateID
			}
			issue, e := p.Create(ctx, linearprovider.CreateInput{TeamID: team, ProjectID: b.LinearProjectID, Title: *in.Title, Description: description, StateID: state})
			if e != nil {
				return nil, s.postFailure(ctx, scope, b, e)
			}
			if checkIssue(issue, b) != nil || issue.TeamID != team {
				return nil, errors.New("created issue receipt scope mismatch")
			}
			return s.postReceipt(ctx, scope, b, map[string]any{"id": issue.ID, "identifier": issue.Identifier, "team_id": issue.TeamID, "project_id": issue.ProjectID})
		case "update":
			issue, e := p.Update(ctx, linearprovider.UpdateInput{ID: in.IssueID, Title: in.Title, Description: in.Description, StateID: in.StateID})
			if e != nil {
				return nil, s.postFailure(ctx, scope, b, e)
			}
			if issue.ID != in.IssueID || checkIssue(issue, b) != nil {
				return nil, errors.New("updated issue receipt scope mismatch")
			}
			return s.postReceipt(ctx, scope, b, map[string]any{"id": issue.ID, "identifier": issue.Identifier, "team_id": issue.TeamID, "project_id": issue.ProjectID})
		default:
			receipt, e := p.Comment(ctx, in.IssueID, in.Body)
			if e != nil {
				return nil, s.postFailure(ctx, scope, b, e)
			}
			if receipt.IssueID != in.IssueID || !linearprovider.ValidID(receipt.ID) {
				return nil, errors.New("comment receipt scope mismatch")
			}
			return s.postReceipt(ctx, scope, b, receipt)
		}
	})
}

func (s *Service) postFailure(ctx context.Context, scope bus.Request, b bus.TaskboardBinding, providerError error) error {
	var changed *ReceiptError
	if err := s.unchanged(ctx, scope, b); errors.As(err, &changed) {
		return errors.Join(providerError, errBindingChangedAfterProviderError)
	}
	return providerError
}

// Execute is the curated adapter boundary shared by the pinned local MCP tools.
func (s *Service) Execute(ctx context.Context, scope bus.Request, operation string, args map[string]any) (_ any, err error) {
	defer func() { s.settle(err) }()
	allowed := map[string][]string{"binding": {}, "context": {}, "issues": {"limit", "cursor"}, "issue": {"id"}, "create": {"title", "description", "state_id", "operation_id"}, "update": {"id", "title", "description", "state_id", "operation_id"}, "comment": {"id", "body", "operation_id"}}
	keys, ok := allowed[operation]
	if !ok {
		return nil, errors.New("unsupported task-board operation")
	}
	for key, value := range args {
		if !contains(keys, key) || value == nil {
			return nil, errors.New("invalid task-board argument")
		}
		if key != "limit" {
			if _, ok := value.(string); !ok {
				return nil, errors.New("task-board fields must be strings")
			}
		}
	}
	if v, ok := args["limit"]; ok {
		switch n := v.(type) {
		case int:
			if n < 1 || n > 100 {
				return nil, errors.New("invalid limit")
			}
		case float64:
			if !(n >= 1 && n <= 100) || n != float64(int(n)) {
				return nil, errors.New("invalid limit")
			}
		default:
			return nil, errors.New("invalid limit")
		}
	}

	switch operation {
	case "binding":
		return s.Binding(ctx, scope)
	case "context":
		return s.Context(ctx, scope)
	case "issues":
		limit := 50
		if v, ok := args["limit"].(int); ok {
			limit = v
		}
		if v, ok := args["limit"].(float64); ok {
			if v != float64(int(v)) {
				return nil, errors.New("invalid limit")
			}
			limit = int(v)
		}
		cursor, _ := args["cursor"].(string)
		return s.Issues(ctx, scope, limit, cursor)
	case "issue":
		id, _ := args["id"].(string)
		return s.Issue(ctx, scope, id)
	case "create", "update", "comment":
		in := WriteInput{Operation: operation}
		in.OperationID, _ = args["operation_id"].(string)
		in.IssueID, _ = args["id"].(string)
		in.Body, _ = args["body"].(string)
		for key, target := range map[string]**string{"title": &in.Title, "description": &in.Description, "state_id": &in.StateID} {
			if v, ok := args[key]; ok {
				text, valid := v.(string)
				if !valid {
					return nil, errors.New("write fields must be strings")
				}
				*target = &text
			}
		}
		return s.Write(ctx, scope, in)
	default:
		return nil, errors.New("unsupported task-board operation")
	}
}
func SortedTeams(ids []string) []string {
	copy := append([]string(nil), ids...)
	sort.Strings(copy)
	return copy
}

// ValidateWrite checks all local options before authority, credentials or provider IO.
func ValidateWrite(in WriteInput) error {
	if !operationPattern.MatchString(in.OperationID) {
		return errors.New("write requires operation_id")
	}
	if in.Title != nil && !validText(*in.Title, 512, false) {
		return errors.New("invalid issue title")
	}
	if in.Description != nil && !validText(*in.Description, 65536, true) {
		return errors.New("invalid issue description")
	}
	if in.StateID != nil && !linearprovider.ValidID(*in.StateID) {
		return errors.New("state requires UUID")
	}
	switch in.Operation {
	case "create":
		if in.Title == nil || in.IssueID != "" || in.Body != "" {
			return errors.New("create requires title and default bound team")
		}
	case "update":
		if in.Body != "" || !linearprovider.ValidID(in.IssueID) || (in.Title == nil && in.Description == nil && in.StateID == nil) {
			return errors.New("update requires issue UUID and explicit changed fields")
		}
	case "comment":
		if !linearprovider.ValidID(in.IssueID) || !validText(in.Body, 65536, false) || in.Title != nil || in.Description != nil || in.StateID != nil {
			return errors.New("comment requires scoped issue UUID and bounded body")
		}
	default:
		return errors.New("unsupported task-board write")
	}

	return nil
}
