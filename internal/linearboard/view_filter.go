package linearboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"clawbridge/internal/linearprovider"
)

// A view's filter is Linear's IssueFilter. ClawBridge stores it as people
// wrote it and evaluates for agents only a subset it can prove on the rows
// it reads (LINEAR-VIEWS-PLAN §13.2, §13.4): team, project, labels, state,
// assignee and priority, joined by one level of and. A filter using
// anything else is still a view for people; agents can't filter by it.

const (
	viewFilterMaxBytes = 16 << 10
	viewFilterMaxDepth = 12
	viewFilterMaxList  = 20
	viewFilterMaxParts = 8
)

// Agent filter answers.
const (
	AgentFilterAvailable   = "available"
	AgentFilterUnavailable = "unavailable"
)

var viewUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var viewStateTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true, "completed": true, "canceled": true}

// ViewFilter is a parsed view filter: its canonical text and digest, and
// whether agents can filter by it.
type ViewFilter struct {
	Canonical string
	SHA256    string
	// Uses names what puts the filter outside the subset, e.g. "or",
	// "priority.gt"; empty when agents can use it.
	Uses    []string
	clauses []viewClause
	raw     any
}

// AgentFilter is AgentFilterAvailable or AgentFilterUnavailable.
func (f ViewFilter) AgentFilter() string {
	if len(f.Uses) > 0 {
		return AgentFilterUnavailable
	}
	return AgentFilterAvailable
}

// viewClause is one condition of the subset; a row must meet every clause.
type viewClause struct {
	key    string // team, project, labels, state_type, state_id, assignee, priority
	ids    []string
	null   bool // assignee: {null: true}
	op     string
	number int
	ints   []int
}

// ErrViewFilter is wrapped by every refusal of a filter's shape or values.
var ErrViewFilter = errors.New("view filter")

func filterError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrViewFilter, fmt.Sprintf(format, args...))
}

// ParseViewFilter reads a filter: one JSON object, at most 16 KiB and 12
// levels deep, with UUIDs in lower case. Values the subset evaluates are
// checked (state types, priorities, list sizes); keys outside it are
// recorded in Uses, not refused. IDs are checked by CheckIDs.
func ParseViewFilter(text []byte) (ViewFilter, error) {
	if len(text) > viewFilterMaxBytes {
		return ViewFilter{}, filterError("larger than 16 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return ViewFilter{}, filterError("not JSON")
	}
	if dec.More() {
		return ViewFilter{}, filterError("more than one JSON value")
	}
	top, ok := raw.(map[string]any)
	if !ok || len(top) == 0 {
		return ViewFilter{}, filterError("not a non-empty object")
	}
	if err := walkFilter(raw, "filter", 0, func(string, string) error { return nil }); err != nil {
		return ViewFilter{}, err
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		return ViewFilter{}, filterError("not JSON")
	}
	sum := sha256.Sum256(canonical)
	f := ViewFilter{Canonical: string(canonical), SHA256: hex.EncodeToString(sum[:]), raw: raw}
	uses := map[string]bool{}
	if err = compileFilter(top, 0, &f.clauses, uses); err != nil {
		return ViewFilter{}, err
	}
	for use := range uses {
		f.Uses = append(f.Uses, use)
	}
	sort.Strings(f.Uses)
	if len(f.Uses) > 0 {
		f.clauses = nil
	}
	return f, nil
}

// walkFilter visits every string value with its path, and enforces the
// depth bound and lower-case UUIDs.
func walkFilter(node any, path string, depth int, visit func(path, value string) error) error {
	if depth > viewFilterMaxDepth {
		return filterError("deeper than %d levels", viewFilterMaxDepth)
	}
	switch v := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := walkFilter(v[k], path+"."+k, depth+1, visit); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range v {
			if err := walkFilter(item, fmt.Sprintf("%s[%d]", path, i), depth+1, visit); err != nil {
				return err
			}
		}
	case string:
		if viewUUID.MatchString(v) && v != strings.ToLower(v) {
			return filterError("%s: write IDs in lower case", path)
		}
		return visit(path, v)
	}
	return nil
}

// ViewScope is what a filter may name: the view's own team, and the
// binding's projects, its teams' labels and states, and their members.
type ViewScope struct {
	Team     string
	Projects map[string]bool
	Labels   map[string]bool
	States   map[string]bool
	Members  map[string]bool
}

// CheckIDs refuses a filter carrying any UUID, under any key, that isn't
// the view's team or one of the binding's projects, labels, states or
// members, naming its path. A team view never names another team.
func (f ViewFilter) CheckIDs(scope ViewScope) error {
	return walkFilter(f.raw, "filter", 0, func(path, value string) error {
		if !viewUUID.MatchString(value) {
			return nil
		}
		if value == scope.Team || scope.Projects[value] || scope.Labels[value] || scope.States[value] || scope.Members[value] {
			return nil
		}
		return filterError("%s names an ID outside this view's team and the binding", path)
	})
}

// compileFilter turns the subset into clauses and records what lies
// outside it. An and is allowed at the top only.
func compileFilter(node map[string]any, depth int, out *[]viewClause, uses map[string]bool) error {
	keys := make([]string, 0, len(node))
	for k := range node {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := node[key]
		switch key {
		case "and":
			parts, ok := value.([]any)
			if depth > 0 || !ok || len(parts) == 0 || len(parts) > viewFilterMaxParts {
				uses["and"] = true
				continue
			}
			for _, part := range parts {
				m, ok := part.(map[string]any)
				if !ok {
					uses["and"] = true
					continue
				}
				if err := compileFilter(m, depth+1, out, uses); err != nil {
					return err
				}
			}
		case "team":
			c, use, err := idClause(key, value, "id", false, false)
			if err := keep(c, use, err, out, uses); err != nil {
				return err
			}
		case "project":
			c, use, err := idClause(key, value, "id", true, false)
			if err := keep(c, use, err, out, uses); err != nil {
				return err
			}
		case "labels":
			some, ok := single(value, "some")
			if !ok {
				uses[key+"."+firstKey(value)] = true
				continue
			}
			c, use, err := idClause(key, some, "id", true, false)
			if use != "" {
				use = "labels.some." + strings.TrimPrefix(use, "labels.")
			}
			if err := keep(c, use, err, out, uses); err != nil {
				return err
			}
		case "assignee":
			c, use, err := idClause(key, value, "id", false, true)
			if err := keep(c, use, err, out, uses); err != nil {
				return err
			}
		case "state":
			c, use, err := stateClause(value)
			if err := keep(c, use, err, out, uses); err != nil {
				return err
			}
		case "priority":
			c, use, err := priorityClause(value)
			if err := keep(c, use, err, out, uses); err != nil {
				return err
			}
		default:
			uses[key] = true
		}
	}
	return nil
}

func keep(c viewClause, use string, err error, out *[]viewClause, uses map[string]bool) error {
	switch {
	case err != nil:
		return err
	case use != "":
		uses[use] = true
	default:
		*out = append(*out, c)
	}
	return nil
}

// single returns the value under an object's only key, when that key is want.
func single(node any, want string) (any, bool) {
	m, ok := node.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, false
	}
	v, ok := m[want]
	return v, ok
}

// firstKey names an unsupported shape for Uses: its first key, or "?".
func firstKey(node any) string {
	m, ok := node.(map[string]any)
	if !ok || len(m) == 0 {
		return "?"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}

// idClause reads {field: {eq: ID}} or, when allowIn, {field: {in: [IDs]}};
// with allowNull also {null: true}.
func idClause(key string, value any, field string, allowIn, allowNull bool) (viewClause, string, error) {
	if allowNull {
		if v, ok := single(value, "null"); ok {
			if v == true {
				return viewClause{key: key, null: true}, "", nil
			}
			return viewClause{}, key + ".null", nil
		}
	}
	cmp, ok := single(value, field)
	if !ok {
		return viewClause{}, key + "." + firstKey(value), nil
	}
	m, _ := cmp.(map[string]any)
	if len(m) != 1 {
		return viewClause{}, key + "." + field + "." + firstKey(cmp), nil
	}
	if id, ok := m["eq"]; ok {
		text, ok := id.(string)
		if !ok || !viewUUID.MatchString(text) {
			return viewClause{}, "", filterError("%s.%s.eq is not an ID", key, field)
		}
		return viewClause{key: key, ids: []string{text}}, "", nil
	}
	if list, ok := m["in"]; ok && allowIn {
		ids, err := idList(list, key+"."+field+".in")
		return viewClause{key: key, ids: ids}, "", err
	}
	return viewClause{}, key + "." + field + "." + firstKey(cmp), nil
}

func idList(list any, path string) ([]string, error) {
	items, ok := list.([]any)
	if !ok || len(items) == 0 || len(items) > viewFilterMaxList {
		return nil, filterError("%s takes 1 to %d IDs", path, viewFilterMaxList)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || !viewUUID.MatchString(text) {
			return nil, filterError("%s holds something other than an ID", path)
		}
		ids = append(ids, text)
	}
	return ids, nil
}

// stateClause reads {type: {in: [types]}} or {id: {in: [IDs]}}.
func stateClause(value any) (viewClause, string, error) {
	if cmp, ok := single(value, "type"); ok {
		list, ok := single(cmp, "in")
		if !ok {
			return viewClause{}, "state.type." + firstKey(cmp), nil
		}
		items, ok := list.([]any)
		if !ok || len(items) == 0 || len(items) > len(viewStateTypes) {
			return viewClause{}, "", filterError("state.type.in takes 1 to %d state types", len(viewStateTypes))
		}
		c := viewClause{key: "state_type"}
		for _, item := range items {
			text, ok := item.(string)
			if !ok || !viewStateTypes[text] {
				return viewClause{}, "", filterError("state.type.in holds something other than Linear's state types (triage, backlog, unstarted, started, completed, canceled)")
			}
			c.ids = append(c.ids, text)
		}
		return c, "", nil
	}
	if cmp, ok := single(value, "id"); ok {
		list, ok := single(cmp, "in")
		if !ok {
			return viewClause{}, "state.id." + firstKey(cmp), nil
		}
		ids, err := idList(list, "state.id.in")
		return viewClause{key: "state_id", ids: ids}, "", err
	}
	return viewClause{}, "state." + firstKey(value), nil
}

// priorityClause reads {eq|lt|lte|gt|gte: n} or {in: [n...]}, n in 0-4.
// Comparisons are plain numeric, as Linear's own IssueFilter evaluates them:
// no priority is 0, so lte 2 includes it and gte 2 doesn't (the tester's
// read-only probe on 27 Oct, live/keyprobe-ledger.txt).
func priorityClause(value any) (viewClause, string, error) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != 1 {
		return viewClause{}, "priority." + firstKey(value), nil
	}
	op := firstKey(value)
	switch op {
	case "eq", "lt", "lte", "gt", "gte":
		n, err := priorityValue(m[op], "priority."+op)
		return viewClause{key: "priority", op: op, number: n}, "", err
	case "in":
		items, ok := m[op].([]any)
		if !ok || len(items) == 0 || len(items) > 5 {
			return viewClause{}, "", filterError("priority.in takes 1 to 5 priorities")
		}
		c := viewClause{key: "priority", op: op}
		for _, item := range items {
			n, err := priorityValue(item, "priority.in")
			if err != nil {
				return viewClause{}, "", err
			}
			c.ints = append(c.ints, n)
		}
		return c, "", nil
	}
	return viewClause{}, "priority." + op, nil
}

func priorityValue(raw any, path string) (int, error) {
	n, ok := raw.(json.Number)
	if !ok {
		return 0, filterError("%s is not a priority (0 none, 1 urgent, 2 high, 3 medium, 4 low)", path)
	}
	v, err := n.Int64()
	if err != nil || v < 0 || v > 4 {
		return 0, filterError("%s is not a priority (0 none, 1 urgent, 2 high, 3 medium, 4 low)", path)
	}
	return int(v), nil
}

// Projects is the filter's project IDs, when it narrows by project, for
// one issue query per project; nil when it doesn't.
func (f ViewFilter) Projects() []string {
	for _, c := range f.clauses {
		if c.key == "project" {
			return append([]string(nil), c.ids...)
		}
	}
	return nil
}

// Match reports whether an issue row meets the filter. A row lacking what
// a clause needs fails closed: unknown names it (assignee_unknown,
// priority_unknown, labels_unknown, project_unknown, state_unknown) so the
// caller can count it. stateTypeOf maps a state ID to its type. Match must
// not be called on a filter agents can't use.
func (f ViewFilter) Match(row linearprovider.IssueV2, stateTypeOf map[string]string) (match bool, unknown string) {
	if len(f.Uses) > 0 {
		return false, "agent_filter_unavailable"
	}
	for _, c := range f.clauses {
		switch c.key {
		case "team":
			if !has(c.ids, row.TeamID) {
				return false, ""
			}
		case "project":
			if !row.ProjectKnown {
				return false, "project_unknown"
			}
			if !has(c.ids, row.ProjectID) {
				return false, ""
			}
		case "labels":
			if !row.LabelsKnown {
				return false, "labels_unknown"
			}
			if !anyOf(c.ids, row.LabelIDs) {
				return false, ""
			}
		case "state_id":
			if row.StateID == "" {
				return false, "state_unknown"
			}
			if !has(c.ids, row.StateID) {
				return false, ""
			}
		case "state_type":
			kind, ok := stateTypeOf[row.StateID]
			if row.StateID == "" || !ok || kind == "" {
				return false, "state_unknown"
			}
			if !has(c.ids, kind) {
				return false, ""
			}
		case "assignee":
			if !row.AssigneeKnown {
				return false, "assignee_unknown"
			}
			if c.null != (row.AssigneeID == "") || (!c.null && row.AssigneeID != c.ids[0]) {
				return false, ""
			}
		case "priority":
			if !row.PriorityKnown || row.Priority == nil {
				return false, "priority_unknown"
			}
			p := *row.Priority
			switch c.op {
			case "eq", "lt", "lte", "gt", "gte":
				if !comparePriority(p, c.op, c.number) {
					return false, ""
				}
			case "in":
				found := false
				for _, n := range c.ints {
					found = found || n == p
				}
				if !found {
					return false, ""
				}
			}
		}
	}
	return true, ""
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func anyOf(want, have []string) bool {
	for _, h := range have {
		if has(want, h) {
			return true
		}
	}
	return false
}

// comparePriority is Linear's numeric comparison: 0 (no priority) is the
// smallest value, so it counts as below Urgent (1).
func comparePriority(p int, op string, n int) bool {
	switch op {
	case "eq":
		return p == n
	case "lt":
		return p < n
	case "lte":
		return p <= n
	case "gt":
		return p > n
	case "gte":
		return p >= n
	}
	return false
}
