package doctor

import (
	"context"
	"encoding/json"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/playbook"
)

// checkPlaybook tells the project's coordinator session whether its project
// keeps a playbook (docs/COORDINATOR.md) on the default branch, which is the
// only copy it follows. Any other session gets no check. It warns, and never
// fails: a project may run without a playbook.
func checkPlaybook(ctx context.Context, read func() playbook.State, config Config, executor Executor, add func(Check)) {
	if executor == nil || config.Session == "" || config.Instance == "" {
		return
	}
	// The overview, a pure read, names the coordinator; whoami stays the
	// agent's own.
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, err := executor.Execute(bounded, bus.Request{Operation: "project_overview", Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: map[string]any{"instance": config.Instance}})
	cancel()
	var overview struct {
		Coordinator *struct {
			Name string `json:"name"`
		} `json:"coordinator"`
	}
	raw, _ := json.Marshal(result.Data)
	if err != nil || json.Unmarshal(raw, &overview) != nil || overview.Coordinator == nil || overview.Coordinator.Name != config.Session {
		return
	}
	s := read()
	c := Check{ID: "coordinator.playbook", Title: "Coordinator playbook", Status: Pass, Path: s.Root}
	const copyIt = "Copy ClawBridge's docs/guides/coordinator-playbook-template.md to docs/COORDINATOR.md, fill it in, and merge it."
	switch {
	case s.Root == "":
		c.Status = Warn
		c.Message = "Not in a git checkout, so the project's playbook can't be checked."
		c.Action = "Run doctor in the project's repository."
	case s.Ref == "":
		c.Status = Warn
		c.Message = "The project's default branch is unknown: " + s.Err.Error() + "."
		c.Action = "Run git remote set-head origin --auto in the checkout."
	case s.Merged:
		c.Message = "This session is the project's coordinator, and " + playbook.Path + " is on " + s.Ref + "."
	case s.Local:
		c.Status = Warn
		c.Message = playbook.Path + " is in this checkout but not on " + s.Ref + ", so the coordinator doesn't follow it yet."
		c.Action = "Merge it to the default branch (git fetch first: this reads your copy of " + s.Ref + ")."
	default:
		c.Status = Warn
		c.Message = "This session is the project's coordinator, and the project has no " + playbook.Path + " on " + s.Ref + ": it runs on the skill's defaults."
		c.Action = copyIt + " If it is merged already, git fetch first: this reads your copy of " + s.Ref + "."
	}
	add(c)
}
