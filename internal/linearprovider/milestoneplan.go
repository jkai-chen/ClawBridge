package linearprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Creating and updating a project's milestones (FR55 PR 2): save_milestone
// {project, id?, name?, targetDate?}, one tool for both. Only the given
// fields are sent; id goes as a UUID, never a name; description is never
// sent.

// MilestonePlanFields are the fields a plan can set, in the tool's spelling.
var MilestonePlanFields = []string{"name", "targetDate"}

// MilestoneCreateRefused is the refusal text when the connection can't.
const MilestoneCreateRefused = "this Linear connection can't create or update milestones; nothing was sent"

// MilestoneNameMax is the longest name, in characters.
const MilestoneNameMax = 80

// MilestonePlanReason is why a milestone can't be created (update false) or
// updated (update true) with fields, or "": save_milestone must take project
// as a string, id as a string for an update, and each named field with the
// recorded type (name a string; targetDate a string or null). Nothing else
// may be required.
func (p *Provider) MilestonePlanReason(update bool, fields []string) string {
	t := p.tools["save_milestone"]
	if t == nil {
		return "no save_milestone tool"
	}
	props, required, e := schema(t)
	if e != nil {
		return "save_milestone schema unreadable"
	}
	if _, ok := field(props, []string{"project"}, "string"); !ok {
		return "save_milestone has no project"
	}
	for _, r := range required {
		if r != "project" {
			return "save_milestone schema changed: " + r + " is required"
		}
	}
	if update {
		if _, ok := field(props, []string{"id"}, "string"); !ok {
			return "save_milestone has no id"
		}
	}
	for _, f := range fields {
		prop, ok := props[f].(map[string]any)
		switch f {
		case "name":
			if !ok || prop["type"] != "string" {
				return "save_milestone schema changed: name"
			}
		case "targetDate":
			if !ok || typedVariant(prop, "string") == nil || typedVariant(prop, "null") == nil {
				return "save_milestone schema changed: targetDate"
			}
		default:
			return "save_milestone plan has no field " + f
		}
	}
	return ""
}

// MilestonePlan is what to save. An empty ID creates (Name is then
// required). A nil Name is not sent; a TargetDate Present with a nil Value
// clears the date (sent as JSON null).
type MilestonePlan struct {
	Project, ID string
	Name        *string
	TargetDate  Field[*string]
}

// Fields are the planned fields' names, in the tool's spelling.
func (pl MilestonePlan) Fields() []string {
	var out []string
	if pl.Name != nil {
		out = append(out, "name")
	}
	if pl.TargetDate.Present {
		out = append(out, "targetDate")
	}
	return out
}

// SaveMilestone sends save_milestone {project, id?, <the given fields>} and
// returns the ID of the milestone the answer names, or "" when it names
// none (the caller reads the project's milestones back either way).
func (p *Provider) SaveMilestone(ctx context.Context, plan MilestonePlan) (string, error) {
	fields := plan.Fields()
	update := plan.ID != ""
	switch {
	case !ValidID(plan.Project), update && !ValidID(plan.ID), len(fields) == 0, !update && plan.Name == nil:
		return "", fail("invalid")
	}
	args := []arg{a(plan.Project, "project")}
	if update {
		args = append(args, a(plan.ID, "id"))
	}
	if plan.Name != nil {
		name, e := CleanMilestoneName(*plan.Name)
		if e != nil || name != *plan.Name {
			return "", fail("invalid")
		}
		args = append(args, a(name, "name"))
	}
	switch {
	case !plan.TargetDate.Present:
	case plan.TargetDate.Value == nil:
		args = append(args, arg{[]string{"targetDate"}, nil, "null"})
	case !validPlanDate(*plan.TargetDate.Value):
		return "", fail("invalid")
	default:
		args = append(args, arg{[]string{"targetDate"}, *plan.TargetDate.Value, "string"})
	}
	if p.MilestonePlanReason(update, fields) != "" {
		return "", fail("capability_unavailable")
	}
	out, e := p.v2Call(ctx, []string{"save_milestone"}, args...)
	return savedMilestoneID(out), e
}

// savedMilestoneID is the milestone ID the save answered with, at the top
// level or under "milestone"; "" when it names none or not a UUID.
func savedMilestoneID(out map[string]any) string {
	id, _ := out["id"].(string)
	if m, ok := out["milestone"].(map[string]any); ok && id == "" {
		id, _ = m["id"].(string)
	}
	id = strings.ToLower(id)
	if !ValidID(id) {
		return ""
	}
	return id
}

// CleanMilestoneName is a milestone name as it is sent: trimmed, 1 to 80
// characters, with no control character, bidi control (U+202A–U+202E,
// U+2066–U+2069, U+200E, U+200F, U+061C) or zero-width character (U+200B–
// U+200D, U+2060, U+FEFF) in it (FR55 N2).
func CleanMilestoneName(s string) (string, error) {
	name := strings.TrimSpace(s)
	if n := utf8.RuneCountInString(name); n < 1 || n > MilestoneNameMax {
		return "", errors.New("a milestone's name is 1–80 characters")
	}
	if !utf8.ValidString(name) {
		return "", errors.New("a milestone's name must be valid UTF-8")
	}
	for _, r := range name {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return "", errors.New("a milestone's name has a control character")
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
			return "", errors.New("a milestone's name has a bidirectional control character")
		case r >= 0x200b && r <= 0x200d, r == 0x2060, r == 0xfeff:
			return "", errors.New("a milestone's name has a zero-width character")
		}
	}
	return name, nil
}

// MilestoneNameKey is a name as two names are compared: Unicode case-folded
// and NFC, so "Beta" and "beta", and a composed and a decomposed é, are one.
func MilestoneNameKey(name string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(strings.TrimSpace(name))))
}

// MilestoneNameHash is the SHA-256 of the name's key: all the server ever
// stores of a created milestone's name (FR55 N1).
func MilestoneNameHash(name string) string {
	sum := sha256.Sum256([]byte(MilestoneNameKey(name)))
	return hex.EncodeToString(sum[:])
}
