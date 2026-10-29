package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Deleting a view (LINEAR-VIEWS-PLAN D4, 3c): the project owner only,
// without a session, with --confirm, and only a view ClawBridge wrote in
// this project. The view stays in Linear until the owner's key deletes it;
// a read-back must then find it gone before the stored row is dropped.

// ViewDeleteInput is one delete; without Confirm it's a preview.
type ViewDeleteInput struct {
	ViewID, OperationID string
	Confirm             bool
	// ExpectedFilterAsOf is the stored row the preview showed; a confirm
	// is refused if the row changed since.
	ExpectedFilterAsOf string
}

// ViewDeleteResult is a preview, or a confirmed delete's outcome.
type ViewDeleteResult struct {
	// Kind is view_delete on a confirmed delete's receipt.
	Kind         string `json:"kind,omitempty"`
	Preview      bool   `json:"preview"`
	Scope        string `json:"scope"`
	TeamID       string `json:"team_id"`
	TeamName     string `json:"team_name,omitempty"`
	Members      int    `json:"members,omitempty"`
	MembersKnown bool   `json:"members_known"`
	ViewID       string `json:"view_id"`
	Name         string `json:"name"`
	// ExpectedFilterAsOf is the stored row the delete must still find.
	ExpectedFilterAsOf string `json:"expected_filter_as_of"`
	// Outcome is deleted or uncertain (the read-back still found it, or
	// couldn't tell).
	Outcome     string `json:"outcome,omitempty"`
	Forgotten   bool   `json:"forgotten"`
	ForgetError string `json:"forget_error,omitempty"`
}

// ViewDelete previews or deletes one view ClawBridge wrote.
func (s *Service) ViewDelete(ctx context.Context, scope bus.Request, in ViewDeleteInput, key ViewKey) (ViewDeleteResult, error) {
	var none ViewDeleteResult
	if !linearprovider.ValidID(in.ViewID) {
		return none, errors.New("a delete names its view by ID")
	}
	if !operationPattern.MatchString(in.OperationID) {
		return none, errors.New("operation_id requires 1–128 ASCII identifier characters")
	}
	if scope.Session != "" {
		return none, errors.New("a session can't delete a Linear view; the project owner runs it in their own terminal")
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
		return none, errors.New("only the project owner, without a session, deletes a Linear view")
	}
	// Only a view ClawBridge wrote here: one it never recorded is not_found.
	stored, e := s.storedView(ctx, scope, in.ViewID)
	if e != nil {
		return none, e
	}
	out := ViewDeleteResult{TeamID: stored.team, ViewID: in.ViewID, Name: stored.name, ExpectedFilterAsOf: stored.asOf}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return none, e
	}
	defer done()
	out.TeamName, out.Members, out.MembersKnown = teamLine(ctx, p, stored.team)
	out.Scope = ScopeLine(out.TeamID, out.TeamName, out.Members, out.MembersKnown)
	if !in.Confirm {
		out.Preview = true
		return out, nil
	}
	if in.ExpectedFilterAsOf == "" {
		return none, errors.New("a confirm carries the preview's filter_as_of; repeat the command the preview printed")
	}
	if in.ExpectedFilterAsOf != stored.asOf {
		return none, errors.New("the stored view changed since the preview; preview again; nothing was sent")
	}
	namespace := digest([]string{s.cfg.Authority, b.WorkspaceID, b.ProjectID, access.SessionID})
	payload := struct {
		Version       int
		Kind, Profile string
		View, Team    string
		ExpectedAsOf  string
	}{1, "view_delete", s.cfg.ProfileID, in.ViewID, stored.team, stored.asOf}
	l := s.operations(ctx)
	if r, exists, e := l.lookup(namespace, in.OperationID, payload); exists || e != nil {
		if e != nil {
			return none, e
		}
		replay, e := deleteReceipt(r)
		if e != nil {
			return none, e
		}
		return s.forgetDeleted(ctx, scope, replay), nil
	}
	receipt, e := l.write(namespace, in.OperationID, ledgerMeta{"view_delete", b.ProjectID}, payload, func() (any, error) {
		// A view changed in Linear since ClawBridge wrote it is someone's
		// work now: it isn't deleted. One already gone is decided below.
		cur, e := key.View(ctx, in.ViewID)
		switch {
		case errors.Is(e, ErrKeyRefused):
			return nil, refusedBeforeDispatch(e)
		case e != nil && !errors.Is(e, ErrKeyNotFound):
			return nil, notDispatched(e)
		case e == nil:
			if drift := viewMismatch(cur, in.ViewID, stored.team, stored.kind, stored.name, stored.filter); len(drift) > 0 {
				return nil, refusedBeforeDispatch(fmt.Errorf("the view was changed in Linear since ClawBridge wrote it (%s); nothing was deleted", strings.Join(drift, ", ")))
			}
		}
		e = key.DeleteView(ctx, in.ViewID)
		switch {
		case errors.Is(e, ErrKeyNotSent):
			return nil, notDispatched(e)
		case errors.Is(e, ErrKeyRefused):
			return nil, refusedBeforeDispatch(e)
		case e != nil && !errors.Is(e, ErrKeyNotFound):
			return nil, e
		}
		// Gone already, or deleted now: the read-back decides.
		r := out
		r.Kind, r.Preview = "view_delete", false
		if _, e = key.View(ctx, in.ViewID); errors.Is(e, ErrKeyNotFound) {
			r.Outcome = "deleted"
		} else {
			r.Outcome = "uncertain"
		}
		return r, nil
	})
	if e != nil {
		return none, e
	}
	r, e := deleteReceipt(receipt)
	if e != nil {
		return none, e
	}
	return s.forgetDeleted(ctx, scope, r), nil
}

// teamLine reads the team's name and member count for the scope line; any
// failure leaves them unknown.
func teamLine(ctx context.Context, p Provider, team string) (string, int, bool) {
	name := ""
	if got, e := p.GetTeam(ctx, team); e == nil && got.ID == team {
		name = got.Name
	}
	v2, ok := p.(ProviderV2)
	if !ok {
		return name, 0, false
	}
	cursor, count := "", 0
	for page := 0; page < viewScopePages; page++ {
		members, e := v2.Members(ctx, team, linearprovider.PageOptions{Limit: viewPageSize, Cursor: cursor})
		if e != nil {
			return name, 0, false
		}
		count += len(members.Items)
		if members.Complete {
			return name, count, true
		}
		cursor = members.NextCursor
	}
	return name, 0, false
}

// forgetDeleted drops the stored row of a view Linear no longer has.
func (s *Service) forgetDeleted(ctx context.Context, scope bus.Request, r ViewDeleteResult) ViewDeleteResult {
	if r.Outcome != "deleted" {
		return r
	}
	scope.Operation, scope.Args = "linear_view_forget", map[string]any{"view_id": r.ViewID, "expected_filter_as_of": r.ExpectedFilterAsOf}
	if _, e := s.cfg.Executor.Execute(ctx, scope); e != nil {
		r.Forgotten, r.ForgetError = false, e.Error()
		return r
	}
	r.Forgotten, r.ForgetError = true, ""
	return r
}

func deleteReceipt(v any) (ViewDeleteResult, error) {
	var r ViewDeleteResult
	raw, e := json.Marshal(v)
	if e != nil || !validDeleteReceipt(raw) || json.Unmarshal(raw, &r) != nil {
		return ViewDeleteResult{}, errors.New("invalid view receipt")
	}
	return r, nil
}

// validDeleteReceipt is the ledger's check of a recorded delete receipt.
func validDeleteReceipt(raw []byte) bool {
	var r ViewDeleteResult
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Kind != "view_delete" || r.Preview {
		return false
	}
	return (r.Outcome == "deleted" || r.Outcome == "uncertain") && linearprovider.ValidID(r.ViewID) && linearprovider.ValidID(r.TeamID)
}
