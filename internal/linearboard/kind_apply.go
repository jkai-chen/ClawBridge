package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Applying the project kind (PROJECT-KIND-LABELS-PLAN P3, reviewer
// conditions a–e): set one kind on each bound project a mapping file names.
// A project's labels are proven by ID only through each label's carriers
// (list_projects filtered by the label's ID); get_project's names must
// match them one for one (K1). save_project replaces the set, so the write
// sends the whole set: the labels read, without this group's other values
// (by stored ID only, K3), plus the kind. The labels are read again before
// the send and after it (K2). The first apply of a setting revision is a
// canary of one project; the full apply needs that canary accepted.

// kindApplier is the owner's MCP connection as the apply uses it;
// *linearprovider.Provider implements it.
type kindApplier interface {
	kindLabels
	ProjectKindApplyReason() string
	ProjectsWithLabel(ctx context.Context, label string) ([]string, error)
	ProjectLabelNames(ctx context.Context, project string) ([]string, error)
	SetProjectLabels(ctx context.Context, project string, labels []string) error
}

const maxKindApplyEntries = 50

// KindApplyInput is one apply: the mapping file, the operation ID each
// project's own derives from, and the confirm's flags.
type KindApplyInput struct {
	File            []byte
	OperationID     string
	Confirm         bool
	ExpectedPlanSHA string
	// Canary applies only the first project to change.
	Canary bool
	// SkipCanary is allowed only when a canary already proved these
	// label IDs (the same group and values) at an earlier revision.
	SkipCanary bool
}

// KindApplyEntry is one project's plan, and with a confirm its outcome.
type KindApplyEntry struct {
	Project string `json:"project"`
	Kind    string `json:"kind"`
	// KindLabel is the kind's label ID.
	KindLabel string `json:"kind_label,omitempty"`
	// Action is set, unchanged or refused.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	// Labels are the project's labels as read, by ID; Add and Remove are
	// what the write changes.
	Labels      []string `json:"labels,omitempty"`
	Add         string   `json:"add,omitempty"`
	Remove      []string `json:"remove,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
	// Outcome is a write's: accepted, uncertain or refused.
	Outcome string `json:"outcome,omitempty"`
}

// KindApplyResult is an apply's preview or outcome.
type KindApplyResult struct {
	Preview  bool             `json:"preview"`
	Scope    string           `json:"scope"`
	TeamID   string           `json:"team_id"`
	Revision int64            `json:"revision"`
	Entries  []KindApplyEntry `json:"entries"`
	Refused  int              `json:"refused"`
	// Reads counts the Linear calls the preview made (bounded by pages).
	Reads   int    `json:"reads"`
	PlanSHA string `json:"plan_sha256"`
	Canary  bool   `json:"canary,omitempty"`
	// Stopped names the project a confirmed apply stopped at.
	Stopped string `json:"stopped,omitempty"`
}

type kindMapping struct {
	Project string `json:"project"`
	Kind    string `json:"kind"`
}

// parseKindMapping reads the file strictly: {"projects": [...]}, 1 to 50
// entries, each a project UUID once and a kind name.
func parseKindMapping(raw []byte) ([]kindMapping, error) {
	var file struct {
		Projects []kindMapping `json:"projects"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&file); e != nil || d.More() {
		return nil, errors.New(`the mapping file is {"projects": [{"project": UUID, "kind": NAME}]}`)
	}
	if len(file.Projects) == 0 || len(file.Projects) > maxKindApplyEntries {
		return nil, fmt.Errorf("the mapping file names 1 to %d projects", maxKindApplyEntries)
	}
	seen := map[string]bool{}
	for i := range file.Projects {
		m := &file.Projects[i]
		m.Project, m.Kind = strings.ToLower(m.Project), strings.TrimSpace(m.Kind)
		if !linearprovider.ValidID(m.Project) || seen[m.Project] || m.Kind == "" {
			return nil, errors.New("each entry names a distinct Linear project UUID and a kind")
		}
		seen[m.Project] = true
	}
	return file.Projects, nil
}

// countingApplier counts the Linear calls a preview makes.
type countingApplier struct {
	kindApplier
	calls *int
}

func (c countingApplier) ProjectsWithLabel(ctx context.Context, label string) ([]string, error) {
	*c.calls++
	return c.kindApplier.ProjectsWithLabel(ctx, label)
}

func (c countingApplier) ProjectLabelNames(ctx context.Context, project string) ([]string, error) {
	*c.calls++
	return c.kindApplier.ProjectLabelNames(ctx, project)
}

// KindApply previews or applies the mapping file.
func (s *Service) KindApply(ctx context.Context, scope bus.Request, in KindApplyInput) (KindApplyResult, error) {
	// A session is refused these: only a person, in their own terminal.
	if !(in.Confirm && scope.Session == "") {
		return s.kindApply(ctx, scope, in)
	}
	// A confirm writes through the person's own profile: the project's
	// hourly budget counts it (FR8).
	var out KindApplyResult
	err := s.budgeted(ctx, scope, "admin", in.OperationID, func(ctx context.Context) error {
		var e error
		out, e = s.kindApply(ctx, scope, in)
		return e
	})
	return out, err
}

func (s *Service) kindApply(ctx context.Context, scope bus.Request, in KindApplyInput) (KindApplyResult, error) {
	var none KindApplyResult
	if !operationPattern.MatchString(in.OperationID) || len(in.OperationID) > 100 {
		return none, errors.New("operation_id requires 1–100 ASCII identifier characters (each project adds a suffix)")
	}
	if scope.Session != "" {
		return none, errors.New("a session can't apply the project kind; the project owner runs linear kind apply in their own terminal")
	}
	mapping, e := parseKindMapping(in.File)
	if e != nil {
		return none, e
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
		return none, errors.New("only the project owner, without a session, applies the project kind")
	}
	setting, e := s.kindSetting(ctx, scope)
	if e != nil {
		return none, e
	}
	if setting.groupLabelID == "" {
		return none, errors.New("the project kind's labels aren't seeded; run linear kind seed first")
	}
	byName := map[string]string{}
	stored := map[string]bool{}
	for _, v := range setting.values {
		if v.LabelID == "" {
			return none, errors.New("the project kind's labels aren't seeded; run linear kind seed first")
		}
		byName[v.Name] = v.LabelID
		stored[v.LabelID] = true
	}
	profile, p, done, e := s.openForWrite(ctx)
	if e != nil {
		return none, e
	}
	defer done()
	if profile.Access != linearconn.Write {
		return none, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	raw, ok := p.(kindApplier)
	if !ok {
		return none, errors.New("native provider lacks the project label tools")
	}
	if reason := raw.ProjectKindApplyReason(); reason != "" {
		return none, errors.New("Linear's project tools are unavailable: " + reason)
	}
	if reason := raw.ProjectLabelsReason(); reason != "" {
		return none, errors.New("Linear's project label tools are unavailable: " + reason)
	}
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return none, e
	}
	out := KindApplyResult{Preview: !in.Confirm, TeamID: setting.team, Revision: setting.revision}
	reads := 0
	labels := countingApplier{raw, &reads}
	rows, e := labels.ProjectLabels(ctx, setting.team)
	reads++
	if e != nil {
		return none, e
	}
	teamName := ""
	if got, e := p.GetTeam(ctx, setting.team); e == nil && got.ID == setting.team {
		teamName = got.Name
	}
	out.Scope = kindScopeLine(setting.team, teamName)
	carriers := map[string][]string{}
	for _, m := range mapping {
		entry := KindApplyEntry{Project: m.Project, Kind: m.Kind, OperationID: kindOperationID(in.OperationID, "apply", m.Project)}
		target, known := byName[m.Kind]
		switch {
		case !slices.Contains(b.LinearProjectIDs, m.Project):
			entry.Action, entry.Reason = "refused", "the project isn't in this project's binding"
		case !known:
			entry.Action, entry.Reason = "refused", "not a kind of the setting (names are exact)"
		default:
			entry.KindLabel = target
			set, err := readProjectLabelSet(ctx, labels, rows, m.Project, stored, target, carriers)
			if err != nil {
				entry.Action, entry.Reason = "refused", err.Error()
				break
			}
			entry.Labels = set
			entry.Action, entry.Add, entry.Remove = planKindLabels(set, stored, target)
		}
		if entry.Action == "refused" {
			out.Refused++
		}
		out.Entries = append(out.Entries, entry)
	}
	out.Reads = reads
	out.PlanSHA = kindApplySHA(out)
	if !in.Confirm {
		return out, nil
	}
	if out.Refused > 0 {
		out.Preview = true
		return out, fmt.Errorf("%d of %d projects are refused; nothing was sent (see the preview)", out.Refused, len(out.Entries))
	}
	switch in.ExpectedPlanSHA {
	case "":
		return out, errors.New("a confirm carries the preview's plan_sha256; repeat the command the preview printed")
	case out.PlanSHA:
	default:
		out.Preview = true
		return out, errors.New("the plan changed since the preview (see this preview); preview again; nothing was sent")
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	l := s.operations(ctx)
	if e = s.kindCanaryGate(l, namespace, setting, in); e != nil {
		out.Preview = true
		return out, e
	}
	out.Canary = in.Canary
	applied := 0
	for i := range out.Entries {
		entry := &out.Entries[i]
		if entry.Action != "set" {
			continue
		}
		if in.Canary && applied == 1 {
			break
		}
		applied++
		if !s.applyKind(ctx, l, namespace, b.ProjectID, raw, rows, stored, setting, entry) {
			out.Stopped = entry.Project
			return out, nil
		}
	}
	if in.Canary && applied == 1 {
		s.markKindCanary(l, namespace, setting)
	}
	return out, nil
}

// readProjectLabelSet proves project's labels by ID (K1): each name
// get_project gives must be exactly one label the team's read lists and
// whose carriers hold the project, and no other label the apply may touch
// (this group's values) may carry it. A label of another team, or two
// labels of one name on it, can't be proven, so the project is refused.
func readProjectLabelSet(ctx context.Context, labels kindApplier, rows []linearprovider.ProjectLabelRow, project string, stored map[string]bool, target string, carriers map[string][]string) ([]string, error) {
	names, e := labels.ProjectLabelNames(ctx, project)
	if e != nil {
		return nil, fmt.Errorf("the project's labels can't be read (%s)", linearCode(e))
	}
	carries := func(label string) (bool, error) {
		list, ok := carriers[label]
		if !ok {
			got, e := labels.ProjectsWithLabel(ctx, label)
			if e != nil {
				return false, fmt.Errorf("label %s's projects can't be read in full (%s)", label, linearCode(e))
			}
			carriers[label], list = got, got
		}
		return slices.Contains(list, project), nil
	}
	set := map[string]bool{}
	for _, name := range names {
		var on []string
		for _, r := range rows {
			if r.Name != name || r.Group {
				continue
			}
			yes, e := carries(r.ID)
			if e != nil {
				return nil, e
			}
			if yes {
				on = append(on, r.ID)
			}
		}
		if len(on) != 1 || set[on[0]] {
			return nil, fmt.Errorf("the project's label %q can't be proven by ID (another team's label, or two labels of that name); nothing will be sent", name)
		}
		set[on[0]] = true
	}
	// An archived or retired label may be on the project without Linear's
	// project read naming it (review M1, probe C open): saving the set read
	// would remove it, so such a project is refused.
	for _, r := range rows {
		if !r.Archived || r.Group || slices.Contains(names, r.Name) {
			continue
		}
		yes, e := carries(r.ID)
		if e != nil {
			return nil, e
		}
		if yes {
			return nil, fmt.Errorf("the project carries an archived or retired label (%s) that Linear's project read doesn't show; saving its labels would remove it, so nothing will be sent", r.ID)
		}
	}
	// This group's values and the kind are checked whether named or not:
	// one carrying the project unseen would be overwritten.
	check := []string{target}
	for id := range stored {
		check = append(check, id)
	}
	for _, id := range check {
		yes, e := carries(id)
		if e != nil {
			return nil, e
		}
		if yes != set[id] {
			return nil, errors.New("the project's labels read by name and by ID disagree; nothing will be sent")
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// planKindLabels decides the write (reviewer c): remove only this group's
// stored values other than the kind, add the kind; anything else is kept.
func planKindLabels(set []string, stored map[string]bool, target string) (action, add string, remove []string) {
	for _, id := range set {
		if stored[id] && id != target {
			remove = append(remove, id)
		}
	}
	if !slices.Contains(set, target) {
		add = target
	}
	if add == "" && len(remove) == 0 {
		return "unchanged", "", nil
	}
	return "set", add, remove
}

// kindNewSet is the set a write sends: set without remove, plus add.
func kindNewSet(set []string, add string, remove []string) []string {
	out := []string{}
	for _, id := range set {
		if !slices.Contains(remove, id) {
			out = append(out, id)
		}
	}
	if add != "" {
		out = append(out, add)
	}
	sort.Strings(out)
	return out
}

func kindApplySHA(r KindApplyResult) string {
	raw, _ := json.Marshal(struct {
		Revision int64
		Team     string
		Entries  []KindApplyEntry
	}{r.Revision, r.TeamID, r.Entries})
	return digest(string(raw))
}

// kindCanaryKey is the canary marker of a setting revision; kindIDsKey the
// one of its label IDs, which --skip-canary needs.
func kindCanaryKey(set kindSettingRow) string {
	return fmt.Sprintf("kind-canary-r%d", set.revision)
}

func kindIDsKey(set kindSettingRow) string {
	ids := []string{set.groupLabelID}
	for _, v := range set.values {
		ids = append(ids, v.LabelID)
	}
	return "kind-canary-" + digest(strings.Join(ids, ","))[:16]
}

// kindCanaryGate: a canary always runs; the full apply needs an accepted
// canary of this revision, or with --skip-canary one of these label IDs.
func (s *Service) kindCanaryGate(l OperationLedger, namespace string, set kindSettingRow, in KindApplyInput) error {
	if in.Canary {
		if in.SkipCanary {
			return errors.New("--canary and --skip-canary don't combine")
		}
		return nil
	}
	key := kindCanaryKey(set)
	if in.SkipCanary {
		key = kindIDsKey(set)
	}
	if r, ok, e := l.lookupAny(namespace, key); e != nil || !ok || !validKindCanary(r) {
		if in.SkipCanary {
			return errors.New("--skip-canary needs an accepted canary of these same labels; run --canary first")
		}
		return errors.New("the first apply of this setting is a canary of one project: confirm with --canary, check it in Linear, then confirm again without it")
	}
	return nil
}

// markKindCanary records an accepted canary, by revision and by label IDs.
func (s *Service) markKindCanary(l OperationLedger, namespace string, set kindSettingRow) {
	for _, key := range []string{kindCanaryKey(set), kindIDsKey(set)} {
		// A marker is recorded accepted at once, so it is never listed.
		_, _ = l.write(namespace, key, ledgerMeta{Operation: "kind_canary"}, struct {
			Kind     string
			Revision int64
		}{"kind_canary", set.revision}, func() (any, error) {
			return map[string]any{"kind": "kind_canary", "revision": set.revision, "outcome": "accepted"}, nil
		})
	}
}

func validKindCanary(v any) bool {
	m, ok := v.(map[string]any)
	return ok && m["kind"] == "kind_canary" && m["outcome"] == "accepted"
}

// kindApplyReceipt is a project write's ledger receipt.
type kindApplyReceipt struct {
	Kind    string   `json:"kind"`
	Project string   `json:"project"`
	Sent    []string `json:"sent"`
	Outcome string   `json:"outcome"`
	Reason  string   `json:"reason,omitempty"`
}

// validKindApplyReceipt is the ledger's check of a recorded apply receipt.
func validKindApplyReceipt(raw []byte) bool {
	var r kindApplyReceipt
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Kind != "kind_apply" || !linearprovider.ValidID(r.Project) || (r.Outcome != "accepted" && r.Outcome != "uncertain") {
		return false
	}
	for _, id := range r.Sent {
		if !linearprovider.ValidID(id) {
			return false
		}
	}
	return true
}

// applyKind writes one project under the ledger: its labels are read again
// first and must be the preview's (else nothing is sent), then the whole
// set is sent and read back; only the same set by ID is accepted (K2).
func (s *Service) applyKind(ctx context.Context, l OperationLedger, namespace, pid string, labels kindApplier, rows []linearprovider.ProjectLabelRow, stored map[string]bool, set kindSettingRow, entry *KindApplyEntry) bool {
	sent := kindNewSet(entry.Labels, entry.Add, entry.Remove)
	payload := struct {
		Version         int
		Kind, Profile   string
		Project, Team   string
		SettingRevision int64
		Labels, Sent    []string
	}{1, "kind_apply", s.cfg.ProfileID, entry.Project, set.team, set.revision, entry.Labels, sent}
	receipt, err := l.write(namespace, entry.OperationID, ledgerMeta{Operation: "kind_apply", ProjectID: pid}, payload, func() (any, error) {
		now, e := readProjectLabelSet(ctx, labels, rows, entry.Project, stored, entry.KindLabel, map[string][]string{})
		if e != nil || !slices.Equal(now, entry.Labels) {
			return nil, refusedBeforeDispatch(errors.New("the project's labels changed since the preview; preview again"))
		}
		if e = labels.SetProjectLabels(ctx, entry.Project, sent); e != nil {
			var provider *linearprovider.Error
			if errors.As(e, &provider) && (provider.NotSent || provider.Kind == "provider_rejected" || provider.Kind == "invalid" || provider.Kind == "capability_unavailable") {
				return nil, refusedBeforeDispatch(e)
			}
		}
		r := kindApplyReceipt{Kind: "kind_apply", Project: entry.Project, Sent: sent, Outcome: "uncertain"}
		back, e := readProjectLabelSet(ctx, labels, rows, entry.Project, stored, entry.KindLabel, map[string][]string{})
		switch {
		case e != nil:
			r.Reason = "read_back_failed"
		case !slices.Equal(back, sent):
			// A person's edit between the read and the save shows here.
			r.Reason = "changed"
		default:
			r.Outcome = "accepted"
		}
		return r, nil
	})
	if err != nil {
		entry.Outcome, entry.Reason = "refused", err.Error()
		return false
	}
	raw, _ := json.Marshal(receipt)
	var r kindApplyReceipt
	if !validKindApplyReceipt(raw) || json.Unmarshal(raw, &r) != nil {
		entry.Outcome, entry.Reason = "uncertain", "invalid apply receipt"
		return false
	}
	entry.Outcome = r.Outcome
	if r.Reason != "" {
		entry.Reason = r.Reason + ": check project " + entry.Project + " in Linear; it is never sent again"
	}
	return r.Outcome == "accepted"
}

// linearCode is an error's code for a reason line, never its text.
func linearCode(e error) string {
	var coded interface{ Code() string }
	if errors.As(e, &coded) {
		return coded.Code()
	}
	return "unavailable"
}
