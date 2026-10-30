package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Agent cancel (AGENT-CANCEL R4-R9, live/AGENT-CANCEL-PLAN.md). An agent
// cancels an issue, or marks it Duplicate, only with these two
// operations, never with a plain state write (closeGuard), and only where
// a person allowed it: the issue's Linear project is of a kind the status
// policy lists in agent_may_cancel_kinds. The kind is read from Linear by
// the server as its ClawBridge app, so an agent cancels only through a
// hosted server; on a member's own profile the kind would be the client's
// claim. A person (no session) may use both anywhere the binding lets
// them write. Nobody re-opens an issue or touches a completed one.

// CancelInputV2 is a cancel or a duplicate.
type CancelInputV2 struct {
	// Operation is cancel or duplicate.
	Operation   string
	OperationID string
	IssueID     string
	// DuplicateOf is the original issue a duplicate names.
	DuplicateOf string
	// Reason is posted as a marked comment: required for a cancel,
	// optional for a duplicate.
	Reason string
}

// ProjectLabelRef is one of a Linear project's labels and its group.
type ProjectLabelRef struct{ ID, ParentID string }

// CancelResultV2 is what a cancel or duplicate did, and what the agent
// does next.
type CancelResultV2 struct {
	Kind        string `json:"kind"`
	Outcome     string `json:"outcome"`
	IssueID     string `json:"issue_id"`
	Identifier  string `json:"identifier,omitempty"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
	// StateLanded is the workflow state the issue is in now; StateSetBy
	// says whether ClawBridge set it or Linear did (a duplicate).
	StateLanded string `json:"state_landed"`
	StateSetBy  string `json:"state_set_by"`
	CommentID   string `json:"comment_id,omitempty"`
	// Relation is the duplicate edge's outcome: accepted,
	// observed_unchanged, or unverified when no read can show it.
	Relation    string   `json:"relation,omitempty"`
	ProjectKind string   `json:"project_kind,omitempty"`
	Unverified  []string `json:"unverified,omitempty"`
	// Recorded says whether the bus recorded an agent's cancel and told
	// the coordinator; absent for a person's.
	Recorded *bool  `json:"recorded,omitempty"`
	Next     string `json:"next"`
}

// cancelNext is the agent's next step: the state stays where the cancel
// put it, so the release names no outcome (a done release would find the
// issue canceled and leave it, from_state).
const cancelNext = "release your assignment without an outcome (`clawbridge assignment release --revision REV`, or MCP assignment_release with no outcome); the state stays Canceled"

// cancelReasonRunes bounds a reason.
const cancelReasonRunes = 2000

// cancelStateTypes are the types an issue is canceled or marked Duplicate
// from: never a completed or canceled issue, never one re-opened.
var cancelStateTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true}

// cancelRefusal is a refusal before anything is sent.
func cancelRefusal(code, sentence string) error {
	return fmt.Errorf("%s: %s. Nothing was sent", code, sentence)
}

var errCancelAppOnly = errors.New("agents cancel only through the ClawBridge Linear app on a hosted server; nothing was sent")

func validateCancelV2(in CancelInputV2) error {
	if in.Operation != "cancel" && in.Operation != "duplicate" {
		return errors.New("unsupported task-board V2 mutation")
	}
	// A step's ID is the caller's and up to 9 more characters.
	if !operationPattern.MatchString(in.OperationID) || len(in.OperationID) > 119 {
		return errors.New(in.Operation + " requires operation_id: 1–119 ASCII identifier characters")
	}
	if !linearprovider.ValidID(in.IssueID) {
		return errors.New(in.Operation + " requires the issue's UUID")
	}
	reason := strings.TrimSpace(in.Reason)
	if in.Operation == "cancel" && reason == "" {
		return errors.New("reason_required: a cancel carries a reason (1–2000 characters), posted as a comment. Nothing was sent")
	}
	if in.Reason != "" && (utf8.RuneCountInString(in.Reason) > cancelReasonRunes || !validText(in.Reason, linearprovider.MaxBodyBytes-linearprovider.CommentMarkerReserve-512, false)) {
		return errors.New("a reason is 1–2000 characters of text. Nothing was sent")
	}
	if e := mentionGuard(&in.Reason); e != nil {
		return e
	}
	switch {
	case in.Operation == "cancel" && in.DuplicateOf != "":
		return errors.New("duplicate_of belongs to a duplicate, not a cancel. Nothing was sent")
	case in.Operation == "duplicate" && in.DuplicateOf == "":
		return cancelRefusal("duplicate_of_required", "a duplicate names the original issue it duplicates (duplicate_of)")
	case in.Operation == "duplicate" && !linearprovider.ValidID(in.DuplicateOf):
		return errors.New("duplicate_of requires the original issue's UUID. Nothing was sent")
	case in.Operation == "duplicate" && in.DuplicateOf == in.IssueID:
		return cancelRefusal("duplicate_of_self", "an issue cannot duplicate itself")
	}
	return nil
}

// cancelArgsV2 is the linear_write argument map ExecuteV2 reads back.
func cancelArgsV2(in CancelInputV2) map[string]any {
	args := map[string]any{"operation_id": in.OperationID, "id": in.IssueID}
	if in.Reason != "" {
		args["reason"] = in.Reason
	}
	if in.DuplicateOf != "" {
		args["duplicate_of"] = in.DuplicateOf
	}
	return args
}

// CancelV2 cancels an issue or marks it Duplicate. Through a hosted server
// with the app it is the server's; otherwise this process makes it, and
// refuses a session's.
func (s *Service) CancelV2(ctx context.Context, scope bus.Request, in CancelInputV2) (any, error) {
	if e := validateCancelV2(in); e != nil {
		return nil, e
	}
	if app, e := s.appWrites(ctx); e != nil || app {
		if e != nil {
			return nil, e
		}
		return s.appWrite(ctx, scope, in.Operation, cancelArgsV2(in))
	}
	// Through the person's own profile: the project's hourly budget counts
	// it (FR8). Its steps share one connection and binding proof.
	var out any
	err := s.budgeted(ctx, scope, "update", in.OperationID, func(ctx context.Context) error {
		held, release := s.hold()
		defer release()
		var e error
		out, e = held.cancelV2(ctx, scope, in)
		return e
	})
	return out, err
}

func (s *Service) cancelV2(ctx context.Context, scope bus.Request, in CancelInputV2) (any, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if !b.Configured {
		return nil, errors.New("task-board is not linked")
	}
	if s.cfg.Authority == "" {
		return nil, errors.New("canonical authority identity required for operation ledger")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return nil, e
	}
	agent := access.SessionID != ""
	// A retry whose last step was already accepted finds the issue closed
	// by this very operation: it completes, and is not refused for it.
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	l := s.operations(ctx)
	last := in.OperationID + ":state"
	if in.Operation == "duplicate" {
		last = in.OperationID + ":relation"
	}
	recorded, _, ok := l.peek(namespace, last)
	if !ok {
		return nil, errors.New("cannot read the operation ledger; nothing was sent")
	}
	resumed := recorded == "accepted"
	_, p, done, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	v, e := s.proveBinding(ctx, p, b)
	if e != nil {
		return nil, e
	}
	p2, isV2 := p.(ProviderV2)
	if !isV2 {
		return nil, errors.New("native provider lacks V2 mutation capability")
	}
	current, e := s.readIssue(ctx, p2, in.IssueID)
	if e != nil {
		return nil, e
	}
	if current.ID != in.IssueID {
		return nil, errors.New("issue identity changed")
	}
	if e = checkIssueV2(current, b, v); e != nil {
		return nil, e
	}
	if e = checkLaneV2(b, access, scope.Session, current.TeamID); e != nil {
		return nil, e
	}
	policy, e := s.statusPolicy(ctx, scope)
	if e != nil {
		return nil, e
	}
	states, e := p.Statuses(ctx, current.TeamID)
	if e != nil {
		return nil, e
	}
	if !resumed {
		if e = s.cancelFromState(ctx, scope, policy, states, current); e != nil {
			return nil, e
		}
	}
	kind := ""
	if agent {
		if s.cfg.ProjectKindLabels == nil {
			return nil, errCancelAppOnly
		}
		if !policy.Enabled || len(policy.AgentMayCancelKinds) == 0 {
			return nil, cancelRefusal("cancel_off", "agents never cancel in this project: no project kind is allowed under its status policy (the project owner or a lead sets it with linear cancel-kinds set)")
		}
		if kind, e = s.issueKind(ctx, scope, b, current); e != nil {
			return nil, e
		}
		allowed := false
		for _, k := range policy.AgentMayCancelKinds {
			allowed = allowed || strings.EqualFold(k, kind)
		}
		if !allowed {
			return nil, cancelRefusal("kind_not_allowed", fmt.Sprintf("the issue's Linear project is of kind %s, and agents cancel or mark Duplicate only in projects of kind %s", kind, strings.Join(policy.AgentMayCancelKinds, ", ")))
		}
	}
	var original linearprovider.IssueV2
	if in.Operation == "duplicate" {
		if original, e = s.duplicateOriginal(ctx, scope, p, p2, b, v, policy, in.DuplicateOf, resumed); e != nil {
			return nil, e
		}
	}
	target, e := s.cancelTarget(ctx, policy, states, current.TeamID, in.Operation == "duplicate")
	if e != nil {
		return nil, e
	}
	out := CancelResultV2{Kind: in.Operation, Outcome: "accepted", IssueID: current.ID, Identifier: provenIdentifierV2(current), ProjectKind: kind, Next: cancelNext}
	// The steps: each its own ledger entry, so a retry with the same
	// operation_id never posts twice and completes a half-done one. Each
	// makes the board's pre-send checks again. The kind is read once,
	// above, before the first step and not again at the state step: the
	// steps follow within seconds, and a retry reads it afresh.
	body := ""
	switch {
	case in.Operation == "cancel":
		body = "Canceled: " + in.Reason
	case in.Reason != "":
		ref := provenIdentifierV2(original)
		if ref == "" {
			ref = original.ID
		}
		body = "Duplicate of " + ref + ": " + in.Reason
	}
	if body != "" {
		comment := WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: in.OperationID + ":comment", IssueID: in.IssueID, Body: body}, cancelStep: true}
		r, e := s.writeV2Personal(ctx, scope, comment)
		if e != nil {
			return nil, e
		}
		out.CommentID = receiptIDV2(r)
	}
	if in.Operation == "duplicate" {
		r, e := s.relationV2Personal(ctx, scope, RelationInputV2{OperationID: in.OperationID + ":relation", Action: "add", Type: "duplicate", FromID: in.IssueID, ToID: original.ID, source: in.IssueID, duplicateStep: true})
		if e != nil {
			return nil, e
		}
		out.DuplicateOf = original.ID
		out.Relation = receiptOutcomeV2(r)
		// Linear moves a duplicate to the team's duplicate state itself;
		// only when it hasn't does ClawBridge set the state. A retry whose
		// state step was sent answers from the ledger below.
		if set, _, _ := l.peek(namespace, in.OperationID+":state"); set != "accepted" {
			after, e := s.rereadIssue(ctx, p2, in.IssueID)
			if e != nil {
				return nil, e
			}
			if landed, e := s.issueStateID(ctx, scope, after); e == nil && landed != "" {
				if t, e := s.stateKind(ctx, policy, states, after.TeamID, landed); e == nil && t == "canceled" {
					out.StateLanded, out.StateSetBy = landed, "linear"
				}
			}
		}
	}
	if out.StateLanded == "" {
		update := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: in.OperationID + ":state", IssueID: in.IssueID, StateID: &target}, cancelStep: true}
		r, e := s.writeV2Personal(ctx, scope, update)
		if e != nil {
			return nil, e
		}
		out.StateLanded, out.StateSetBy = target, "clawbridge"
		if receipt, ok := r.(ReceiptV2); ok {
			out.Unverified = receipt.Unverified
		}
	}
	if agent {
		ok := s.recordAgentCancel(ctx, scope, in, out, current, kind)
		out.Recorded = &ok
	}
	return out, nil
}

// cancelFromState refuses an issue that is not open: completed, canceled
// or a duplicate already, or of a type nothing can tell.
func (s *Service) cancelFromState(ctx context.Context, scope bus.Request, policy bus.StatusPolicy, states []linearprovider.Status, issue linearprovider.IssueV2) error {
	state, e := s.issueStateID(ctx, scope, issue)
	if e != nil && !errors.Is(e, errStateUnresolved) {
		return e
	}
	kind := ""
	if state != "" {
		if kind, e = s.stateKind(ctx, policy, states, issue.TeamID, state); e != nil {
			return errStateTypeUnread
		}
	}
	switch {
	case cancelStateTypes[kind]:
		return nil
	case kind == "":
		return cancelRefusal("state_unknown", "cannot verify the issue's current workflow state type, so it cannot be canceled")
	}
	return cancelRefusal("from_state_closed", "the issue is "+kind+" already: never re-opened, never a completed or canceled issue")
}

// duplicateOriginal reads the original a duplicate names: an issue the
// binding lets agents comment on at least, and still open.
func (s *Service) duplicateOriginal(ctx context.Context, scope bus.Request, p Provider, p2 ProviderV2, b bus.TaskboardBindingV2, v verifiedV2, policy bus.StatusPolicy, id string, resumed bool) (linearprovider.IssueV2, error) {
	original, e := s.readIssue(ctx, p2, id)
	if e != nil {
		return original, e
	}
	if original.ID != id {
		return original, errors.New("the original issue's identity changed")
	}
	if e = checkCommentIssueV2(original, b, v); e != nil {
		return original, cancelRefusal("duplicate_target_out_of_scope", "the original issue is outside this project's binding")
	}
	if resumed {
		return original, nil
	}
	state, e := s.issueStateID(ctx, scope, original)
	if e != nil && !errors.Is(e, errStateUnresolved) {
		return original, e
	}
	kind := ""
	if state != "" {
		var states []linearprovider.Status
		if states, e = p.Statuses(ctx, original.TeamID); e != nil {
			return original, e
		}
		if kind, e = s.stateKind(ctx, policy, states, original.TeamID, state); e != nil {
			return original, errStateTypeUnread
		}
	}
	switch kind {
	case "completed", "canceled":
		return original, cancelRefusal("duplicate_target_closed", "the original issue is "+kind+": mark a duplicate of an open issue")
	case "":
		return original, cancelRefusal("state_unknown", "cannot verify the original issue's workflow state type")
	}
	return original, nil
}

// rawStateType is a state's own Linear type: the status list's raw type,
// the policy's workflow snapshot, the list's type, then StateType (a
// hosted server's app). "" is unknown.
func (s *Service) rawStateType(ctx context.Context, policy bus.StatusPolicy, team string, st linearprovider.Status) string {
	if st.Raw != "" {
		return strings.ToLower(st.Raw)
	}
	if kind := policy.Teams[strings.ToLower(team)].Workflow[strings.ToLower(st.ID)]; kind != "" {
		return kind
	}
	if st.Type != "" {
		return strings.ToLower(st.Type)
	}
	if s.cfg.StateType != nil {
		if raw, e := s.cfg.StateType(ctx, team, strings.ToLower(st.ID)); e == nil {
			return strings.ToLower(raw)
		}
	}
	return ""
}

// cancelTarget is the state a cancel sets: the team's one state of type
// canceled (never its duplicate state), else the one of them named
// Canceled; a duplicate prefers the team's one state of type duplicate.
func (s *Service) cancelTarget(ctx context.Context, policy bus.StatusPolicy, states []linearprovider.Status, team string, duplicate bool) (string, error) {
	var canceled, duplicates, named []string
	for _, st := range states {
		if !strings.EqualFold(st.TeamID, team) || !linearprovider.ValidID(strings.ToLower(st.ID)) {
			continue
		}
		raw := s.rawStateType(ctx, policy, team, st)
		id := strings.ToLower(st.ID)
		switch raw {
		case "canceled":
			canceled = append(canceled, id)
		case "duplicate":
			duplicates = append(duplicates, id)
		}
		if (raw == "canceled" || raw == "") && strings.EqualFold(strings.TrimSpace(st.Name), "Canceled") {
			named = append(named, id)
		}
	}
	switch {
	case duplicate && len(duplicates) == 1:
		return duplicates[0], nil
	case len(canceled) == 1:
		return canceled[0], nil
	case len(named) == 1:
		return named[0], nil
	}
	return "", cancelRefusal("target_ambiguous", "the issue's team has no single canceled state (one of type canceled, or one named Canceled) to set")
}

// issueKind is the kind of the issue's Linear project, read from Linear
// (ProjectKindLabels), never the caller's claim: the one label of the
// project in the kind group whose ID is a stored kind value's. Anything
// else is unknown.
func (s *Service) issueKind(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2, issue linearprovider.IssueV2) (string, error) {
	unknown := func(why string) error {
		return cancelRefusal("kind_unknown", why+", so agents may not cancel or mark Duplicate its issues")
	}
	project := strings.ToLower(issue.ProjectID)
	if !issue.ProjectKnown || project == "" {
		return "", unknown("the issue is in no Linear project")
	}
	if !contains(b.LinearProjectIDs, project) {
		return "", unknown("the issue's Linear project is not one of this binding's projects")
	}
	data, e := s.ProjectKind(ctx, scope)
	if e != nil {
		return "", unknown("the project kind setting could not be read")
	}
	raw, _ := json.Marshal(data)
	var setting struct {
		Group  *string                `json:"group_label_id"`
		Values []bus.ProjectKindValue `json:"values"`
	}
	if json.Unmarshal(raw, &setting) != nil || setting.Group == nil || *setting.Group == "" {
		return "", unknown("the project kinds have no Linear labels yet (linear kind seed)")
	}
	values := map[string]string{}
	for _, v := range setting.Values {
		if v.LabelID != "" {
			values[strings.ToLower(v.LabelID)] = v.Name
		}
	}
	labels, e := s.cfg.ProjectKindLabels(ctx, project)
	if e != nil {
		return "", unknown("the Linear project's labels could not be read")
	}
	var found []string
	for _, l := range labels {
		if name, ok := values[strings.ToLower(l.ID)]; ok && strings.EqualFold(l.ParentID, *setting.Group) {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return "", unknown("the issue's Linear project carries no kind label")
	case 1:
		return found[0], nil
	}
	return "", unknown("the issue's Linear project carries several kind labels")
}

// recordAgentCancel has the bus record an agent's cancel and tell the
// coordinator and the issue's assignee. The cancel stands either way: a
// failed record is logged and answered as recorded: false.
func (s *Service) recordAgentCancel(ctx context.Context, scope bus.Request, in CancelInputV2, out CancelResultV2, issue linearprovider.IssueV2, kind string) bool {
	action := "canceled"
	if in.Operation == "duplicate" {
		action = "duplicated"
	}
	identifier := provenIdentifierV2(issue)
	if identifier == "" {
		identifier = issue.ID
	}
	scope.Operation = "agent_cancel_record"
	scope.Args = instanceArgs(scope.Args)
	for k, v := range map[string]any{"operation_id": in.OperationID, "issue": issue.ID, "identifier": identifier, "action": action, "reason": in.Reason, "linear_project": strings.ToLower(issue.ProjectID), "kind": kind} {
		scope.Args[k] = v
	}
	if out.DuplicateOf != "" {
		scope.Args["duplicate_of"] = out.DuplicateOf
	}
	if _, e := s.cfg.Executor.Execute(context.WithoutCancel(ctx), scope); e != nil {
		code := "failed"
		var coded interface{ Code() string }
		if errors.As(e, &coded) {
			code = coded.Code()
		}
		log.Printf("clawbridge: agent cancel: the record and notice were not made: %s", code)
		return false
	}
	return true
}

// receiptIDV2 is the ID a write's receipt names, as written or replayed
// from a ledger.
func receiptIDV2(r any) string {
	switch v := r.(type) {
	case ReceiptV2:
		return v.ID
	case *ReceiptV2:
		if v != nil {
			return v.ID
		}
	case linearprovider.Receipt:
		return v.ID
	case map[string]any:
		id, _ := v["id"].(string)
		return id
	}
	raw, _ := json.Marshal(r)
	var x struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &x)
	return x.ID
}

// receiptOutcomeV2 is an edge receipt's outcome: unverified when no read
// could show the edge.
func receiptOutcomeV2(r any) string {
	raw, _ := json.Marshal(r)
	var x struct {
		Outcome    string   `json:"outcome"`
		Unverified []string `json:"unverified"`
	}
	_ = json.Unmarshal(raw, &x)
	if len(x.Unverified) > 0 {
		return "unverified"
	}
	return x.Outcome
}

// projectKindLabelsQuery reads a Linear project's labels and each one's
// group (AGENT-CANCEL R5). No schema of Linear's is vendored here; a field
// shape Linear refuses errors, and the cancel is refused as kind_unknown.
// It reads the first page only (Linear's default of 50 labels): a project
// carries a handful, and a kind label past the first page would read as no
// kind, which refuses.
const projectKindLabelsQuery = `query($id:String!){ project(id:$id){ labels { nodes { id parent { id } } } } }`

// GraphQLProjectKindLabels reads a Linear project's labels over GraphQL (a
// hosted server's Linear app), uncached: one read per cancel. Every ID
// must be a UUID.
func GraphQLProjectKindLabels(query func(ctx context.Context, q string, vars map[string]any, out any) error) func(context.Context, string) ([]ProjectLabelRef, error) {
	return func(ctx context.Context, project string) ([]ProjectLabelRef, error) {
		var out struct {
			Project *struct {
				Labels struct {
					Nodes []struct {
						ID     string
						Parent *struct{ ID string }
					}
				}
			}
		}
		if e := query(ctx, projectKindLabelsQuery, map[string]any{"id": project}, &out); e != nil {
			return nil, e
		}
		if out.Project == nil {
			return nil, errors.New("the Linear project could not be read")
		}
		refs := []ProjectLabelRef{}
		for _, n := range out.Project.Labels.Nodes {
			ref := ProjectLabelRef{ID: strings.ToLower(n.ID)}
			if n.Parent != nil {
				ref.ParentID = strings.ToLower(n.Parent.ID)
			}
			if !linearprovider.ValidID(ref.ID) || (ref.ParentID != "" && !linearprovider.ValidID(ref.ParentID)) {
				return nil, errors.New("the project label read answered an unverified label")
			}
			refs = append(refs, ref)
		}
		return refs, nil
	}
}
