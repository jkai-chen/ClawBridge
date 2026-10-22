package cli

import (
	"context"
	"encoding/json"
	"errors"

	"clawbridge/internal/bus"
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
		_, err := s.manager.Connect(ctx, linearconn.ConnectOptions{Name: r.Name, Access: r.Access, PresentURL: func(url string) error { return openLinearBrowser(ctx, url) }})
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
		result, err := service.LinkV2(ctx, s.scope, input)
		if err != nil {
			return data, err
		}
		if r.Operation == "link" {
			binding, e := bus.ParseTaskboardBindingV2(result)
			data.BindingV2, err = &binding, e
		} else {
			preview, ok := result.(linearboard.LinkPreviewV2)
			if !ok || preview.Binding.Revision != input.ExpectedRevision {
				return data, errors.New("task-board preview revision changed; refresh and preview again")
			}
			data.BindingV2 = &preview.Binding
			raw, _ := json.MarshalIndent(preview, "", "  ")
			data.Preview = string(raw)
		}
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
		issue, err := service.IssueV2(ctx, s.scope, r.IssueID)
		if err == nil {
			data.IssueV2 = &issue
		}
		return data, err
	default:
		return data, errors.New("unsupported native Linear action")
	}
}
