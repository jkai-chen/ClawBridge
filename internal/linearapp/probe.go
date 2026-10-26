package linearapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// projectUpdateProbe is the only mutation the app ever sends outside the
// board write path: a project update for a project that does not exist, so
// Linear's answer tells whether the app's scopes allow project updates
// without anything being created.
const projectUpdateProbe = `mutation ProbeProjectUpdate($input: ProjectUpdateCreateInput!) { projectUpdateCreate(input: $input) { success projectUpdate { id } } }`

// projectShapeProbe reads what project health would need from one project.
const projectShapeProbe = `query ProbeProject($id: String!) { project(id: $id) { targetDate startDate createdAt health healthUpdatedAt lastUpdate { createdAt health user { id } } } }`

// ScopeProbe is Linear's answer to the project-update probe, reduced to a
// class and Linear's own error type names: never a message, ID or token.
type ScopeProbe struct {
	// Class is created, scope, not_found, validation, token_rejected,
	// unavailable or other.
	Class  string   `json:"class"`
	Status int      `json:"http_status"`
	Types  []string `json:"error_types,omitempty"`
}

// errorType keeps an error type name only when it is a plain word list.
var errorType = regexp.MustCompile(`^[A-Za-z][A-Za-z _-]{0,39}$`)

// ProbeProjectUpdateScope sends projectUpdateCreate once for a random,
// non-existent project with body "probe" and health onTrack. It is never
// retried and never re-mints: a refused token is reported, not replaced.
func (a *App) ProbeProjectUpdateScope(ctx context.Context) (ScopeProbe, error) {
	token, _, err := a.Token(ctx)
	if err != nil {
		return ScopeProbe{}, err
	}
	body, _ := json.Marshal(map[string]any{"query": projectUpdateProbe, "variables": map[string]any{"input": map[string]any{"projectId": randomUUID(), "body": "probe", "health": "onTrack"}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, bytes.NewReader(body))
	if err != nil {
		return ScopeProbe{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := a.cfg.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ScopeProbe{}, ctx.Err()
		}
		return ScopeProbe{Class: "unavailable"}, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	return classifyProbe(res.StatusCode, raw), nil
}

func classifyProbe(status int, raw []byte) ScopeProbe {
	out := ScopeProbe{Class: "other", Status: status}
	if status == http.StatusUnauthorized {
		out.Class = "token_rejected"
		return out
	}
	var envelope struct {
		Data struct {
			Create *struct {
				Success       bool `json:"success"`
				ProjectUpdate *struct {
					ID string `json:"id"`
				} `json:"projectUpdate"`
			} `json:"projectUpdateCreate"`
		} `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		if status >= 500 {
			out.Class = "unavailable"
		}
		return out
	}
	if c := envelope.Data.Create; c != nil && (c.Success || (c.ProjectUpdate != nil && c.ProjectUpdate.ID != "")) {
		out.Class = "created"
		return out
	}
	words := []string{}
	seen := map[string]bool{}
	for _, e := range envelope.Errors {
		words = append(words, strings.ToLower(e.Message))
		for _, k := range []string{"type", "code"} {
			if v, ok := e.Extensions[k].(string); ok {
				words = append(words, strings.ToLower(v))
				if errorType.MatchString(v) && !seen[v] {
					seen[v] = true
					out.Types = append(out.Types, v)
				}
			}
		}
	}
	sort.Strings(out.Types)
	text := strings.Join(words, " ")
	switch {
	case len(envelope.Errors) == 0:
	case strings.Contains(text, "scope") || strings.Contains(text, "forbidden") || strings.Contains(text, "permission") || strings.Contains(text, "not authorized") || strings.Contains(text, "unauthorized"):
		out.Class = "scope"
	case strings.Contains(text, "not found") || strings.Contains(text, "does not exist"):
		out.Class = "not_found"
	case strings.Contains(text, "invalid") || strings.Contains(text, "validation") || strings.Contains(text, "argument"):
		out.Class = "validation"
	}
	return out
}

// Tools lists the MCP tools Linear offers the app, with their input field
// names. Opening the connection is Linear's initialize and tools/list only.
func (a *App) Tools(ctx context.Context) (map[string][]string, error) {
	p, err := a.Provider(ctx, false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.Tools(), nil
}

// ProjectShape is what project health would read from one project, as
// value kinds only: no date, health or user ID.
type ProjectShape struct {
	Fields map[string]string `json:"fields"`
	// LastUpdateByApp is "app", "other" or "none".
	LastUpdateByApp string `json:"last_update_by"`
}

// ProbeProjectShape reads one project's health inputs as the app.
func (a *App) ProbeProjectShape(ctx context.Context, project string) (ProjectShape, error) {
	if !validUUID(project) {
		return ProjectShape{}, fail("invalid")
	}
	var out struct {
		Project map[string]any `json:"project"`
	}
	if err := a.GraphQL(ctx, projectShapeProbe, map[string]any{"id": project}, &out); err != nil {
		return ProjectShape{}, err
	}
	if out.Project == nil {
		return ProjectShape{}, fail("not_found")
	}
	_, id, err := a.Token(ctx)
	if err != nil {
		return ProjectShape{}, err
	}
	shape := ProjectShape{Fields: map[string]string{}, LastUpdateByApp: "none"}
	for k, v := range out.Project {
		shape.Fields[k] = valueKind(v)
	}
	if last, ok := out.Project["lastUpdate"].(map[string]any); ok {
		for k, v := range last {
			shape.Fields["lastUpdate."+k] = valueKind(v)
		}
		user, _ := last["user"].(map[string]any)
		if uid, _ := user["id"].(string); strings.EqualFold(uid, id.AppUserID) {
			shape.LastUpdateByApp = "app"
		} else {
			shape.LastUpdateByApp = "other"
		}
	}
	return shape, nil
}

func valueKind(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if t == "" {
			return "string(empty)"
		}
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case []any:
		return fmt.Sprintf("array[%d]", len(t))
	case map[string]any:
		return "object"
	}
	return "other"
}

// randomUUID is a version-4 UUID no Linear project has.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
