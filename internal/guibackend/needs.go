package guibackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
)

// Needs you (GUI-DESIGN §4.5) is one queue of things waiting on a person. The
// backend composes it from existing read operations only; the server gets no
// new aggregate op, and P2 writes nothing. Every action is listed but
// disabled, and "who may act" is for display: the server stays the guard.

// Needs kinds.
const (
	KindEscalation = "escalation"
	KindClaim      = "claim"
	KindIntake     = "intake"
	KindPerson     = "person"
	KindDoctor     = "doctor"
)

// needsKinds is the display order.
var needsKinds = []string{KindEscalation, KindClaim, KindIntake, KindPerson, KindDoctor}

// NeedsChangedEvent is pushed when the set of items changed.
const NeedsChangedEvent = "needs.changed"

// needsReadOps is every operation the composer may ask for. The executor
// wrapper refuses anything else, so a write cannot be reached from here.
var needsReadOps = map[string]bool{
	"dispatcher_escalations_list": true,
	"list_sessions":               true,
	"intake_list":                 true,
	"user_list":                   true,
	"team_roles_get":              true,
	"list_members":                true,
	"list_projects":               true,
}

// Reader is the part of an executor the composer uses.
type Reader interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}

// NeedsConfig is what the composer needs. Scope routes the reads; Mode and
// Principal say who is looking, for the who-may-act text.
type NeedsConfig struct {
	Exec      Reader
	Scope     Scope
	Doctor    func(context.Context) (doctor.Report, error)
	Mode      string
	Principal string
	// Timeout bounds each read (default 10 s).
	Timeout time.Duration
	// Now is the clock (default time.Now); tests fix it.
	Now func() time.Time
}

// NeedsAction is an action a kind offers. All are disabled in P2.
type NeedsAction struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

// NeedsActor says whether the viewer may act, for display. State is yes,
// no, depends, unknown or n/a.
type NeedsActor struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// NeedsItem is one thing waiting on a person.
type NeedsItem struct {
	Kind      string            `json:"kind"`
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Project   string            `json:"project"`
	CreatedAt string            `json:"created_at,omitempty"`
	Facts     map[string]string `json:"facts"`
	// Preview is the server's text for the item where the op gives one.
	Preview string        `json:"preview,omitempty"`
	Actions []NeedsAction `json:"actions"`
	Actor   NeedsActor    `json:"who_may_act"`
}

// NeedsKind is how one kind's read went.
type NeedsKind struct {
	Kind   string `json:"kind"`
	Status string `json:"status"` // ok or unavailable
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count"`
}

// NeedsResult is needs.list's answer.
type NeedsResult struct {
	Items       []NeedsItem `json:"items"`
	Kinds       []NeedsKind `json:"kinds"`
	GeneratedAt string      `json:"generated_at"`
	// Viewer is the viewer's project role as the server reports it, or "".
	Viewer string `json:"viewer_role"`
}

const notYet = "Not in this build yet."

func actions(labels ...string) []NeedsAction {
	out := make([]NeedsAction, 0, len(labels))
	for _, l := range labels {
		out = append(out, NeedsAction{ID: strings.ToLower(strings.NewReplacer(" ", "-", "…", "").Replace(l)), Label: l, Enabled: false, Reason: notYet})
	}
	return out
}

// readOnly refuses any operation outside needsReadOps.
type readOnly struct{ r Reader }

func (x readOnly) Execute(ctx context.Context, req bus.Request) (bus.Result, error) {
	if !needsReadOps[req.Operation] {
		return bus.Result{}, fmt.Errorf("needs.list may not call %s", req.Operation)
	}
	// The UI is never a session: a request that names one is refused, so
	// a read can never heartbeat a session or act as one.
	if req.Session != "" {
		return bus.Result{}, errors.New("needs.list may not act as a session")
	}
	return x.r.Execute(ctx, req)
}

// decode converts an executor answer into v.
func decodeNeeds(data any, v any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// reason is a refusal or failure reduced to one short line.
func oneLine(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

type composer struct {
	cfg  NeedsConfig
	exec readOnly
	// cached reads shared by several kinds
	roles    *rolesRead
	rolesErr error
	rolesOK  bool
}

type rolesRead struct {
	People []struct {
		LinearUserID string   `json:"linear_user_id"`
		Role         string   `json:"role"`
		Teams        []string `json:"teams"`
		Principal    string   `json:"principal"`
	} `json:"people"`
}

func (c *composer) read(ctx context.Context, op string, args map[string]any) (bus.Result, error) {
	d := c.cfg.Timeout
	if d <= 0 {
		d = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	s := c.cfg.Scope
	res, err := c.exec.Execute(ctx, bus.Request{Operation: op, Workspace: s.Workspace, Project: s.Project, Session: s.Session, Args: args})
	if err == nil && res.Source == "offline-mirror" {
		return res, errors.New("only an offline copy is available")
	}
	return res, err
}

func (c *composer) teamRoles(ctx context.Context) (*rolesRead, error) {
	if c.rolesOK {
		return c.roles, c.rolesErr
	}
	c.rolesOK = true
	res, err := c.read(ctx, "team_roles_get", map[string]any{})
	if err != nil {
		c.rolesErr = err
		return nil, err
	}
	var r rolesRead
	if err = decodeNeeds(res.Data, &r); err != nil {
		c.rolesErr = errors.New("the server's team roles could not be read")
		return nil, c.rolesErr
	}
	c.roles = &r
	return c.roles, nil
}

// viewerRole is the viewer's project role: "owner" in local mode (one
// person owns everything), else list_members' row for the viewer. "" means
// it could not be told.
func (c *composer) viewerRole(ctx context.Context) string {
	if c.cfg.Mode == "local" {
		return "owner"
	}
	if c.cfg.Principal == "" {
		return ""
	}
	res, err := c.read(ctx, "list_members", map[string]any{})
	if err != nil {
		return ""
	}
	var m struct {
		Members []struct {
			Principal string `json:"principal"`
			Role      string `json:"role"`
		} `json:"members"`
	}
	if decodeNeeds(res.Data, &m) != nil {
		return ""
	}
	for _, x := range m.Members {
		if strings.EqualFold(x.Principal, c.cfg.Principal) {
			return x.Role
		}
	}
	return ""
}

// projectLabel is the project's slug for display ("atlas-api"), from
// list_projects, which Compose reads once per cycle. The scope may hold the
// project's id; it stays as it is only when the read fails or has no match.
func (c *composer) projectLabel(ctx context.Context) string {
	id := c.cfg.Scope.Project
	if id == "" {
		return ""
	}
	res, err := c.read(ctx, "list_projects", map[string]any{})
	if err != nil {
		return id
	}
	var l struct {
		Projects []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if decodeNeeds(res.Data, &l) != nil {
		return id
	}
	for _, p := range l.Projects {
		if (p.ID == id || p.Slug == id) && p.Slug != "" {
			return p.Slug
		}
	}
	return id
}

// ledTeams is how many teams the viewer leads, per the team roles read; -1
// when unknown.
func (c *composer) ledTeams(ctx context.Context) int {
	r, err := c.teamRoles(ctx)
	if err != nil || c.cfg.Principal == "" {
		return -1
	}
	n := 0
	for _, p := range r.People {
		if p.Role == "lead" && p.Principal != "" && strings.EqualFold(p.Principal, c.cfg.Principal) {
			n += len(p.Teams)
		}
	}
	return n
}

// claimActor is FR45's rule, shown: the project owner, or the team-roles
// lead of the issue's team. The reads don't say which team an issue is in, so
// a lead gets "depends"; the server checks it.
func claimActor(role string, led int) NeedsActor {
	switch {
	case role == "owner":
		return NeedsActor{"yes", "You can approve this as the project owner."}
	case led > 0:
		return NeedsActor{"depends", "You lead a team. You can approve it if the issue is in that team; the server checks."}
	case role == "" || led < 0:
		return NeedsActor{"unknown", "Who may approve could not be read. The server decides."}
	}
	return NeedsActor{"no", "Only the project owner or a team lead can approve."}
}

func intakeActor(role string, led int) NeedsActor {
	switch {
	case role == "owner" || role == "lead":
		return NeedsActor{"yes", "You can dismiss or rearm this as a project " + role + "."}
	case led > 0:
		return NeedsActor{"depends", "You lead a team. The server checks that you may act on this issue."}
	case role == "":
		return NeedsActor{"unknown", "Who may act could not be read. The server decides."}
	}
	return NeedsActor{"no", "Only the project owner, a lead or the coordinator can dismiss or rearm."}
}

// Compose reads every kind and returns the queue. A kind whose read fails
// or is refused is unavailable with its reason; the rest still show.
func Compose(ctx context.Context, cfg NeedsConfig, now time.Time) NeedsResult {
	c := &composer{cfg: cfg, exec: readOnly{cfg.Exec}}
	out := NeedsResult{Items: []NeedsItem{}, GeneratedAt: now.UTC().Format(time.RFC3339)}
	role := c.viewerRole(ctx)
	out.Viewer = role
	led := c.ledTeams(ctx)
	project := c.projectLabel(ctx)

	finish := func(kind string, items []NeedsItem, err error) {
		k := NeedsKind{Kind: kind, Status: "ok", Count: len(items)}
		if err != nil {
			k = NeedsKind{Kind: kind, Status: "unavailable", Reason: oneLine(err)}
			items = nil
		}
		out.Kinds = append(out.Kinds, k)
		out.Items = append(out.Items, items...)
	}

	// Dispatcher escalations: pending claims' escalations, newest first.
	{
		var items []NeedsItem
		res, err := c.read(ctx, "dispatcher_escalations_list", map[string]any{})
		if err == nil {
			var raw struct {
				Escalations []map[string]any `json:"escalations"`
			}
			if err = decodeNeeds(res.Data, &raw); err == nil {
				for _, e := range raw.Escalations {
					if anyStr(e["state"]) == "answered" {
						continue
					}
					facts := map[string]string{"session": anyStr(e["session"]), "issue": anyStr(e["issue_identifier"]), "revision": anyStr(e["revision"]), "reason": anyStr(e["reason_code"])}
					if opts := optionList(e["options"]); opts != "" {
						facts["options"] = opts
					}
					title := anyStr(e["session"]) + " needs an answer on " + anyStr(e["issue_identifier"])
					preview := anyStr(e["question"])
					items = append(items, NeedsItem{Kind: KindEscalation, ID: anyStr(e["id"]), Title: title, Project: project, CreatedAt: anyStr(e["created_at"]),
						Facts: facts, Preview: preview, Actions: actions("Approve claim", "Release", "Wait"), Actor: claimActor(role, led)})
				}
			}
		}
		finish(KindEscalation, items, err)
	}

	// Claims awaiting confirmation.
	{
		var items []NeedsItem
		res, err := c.read(ctx, "list_sessions", map[string]any{"assignment_state": "proposed", "limit": 100, "after": int64(0)})
		if err == nil {
			var raw struct {
				Sessions []map[string]any `json:"sessions"`
			}
			if err = decodeNeeds(res.Data, &raw); err == nil {
				for _, row := range raw.Sessions {
					a, _ := row["assignment"].(map[string]any)
					if a == nil || anyStr(a["state"]) != "proposed" {
						continue
					}
					issue := firstOf(anyStr(a["issue_identifier"]), anyStr(a["issue_uuid"]))
					facts := map[string]string{"session": anyStr(row["name"]), "issue": issue, "revision": anyStr(a["revision"])}
					items = append(items, NeedsItem{Kind: KindClaim, ID: firstOf(anyStr(a["id"]), anyStr(row["id"])), Title: anyStr(row["name"]) + " proposed " + issue, Project: project,
						CreatedAt: anyStr(a["proposed_at"]), Facts: facts, Actions: actions("Approve", "Release", "Send to session"), Actor: claimActor(role, led)})
				}
			}
		}
		finish(KindClaim, items, err)
	}

	// Intake items queued.
	{
		var items []NeedsItem
		res, err := c.read(ctx, "intake_list", map[string]any{"state": "queued"})
		if err == nil {
			var rows []map[string]any
			if err = decodeNeeds(res.Data, &rows); err == nil {
				for _, r := range rows {
					id := firstOf(anyStr(r["issue_identifier"]), anyStr(r["issue_uuid"]))
					facts := map[string]string{"issue": id, "state": anyStr(r["state"]), "offers": anyStr(r["offers"])}
					if sg := anyStr(r["signals"]); sg != "" {
						facts["signals"] = sg
					}
					items = append(items, NeedsItem{Kind: KindIntake, ID: firstOf(anyStr(r["issue_uuid"]), id), Title: id + " is queued for an agent", Project: project,
						CreatedAt: anyStr(r["created_at"]), Facts: facts, Preview: anyStr(r["reason"]), Actions: actions("Send to session", "Dismiss", "Rearm"), Actor: intakeActor(role, led)})
				}
			}
		}
		finish(KindIntake, items, err)
	}

	// Unlinked people: Linear people in the team roles mapping with no
	// ClawBridge account, which only the workspace owner can tell (user_list).
	{
		var items []NeedsItem
		var err error
		users, uerr := c.read(ctx, "user_list", map[string]any{})
		if uerr != nil {
			err = uerr
		} else {
			var ul struct {
				Users []struct {
					Principal string `json:"principal"`
				} `json:"users"`
			}
			if err = decodeNeeds(users.Data, &ul); err == nil {
				known := map[string]bool{}
				for _, u := range ul.Users {
					known[strings.ToLower(u.Principal)] = true
				}
				var r *rolesRead
				if r, err = c.teamRoles(ctx); err == nil {
					for _, p := range r.People {
						why := ""
						switch {
						case p.Principal == "":
							why = "no_link"
						case !known[strings.ToLower(p.Principal)]:
							why = "link_not_a_member"
						default:
							continue
						}
						facts := map[string]string{"linear_user": p.LinearUserID, "role": p.Role, "teams": strings.Join(p.Teams, ", "), "reason": why}
						actor := NeedsActor{"no", "Only the workspace owner can link or invite."}
						if role == "owner" {
							actor = NeedsActor{"yes", "You can link or invite as the workspace owner."}
						}
						items = append(items, NeedsItem{Kind: KindPerson, ID: p.LinearUserID, Title: "Linear person " + p.LinearUserID + " has no ClawBridge account", Project: project,
							Facts: facts, Actions: actions("Link…", "Invite"), Actor: actor})
					}
				}
			}
		}
		finish(KindPerson, items, err)
	}

	// Doctor warnings and failures.
	{
		var items []NeedsItem
		var err error
		if cfg.Doctor == nil {
			err = errors.New("doctor is not available")
		} else {
			var rep doctor.Report
			if rep, err = cfg.Doctor(ctx); err == nil {
				for _, ch := range rep.Checks {
					if ch.Status != doctor.Warn && ch.Status != doctor.Fail {
						continue
					}
					facts := map[string]string{"check": ch.ID, "status": ch.Status}
					if ch.Path != "" {
						facts["path"] = ch.Path
					}
					if ch.Action != "" {
						facts["fix"] = ch.Action
					}
					items = append(items, NeedsItem{Kind: KindDoctor, ID: ch.ID, Title: ch.Title, Project: project, CreatedAt: rep.GeneratedAt,
						Facts: facts, Preview: ch.Message, Actions: actions("Fix", "Dismiss"), Actor: NeedsActor{"n/a", "Anyone can see this; fixing it is local."}})
				}
			}
		}
		finish(KindDoctor, items, err)
	}
	sortItems(out.Items)
	return out
}

// sortItems orders by kind, then oldest first, then id, so the list is
// stable between reads.
func sortItems(items []NeedsItem) {
	rank := map[string]int{}
	for i, k := range needsKinds {
		rank[k] = i
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return a.ID < b.ID
	})
}

func anyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprint(x)
	}
	return fmt.Sprint(v)
}

func firstOf(a ...string) string {
	for _, x := range a {
		if x != "" {
			return x
		}
	}
	return ""
}

// optionList names an escalation's options (strings, or objects with an id
// or label).
func optionList(v any) string {
	list, _ := v.([]any)
	var names []string
	for _, o := range list {
		switch x := o.(type) {
		case string:
			names = append(names, x)
		case map[string]any:
			names = append(names, firstOf(anyStr(x["label"]), anyStr(x["id"]), anyStr(x["option"])))
		}
	}
	return strings.Join(names, ", ")
}

// NeedsList is needs.list.
func NeedsList(cfg NeedsConfig) Method {
	return Method{Class: Read, Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		now := time.Now
		if cfg.Now != nil {
			now = cfg.Now
		}
		return Compose(ctx, cfg, now()), nil
	}}
}

// fingerprint is a hash of what a person would see change: the items and
// each kind's status, not the time it was read.
func fingerprint(r NeedsResult) string {
	r.GeneratedAt = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NeedsWatcher notices when the set of items changed. The first look is the
// baseline: nothing is announced for it.
type NeedsWatcher struct {
	mu   sync.Mutex
	seen string
}

// Step records r and reports whether it differs from the last one seen.
func (w *NeedsWatcher) Step(r NeedsResult) bool {
	fp := fingerprint(r)
	w.mu.Lock()
	defer w.mu.Unlock()
	changed := w.seen != "" && w.seen != fp
	w.seen = fp
	return changed
}

// PushNeedsChanged recomputes the queue every interval (30 s in the client)
// and sends needs.changed {count, unavailable} only when it changed.
func PushNeedsChanged(ctx context.Context, srv *Server, cfg NeedsConfig, interval time.Duration) {
	var w NeedsWatcher
	look := func() {
		r := Compose(ctx, cfg, time.Now())
		if !w.Step(r) {
			return
		}
		down := 0
		for _, k := range r.Kinds {
			if k.Status != "ok" {
				down++
			}
		}
		_ = srv.Notify(NeedsChangedEvent, map[string]int{"count": len(r.Items), "unavailable": down})
	}
	look()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			look()
		}
	}
}
