package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Seeding the project kind's labels (PROJECT-KIND-LABELS-PLAN P2): make the
// setting's group and values as project labels of its team, or adopt the
// ones already there, over the owner's own MCP profile. The preview reads
// the team's and the workspace's project labels and plans every name;
// nothing is sent while any entry is refused (K4). The confirm carries the
// preview's plan digest. Each create is one ledger operation, read back
// in the team's list; the proven IDs are then recorded in the setting.

// kindLabels is the owner's MCP connection as the seed uses it;
// *linearprovider.Provider implements it.
type kindLabels interface {
	ProjectLabelsReason() string
	ProjectLabels(ctx context.Context, team string) ([]linearprovider.ProjectLabelRow, error)
	CreateProjectLabelGroup(ctx context.Context, team, name string) (string, error)
	CreateProjectLabelChoice(ctx context.Context, team, parent, name string) (string, error)
}

// KindSeedInput is one seed: without Confirm a preview.
type KindSeedInput struct {
	OperationID     string
	Confirm         bool
	ExpectedPlanSHA string
}

// KindSeedEntry is the plan, and with Confirm the outcome, for the group or
// one value.
type KindSeedEntry struct {
	Name string `json:"name"`
	// Action is create, adopt or refused.
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	LabelID string `json:"label_id,omitempty"`
	// Outcome is a create's: accepted or uncertain.
	Outcome string `json:"outcome,omitempty"`
}

// KindSeedResult is a seed's preview or outcome.
type KindSeedResult struct {
	Preview  bool            `json:"preview"`
	Scope    string          `json:"scope"`
	TeamID   string          `json:"team_id"`
	Revision int64           `json:"revision"`
	Group    KindSeedEntry   `json:"group"`
	Values   []KindSeedEntry `json:"values"`
	// Untouched are the adopted group's choices the setting doesn't name:
	// left as they are.
	Untouched []string `json:"untouched,omitempty"`
	Refused   int      `json:"refused"`
	PlanSHA   string   `json:"plan_sha256"`
	// Stopped names the entry a confirmed seed stopped at.
	Stopped  string `json:"stopped,omitempty"`
	Recorded bool   `json:"recorded"`
	// RecordError is set when the labels were proven but storing their IDs
	// failed; running the same command again records them.
	RecordError string `json:"record_error,omitempty"`
}

// kindClash is what a refused name offers (PROJECT-KIND-LABELS-PLAN §4).
const kindClash = "exists_in_workspace: a workspace project label has this name, and Linear refuses a team label that takes it; choose another name (linear kind set), or rename the workspace label in Linear (Settings, Projects, Labels)"

// KindSeed previews or makes the project kind's labels.
func (s *Service) KindSeed(ctx context.Context, scope bus.Request, in KindSeedInput) (KindSeedResult, error) {
	var none KindSeedResult
	if !operationPattern.MatchString(in.OperationID) || len(in.OperationID) > 100 {
		return none, errors.New("operation_id requires 1–100 ASCII identifier characters (each label adds a suffix)")
	}
	if scope.Session != "" {
		return none, errors.New("a session can't seed the project kind; the project owner runs linear kind seed in their own terminal")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return none, e
	}
	if !b.Configured {
		return none, errors.New("task-board is not linked")
	}
	if s.cfg.Authority == "" {
		return none, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return none, e
	}
	if access.ProjectRole != "owner" || access.SessionID != "" {
		return none, errors.New("only the project owner, without a session, seeds the project kind")
	}
	setting, e := s.kindSetting(ctx, scope)
	if e != nil {
		return none, e
	}
	profile, p, done, e := s.openForWrite(ctx)
	if e != nil {
		return none, e
	}
	defer done()
	if profile.Access != linearconn.Write {
		return none, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	labels, ok := p.(kindLabels)
	if !ok {
		return none, errors.New("native provider lacks the project label tools")
	}
	if reason := labels.ProjectLabelsReason(); reason != "" {
		return none, errors.New("Linear's project label tools are unavailable: " + reason)
	}
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return none, e
	}
	read, e := readKindLabels(ctx, labels, setting.team)
	if e != nil {
		return none, e
	}
	out := planKindSeed(setting, read)
	out.Preview = !in.Confirm
	teamName := ""
	if got, e := p.GetTeam(ctx, setting.team); e == nil && got.ID == setting.team {
		teamName = got.Name
	}
	out.Scope = kindScopeLine(setting.team, teamName)
	out.PlanSHA = kindPlanSHA(out)
	if !in.Confirm {
		return out.KindSeedResult, nil
	}
	if out.Refused > 0 {
		out.Preview = true
		return out.KindSeedResult, fmt.Errorf("%d of the kind's names are refused; nothing was sent (see the preview)", out.Refused)
	}
	switch in.ExpectedPlanSHA {
	case "":
		return out.KindSeedResult, errors.New("a confirm carries the preview's plan_sha256; repeat the command the preview printed")
	case out.PlanSHA:
	default:
		out.Preview = true
		return out.KindSeedResult, errors.New("the plan changed since the preview (see this preview); preview again; nothing was sent")
	}
	return s.sendKindSeed(ctx, b, access.SessionID, in.OperationID, scope, setting, labels, out.KindSeedResult)
}

// kindSettingRow is the stored setting the seed works from.
type kindSettingRow struct {
	revision     int64
	team, group  string
	groupLabelID string
	values       []bus.ProjectKindValue
}

func (s *Service) kindSetting(ctx context.Context, scope bus.Request) (kindSettingRow, error) {
	data, e := s.ProjectKind(ctx, scope)
	if e != nil {
		return kindSettingRow{}, e
	}
	raw, _ := json.Marshal(data)
	var m struct {
		Revision     int64                  `json:"revision"`
		TeamID       string                 `json:"team_id"`
		Scope        string                 `json:"scope"`
		GroupName    string                 `json:"group_name"`
		GroupLabelID string                 `json:"group_label_id"`
		Values       []bus.ProjectKindValue `json:"values"`
		TeamBound    *bool                  `json:"team_bound"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return kindSettingRow{}, errors.New("the project kind setting is unreadable")
	}
	switch {
	case m.Revision == 0 || len(m.Values) == 0:
		return kindSettingRow{}, errors.New("no project kind is set; set it first: clawbridge linear kind set --values A,B,C --expected-revision 0")
	case m.Scope != "team" || !linearprovider.ValidID(m.TeamID):
		return kindSettingRow{}, errors.New("the project kind setting is unreadable")
	case m.TeamBound == nil || !*m.TeamBound:
		// Review L1: a binding change can drop the kind's team.
		return kindSettingRow{}, errors.New("the project kind's team left the binding; set the kind again on a bound team (linear kind set --team UUID)")
	}
	return kindSettingRow{m.Revision, m.TeamID, m.GroupName, m.GroupLabelID, m.Values}, nil
}

// kindRead is the project labels a seed reads: the team's own (listed with
// the team, absent from the workspace's list) and the workspace's.
type kindRead struct {
	team, workspace []linearprovider.ProjectLabelRow
}

func readKindLabels(ctx context.Context, labels kindLabels, team string) (kindRead, error) {
	withTeam, e := labels.ProjectLabels(ctx, team)
	if e != nil {
		return kindRead{}, e
	}
	ws, e := labels.ProjectLabels(ctx, "")
	if e != nil {
		return kindRead{}, e
	}
	inWorkspace := map[string]bool{}
	for _, l := range ws {
		inWorkspace[l.ID] = true
	}
	r := kindRead{workspace: ws}
	for _, l := range withTeam {
		if !inWorkspace[l.ID] {
			r.team = append(r.team, l)
		}
	}
	return r, nil
}

// kindPlan is a plan as planKindSeed makes it.
type kindPlan struct{ KindSeedResult }

func sameName(a, b string) bool { return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) }

// planKindSeed decides the group and each value (§4, K4): adopt the one
// live single-select team group of that name, else create it; adopt each
// value that is a live choice of it with exactly that name, else create
// it. A name the workspace has, or the team has as another label, is
// refused before anything is sent, so a create can't fail midway.
func planKindSeed(set kindSettingRow, r kindRead) kindPlan {
	var out kindPlan
	out.TeamID, out.Revision = set.team, set.revision
	taken := func(name string, own func(linearprovider.ProjectLabelRow) bool) string {
		for _, l := range r.workspace {
			if sameName(l.Name, name) {
				return kindClash
			}
		}
		for _, l := range r.team {
			if sameName(l.Name, name) && !own(l) {
				return "the team already has a project label with this name that isn't this kind's; choose another name, or rename that label in Linear"
			}
		}
		return ""
	}
	refuse := func(e *KindSeedEntry, reason string) {
		e.Action, e.Reason = "refused", reason
		out.Refused++
	}
	out.Group = KindSeedEntry{Name: set.group}
	var groups []linearprovider.ProjectLabelRow
	for _, l := range r.team {
		if l.Group && sameName(l.Name, set.group) {
			groups = append(groups, l)
		}
	}
	var group linearprovider.ProjectLabelRow
	switch {
	case len(groups) > 1:
		refuse(&out.Group, "the team has several project-label groups with this name; ClawBridge adopts exactly one")
	case len(groups) == 1:
		group = groups[0]
		switch {
		case group.Archived:
			refuse(&out.Group, "the team's group with this name is archived or retired")
		case group.GroupType != "singleSelect":
			refuse(&out.Group, "the team's group with this name is multi-select; a project kind needs a single-select group")
		case group.Name != set.group:
			// Review L2: the setting names the label exactly as Linear does.
			refuse(&out.Group, fmt.Sprintf("Linear names this group %q; set the kind with exactly that name", group.Name))
		case taken(set.group, func(l linearprovider.ProjectLabelRow) bool { return l.ID == group.ID }) == kindClash:
			// The workspace has the name too: its choices' parent name
			// would be ambiguous.
			refuse(&out.Group, kindClash)
		default:
			out.Group.Action, out.Group.LabelID = "adopt", group.ID
		}
	default:
		if why := taken(set.group, func(linearprovider.ProjectLabelRow) bool { return false }); why != "" {
			refuse(&out.Group, why)
		} else {
			out.Group.Action = "create"
		}
	}
	adopting := out.Group.Action == "adopt"
	choices := map[string][]linearprovider.ProjectLabelRow{}
	if adopting {
		for _, l := range r.team {
			if !l.Group && l.ParentName == group.Name {
				choices[strings.ToLower(strings.TrimSpace(l.Name))] = append(choices[strings.ToLower(strings.TrimSpace(l.Name))], l)
			}
		}
	}
	named := map[string]bool{}
	for _, v := range set.values {
		e := KindSeedEntry{Name: v.Name}
		key := strings.ToLower(strings.TrimSpace(v.Name))
		named[key] = true
		found := choices[key]
		switch {
		case len(found) > 1:
			refuse(&e, "the group has several choices with this name; ClawBridge adopts exactly one")
		case len(found) == 1 && found[0].Archived:
			refuse(&e, "the group's choice with this name is archived or retired")
		case len(found) == 1 && found[0].ParentGroupType != "singleSelect":
			refuse(&e, "the choice's group isn't single-select")
		case len(found) == 1 && found[0].Name != v.Name:
			refuse(&e, fmt.Sprintf("Linear names this choice %q; set the kind with exactly that name", found[0].Name))
		case len(found) == 1:
			e.Action, e.LabelID = "adopt", found[0].ID
		default:
			if why := taken(v.Name, func(linearprovider.ProjectLabelRow) bool { return false }); why != "" {
				refuse(&e, why)
			} else {
				e.Action = "create"
			}
		}
		out.Values = append(out.Values, e)
	}
	for key, rows := range choices {
		if !named[key] {
			for _, l := range rows {
				out.Untouched = append(out.Untouched, l.Name)
			}
		}
	}
	sort.Strings(out.Untouched)
	return out
}

func kindScopeLine(team, name string) string {
	label := team
	if name != "" {
		label = name + " (" + team + ")"
	}
	return "scope: team " + label + ", project labels: seen on every project of that team"
}

// kindPlanSHA digests the plan as shown: the setting's revision and team,
// and every entry's action and label.
func kindPlanSHA(p kindPlan) string {
	raw, _ := json.Marshal(struct {
		Revision  int64
		Team      string
		Group     KindSeedEntry
		Values    []KindSeedEntry
		Untouched []string
	}{p.Revision, p.TeamID, p.Group, p.Values, p.Untouched})
	return digest(string(raw))
}

// kindOperationID is one label's operation ID, from the owner's: the same
// base and name always give the same ID, so a re-run never sends twice.
func kindOperationID(base, role, name string) string {
	return base + "-" + digest(role + "|" + strings.ToLower(strings.TrimSpace(name)))[:12]
}

// sendKindSeed makes the planned labels, the group first, each one ledger
// operation read back in the team's list, then records the proven IDs. It
// stops at the first label not accepted.
func (s *Service) sendKindSeed(ctx context.Context, b bus.TaskboardBindingV2, actor, base string, scope bus.Request, set kindSettingRow, labels kindLabels, out KindSeedResult) (KindSeedResult, error) {
	out.Preview = false
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, actor})
	l := s.operations(ctx)
	create := func(e *KindSeedEntry, parent string) bool {
		if e.Action != "create" {
			return true
		}
		role := "value"
		if parent == "" {
			role = "group"
		}
		payload := struct {
			Version            int
			Kind, Profile      string
			Team, Name, Parent string
			SettingRevision    int64
		}{1, "kind_label", s.cfg.ProfileID, set.team, e.Name, parent, set.revision}
		receipt, err := l.write(namespace, kindOperationID(base, role, e.Name), ledgerMeta{"kind_label", b.ProjectID}, payload, func() (any, error) {
			var id string
			var err error
			if parent == "" {
				id, err = labels.CreateProjectLabelGroup(ctx, set.team, e.Name)
			} else {
				id, err = labels.CreateProjectLabelChoice(ctx, set.team, parent, e.Name)
			}
			var provider *linearprovider.Error
			if err != nil && errors.As(err, &provider) && (provider.NotSent || provider.Kind == "provider_rejected" || provider.Kind == "invalid" || provider.Kind == "capability_unavailable") {
				return nil, refusedBeforeDispatch(err)
			}
			// Sent, or maybe sent: the team's list decides.
			r := kindLabelReceipt{Kind: "kind_label", Name: e.Name, Parent: parent, Team: set.team}
			r.ID, r.Outcome = proveKindLabel(ctx, labels, set.team, e.Name, parent, id)
			return r, nil
		})
		if err != nil {
			e.Outcome, e.Reason = "refused", err.Error()
			return false
		}
		r, err := kindReceipt(receipt)
		if err != nil {
			e.Outcome, e.Reason = "uncertain", err.Error()
			return false
		}
		e.Outcome, e.LabelID = r.Outcome, r.ID
		return r.Outcome == "accepted"
	}
	if !create(&out.Group, "") {
		out.Stopped = out.Group.Name
		return out, nil
	}
	for i := range out.Values {
		if !create(&out.Values[i], out.Group.LabelID) {
			out.Stopped = out.Values[i].Name
			return out, nil
		}
	}
	ids := make([]string, len(out.Values))
	for i, v := range out.Values {
		ids[i] = v.LabelID
	}
	action := "seeded"
	if out.Group.Action == "adopt" {
		action = "adopted"
	}
	rec := scope
	rec.Operation, rec.Args = "project_kind_record", map[string]any{"expected_revision": set.revision, "action": action, "proof": "team_list", "group_label_id": out.Group.LabelID, "label_ids": ids}
	if _, e := s.cfg.Executor.Execute(ctx, rec); e != nil {
		// The labels are in Linear: a new preview adopts them.
		out.RecordError = e.Error() + "; preview the seed again: it adopts the labels made"
		return out, nil
	}
	out.Recorded = true
	return out, nil
}

// kindLabelReceipt is a create's ledger receipt.
type kindLabelReceipt struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Parent  string `json:"parent,omitempty"`
	Team    string `json:"team_id"`
	ID      string `json:"id,omitempty"`
	Outcome string `json:"outcome"`
}

func kindReceipt(v any) (kindLabelReceipt, error) {
	raw, e := json.Marshal(v)
	var r kindLabelReceipt
	if e != nil || !validKindReceipt(raw) || json.Unmarshal(raw, &r) != nil {
		return kindLabelReceipt{}, errors.New("invalid project label receipt")
	}
	return r, nil
}

// validKindReceipt is the ledger's check of a recorded label receipt: an
// accepted one names the proven ID.
func validKindReceipt(raw []byte) bool {
	var r kindLabelReceipt
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Kind != "kind_label" || !linearprovider.ValidID(r.Team) || (r.Parent != "" && !linearprovider.ValidID(r.Parent)) {
		return false
	}
	switch r.Outcome {
	case "accepted":
		return linearprovider.ValidID(r.ID)
	case "uncertain":
		return r.ID == "" || linearprovider.ValidID(r.ID)
	}
	return false
}

// proveKindLabel reads the team's labels after a create (K3's name proof,
// reviewer 01:5x): accepted only for exactly one live label of the team
// with that name, not in the workspace's list, and either a single-select
// group or a choice of the group (by its name, unambiguous since K4) of a
// single-select group; when the create answered an ID, it must be that
// label's.
func proveKindLabel(ctx context.Context, labels kindLabels, team, name, parent, answered string) (string, string) {
	read, e := readKindLabels(ctx, labels, team)
	if e != nil {
		return answered, "uncertain"
	}
	groupName := ""
	for _, l := range read.team {
		if l.ID == parent {
			groupName = l.Name
		}
	}
	var found []linearprovider.ProjectLabelRow
	for _, l := range read.team {
		if l.Name != name || l.Archived {
			continue
		}
		if parent == "" && l.Group && l.GroupType == "singleSelect" {
			found = append(found, l)
		}
		if parent != "" && !l.Group && groupName != "" && l.ParentName == groupName && l.ParentGroupType == "singleSelect" {
			found = append(found, l)
		}
	}
	if len(found) != 1 || (answered != "" && found[0].ID != answered) {
		return answered, "uncertain"
	}
	return found[0].ID, "accepted"
}
