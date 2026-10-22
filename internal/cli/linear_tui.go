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
		b, err := service.Binding(ctx, s.scope)
		data.Binding = b
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
		result, err := service.Link(ctx, s.scope, r.Link)
		if err != nil {
			return data, err
		}
		if r.Operation == "link" {
			data.Binding, err = bus.ParseTaskboardBinding(result)
		} else {
			preview, ok := result.(linearboard.LinkPreview)
			if !ok || preview.Binding.Revision != r.Link.ExpectedRevision {
				return data, errors.New("task-board preview revision changed; refresh and preview again")
			}
			data.Binding = preview.Binding
			raw, _ := json.MarshalIndent(preview, "", "  ")
			data.Preview = string(raw)
		}
		return data, err
	case "unlink-preview", "unlink":
		result, err := service.Unlink(ctx, s.scope, r.ExpectedRevision, r.Confirm)
		if err != nil {
			return data, err
		}
		if r.Operation == "unlink" {
			data.Binding, err = bus.ParseTaskboardBinding(result)
		} else {
			raw, _ := json.MarshalIndent(result, "", "  ")
			data.Preview = string(raw)
		}
		return data, err
	case "issues":
		page, err := service.Issues(ctx, s.scope, r.Limit, r.Cursor)
		data.Issues = page
		return data, err
	case "issue":
		issue, err := service.Issue(ctx, s.scope, r.IssueID)
		if err == nil {
			data.Issue = &issue
		}
		return data, err
	default:
		return data, errors.New("unsupported native Linear action")
	}
}
