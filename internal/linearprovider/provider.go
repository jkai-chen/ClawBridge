// Package linearprovider is a curated client for Linear's official remote MCP.
// Provider content is untrusted data; this package never executes instructions.
package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Endpoint = "https://mcp.linear.app/mcp"
const MaxResultBytes = 1 << 20
const MaxBodyBytes = 64 << 10

// Error is a provider failure with only safe, bounded detail: a kind, the
// HTTP status, a Retry-After value and a Linear error code, never free text.
type Error struct {
	Kind       string
	Status     int
	RetryAfter string
	LinearCode string
	// NotSent means the request provably never reached Linear's MCP server
	// tool: it failed before its headers were written, or the server's HTTP
	// layer refused it (429, 401, 403) before parsing it.
	NotSent bool
}

func (e *Error) Error() string { return "Linear provider: " + e.Detail() }
func (e *Error) Code() string  { return e.Kind }

// Detail is the kind plus its safe detail, for messages and records.
func (e *Error) Detail() string {
	s := e.Kind
	if e.Status != 0 {
		s += fmt.Sprintf(", HTTP %d", e.Status)
	}
	if e.RetryAfter != "" {
		s += ", retry after " + e.RetryAfter
	}
	if e.LinearCode != "" {
		s += ", Linear code " + e.LinearCode
	}
	return s
}
func fail(s string) error { return &Error{Kind: s} }

var linearCode = regexp.MustCompile(`^[A-Z_]{3,40}$`)
var retryAfter = regexp.MustCompile(`^(?:[0-9]{1,6}|[A-Za-z0-9 ,:]{1,40})$`)

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
	// broken records any failed call or missing capability, so a kept-open
	// connection is reopened with a fresh tools/list before its next command.
	broken atomic.Bool
	sent   atomic.Uint64
	// capHits counts responses the transport cut at MaxResultBytes. go-sdk
	// reports that failure only as text, so invoke compares this count
	// instead. It is nil for an injected caller. The count is per connection,
	// which is sound because a provider runs one call at a time (the MCP
	// adapter and the board serialise commands per process).
	capHits *atomic.Uint64
	// reopen opens a fresh session on the same transport, replacing one a
	// cut response closed. It is nil for an injected caller.
	reopen func(context.Context) (ToolCaller, interface{ Close() error }, error)
	// stale is set when a call ended at the response cap: go-sdk closes the
	// session then, so the next call reopens it first. The cut call itself
	// is never sent again.
	stale atomic.Bool
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
	// Type is the workflow state type (triage, backlog, unstarted,
	// started, completed or canceled) when Linear reports one, else "".
	Type string `json:"type,omitempty"`
}

// statusTypes are the workflow state types Linear defines.
var statusTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true, "completed": true, "canceled": true}

// StatusType is a workflow state's type as ClawBridge knows it: one of
// statusTypes, lower-cased, or "" for any other (such as Linear's
// duplicate type), which no status policy may target or move from.
func StatusType(raw string) string {
	kind := strings.ToLower(raw)
	if !statusTypes[kind] {
		return ""
	}
	return kind
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
	// ParentID is the comment a reply answers; empty for any other write.
	ParentID string `json:"parent_id,omitempty"`
	// BodySHA256 is the SHA-256 of the exact comment body posted, the agent
	// marker included.
	BodySHA256 string `json:"body_sha256,omitempty"`
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
	hits, transient := new(atomic.Uint64), new(atomic.Bool)
	bounded.Transport = boundedTransport{base, hits, transient}
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
	connect := func(ctx context.Context) (*mcp.ClientSession, error) {
		c := mcp.NewClient(&mcp.Implementation{Name: "clawbridge-linear", Version: "1"}, nil)
		s, e := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &bounded, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: MaxResultBytes}, nil)
		if e != nil {
			return nil, fail("unavailable")
		}
		return s, nil
	}
	open := func(ctx context.Context) (*mcp.ClientSession, *Provider, error) {
		s, e := connect(ctx)
		if e != nil {
			return nil, nil, e
		}
		p, e := NewWithCaller(ctx, s)
		if e != nil {
			s.Close()
			return nil, nil, e
		}
		return s, p, nil
	}
	// Initialize and the first tools/list are retried once after a 5xx or a
	// broken connection: they change nothing, unlike a tool call, which is
	// never retried.
	s, p, e := open(ctx)
	if e != nil && transient.Load() && ctx.Err() == nil {
		select {
		case <-time.After(connectRetryDelay()):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s, p, e = open(ctx)
	}
	if e != nil {
		return nil, e
	}
	p.closer, p.capHits = s, hits
	p.reopen = func(ctx context.Context) (ToolCaller, interface{ Close() error }, error) {
		s, e := connect(ctx)
		return s, s, e
	}
	return p, nil
}

// reopenSession replaces a session a cut response closed; the advertised
// tools are kept, since the endpoint is the same. Closing the old session and
// initializing the new one are both bounded by ctx. The connection stays
// marked broken, so the pool opens a new one for the next command.
func (p *Provider) reopenSession(ctx context.Context) error {
	if p.reopen == nil {
		return nil
	}
	if old := p.closer; old != nil {
		closed := make(chan struct{})
		go func() {
			old.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.closer = nil
	}
	caller, closer, e := p.reopen(ctx)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	p.caller, p.closer = caller, closer
	return nil
}

type boundedTransport struct {
	base http.RoundTripper
	hits *atomic.Uint64
	// transient records a 5xx or a broken connection, which New retries
	// once while connecting.
	transient *atomic.Bool
}

// connectRetryDelay is the pause before New's one retry: 500 ms plus up to
// 250 ms of jitter. Tests replace it.
var connectRetryDelay = func() time.Duration {
	return 500*time.Millisecond + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
}

// RoundTrip marks the failures that prove a request never reached the
// server's tool: an error before any header was written (DNS, dial, TLS,
// a changed profile), and a 429, 401 or 403 from the HTTP layer. Every other
// failure stays a plain error, which callers treat as possibly sent.
func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }}
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	response, err := t.base.RoundTrip(r)
	if err != nil && r.Context().Err() == nil && t.transient != nil {
		t.transient.Store(true)
	}
	if err == nil && response.StatusCode >= 500 && t.transient != nil {
		t.transient.Store(true)
	}
	if err != nil {
		if !wrote.Load() && r.Context().Err() == nil {
			return nil, &Error{Kind: "unavailable", NotSent: true}
		}
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		refused := &Error{Kind: "unauthorized", Status: response.StatusCode, NotSent: true}
		if response.StatusCode == http.StatusTooManyRequests {
			refused.Kind = "rate_limited"
			if v := strings.TrimSpace(response.Header.Get("Retry-After")); retryAfter.MatchString(v) {
				refused.RetryAfter = v
			}
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, MaxBodyBytes))
		response.Body.Close()
		return nil, refused
	}
	if response.Body != nil {
		response.Body = &boundedBody{ReadCloser: response.Body, left: MaxResultBytes, hits: t.hits}
	}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	left int
	hits *atomic.Uint64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, e := b.ReadCloser.Read(probe[:])
		if n > 0 {
			if b.hits != nil {
				b.hits.Add(1)
			}
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

// Tools lists the advertised tools by name, each with its input schema's
// property names (required ones marked *), sorted. It reads only the tool
// list the connection already negotiated.
func (p *Provider) Tools() map[string][]string {
	out := map[string][]string{}
	for name, t := range p.tools {
		raw, _ := json.Marshal(t.InputSchema)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		_ = json.Unmarshal(raw, &schema)
		required := map[string]bool{}
		for _, r := range schema.Required {
			required[r] = true
		}
		fields := []string{}
		for field := range schema.Properties {
			if required[field] {
				field += "*"
			}
			fields = append(fields, field)
		}
		sort.Strings(fields)
		out[name] = fields
	}
	return out
}

// Alive reports whether a kept-open connection may serve another command: no
// call on it has failed or missed a capability, and the remote session still
// answers a ping. A dead session is reopened instead of failing the command.
func (p *Provider) Alive(ctx context.Context) bool {
	if p.broken.Load() {
		return false
	}
	pinger, ok := p.caller.(interface {
		Ping(context.Context, *mcp.PingParams) error
	})
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if e := pinger.Ping(ctx, nil); e != nil && !statelessPingRejected(e) {
		p.broken.Store(true)
		return false
	}
	return true
}

// statelessPingRejected matches the one ping failure that is not a dead
// session. go-sdk v1.8.0's ClientSession.Ping omits the protocolVersion _meta
// that a sessionless (2026-07-28) server requires, so that server answers
// -32602 naming the field; it has no session to lose. Every other error,
// including any other -32602, still counts as dead. Remove this once upstream
// sends _meta on ping.
func statelessPingRejected(err error) bool {
	var rpc *jsonrpc.Error
	return errors.As(err, &rpc) && rpc.Code == jsonrpc.CodeInvalidParams && strings.Contains(rpc.Message, `_meta field "io.modelcontextprotocol/protocolVersion"`)
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
	for _, name := range []string{"identifier", "title", "description", "stateId", "state", "status", "statusType", "assignee", "url", "updatedAt", "parentId", "parent", "subIssues", "subissues", "relations", "dependencies"} {
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
		p.broken.Store(true)
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
	if p.stale.Load() {
		// Reopen before sending; a failed reopen sent nothing.
		if e := p.reopenSession(ctx); e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &Error{Kind: "unavailable", NotSent: true}
		}
		p.stale.Store(false)
	}
	var before uint64
	if p.capHits != nil {
		before = p.capHits.Load()
	}
	out, e := p.invokeOnce(ctx, chosen, values)
	if e != nil && p.capHits != nil && p.capHits.Load() != before {
		e = fail("result_limit")
	}
	var cut *Error
	if p.reopen != nil && errors.As(e, &cut) && cut.Kind == "result_limit" {
		p.stale.Store(true)
	}
	var unsent *Error
	if !errors.As(e, &unsent) || !unsent.NotSent {
		p.sent.Add(1)
	}
	if e != nil {
		p.broken.Store(true)
	}
	return out, e
}

// Sent counts tool calls that may have reached Linear. A write whose count
// did not change sent nothing.
func (p *Provider) Sent() uint64 { return p.sent.Load() }

// rejectionCode returns Linear's error code from a rejected tool result, only
// when its JSON carries one in the expected place and shape.
func rejectionCode(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Content {
		t, ok := c.(*mcp.TextContent)
		if !ok || len(t.Text) > MaxBodyBytes {
			continue
		}
		var body struct {
			Code       string `json:"code"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
			Errors []struct {
				Extensions struct {
					Code string `json:"code"`
				} `json:"extensions"`
			} `json:"errors"`
		}
		if json.Unmarshal([]byte(t.Text), &body) != nil {
			continue
		}
		candidates := []string{body.Extensions.Code, body.Code}
		for _, e := range body.Errors {
			candidates = append(candidates, e.Extensions.Code)
		}
		for _, code := range candidates {
			if linearCode.MatchString(code) {
				return code
			}
		}
	}
	return ""
}
func (p *Provider) invokeOnce(ctx context.Context, chosen string, values map[string]any) (map[string]any, error) {
	var e error
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, e := p.caller.CallTool(ctx, &mcp.CallToolParams{Name: chosen, Arguments: values})
	if e != nil {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return nil, e
		}
		var transport *Error
		if errors.As(e, &transport) {
			copied := *transport
			return nil, &copied
		}
		return nil, fail("unavailable")
	}
	if r == nil || r.IsError {
		return nil, &Error{Kind: "provider_rejected", LinearCode: rejectionCode(r)}
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
		keys := map[string]string{"list_teams": "teams", "list_projects": "projects", "list_issues": "issues", "list_issue_statuses": "statuses", "list_issue_labels": "labels", "list_users": "users", "list_team_members": "members", "list_comments": "comments"}
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
	items, next, complete, e := listRows(m, key)
	if e != nil {
		return nil, "", false, e
	}
	return items, next, complete, nil
}

// listRows is list, except that a page whose continuation is unusable
// (hasNextPage true without a recognised cursor) still returns its rows with
// the pagination_unavailable error, for reads that can use them.
func listRows(m map[string]any, key string) ([]map[string]any, string, bool, error) {
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
			return items, "", false, fail("pagination_unavailable")
		}
	}
	if pi := obj(m, "pageInfo"); pi != nil {
		if b, ok := pi["hasNextPage"].(bool); ok {
			complete = !b
			if b {
				next = str(pi, "endCursor")
				if next == "" {
					return items, "", false, fail("pagination_unavailable")
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

// ProjectListing is one row of a team's project list.
type ProjectListing struct {
	Project
	Trashed bool `json:"trashed"`
}

var projectListFields = []string{"id", "uuid", "name", "teams", "trashed"}

// teamProjectsSupported reports whether list_projects advertises the exact
// team, archive, projection and paging inputs TeamProjects sends.
func (p *Provider) teamProjectsSupported() bool {
	t := p.tools["list_projects"]
	if t == nil {
		return false
	}
	props, req, e := schema(t)
	if e != nil {
		return false
	}
	for _, f := range []struct{ name, kind string }{{"team", "string"}, {"includeArchived", "boolean"}, {"fields", "array"}, {"limit", "integer"}, {"cursor", "string"}} {
		if k, ok := field(props, []string{f.name}, f.kind); !ok || k != f.name {
			return false
		}
	}
	for _, k := range req {
		if k != "team" {
			return false
		}
	}
	items, _ := obj(props, "fields")["items"].(map[string]any)
	if enum, listed := items["enum"].([]any); listed {
		for _, want := range projectListFields {
			found := false
			for _, v := range enum {
				found = found || v == want
			}
			if !found {
				return false
			}
		}
	}
	return true
}

// Workspace is the Linear workspace (organisation) a connection works in.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Workspace reads the connection's workspace with get_workspace, which
// takes no arguments. It fails with capability_unavailable when the tool is
// not advertised.
func (p *Provider) Workspace(ctx context.Context) (Workspace, error) {
	if p.tools["get_workspace"] == nil {
		return Workspace{}, fail("capability_unavailable")
	}
	m, e := p.call(ctx, []string{"get_workspace"})
	if e != nil {
		return Workspace{}, e
	}
	if nested := obj(m, "workspace"); nested != nil {
		m = nested
	}
	i, _ := m["id"].(string)
	i = strings.ToLower(i)
	if !ValidID(i) {
		return Workspace{}, fail("invalid_response")
	}
	return Workspace{ID: i, Name: str(m, "name")}, nil
}

// TeamProjects lists one page of a team's projects, trashed ones included and
// marked, so a caller can verify many projects in one read. Without the
// advertised inputs it fails with capability_unavailable before any call, and
// the connection stays usable.
func (p *Provider) TeamProjects(ctx context.Context, team string, o PageOptions) (Page[ProjectListing], error) {
	if !ValidID(team) || o.Limit < 0 || o.Limit > 50 || len(o.Cursor) > 2048 {
		return Page[ProjectListing]{}, fail("invalid")
	}
	if !p.teamProjectsSupported() {
		return Page[ProjectListing]{}, fail("capability_unavailable")
	}
	if o.Limit == 0 {
		o.Limit = 50
	}
	args := []arg{a(team, "team"), {[]string{"includeArchived"}, true, "boolean"}, {[]string{"fields"}, projectListFields, "array"}, {[]string{"limit"}, o.Limit, "integer"}}
	if o.Cursor != "" {
		args = append(args, a(o.Cursor, "cursor"))
	}
	m, e := p.call(ctx, []string{"list_projects"}, args...)
	if e != nil {
		return Page[ProjectListing]{}, e
	}
	rows, n, c, e := list(m, "projects")
	if e != nil {
		return Page[ProjectListing]{}, e
	}
	if !c && n == "" {
		return Page[ProjectListing]{}, fail("pagination_unavailable")
	}
	out := Page[ProjectListing]{Items: []ProjectListing{}, NextCursor: n, Complete: c}
	for _, r := range rows {
		var trashed bool
		switch v := r["trashed"].(type) {
		case nil:
		case bool:
			trashed = v
		default:
			return Page[ProjectListing]{}, fail("invalid_response")
		}
		pr, e := parseProject(r)
		if e != nil {
			return Page[ProjectListing]{}, e
		}
		out.Items = append(out.Items, ProjectListing{pr, trashed})
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
	raw, next, complete, e := list(m, "statuses")
	if e != nil {
		return nil, e
	}
	// list_issue_statuses cannot page, so a response that says more follows
	// cannot be completed and is refused rather than treated as the team's
	// whole workflow. (hasNextPage true without a cursor already failed.)
	if next != "" && !complete {
		return nil, fail("pagination_unavailable")
	}
	filtered := p.statusTeamFilterReason() == ""
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
		} else if !filtered {
			// A team-less row belongs to the team only through the
			// documented filter.
			return nil, fail("capability_unavailable")
		}
		if tid != team {
			return nil, fail("scope_mismatch")
		}
		out = append(out, Status{ID: i, Name: str(v, "name"), TeamID: team, Type: StatusType(str(v, "type"))})
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
	return p.CommentOnIssue(ctx, issue, "", body)
}

// CommentOnIssue is Comment for an issue whose display identifier was proven
// by a read, so the readback also accepts a row that names the issue by it.
func (p *Provider) CommentOnIssue(ctx context.Context, issue, identifier, body string) (Receipt, error) {
	if !ValidID(issue) || body == "" || !textOK(body, MaxBodyBytes) || (identifier != "" && !ValidIssueIdentifier(identifier)) {
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
	parent, known, e := commentParent(m)
	if e != nil {
		return Receipt{}, e
	}
	if known && parent != issue {
		return Receipt{}, fail("scope_mismatch")
	}
	if !commentBodyMatches(m, body) {
		return Receipt{}, fail("receipt_mismatch")
	}
	// A receipt that does not name its issue is confirmed by reading the
	// issue's comments; without that evidence the comment stays uncertain.
	if !known {
		row, e := p.findComment(ctx, issue, identifier, i)
		if e != nil || row == nil {
			return Receipt{}, fail("receipt_unverified")
		}
		if !commentBodyMatches(row, body) {
			return Receipt{}, fail("receipt_mismatch")
		}
	}
	return Receipt{ID: i, IssueID: issue, BodySHA256: bodySHA256(body)}, nil
}

// commentParent reads the issue UUID a comment receipt names, flat or nested.
// A display identifier names no UUID, so the parent is then unknown.
func commentParent(m map[string]any) (string, bool, error) {
	parent, known := "", false
	take := func(v string) error {
		if known && v != parent {
			return fail("scope_mismatch")
		}
		parent, known = v, true
		return nil
	}
	if v, ok := m["issueId"].(string); ok && ValidID(v) {
		if e := take(v); e != nil {
			return "", false, e
		}
	}
	if nested := obj(m, "issue"); nested != nil {
		if v, e := id(nested); e == nil {
			if e = take(v); e != nil {
				return "", false, e
			}
		}
	}
	return parent, known, nil
}

// commentPages bounds the read that confirms a comment's issue: 4 pages of up
// to 50 in Linear's default order, each retried smaller when it is larger than
// the response cap. That order is last update, newest first (observed, not
// documented), so the new comment is normally on page 1; if it is not found
// within the bound, the receipt stays unverified (see docs/linear.md).
const commentPages = 4

func (p *Provider) commentOnIssue(ctx context.Context, issue, identifier, comment string) (bool, error) {
	row, e := p.findComment(ctx, issue, identifier, comment)
	return row != nil, e
}

// findComment returns the row of comment found by the commentPages read of
// issue, or nil. A row found on a page is returned even when that page's
// continuation is unusable.
func (p *Provider) findComment(ctx context.Context, issue, identifier, comment string) (map[string]any, error) {
	cursor, size, calls := "", commentPageSizeV2, 0
	for page := 0; page < commentPages; page++ {
		m, e := p.pageShrinking(ctx, &size, &calls, commentCallsV2, func(limit int) (map[string]any, int, error) {
			args := []arg{a(issue, "issueId"), {[]string{"limit"}, limit, "integer"}}
			if cursor != "" {
				args = append(args, a(cursor, "cursor"))
			}
			m, e := p.call(ctx, []string{"list_comments"}, args...)
			return m, limit, e
		})
		if e != nil {
			return nil, e
		}
		// The page's rows are searched before an unusable continuation fails
		// the read, so a comment on this page is still found. A row that names
		// another issue is not evidence.
		rows, next, done, e := listRows(m, "comments")
		for _, row := range rows {
			if i, e := id(row); e == nil && i == comment && commentIssueRef(row, issue, identifier) == nil {
				return row, nil
			}
		}
		if e != nil {
			return nil, e
		}
		if done || next == "" || next == cursor {
			return nil, nil
		}
		cursor = next
	}
	return nil, nil
}

// commentReplyReason is why a threaded reply cannot be written: the comment
// write must accept and document parentId, and the comment read that proves
// the parent and confirms the receipt must be pinned.
func (p *Provider) commentReplyReason() string {
	sample := "00000000-0000-4000-8000-000000000001"
	name, _, e := p.v2Negotiation([]string{"create_comment", "save_comment"}, []arg{a("body", "body"), a(sample, "parentId")})
	if e != nil {
		return "no comment write tool accepts parentId"
	}
	if !strings.Contains(p.tools[name].Description, "parentId") {
		return name + " no longer documents replies by parentId"
	}
	if reason := p.commentsReason(); reason != "" {
		return "comment read unavailable: " + reason
	}
	return ""
}

// IssueHasComment reports whether comment is one of issue's comments, by the
// same bounded read that confirms a comment receipt (commentPages pages,
// newest first). A comment beyond that read is reported absent. When the
// comment is itself a reply, repliesTo is the comment it answers ("?" when
// the row names a parent that is not a UUID).
func (p *Provider) IssueHasComment(ctx context.Context, issue, identifier, comment string) (found bool, repliesTo string, err error) {
	if !ValidID(issue) || !ValidID(comment) || (identifier != "" && !ValidIssueIdentifier(identifier)) {
		return false, "", fail("invalid")
	}
	if p.commentReplyReason() != "" {
		return false, "", fail("capability_unavailable")
	}
	row, e := p.findComment(ctx, issue, identifier, comment)
	if row == nil {
		return false, "", e
	}
	if parent, named := commentReplyTo(row); named && parent != "" {
		if !ValidID(parent) {
			parent = "?"
		}
		return true, parent, nil
	}
	return true, "", nil
}

// ReplyOnIssue posts body as a reply to parent, which the caller has proven
// is a comment on issue (IssueHasComment). Linear ignores the issue when a
// parent is given, so only the parent and body are sent, and that proof is
// the only link to the issue. The receipt is accepted only when
// it names this issue and this parent; when it leaves either out, a read of
// the issue's comments must find the new comment with this parent. A receipt
// or row naming another parent, or none, is a receipt_mismatch.
func (p *Provider) ReplyOnIssue(ctx context.Context, issue, identifier, parent, body string) (Receipt, error) {
	if !ValidID(issue) || !ValidID(parent) || body == "" || !textOK(body, MaxBodyBytes) || (identifier != "" && !ValidIssueIdentifier(identifier)) {
		return Receipt{}, fail("invalid")
	}
	if p.commentReplyReason() != "" {
		return Receipt{}, fail("capability_unavailable")
	}
	m, e := p.call(ctx, []string{"create_comment", "save_comment"}, a(body, "body"), a(parent, "parentId"))
	if e != nil {
		return Receipt{}, e
	}
	m = unwrap(m, "comment")
	i, e := id(m)
	if e != nil {
		return Receipt{}, e
	}
	owner, known, e := commentParent(m)
	if e != nil {
		return Receipt{}, e
	}
	if known && owner != issue {
		return Receipt{}, fail("scope_mismatch")
	}
	replyTo, named := commentReplyTo(m)
	if (named && replyTo != parent) || !commentBodyMatches(m, body) {
		return Receipt{}, fail("receipt_mismatch")
	}
	if !known || !named {
		row, e := p.findComment(ctx, issue, identifier, i)
		if e != nil || row == nil {
			return Receipt{}, fail("receipt_unverified")
		}
		if replyTo, named = commentReplyTo(row); !named || replyTo != parent || !commentBodyMatches(row, body) {
			return Receipt{}, fail("receipt_mismatch")
		}
	}
	return Receipt{ID: i, IssueID: issue, ParentID: parent, BodySHA256: bodySHA256(body)}, nil
}

// commentReplyTo reads the parent comment a comment row or receipt names,
// flat (parentId) or nested (parent.id). named is true whenever either key
// is present, so an explicit null names no parent and reads as "".
func commentReplyTo(m map[string]any) (string, bool) {
	if raw, present := m["parentId"]; present {
		v, _ := raw.(string)
		return v, true
	}
	if raw, present := m["parent"]; present {
		nested, _ := raw.(map[string]any)
		v, _ := nested["id"].(string)
		return v, true
	}
	return "", false
}
