package guibackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/doctor"
	"clawbridge/internal/selfupdate"
)

// Settings → Updates, read-only (GUI-DESIGN §8.1, §12.5). The page has no
// "GUI app" row: the UI is inside the client binary.

// SkillRow is one agent's installed skill.
type SkillRow struct {
	Agent            string `json:"agent"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
	Current          bool   `json:"current"`
}

// MCPRow is one configured agent's MCP entry as doctor checked it. An entry
// records no version, so the page shows the check's outcome.
type MCPRow struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// CheckCacheTTL is how long a good updates.check answer is reused.
const CheckCacheTTL = 60 * time.Second

// UpdatesConfig is what the update reads need; each is a seam for tests.
type UpdatesConfig struct {
	// ClientVersion is this running binary's version.
	ClientVersion string
	// DiskVersion is the version of the binary file on disk now ("" when it
	// can't be read); after an update it is newer than the running one.
	DiskVersion func(context.Context) (string, error)
	// Skill reads the installed skill versions.
	Skill func() ([]SkillRow, error)
	// Doctor supplies the MCP entry checks.
	Doctor func(context.Context) (doctor.Report, error)
	// Check is `self-update --check`: the only call that reaches the network.
	Check func(context.Context) (selfupdate.CheckResult, error)
	// AgentMarker names the marker an agent's environment carries, or "".
	AgentMarker func() string
	Now         func() time.Time
}

// UpdatesStatus is updates.status: versions only, no network.
func UpdatesStatus(cfg UpdatesConfig) Method {
	return Method{Class: Read, Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		client := map[string]any{"version": cfg.ClientVersion, "on_disk": "", "restart_needed": false}
		if cfg.DiskVersion != nil {
			if v, err := cfg.DiskVersion(ctx); err == nil && v != "" {
				client["on_disk"] = v
				client["restart_needed"] = selfupdate.Newer(v, cfg.ClientVersion)
			}
		}
		skills := []SkillRow{}
		var skillNote string
		if cfg.Skill != nil {
			rows, err := cfg.Skill()
			if err != nil {
				skillNote = oneLine(err)
			} else if rows != nil {
				skills = rows
			}
		}
		mcp := []MCPRow{}
		var mcpNote string
		if cfg.Doctor != nil {
			rep, err := cfg.Doctor(ctx)
			if err != nil {
				mcpNote = oneLine(err)
			}
			for _, ch := range rep.Checks {
				if !strings.HasPrefix(ch.ID, "mcp.") || ch.ID == "mcp.entries" {
					continue
				}
				parts := strings.SplitN(strings.TrimPrefix(ch.ID, "mcp."), ".", 2)
				row := MCPRow{Agent: parts[0], Status: ch.Status, Message: ch.Message}
				if len(parts) == 2 {
					row.Name = parts[1]
				}
				mcp = append(mcp, row)
			}
		}
		return map[string]any{
			"client":     client,
			"skill":      skills,
			"skill_note": skillNote,
			"mcp":        mcp,
			"mcp_note":   mcpNote,
			// FR24's automatic updates and their state file are not in this
			// build, so there is no last check or schedule to show.
			"auto": map[string]any{"available": false},
		}, nil
	}}
}

// UpdatesCheck is updates.check: the same logic as `self-update --check`,
// run only when a person clicks Check now. Refused under an agent marker.
//
// GitHub's unauthenticated limit is 60 requests an hour per address, shared
// with self-update, so a good answer is kept for CheckCacheTTL: a second
// click inside it returns the same answer, with the time it was checked.
func UpdatesCheck(cfg UpdatesConfig) Method {
	var (
		mu     sync.Mutex
		cached map[string]any
		at     time.Time
	)
	clock := time.Now
	if cfg.Now != nil {
		clock = cfg.Now
	}
	return Method{Class: Read, Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		if cfg.AgentMarker != nil {
			if m := cfg.AgentMarker(); m != "" {
				return nil, &Error{Code: CodeMethod, Message: "checking for updates is a person's action, and " + m + " says an agent runs this"}
			}
		}
		if cfg.Check == nil {
			return nil, errors.New("update checks are not available")
		}
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && clock().Sub(at) < CheckCacheTTL {
			out := map[string]any{}
			for k, v := range cached {
				out[k] = v
			}
			out["cached"] = true
			return out, nil
		}
		r, err := cfg.Check(ctx)
		if err != nil {
			return nil, err
		}
		now := clock
		out := map[string]any{
			"current":          r.Current,
			"target":           r.Target,
			"action":           string(r.Action),
			"update_available": r.Action == selfupdate.Update && !r.Downgrade,
			"manual_required":  r.ManualRequired,
			"manual_text":      r.ManualText,
			"hold":             r.Hold,
			"checked_at":       now().UTC().Format(time.RFC3339),
			"cached":           false,
		}
		cached, at = out, now()
		return out, nil
	}}
}
