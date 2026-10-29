package linearprovider

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
)

// Project planning fields (FR13): save_project {id, priority, targetDate,
// startDate}, only the ones given. The *Resolution fields are never sent;
// their presence in the schema, as optional properties, is what says a date
// alone is accepted.

// ProjectPlanFields are the fields a plan can set, in the tool's spelling.
var ProjectPlanFields = []string{"priority", "targetDate", "startDate"}

// ProjectPlanReason is why setting fields is unavailable, or "": save_project
// must take id plus each named field with the recorded catalog's type
// (priority an integer 0-4; the dates a string or null), and offer the
// matching *Resolution property as an optional one.
func (p *Provider) ProjectPlanReason(fields []string) string {
	t := p.tools["save_project"]
	if t == nil {
		return "no save_project tool"
	}
	props, required, e := schema(t)
	if e != nil {
		return "save_project schema unreadable"
	}
	if _, ok := field(props, []string{"id"}, "string"); !ok {
		return "save_project has no id"
	}
	for _, r := range required {
		if r != "id" {
			return "save_project schema changed: " + r + " is required"
		}
	}
	for _, f := range fields {
		prop, ok := props[f].(map[string]any)
		switch f {
		case "priority":
			if !ok {
				return "save_project has no priority"
			}
			lo, loOK := prop["minimum"].(float64)
			hi, hiOK := prop["maximum"].(float64)
			if prop["type"] != "integer" || !loOK || !hiOK || lo != 0 || hi != 4 {
				return "save_project schema changed: priority"
			}
		case "targetDate", "startDate":
			if !ok {
				return "save_project has no " + f
			}
			if typedVariant(prop, "string") == nil || typedVariant(prop, "null") == nil {
				return "save_project schema changed: " + f
			}
			if _, ok := props[f+"Resolution"].(map[string]any); !ok {
				return "save_project has no " + f + "Resolution"
			}
		default:
			return "save_project plan has no field " + f
		}
	}
	return ""
}

// ProjectPlan is what to set. A nil Priority is not sent; a date Field that
// is Present with a nil Value clears the date (sent as JSON null).
type ProjectPlan struct {
	Priority              *int
	TargetDate, StartDate Field[*string]
}

func (pl ProjectPlan) fields() []string {
	var out []string
	if pl.Priority != nil {
		out = append(out, "priority")
	}
	if pl.TargetDate.Present {
		out = append(out, "targetDate")
	}
	if pl.StartDate.Present {
		out = append(out, "startDate")
	}
	return out
}

// SaveProjectPlan sends save_project {id: project, <the given fields>}. The
// caller reads the project back to learn whether it took.
func (p *Provider) SaveProjectPlan(ctx context.Context, project string, plan ProjectPlan) error {
	fields := plan.fields()
	if !ValidID(project) || len(fields) == 0 {
		return fail("invalid")
	}
	if plan.Priority != nil && (*plan.Priority < 0 || *plan.Priority > 4) {
		return fail("invalid")
	}
	args := []arg{a(project, "id")}
	if plan.Priority != nil {
		args = append(args, arg{[]string{"priority"}, *plan.Priority, "integer"})
	}
	for _, d := range []struct {
		key string
		f   Field[*string]
	}{{"targetDate", plan.TargetDate}, {"startDate", plan.StartDate}} {
		switch {
		case !d.f.Present:
		case d.f.Value == nil:
			args = append(args, arg{[]string{d.key}, nil, "null"})
		case !validPlanDate(*d.f.Value):
			return fail("invalid")
		default:
			args = append(args, arg{[]string{d.key}, *d.f.Value, "string"})
		}
	}
	if p.ProjectPlanReason(fields) != "" {
		return fail("capability_unavailable")
	}
	_, e := p.v2Call(ctx, []string{"save_project"}, args...)
	return e
}

// Priority is the project's priority 0-4 as the snapshot read it; false when
// the read left it out or gave another shape.
func (s ProjectSnapshot) Priority() (int, bool) {
	var raw any
	if !s.rawField("priority", &raw) {
		return 0, false
	}
	p, ok := parsePriority(raw)
	if !ok {
		return 0, false
	}
	return *p, true
}

// TargetDate and StartDate are YYYY-MM-DD, or "" with ok when the read said
// none; ok is false when the read left the field out or gave another shape.
func (s ProjectSnapshot) TargetDate() (string, bool) { return s.date("targetDate") }
func (s ProjectSnapshot) StartDate() (string, bool)  { return s.date("startDate") }

func (s ProjectSnapshot) date(key string) (string, bool) {
	var raw any
	if !s.rawField(key, &raw) {
		return "", false
	}
	d, ok := parseDateValue(raw)
	if !ok {
		return "", false
	}
	if d == nil {
		return "", true
	}
	return *d, true
}

// Resolution is a date's precision as Linear reported it ("month",
// "quarter", ...), or "" for an exact day. field is "targetDate" or
// "startDate" (the *Resolution spelling is accepted too); ok is false when
// the read left it out or gave another shape.
func (s ProjectSnapshot) Resolution(field string) (string, bool) {
	switch field {
	case "targetDate", "startDate":
		field += "Resolution"
	case "targetDateResolution", "startDateResolution":
	default:
		return "", false
	}
	var raw any
	if !s.rawField(field, &raw) {
		return "", false
	}
	switch v := raw.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	}
	return "", false
}

func (s ProjectSnapshot) rawField(key string, into *any) bool {
	text, ok := s.fields[key]
	return ok && json.Unmarshal([]byte(text), into) == nil
}

// WithField is s with one compared field (status, state, the dates, their
// resolutions, labels, priority or name) as get_project would give it: a
// fake provider's snapshot. Any other key is ignored.
func (s ProjectSnapshot) WithField(key string, value any) ProjectSnapshot {
	if !slices.Contains(projectSnapshotFields, key) {
		return s
	}
	b, e := json.Marshal(value)
	if e != nil {
		return s
	}
	fields := maps.Clone(s.fields)
	if fields == nil {
		fields = map[string]string{}
	}
	fields[key] = string(b)
	s.fields = fields
	return s
}
