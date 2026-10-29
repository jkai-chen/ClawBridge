package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// checkTeamRoles reports the project's team roles: how many, a team that
// left the binding, and, with a Linear connection, a mapped person who is
// no longer an active, non-guest user of the workspace (T2'). It warns and
// never fails; no consumer uses a role that fails these.
func checkTeamRoles(ctx context.Context, executor Executor, wid, pid string, users func(context.Context, string, string) ([]linearprovider.WorkspaceUser, error), add func(Check)) {
	c := Check{ID: "linear.team_roles", Title: "Team roles", Status: Pass}
	r, err := executor.Execute(ctx, bus.Request{Operation: "team_roles_get", Workspace: wid, Project: pid, Args: map[string]any{}})
	var view struct {
		Revision int64 `json:"revision"`
		Teams    []struct {
			TeamID    string `json:"team_id"`
			TeamBound bool   `json:"team_bound"`
		} `json:"teams"`
		People []bus.TeamRolePerson `json:"people"`
	}
	raw, _ := json.Marshal(r.Data)
	switch {
	case err != nil || json.Unmarshal(raw, &view) != nil:
		c.Status = Warn
		c.Message = "The team roles could not be read (" + safeCode(err) + ")."
		c.Action = "Check project access; `clawbridge linear roles show` shows them."
		add(c)
		return
	case view.Revision == 0:
		c.Message = "No team roles are set."
		add(c)
		return
	}
	notes := []string{fmt.Sprintf("Revision %d: %d team(s), %d people.", view.Revision, len(view.Teams), len(view.People))}
	unbound := 0
	for _, t := range view.Teams {
		if !t.TeamBound {
			unbound++
		}
	}
	if unbound > 0 {
		c.Status = Warn
		notes = append(notes, fmt.Sprintf("%d mapped team(s) left the binding; their roles are ignored.", unbound))
		c.Action = "The project owner updates them with `clawbridge linear roles set`."
	}
	if users != nil && len(view.People) > 0 {
		bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
		list, e := users(bounded, wid, pid)
		cancel()
		if e != nil {
			c.Status = Warn
			notes = append(notes, "The Linear workspace's users could not be read, so no one's status was checked ("+safeCode(e)+").")
		} else {
			eligible := map[string]bool{}
			for _, u := range list {
				eligible[strings.ToLower(u.ID)] = u.Eligible()
			}
			var lost []string
			for _, p := range view.People {
				if !eligible[strings.ToLower(p.LinearUserID)] {
					lost = append(lost, p.LinearUserID)
				}
			}
			if len(lost) > 0 {
				c.Status = Warn
				notes = append(notes, fmt.Sprintf("%d mapped person(s) are no longer active, non-guest users of the Linear workspace: %s. No consumer uses their roles.", len(lost), strings.Join(lost, ", ")))
				c.Action = "The project owner removes them with `clawbridge linear roles person USER_ID --remove`."
			}
		}
	}
	c.Message = strings.Join(notes, " ")
	add(c)
}

// checkTeamRolesDoc reports the generated "Team roles" document (design
// §4, §13) for the project owner: edited in Linear (the next publish
// overwrites it), behind the mapping, archived or unreadable, and a
// document a --recreate left for archiving by hand. Silent for anyone else
// and when nothing was published. It warns and never fails.
func checkTeamRolesDoc(ctx context.Context, wid, pid string, read func(context.Context, string, string) (RolesDocState, error), add func(Check)) {
	if read == nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	d, err := read(bounded, wid, pid)
	cancel()
	c := Check{ID: "linear.team_roles_doc", Title: "Team roles document", Status: Pass}
	switch {
	case err != nil && safeCode(err) == "forbidden":
		return
	case err != nil:
		c.Status, c.Message = Warn, "The team roles document's record could not be read ("+safeCode(err)+")."
		add(c)
		return
	case !d.Published:
		return
	}
	notes := []string{fmt.Sprintf("Published from revision %d; the roles are at revision %d.", d.PublishedRevision, d.Revision)}
	publish := "Run `clawbridge linear roles publish --confirm`."
	switch {
	case d.Pending:
		c.Status = Warn
		notes = append(notes, "A publish was interrupted before it was recorded.")
		c.Action = publish
	case d.Unreadable:
		c.Status = Warn
		notes = append(notes, "The document could not be read in Linear.")
		c.Action = "Check your Linear connection; if the document was deleted, run `clawbridge linear roles publish --recreate --confirm`."
	case d.Archived:
		c.Status = Warn
		notes = append(notes, "The document is archived in Linear.")
		c.Action = "Run `clawbridge linear roles publish --recreate --confirm` to create it again."
	case d.Edited:
		c.Status = Warn
		notes = append(notes, "Team roles was edited in Linear; the next publish overwrites it. Change roles with clawbridge linear roles.")
		c.Action = publish
	case d.PublishedRevision != d.Revision:
		c.Status = Warn
		notes = append(notes, "The document is out of date.")
		c.Action = publish
	}
	if d.Replaced != "" {
		c.Status = Warn
		notes = append(notes, "An earlier team roles document, "+d.Replaced+", was replaced and is still in Linear.")
		if c.Action == "" {
			c.Action = "Archive it in Linear by hand."
		} else {
			c.Action += " Archive the earlier document in Linear by hand."
		}
	}
	c.Message = strings.Join(notes, " ")
	add(c)
}
