// Package linearprovider is a curated client for Linear's official remote MCP.
// Provider content is untrusted data; this package never executes instructions.
package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Endpoint = "https://mcp.linear.app/mcp"
const MaxResultBytes = 1 << 20
const MaxBodyBytes = 64 << 10

type Error struct{ Kind string }

func (e *Error) Error() string { return "Linear provider: " + e.Kind }
func (e *Error) Code() string  { return e.Kind }
func fail(s string) error      { return &Error{Kind: s} }

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidID(s string) bool { return uuid.MatchString(s) }

type ToolCaller interface {
	ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}
type Provider struct {
	caller ToolCaller
	tools  map[string]*mcp.Tool
	closer interface{ Close() error }
	// parentProven is the only source of parent destination preservation. No
	// tool schema, environment or flag sets it; until a reviewed proof
	// mechanism exists, only in-package tests do.
	parentProven bool
}
type PageOptions struct {
	Limit  int
	Cursor string
}
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Complete   bool   `json:"complete"`
}
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}
type Project struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	TeamIDs []string `json:"team_ids"`
}
type Issue struct {
	ID              string         `json:"id"`
	Identifier      string         `json:"identifier"`
	Title           string         `json:"title"`
	Description     string         `json:"description,omitempty"`
	TeamID          string         `json:"team_id"`
	ProjectID       string         `json:"project_id,omitempty"`
	StateID         string         `json:"state_id,omitempty"`
	State           string         `json:"state,omitempty"`
	Assignee        string         `json:"assignee,omitempty"`
	URL             string         `json:"url,omitempty"`
	Context         map[string]any `json:"context,omitempty"`
	ContextComplete bool           `json:"context_complete"`
}
type Status struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	TeamID string `json:"team_id"`
}
type IssueQuery struct {
	TeamID, ProjectID string
	Limit             int
	Cursor            string
}
type CreateInput struct{ TeamID, ProjectID, Title, Description, StateID string }
type UpdateInput struct {
	ID                          string
	Title, Description, StateID *string
}
type Receipt struct {
	ID      string `json:"id"`
	IssueID string `json:"issue_id"`
}

func New(ctx context.Context, h *http.Client, readonly bool) (*Provider, error) {
	if h == nil {
		return nil, fail("invalid")
	}
	endpoint := Endpoint
	if readonly {
		endpoint += "/readonly"
	}
	bounded := *h
	if bounded.Timeout <= 0 || bounded.Timeout > 45*time.Second {
		bounded.Timeout = 45 * time.Second
	}
	base := bounded.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	bounded.Transport = boundedTransport{base}
	previousRedirect := bounded.CheckRedirect
	bounded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.Host != "mcp.linear.app" || len(via) > 5 {
			return fail("redirect_refused")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "clawbridge-linear", Version: "1"}, nil)
	s, e := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &bounded, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: MaxResultBytes}, nil)
	if e != nil {
		return nil, fail("unavailable")
	}
	p, e := NewWithCaller(ctx, s)
	if e != nil {
		s.Close()
		return nil, e
	}
	p.closer = s
	return p, nil
}

type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if response.Body != nil {
		response.Body = &boundedBody{ReadCloser: response.Body, left: MaxResultBytes}
	}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	left int
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, e := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, fail("result_limit")
		}
		return 0, e
	}
	if len(p) > b.left {
		p = p[:b.left]
	}
	n, e := b.ReadCloser.Read(p)
	b.left -= n
	return n, e
}
func NewWithCaller(ctx context.Context, c ToolCaller) (*Provider, error) {
	if c == nil {
		return nil, fail("invalid")
	}
	p := &Provider{caller: c, tools: map[string]*mcp.Tool{}}
	cursor := ""
	seen := map[string]bool{}
	for n := 0; n < 20; n++ {
		r, e := c.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if e != nil || r == nil {
			return nil, fail("unavailable")
		}
		if len(r.Tools) > 256 {
			return nil, fail("invalid_response")
		}
		for _, t := range r.Tools {
			if t == nil || p.tools[t.Name] != nil {
				return nil, fail("invalid_response")
			}
			p.tools[t.Name] = t
			if len(p.tools) > 256 {
				return nil, fail("result_limit")
			}
		}
		if r.NextCursor == "" {
			return p, nil
		}
		if seen[r.NextCursor] {
			return nil, fail("invalid_response")
		}
		seen[r.NextCursor] = true
		cursor = r.NextCursor
	}
	return nil, fail("result_limit")
}
func (p *Provider) Close() error {
	if p.closer != nil {
		return p.closer.Close()
	}
	return nil
}

// Verify retains the read-only connection verification contract.
func (p *Provider) Verify(ctx context.Context) error { return p.VerifyAccess(ctx, false) }

// VerifyAccess performs a bounded read and verifies the advertised write schemas
// without dispatching any mutation. OAuth scope alone is not a write capability.
func (p *Provider) VerifyAccess(ctx context.Context, write bool) error {
	if _, err := p.Teams(ctx, PageOptions{Limit: 1}); err != nil {
		return err
	}
	if !write {
		return nil
	}
	for _, operation := range []struct {
		names []string
		args  []arg
	}{
		{[]string{"create_issue", "save_issue"}, []arg{a("", "team", "teamId"), a("", "title"), a("", "description")}},
		{[]string{"update_issue", "save_issue"}, []arg{a("", "id", "issueId"), a("", "title"), a("", "description"), a("", "state", "stateId")}},
		{[]string{"create_comment", "save_comment"}, []arg{a("", "issueId", "issue"), a("", "body")}},
	} {
		if _, _, err := p.negotiate(operation.names, operation.args...); err != nil {
			return err
		}
	}
	return nil
}

func schema(t *mcp.Tool) (map[string]any, []string, error) {
	b, e := json.Marshal(t.InputSchema)
	if e != nil || len(b) > MaxResultBytes {
		return nil, nil, fail("capability_unavailable")
	}
	var s map[string]any
	if json.Unmarshal(b, &s) != nil || s["type"] != "object" {
		return nil, nil, fail("capability_unavailable")
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return nil, nil, fail("capability_unavailable")
	}
	var req []string
	if r, ok := s["required"].([]any); ok {
		for _, v := range r {
			x, ok := v.(string)
			if !ok {
				return nil, nil, fail("capability_unavailable")
			}
			req = append(req, x)
		}
	}
	return props, req, nil
}

// field maps a curated semantic field to an advertised, typed alias.
func field(props map[string]any, aliases []string, kind string) (string, bool) {
	matches := func(t any) bool { return t == kind || (kind == "integer" && t == "number") }
	for _, k := range aliases {
		v, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		if matches(v["type"]) {
			return k, true
		}
		if ts, ok := v["type"].([]any); ok {
			for _, t := range ts {
				if matches(t) {
					return k, true
				}
			}
		}
		if arr, ok := v["anyOf"].([]any); ok {
			for _, t := range arr {
				if m, ok := t.(map[string]any); ok && matches(m["type"]) {
					return k, true
				}
			}
		}
	}
	return "", false
}

// issueReadProjection selects only fixed native fields, never caller-supplied
// schema properties. Missing scope values must be returned explicitly as null.
func issueReadProjection(props map[string]any) ([]string, error) {
	raw, advertised := props["fields"]
	if !advertised {
		return nil, nil
	}
	bad := func() ([]string, error) { return nil, fail("capability_unavailable") }
	array, ok := raw.(map[string]any)
	if !ok {
		return bad()
	}
	if variants, union := array["anyOf"]; union {
		for key := range array {
			if key != "anyOf" && key != "description" && key != "title" && key != "default" {
				return bad()
			}
		}
		branches, ok := variants.([]any)
		if !ok || len(branches) != 2 {
			return bad()
		}
		var selected map[string]any
		nulls := 0
		for _, branch := range branches {
			node, ok := branch.(map[string]any)
			if !ok {
				return bad()
			}
			if node["type"] == "null" {
				for key := range node {
					if key != "type" && key != "description" && key != "title" {
						return bad()
					}
				}
				nulls++
			} else if node["type"] == "array" && selected == nil {
				selected = node
			} else {
				return bad()
			}
		}
		if selected == nil || nulls != 1 {
			return bad()
		}
		array = selected
	}
	for key := range array {
		switch key {
		case "type", "items", "minItems", "maxItems", "uniqueItems", "description", "title", "default":
		default:
			return bad()
		}
	}
	if kinds, ok := array["type"].([]any); ok {
		seen := map[string]bool{}
		for _, kind := range kinds {
			text, ok := kind.(string)
			if !ok || (text != "array" && text != "null") || seen[text] {
				return bad()
			}
			seen[text] = true
		}
		if !seen["array"] || len(kinds) > 2 {
			return bad()
		}
	} else if array["type"] != "array" {
		return bad()
	}
	items, ok := array["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		return bad()
	}
	for key := range items {
		if key != "type" && key != "enum" && key != "description" && key != "title" {
			return bad()
		}
	}
	choices, ok := items["enum"].([]any)
	if !ok || len(choices) == 0 || len(choices) > 256 {
		return bad()
	}
	allowed := map[string]bool{}
	for _, choice := range choices {
		name, ok := choice.(string)
		if !ok || !referenceIdentifier.MatchString(name) || allowed[name] {
			return bad()
		}
		allowed[name] = true
	}
	selected := []string{"id", "uuid", "teamId", "projectId"}
	for _, name := range selected {
		if !allowed[name] {
			return bad()
		}
	}
	for _, name := range []string{"identifier", "title", "description", "stateId", "state", "status", "statusType", "assignee", "url", "parentId", "parent", "subIssues", "subissues", "relations", "dependencies"} {
		if allowed[name] {
			selected = append(selected, name)
		}
	}
	for _, bound := range []string{"minItems", "maxItems"} {
		if raw, present := array[bound]; present {
			value, ok := raw.(float64)
			if !ok || value < 0 || value > 256 || value != float64(int(value)) {
				return bad()
			}
			if (bound == "minItems" && len(selected) < int(value)) || (bound == "maxItems" && len(selected) > int(value)) {
				return bad()
			}
		}
	}
	if unique, present := array["uniqueItems"]; present {
		if _, ok := unique.(bool); !ok {
			return bad()
		}
	}
	return selected, nil
}

type arg struct {
	aliases []string
	value   any
	kind    string
}

func a(value any, aliases ...string) arg { return arg{aliases, value, "string"} }
func (p *Provider) negotiate(names []string, args ...arg) (string, map[string]any, error) {
	var chosen string
	var values map[string]any
	for _, name := range names {
		t := p.tools[name]
		if t == nil {
			continue
		}
		props, req, e := schema(t)
		if e != nil {
			continue
		}
		v := map[string]any{}
		valid := true
		for _, x := range args {
			k, ok := field(props, x.aliases, x.kind)
			if !ok {
				valid = false
				break
			}
			v[k] = x.value
		}
		if name == "get_issue" || name == "list_issues" {
			projection, projectionErr := issueReadProjection(props)
			if projectionErr != nil {
				valid = false
			} else if projection != nil {
				v["fields"] = projection
			}
		}
		for _, k := range req {
			if _, ok := v[k]; !ok {
				valid = false
			}
		}
		if valid {
			chosen = name
			values = v
			break
		}
	}
	if chosen == "" {
		return "", nil, fail("capability_unavailable")
	}
	return chosen, values, nil
}
func (p *Provider) call(ctx context.Context, names []string, args ...arg) (map[string]any, error) {
	chosen, values, e := p.negotiate(names, args...)
	if e != nil {
		return nil, e
	}
	return p.invoke(ctx, chosen, values)
}
func (p *Provider) invoke(ctx context.Context, chosen string, values map[string]any) (map[string]any, error) {
	var e error
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, e := p.caller.CallTool(ctx, &mcp.CallToolParams{Name: chosen, Arguments: values})
	if e != nil {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return nil, e
		}
		return nil, fail("unavailable")
	}
	if r == nil || r.IsError {
		return nil, fail("provider_rejected")
	}
	var b []byte
	if r.StructuredContent != nil {
		b, e = json.Marshal(r.StructuredContent)
	} else {
		var parts []string
		for _, c := range r.Content {
			t, ok := c.(*mcp.TextContent)
			if !ok {
				return nil, fail("invalid_response")
			}
			parts = append(parts, t.Text)
		}
		b = []byte(strings.Join(parts, "\n"))
	}
	if e != nil || len(b) > MaxResultBytes {
		return nil, fail("result_limit")
	}
	var decoded any
	if json.Unmarshal(b, &decoded) != nil {
		return nil, fail("invalid_response")
	}
	if rows, ok := decoded.([]any); ok {
		keys := map[string]string{"list_teams": "teams", "list_projects": "projects", "list_issues": "issues", "list_issue_statuses": "statuses", "list_issue_labels": "labels", "list_users": "users", "list_team_members": "members"}
		key := keys[chosen]
		if key == "" {
			return nil, fail("invalid_response")
		}
		decoded = map[string]any{key: rows}
	}
	obj, ok := decoded.(map[string]any)
	if !ok || obj == nil {
		return nil, fail("invalid_response")
	}
	return obj, nil
}
func str(m map[string]any, k string) string         { x, _ := m[k].(string); return x }
func obj(m map[string]any, k string) map[string]any { x, _ := m[k].(map[string]any); return x }
func id(m map[string]any) (string, error) {
	// Some provider entities expose a display identifier in id and the
	// immutable identity in uuid. A display identifier never supplies scope.
	var canonical string
	if raw, present := m["uuid"]; present {
		value, ok := raw.(string)
		if !ok || !ValidID(value) {
			return "", fail("invalid_response")
		}
		canonical = value
	}
	if raw, present := m["id"]; present {
		value, ok := raw.(string)
		if !ok || !referenceIdentifier.MatchString(value) {
			return "", fail("invalid_response")
		}
		if ValidID(strings.ToLower(value)) {
			if !ValidID(value) {
				return "", fail("invalid_response")
			}
			if canonical != "" && canonical != value {
				return "", fail("scope_mismatch")
			}
			canonical = value
		} else if canonical == "" {
			return "", fail("invalid_response")
		}
	}
	if canonical == "" {
		return "", fail("invalid_response")
	}
	return canonical, nil
}

// scopedID accepts equivalent flat/nested UUID representations, but never lets
// one overwrite another. Explicit null is valid only for an optional relation.
func scopedID(m map[string]any, flat, nested string, optional bool) (string, error) {
	var result string
	seen := false
	accept := func(v any) error {
		var value string
		if v == nil {
			if !optional {
				return fail("invalid_response")
			}
		} else {
			var ok bool
			value, ok = v.(string)
			if !ok || !ValidID(value) {
				return fail("invalid_response")
			}
		}
		if seen && value != result {
			return fail("scope_mismatch")
		}
		result = value
		seen = true
		return nil
	}
	if v, ok := m[flat]; ok {
		if err := accept(v); err != nil {
			return "", err
		}
	}
	if v, ok := m[nested]; ok {
		// Descriptive labels carry no identity and cannot repair missing scope.
		if _, label := v.(string); label && (nested == "team" || nested == "project") {
			if !seen || !ValidID(result) {
				return "", fail("invalid_response")
			}
			return result, nil
		}
		if v == nil {
			if err := accept(nil); err != nil {
				return "", err
			}
		} else {
			node, ok := v.(map[string]any)
			if !ok {
				return "", fail("invalid_response")
			}
			value, err := entityID(node, flat)
			if err != nil {
				return "", err
			}
			if err := accept(value); err != nil {
				return "", err
			}
		}
	}
	if !seen {
		return "", fail("scope_metadata_missing")
	}
	return result, nil
}
func entityID(m map[string]any, aliases ...string) (string, error) {
	value, err := id(m)
	if err != nil {
		return "", err
	}
	for _, key := range aliases {
		if other, ok := m[key]; ok {
			s, ok := other.(string)
			if !ok || !ValidID(s) {
				return "", fail("invalid_response")
			}
			if s != value {
				return "", fail("scope_mismatch")
			}
		}
	}
	return value, nil
}
func unwrap(m map[string]any, key string) map[string]any {
	if _, present := m[key]; present && obj(m, key) == nil {
		return nil
	}
	if v := obj(m, key); v != nil {
		_, hasID := m["id"]
		_, hasUUID := m["uuid"]
		if hasID || hasUUID {
			outer, outerErr := id(m)
			inner, innerErr := id(v)
			if outerErr != nil || innerErr != nil || outer != inner {
				return nil
			}
		}
		return v
	}
	return m
}
func list(m map[string]any, key string) ([]map[string]any, string, bool, error) {
	raw, ok := m[key].([]any)
	if !ok {
		return nil, "", false, fail("invalid_response")
	}
	if len(raw) > 250 {
		return nil, "", false, fail("result_limit")
	}
	items := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		x, ok := v.(map[string]any)
		if !ok {
			return nil, "", false, fail("invalid_response")
		}
		items = append(items, x)
	}
	next := str(m, "nextCursor")
	if next == "" {
		next = str(m, "cursor")
	}
	complete := false
	if b, ok := m["hasNextPage"].(bool); ok {
		complete = !b
		if b && next == "" {
			return nil, "", false, fail("pagination_unavailable")
		}
	}
	if pi := obj(m, "pageInfo"); pi != nil {
		if b, ok := pi["hasNextPage"].(bool); ok {
			complete = !b
			if b {
				next = str(pi, "endCursor")
				if next == "" {
					return nil, "", false, fail("pagination_unavailable")
				}
			}
		}
	}
	if len(next) > 2048 {
		return nil, "", false, fail("invalid_response")
	}
	return items, next, complete, nil
}
func pageArgs(o PageOptions) ([]arg, error) {
	if o.Limit == 0 {
		o.Limit = 50
	}
	if o.Limit < 1 || o.Limit > 100 || len(o.Cursor) > 2048 {
		return nil, fail("invalid")
	}
	args := []arg{{[]string{"limit", "first"}, o.Limit, "integer"}}
	if o.Cursor != "" {
		args = append(args, a(o.Cursor, "cursor", "after"))
	}
	return args, nil
}
func parseTeam(m map[string]any) (Team, error) {
	i, e := entityID(m, "teamId")
	if e != nil {
		return Team{}, e
	}
	return Team{i, str(m, "name"), str(m, "key")}, nil
}
func (p *Provider) Teams(ctx context.Context, o PageOptions) (Page[Team], error) {
	args, e := pageArgs(o)
	if e != nil {
		return Page[Team]{}, e
	}
	m, e := p.call(ctx, []string{"list_teams"}, args...)
	if e != nil {
		return Page[Team]{}, e
	}
	rows, n, c, e := list(m, "teams")
	if e != nil {
		return Page[Team]{}, e
	}
	out := Page[Team]{Items: []Team{}, NextCursor: n, Complete: c}
	for _, r := range rows {
		t, e := parseTeam(r)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, t)
	}
	return out, nil
}
func (p *Provider) GetTeam(ctx context.Context, i string) (Team, error) {
	if !ValidID(i) {
		return Team{}, fail("invalid")
	}
	m, e := p.call(ctx, []string{"get_team"}, a(i, "query", "id", "teamId"))
	if e != nil {
		return Team{}, e
	}
	t, e := parseTeam(unwrap(m, "team"))
	if e == nil && t.ID != i {
		return Team{}, fail("scope_mismatch")
	}
	return t, e
}
func parseProject(m map[string]any) (result Project, err error) {
	defer func() {
		if err != nil {
			result = Project{}
		}
	}()
	i, e := entityID(m, "projectId")
	if e != nil {
		return Project{}, e
	}
	p := Project{ID: i, Name: str(m, "name"), TeamIDs: []string{}}
	raw, ok := m["teams"].([]any)
	if !ok {
		if v := obj(m, "teams"); v != nil {
			if _, present := v["pageInfo"]; present && obj(v, "pageInfo") == nil {
				return p, fail("invalid_response")
			}
			if pi := obj(v, "pageInfo"); pi != nil {
				more, ok := pi["hasNextPage"].(bool)
				if !ok {
					return p, fail("invalid_response")
				}
				if more {
					return p, fail("scope_metadata_missing")
				}
			}
			raw, ok = v["nodes"].([]any)
		}
	}
	if !ok {
		if _, supplied := m["teams"]; supplied {
			return p, fail("invalid_response")
		}
		return p, fail("scope_metadata_missing")
	}
	seen := map[string]bool{}
	for _, v := range raw {
		t, ok := v.(map[string]any)
		if !ok {
			return p, fail("invalid_response")
		}
		tid, e := entityID(t, "teamId")
		if e != nil {
			return p, e
		}
		if seen[tid] {
			return p, fail("invalid_response")
		}
		seen[tid] = true
		p.TeamIDs = append(p.TeamIDs, tid)
	}
	if len(p.TeamIDs) == 0 {
		return p, fail("scope_metadata_missing")
	}
	return p, nil
}
func (p *Provider) Projects(ctx context.Context, o PageOptions) (Page[Project], error) {
	args, e := pageArgs(o)
	if e != nil {
		return Page[Project]{}, e
	}
	m, e := p.call(ctx, []string{"list_projects"}, args...)
	if e != nil {
		return Page[Project]{}, e
	}
	rows, n, c, e := list(m, "projects")
	if e != nil {
		return Page[Project]{}, e
	}
	out := Page[Project]{Items: []Project{}, NextCursor: n, Complete: c}
	for _, r := range rows {
		pr, e := parseProject(r)
		if e != nil {
			if pe, ok := e.(*Error); ok && pe.Kind == "scope_metadata_missing" {
				canonical, identityErr := entityID(r, "projectId")
				if identityErr != nil {
					return out, identityErr
				}
				pr, e = p.GetProject(ctx, canonical)
			}
		}
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, pr)
	}
	return out, nil
}
func (p *Provider) GetProject(ctx context.Context, i string) (Project, error) {
	if !ValidID(i) {
		return Project{}, fail("invalid")
	}
	m, e := p.call(ctx, []string{"get_project"}, a(i, "query", "id", "projectId"))
	if e != nil {
		return Project{}, e
	}
	pr, e := parseProject(unwrap(m, "project"))
	if e == nil && pr.ID != i {
		return Project{}, fail("scope_mismatch")
	}
	return pr, e
}
func textOK(s string, n int) bool { return len(s) <= n && strings.IndexByte(s, 0) < 0 }
func (p *Provider) parseIssue(ctx context.Context, m map[string]any) (result Issue, err error) {
	defer func() {
		if err != nil {
			result = Issue{}
		}
	}()
	i, e := entityID(m, "issueId")
	if e != nil {
		return Issue{}, e
	}
	team, e := scopedID(m, "teamId", "team", false)
	if e != nil {
		return Issue{}, e
	}
	project, e := scopedID(m, "projectId", "project", true)
	if e != nil {
		return Issue{}, e
	}
	identifier := str(m, "identifier")
	if identifier == "" && !ValidID(str(m, "id")) {
		identifier = str(m, "id")
	}
	x := Issue{ID: i, Identifier: identifier, Title: str(m, "title"), Description: str(m, "description"), TeamID: team, ProjectID: project, URL: str(m, "url"), Context: map[string]any{}}
	_, hasState := m["state"]
	if _, flat := m["stateId"]; flat || hasState {
		state, e := scopedID(m, "stateId", "state", true)
		if e != nil {
			return Issue{}, e
		}
		x.StateID = state
		if stateObj := obj(m, "state"); stateObj != nil {
			x.State = str(stateObj, "name")
			_, hasTeam := stateObj["team"]
			if _, a := stateObj["teamId"]; a || hasTeam {
				stateTeam, e := scopedID(stateObj, "teamId", "team", false)
				if e != nil {
					return Issue{}, e
				}
				if stateTeam != team {
					return Issue{}, fail("scope_mismatch")
				}
			}
		}
	} else {
		x.State = str(m, "status")
	}
	if x.State == "" {
		x.State = str(m, "status")
	}
	if as := obj(m, "assignee"); as != nil {
		x.Assignee = str(as, "name")
	} else {
		x.Assignee = str(m, "assignee")
	}
	if !textOK(x.Title, 1024) || !textOK(x.Description, MaxBodyBytes) {
		return x, fail("result_limit")
	}
	// Linked issues are references, not independently authorized bodies. Never
	// forward nested descriptions, comments or arbitrary provider objects.
	if parent := str(m, "parentId"); ValidID(parent) {
		x.Context["parentId"] = parent
	}
	for _, key := range []string{"parent", "subIssues", "subissues", "relations", "dependencies"} {
		if refs := linkedReferences(m[key]); len(refs) > 0 {
			x.Context[key] = refs
		}
	}
	x.ContextComplete = false
	return x, nil
}

// linkedReferences copies only bounded reference metadata. It deliberately
// does not assert the linked issue belongs to the configured board; opening a
// reference requires a separate scope-checked get operation.
func linkedReferences(value any) []map[string]string {
	refs := []map[string]string{}
	seen := map[string]bool{}
	var visit func(any, string, int)
	visit = func(v any, relation string, depth int) {
		if depth > 4 || len(refs) >= 128 {
			return
		}
		switch node := v.(type) {
		case []any:
			for _, child := range node {
				visit(child, relation, depth+1)
				if len(refs) >= 128 {
					break
				}
			}
		case map[string]any:
			// Relation wrappers may carry their own ID; prefer the actual issue.
			for _, key := range []string{"issue", "relatedIssue", "targetIssue"} {
				if child := obj(node, key); child != nil {
					visit(child, relation, depth+1)
					return
				}
			}
			if identifier, identityErr := entityID(node, "issueId"); identityErr == nil {
				if seen[identifier] {
					return
				}
				seen[identifier] = true
				ref := map[string]string{"id": identifier}
				label := str(node, "identifier")
				if label == "" && !ValidID(str(node, "id")) {
					label = str(node, "id")
				}
				if referenceIdentifier.MatchString(label) {
					ref["identifier"] = label
				}
				kind := str(node, "type")
				if kind == "" {
					kind = relation
				}
				switch kind {
				case "blocks", "blockedBy", "related", "duplicate", "parent", "child":
					ref["relation"] = kind
				}
				refs = append(refs, ref)
				return
			}
			for _, key := range []string{"nodes", "items", "blocks", "blockedBy", "related", "duplicates", "parents", "children"} {
				if child, ok := node[key]; ok {
					kind := relation
					switch key {
					case "blocks", "blockedBy", "related":
						kind = key
					case "duplicates":
						kind = "duplicate"
					case "parents":
						kind = "parent"
					case "children":
						kind = "child"
					}
					visit(child, kind, depth+1)
				}
			}
		}
	}
	visit(value, "", 0)
	return refs
}

var referenceIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (p *Provider) Issues(ctx context.Context, q IssueQuery) (Page[Issue], error) {
	if !ValidID(q.TeamID) || (q.ProjectID != "" && !ValidID(q.ProjectID)) {
		return Page[Issue]{}, fail("invalid")
	}
	args, e := pageArgs(PageOptions{q.Limit, q.Cursor})
	if e != nil {
		return Page[Issue]{}, e
	}
	args = append(args, a(q.TeamID, "team", "teamId"))
	if q.ProjectID != "" {
		args = append(args, a(q.ProjectID, "project", "projectId"))
	}
	m, e := p.call(ctx, []string{"list_issues"}, args...)
	if e != nil {
		return Page[Issue]{}, e
	}
	rows, n, c, e := list(m, "issues")
	if e != nil {
		return Page[Issue]{}, e
	}
	out := Page[Issue]{Items: []Issue{}, NextCursor: n, Complete: c}
	for _, r := range rows {
		x, e := p.parseIssue(ctx, r)
		if pe, ok := e.(*Error); ok && pe.Kind == "scope_metadata_missing" {
			canonical, identityErr := entityID(r, "issueId")
			if identityErr != nil {
				return out, identityErr
			}
			x, e = p.GetIssue(ctx, canonical)
		}
		if e != nil {
			return out, e
		}
		if x.TeamID != q.TeamID || (q.ProjectID != "" && x.ProjectID != q.ProjectID) {
			return out, fail("scope_mismatch")
		}
		out.Items = append(out.Items, x)
	}
	return out, nil
}
func (p *Provider) GetIssue(ctx context.Context, i string) (Issue, error) {
	if !ValidID(i) {
		return Issue{}, fail("invalid")
	}
	m, e := p.call(ctx, []string{"get_issue"}, a(i, "id", "query", "issueId"))
	if e != nil {
		return Issue{}, e
	}
	x, e := p.parseIssue(ctx, unwrap(m, "issue"))
	if e == nil && x.ID != i {
		return Issue{}, fail("scope_mismatch")
	}
	return x, e
}
func (p *Provider) Statuses(ctx context.Context, team string) ([]Status, error) {
	if !ValidID(team) {
		return nil, fail("invalid")
	}
	m, e := p.call(ctx, []string{"list_issue_statuses"}, a(team, "team", "teamId"))
	if e != nil {
		return nil, e
	}
	raw, _, _, e := list(m, "statuses")
	if e != nil {
		return nil, e
	}
	out := []Status{}
	for _, v := range raw {
		i, e := entityID(v, "stateId", "statusId")
		if e != nil {
			return nil, e
		}
		tid := team
		_, hasTeam := v["team"]
		if _, flat := v["teamId"]; flat || hasTeam {
			resolved, e := scopedID(v, "teamId", "team", false)
			if e != nil {
				return nil, e
			}
			tid = resolved
		}
		if tid != team {
			return nil, fail("scope_mismatch")
		}
		out = append(out, Status{i, str(v, "name"), team})
	}
	return out, nil
}
func (p *Provider) Create(ctx context.Context, in CreateInput) (Issue, error) {
	if !ValidID(in.TeamID) || (in.ProjectID != "" && !ValidID(in.ProjectID)) || (in.StateID != "" && !ValidID(in.StateID)) || in.Title == "" || !textOK(in.Title, 1024) || !textOK(in.Description, MaxBodyBytes) {
		return Issue{}, fail("invalid")
	}
	args := []arg{a(in.TeamID, "team", "teamId"), a(in.Title, "title"), a(in.Description, "description")}
	if in.ProjectID != "" {
		args = append(args, a(in.ProjectID, "project", "projectId"))
	}
	if in.StateID != "" {
		args = append(args, a(in.StateID, "state", "stateId"))
	}
	m, e := p.call(ctx, []string{"create_issue", "save_issue"}, args...)
	if e != nil {
		return Issue{}, e
	}
	x, e := p.parseIssue(ctx, unwrap(m, "issue"))
	if e == nil && (x.TeamID != in.TeamID || x.ProjectID != in.ProjectID) {
		e = fail("scope_mismatch")
	}
	return x, e
}
func (p *Provider) Update(ctx context.Context, in UpdateInput) (Issue, error) {
	if !ValidID(in.ID) || (in.Title == nil && in.Description == nil && in.StateID == nil) {
		return Issue{}, fail("invalid")
	}
	args := []arg{a(in.ID, "id", "issueId")}
	for _, v := range []struct {
		p    *string
		keys []string
		max  int
	}{{in.Title, []string{"title"}, 1024}, {in.Description, []string{"description"}, MaxBodyBytes}, {in.StateID, []string{"state", "stateId"}, 128}} {
		if v.p != nil {
			if !textOK(*v.p, v.max) {
				return Issue{}, fail("invalid")
			}
			args = append(args, a(*v.p, v.keys...))
		}
	}
	if in.StateID != nil && !ValidID(*in.StateID) {
		return Issue{}, fail("invalid")
	}
	m, e := p.call(ctx, []string{"update_issue", "save_issue"}, args...)
	if e != nil {
		return Issue{}, e
	}
	x, e := p.parseIssue(ctx, unwrap(m, "issue"))
	if e == nil && x.ID != in.ID {
		e = fail("scope_mismatch")
	}
	return x, e
}
func (p *Provider) Comment(ctx context.Context, issue, body string) (Receipt, error) {
	if !ValidID(issue) || body == "" || !textOK(body, MaxBodyBytes) {
		return Receipt{}, fail("invalid")
	}
	m, e := p.call(ctx, []string{"create_comment", "save_comment"}, a(issue, "issueId", "issue"), a(body, "body"))
	if e != nil {
		return Receipt{}, e
	}
	m = unwrap(m, "comment")
	i, e := id(m)
	if e != nil {
		return Receipt{}, e
	}
	if parent := str(m, "issueId"); parent != "" && parent != issue {
		return Receipt{}, fail("scope_mismatch")
	}
	return Receipt{i, issue}, nil
}
