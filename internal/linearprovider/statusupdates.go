package linearprovider

import (
	"context"
	"strings"
)

// Project status updates through Linear's MCP tools save_status_update,
// get_status_updates and delete_status_update (probe P7, 26 Oct). Only the
// operator's P8 probe uses them so far. Their input schemas are read from
// the tool list and checked before anything is sent; their answers'
// shapes are not yet observed, so each is read defensively.
var statusUpdateTools = []string{"save_status_update", "get_status_updates", "delete_status_update"}

// MaxStatusUpdateBody bounds a status update body.
const MaxStatusUpdateBody = 4096

// StatusUpdateRef is one listed status update: its ID, its health, and
// whether its body carries the caller's marker. Never the body.
type StatusUpdateRef struct {
	ID     string `json:"id"`
	Health string `json:"health"`
	Marked bool   `json:"marked"`
}

// StatusUpdateSchemas describes the three tools' inputs: each property's
// type and enum, and which are required. It names fields and allowed
// values, never a value sent or read.
func (p *Provider) StatusUpdateSchemas() map[string]any {
	out := map[string]any{}
	for _, name := range statusUpdateTools {
		t := p.tools[name]
		if t == nil {
			out[name] = "absent"
			continue
		}
		props, req, e := schema(t)
		if e != nil {
			out[name] = "unreadable"
			continue
		}
		fields := map[string]any{}
		for k, v := range props {
			m, _ := v.(map[string]any)
			d := map[string]any{"type": m["type"]}
			if enum, ok := m["enum"]; ok {
				d["enum"] = enum
			}
			fields[k] = d
		}
		out[name] = map[string]any{"fields": fields, "required": req}
	}
	return out
}

// enumValue is the tool's own spelling of want for field: want itself
// when the field has no enum, the enum's matching value (ignoring case)
// when it has one, and "" when the enum lacks it.
func (p *Provider) enumValue(tool, field, want string) string {
	props, _, e := schema(p.tools[tool])
	if e != nil {
		return ""
	}
	m, _ := props[field].(map[string]any)
	enum, ok := m["enum"].([]any)
	if !ok {
		return want
	}
	for _, v := range enum {
		if s, _ := v.(string); strings.EqualFold(s, want) {
			return s
		}
	}
	return ""
}

func (p *Provider) saveStatusArgs(project, body, health string) ([]arg, bool) {
	if p.tools["save_status_update"] == nil {
		return nil, false
	}
	kind := p.enumValue("save_status_update", "type", "project")
	h := p.enumValue("save_status_update", "health", health)
	if kind == "" || h == "" {
		return nil, false
	}
	return []arg{a(project, "project", "projectId"), a(body, "body"), a(h, "health"), a(kind, "type")}, true
}

// StatusUpdatePreflight checks, sending nothing, that all three tools are
// offered and accept what the probe would send: a project update with
// health, its listing by project, and its deletion by ID. A probe that
// could create an update must also be able to find and delete it.
func (p *Provider) StatusUpdatePreflight(health string) error {
	sample := "00000000-0000-4000-8000-000000000001"
	save, ok := p.saveStatusArgs(sample, "probe", health)
	if !ok {
		return fail("capability_unavailable")
	}
	for _, c := range []struct {
		tool string
		args []arg
	}{{"save_status_update", save}, {"get_status_updates", []arg{a(sample, "project", "projectId")}}, {"delete_status_update", []arg{a(sample, "id", "statusUpdateId")}}} {
		if _, _, e := p.v2Negotiation([]string{c.tool}, c.args); e != nil {
			return fail("capability_unavailable")
		}
	}
	return nil
}

// SaveStatusUpdate creates a project status update and returns its ID, or
// "" with no error when Linear answered without a readable ID.
func (p *Provider) SaveStatusUpdate(ctx context.Context, project, body, health string) (string, error) {
	if !ValidID(project) || body == "" || !textOK(body, MaxStatusUpdateBody) {
		return "", fail("invalid")
	}
	if e := p.StatusUpdatePreflight(health); e != nil {
		return "", e
	}
	args, _ := p.saveStatusArgs(project, body, health)
	m, e := p.v2Call(ctx, []string{"save_status_update"}, args...)
	if e != nil {
		return "", e
	}
	return statusUpdateID(m), nil
}

// statusUpdateID reads the ID of an answered update: at the top, or under
// one wrapping key.
func statusUpdateID(m map[string]any) string {
	if i, e := entityID(m); e == nil {
		return i
	}
	for _, k := range []string{"statusUpdate", "projectUpdate", "update", "data"} {
		if v := obj(m, k); v != nil {
			if i, e := entityID(v); e == nil {
				return i
			}
		}
	}
	return ""
}

// StatusUpdates lists a project's status updates, newest page only.
func (p *Provider) StatusUpdates(ctx context.Context, project, marker string) ([]StatusUpdateRef, error) {
	if !ValidID(project) || marker == "" {
		return nil, fail("invalid")
	}
	m, e := p.v2Call(ctx, []string{"get_status_updates"}, a(project, "project", "projectId"))
	if e != nil {
		return nil, e
	}
	var rows []map[string]any
	found := false
	for _, k := range []string{"statusUpdates", "projectUpdates", "updates", "nodes", "items"} {
		if _, ok := m[k]; ok {
			found = true
			// A page that says there is more still lists the newest.
			if rows, _, _, e = listRows(m, k); e != nil && rows == nil {
				return nil, e
			}
			break
		}
	}
	if !found {
		return nil, fail("invalid_response")
	}
	out := []StatusUpdateRef{}
	for _, row := range rows {
		i, e := entityID(row)
		if e != nil {
			return nil, fail("invalid_response")
		}
		out = append(out, StatusUpdateRef{ID: i, Health: str(row, "health"), Marked: strings.Contains(str(row, "body"), marker)})
	}
	return out, nil
}

// DeleteStatusUpdate deletes one status update by its ID.
func (p *Provider) DeleteStatusUpdate(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fail("invalid")
	}
	_, e := p.v2Call(ctx, []string{"delete_status_update"}, a(id, "id", "statusUpdateId"))
	return e
}
