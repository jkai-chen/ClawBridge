package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"clawbridge/internal/bus"
	"clawbridge/internal/docseed"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/tui"
)

type linearTUISource struct {
	initErr   error
	directory string
	authority string
	manager   *linearconn.Manager
	executor  linearboard.Executor
	scope     bus.Request
}

func newLinearTUISource(c Config, executor linearboard.Executor) (tui.LinearSource, error) {
	directory, err := linearDirectory()
	if err != nil {
		return &linearTUISource{initErr: err}, nil
	}
	scope := bus.Request{Workspace: c.Workspace, Project: c.Project, Session: c.Session, Args: map[string]any{}}
	if c.Instance != "" {
		scope.Args["instance"] = c.Instance
	}
	return &linearTUISource{directory: directory, manager: newLinearManager(directory), executor: executor, scope: scope, authority: linearAuthority(c)}, nil
}

// tuiClosesAllowed refuses, before anything is sent, a confirmed link that
// changes who closes from a TUI bound to a session, or run under an
// agent's environment: who closes is a person's decision (A3 §10.8).
func tuiClosesAllowed(scope bus.Request, in linearboard.LinkInputV2) error {
	if !in.Confirm || len(in.Closes) == 0 {
		return nil
	}
	if scope.Session != "" {
		return errors.New("who closes is never set with a session; open the TUI without --session to change it. Nothing was changed")
	}
	for _, who := range in.Closes {
		if who != linearboard.ClosesAgent {
			continue
		}
		if marker := agentMarker(closesEnv()); marker != "" {
			return fmt.Errorf("letting agents close issues is a person's decision, and %s says an agent runs this TUI; nothing was changed", marker)
		}
	}
	return nil
}

// linkResult reads a confirmed link's answer: the binding, plus what it
// did about who closes when it set any. The binding is parsed without the
// outcomes, which aren't binding fields.
func linkResult(result any) (bus.TaskboardBindingV2, []linearboard.ClosesOutcome, error) {
	var outcomes []linearboard.ClosesOutcome
	if m, ok := result.(map[string]any); ok {
		if o, ok := m["closes"].([]linearboard.ClosesOutcome); ok {
			outcomes = o
			binding := map[string]any{}
			for k, v := range m {
				if k != "closes" {
					binding[k] = v
				}
			}
			result = binding
		}
	}
	b, err := bus.ParseTaskboardBindingV2(result)
	return b, outcomes, err
}

func (s *linearTUISource) Do(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
	var data tui.LinearData
	if s.initErr != nil {
		return data, s.initErr
	}
	service := newLinearService(s.directory, r.ProfileID, s.manager, s.executor, s.authority)
	loadProfiles := func() error {
		profiles, err := s.manager.Profiles()
		if err != nil {
			return err
		}
		data.Profiles = profiles
		b, err := service.BindingV2(ctx, s.scope)
		data.BindingV2 = &b
		return err
	}
	switch r.Operation {
	case "profiles":
		return data, loadProfiles()
	case "connect":
		_, err := s.manager.Connect(ctx, linearconn.ConnectOptions{Name: r.Name, Access: r.Access, PresentURL: func(url string) error { _, e := openLinearBrowser(ctx, url); return e }})
		if err != nil {
			return data, err
		}
		return data, loadProfiles()
	case "logout":
		if err := s.manager.Logout(ctx, r.ProfileID); err != nil {
			return data, err
		}
		return data, loadProfiles()
	case "teams", "projects":
		profile, err := s.manager.Status(ctx, r.ProfileID)
		if err != nil {
			return data, err
		}
		h, err := s.manager.HTTPClient(ctx, r.ProfileID)
		if err != nil {
			return data, err
		}
		p, err := openLinearProvider(ctx, h, profile.Access == linearconn.Read)
		if err != nil {
			return data, err
		}
		defer p.Close()
		if r.Operation == "teams" {
			data.Teams, err = p.Teams(ctx, linearprovider.PageOptions{Limit: r.Limit, Cursor: r.Cursor})
		} else {
			data.Projects, err = p.Projects(ctx, linearprovider.PageOptions{Limit: r.Limit, Cursor: r.Cursor})
		}
		return data, err
	case "preview", "link":
		input := linearboard.LinkInputV2{TeamIDs: r.Link.TeamIDs, DefaultTeamID: r.Link.DefaultTeamID, ProjectIDs: []string{}, ExpectedRevision: r.Link.ExpectedRevision, TeamBoard: r.Link.TeamBoard, Confirm: r.Link.Confirm}
		if r.Link.ProjectID != "" {
			input.ProjectIDs = []string{r.Link.ProjectID}
		}
		if r.LinkV2 != nil {
			input = *r.LinkV2
		}
		if err := tuiClosesAllowed(s.scope, input); err != nil {
			return data, err
		}
		result, err := service.LinkV2(ctx, s.scope, input)
		if err != nil {
			return data, err
		}
		if r.Operation == "link" {
			binding, outcomes, e := linkResult(result)
			data.BindingV2, data.ClosesOutcomes, err = &binding, outcomes, e
		} else {
			preview, ok := result.(linearboard.LinkPreviewV2)
			if !ok || preview.Binding.Revision != input.ExpectedRevision {
				return data, errors.New("task-board preview revision changed; refresh and preview again")
			}
			data.BindingV2 = &preview.Binding
			data.Closes, data.MinimalPolicy = preview.Closes, preview.MinimalPolicy
			raw, _ := json.MarshalIndent(preview, "", "  ")
			data.Preview = string(raw)
		}
		return data, err
	case "names-refresh":
		_, err := service.RefreshNamesV2(ctx, s.scope)
		return data, err
	case "unlink-preview", "unlink":
		result, err := service.UnlinkV2(ctx, s.scope, r.ExpectedRevision, r.Confirm)
		if err != nil {
			return data, err
		}
		if r.Operation == "unlink" {
			binding, e := bus.ParseTaskboardBindingV2(result)
			data.BindingV2, err = &binding, e
		} else {
			raw, _ := json.MarshalIndent(result, "", "  ")
			data.Preview = string(raw)
		}
		return data, err
	case "issues":
		page, err := service.IssuesV2(ctx, s.scope, linearboard.IssuesInputV2{Limit: r.Limit, Cursor: r.Cursor})
		data.IssuesV2 = &page
		return data, err
	case "issue":
		if r.ProfileID == "" {
			// Opened from a session's issue (i) before a connection was
			// chosen.
			var err error
			if service, err = s.onlyProfile(); err != nil {
				return data, err
			}
		}
		issue, err := service.IssueV2(ctx, s.scope, r.IssueID)
		if err == nil {
			data.IssueV2 = &issue
		}
		return data, err
	case "intake":
		if r.ProfileID == "" {
			// Opened from Overview before a connection was chosen.
			var err error
			if service, err = s.onlyProfile(); err != nil {
				return data, err
			}
		}
		return data, s.intake(ctx, service, &data)
	default:
		return data, errors.New("unsupported native Linear action")
	}
}

// onlyProfile is the service on the viewer's only Linear connection, as
// the CLI's lookup picks it; with none or several, the Linear screen asks.
func (s *linearTUISource) onlyProfile() (*linearboard.Service, error) {
	profiles, err := s.manager.Profiles()
	if err != nil {
		return nil, err
	}
	if len(profiles) != 1 {
		return nil, errors.New("choose your Linear connection on the Linear screen first")
	}
	return newLinearService(s.directory, profiles[0].ID, s.manager, s.executor, s.authority), nil
}

// intakePages bounds each team's reads for the Intake view, as the
// watcher bounds its own.
const intakePages = 5

// intake reads, through the viewer's own connection, the bound teams'
// members (names for the trusted list and the picker) and each team's
// needs-agent labels by the watcher's rule: the exact name, live, not a
// group, exactly one.
func (s *linearTUISource) intake(ctx context.Context, service *linearboard.Service, data *tui.LinearData) error {
	binding, err := service.BindingV2(ctx, s.scope)
	if err != nil {
		return err
	}
	if !binding.Configured {
		return errors.New("this project has no Linear board linked")
	}
	for _, team := range binding.LinearTeamIDs {
		cursor := ""
		for page := 0; page < intakePages; page++ {
			members, e := service.MembersV2(ctx, s.scope, team, 100, cursor)
			if e != nil {
				return e
			}
			data.Members = append(data.Members, members.Items...)
			if cursor = members.NextCursor; members.Complete || cursor == "" {
				break
			}
		}
		check := tui.IntakeLabel{TeamID: team}
		cursor = ""
		for page := 0; page < intakePages; page++ {
			labels, e := service.LabelsV2(ctx, s.scope, team, 100, cursor)
			if e != nil {
				return e
			}
			check.Found += needsAgentLabels(labels.Items)
			if cursor = labels.NextCursor; cursor == "" {
				check.Complete = true
				break
			}
		}
		data.IntakeLabels = append(data.IntakeLabels, check)
	}
	return nil
}

// needsAgentLabels counts the labels S1 would take: named exactly
// needs-agent, live, not a group (the watcher's rule).
func needsAgentLabels(labels []linearprovider.Label) int {
	n := 0
	for _, l := range labels {
		if l.Name == docseed.NeedsAgentLabel && !l.Archived && !l.Group {
			n++
		}
	}
	return n
}
