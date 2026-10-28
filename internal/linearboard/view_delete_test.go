package linearboard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const deleteAsOf = "2026-10-27T21:00:00.000Z"

func storedDeletable(t *testing.T, a *viewAuthority) {
	t.Helper()
	storeView(t, a, `{"priority":{"eq":1}}`, true)
}

// inLinear is the stored view as Linear shows it, unchanged.
func inLinear() KeyView {
	return KeyView{ID: vwView, Name: "Urgent bugs", TeamID: teamID, Shared: true, Filter: `{"priority":{"eq":1}}`, UpdatedAt: deleteAsOf}
}

// A preview names the view and its scope and sends nothing; the confirm
// deletes, reads the view back as gone, and forgets the stored row; a
// replay sends nothing more.
func TestViewDelete(t *testing.T) {
	s, a, _, scope := viewWriteTest(t)
	storedDeletable(t, a)
	gone := func(KeyView) (KeyView, error) { return KeyView{}, fmt.Errorf("%w: gone", ErrKeyNotFound) }
	key := &fakeKey{back: gone}
	in := ViewDeleteInput{ViewID: vwView, OperationID: "del-1"}
	got, e := s.ViewDelete(context.Background(), scope, in, key)
	if e != nil || !got.Preview || got.Name != "Urgent bugs" || !strings.HasPrefix(got.Scope, "scope: team Team ("+teamID+")") || !strings.HasSuffix(got.Scope, "(2 members)") || len(key.deletes) != 0 {
		t.Fatal(got, e)
	}
	in.Confirm, in.ExpectedFilterAsOf = true, deleteAsOf
	var forgets []map[string]any
	a.onForget = func(args map[string]any) { forgets = append(forgets, args) }
	if got, e = s.ViewDelete(context.Background(), scope, in, key); e != nil || got.Outcome != "deleted" || !got.Forgotten || got.Preview {
		t.Fatal(got, e)
	}
	if !reflect.DeepEqual(key.deletes, []string{vwView}) || len(forgets) != 1 || forgets[0]["expected_filter_as_of"] != "2026-10-27T21:00:00.000Z" {
		t.Fatal(key.deletes, forgets)
	}
	if again, e := s.ViewDelete(context.Background(), scope, in, key); e != nil || again.Outcome != "deleted" || len(key.deletes) != 1 {
		t.Fatal("a replay deleted again:", again, e)
	}
}

// A view still found after the delete is uncertain and stays stored; one
// Linear no longer has (deleted by hand) is forgotten; the key's refusals
// sort as for writes.
func TestViewDeleteOutcomes(t *testing.T) {
	s, a, _, scope := viewWriteTest(t)
	storedDeletable(t, a)
	forgot := 0
	a.onForget = func(map[string]any) { forgot++ }
	key := &fakeKey{last: inLinear()}
	if got, e := s.ViewDelete(context.Background(), scope, ViewDeleteInput{ViewID: vwView, OperationID: "del-1", Confirm: true, ExpectedFilterAsOf: deleteAsOf}, key); e != nil || got.Outcome != "uncertain" || got.Forgotten || forgot != 0 {
		t.Fatal(got, e)
	}
	key = &fakeKey{err: fmt.Errorf("%w: gone", ErrKeyNotFound), back: func(KeyView) (KeyView, error) { return KeyView{}, ErrKeyNotFound }}
	if got, e := s.ViewDelete(context.Background(), scope, ViewDeleteInput{ViewID: vwView, OperationID: "del-2", Confirm: true, ExpectedFilterAsOf: deleteAsOf}, key); e != nil || got.Outcome != "deleted" || !got.Forgotten {
		t.Fatal(got, e)
	}
	for i, tc := range []struct {
		err   error
		check func(error) bool
	}{
		{fmt.Errorf("%w: agent", ErrKeyNotSent), func(e error) bool { var n notDispatchedError; return errors.As(e, &n) }},
		{fmt.Errorf("%w: input", ErrKeyRefused), func(e error) bool { var r refusedBeforeDispatchError; return errors.As(e, &r) }},
		{errors.New("unavailable"), IsUncertain},
	} {
		key = &fakeKey{err: tc.err, last: inLinear()}
		if _, e := s.ViewDelete(context.Background(), scope, ViewDeleteInput{ViewID: vwView, OperationID: fmt.Sprintf("del-x%d", i), Confirm: true, ExpectedFilterAsOf: deleteAsOf}, key); !tc.check(e) {
			t.Fatal(tc.err, e)
		}
	}
}

// Refused before anything is sent: a session, a lead, a view ClawBridge
// didn't write, a bad ID.
func TestViewDeleteRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(*viewAuthority, *ViewDeleteInput, *string)
		want string
	}{
		"session":  {func(_ *viewAuthority, _ *ViewDeleteInput, session *string) { *session = "ses" }, "a session can't"},
		"lead":     {func(a *viewAuthority, _ *ViewDeleteInput, _ *string) { a.role = "lead" }, "only the project owner"},
		"not ours": {func(a *viewAuthority, _ *ViewDeleteInput, _ *string) { a.stored = nil }, "not_found"},
		"no as of": {func(_ *viewAuthority, in *ViewDeleteInput, _ *string) { in.ExpectedFilterAsOf = "" }, "repeat the command the preview printed"},
		"as of": {func(_ *viewAuthority, in *ViewDeleteInput, _ *string) {
			in.ExpectedFilterAsOf = "2026-10-27T20:00:00.000Z"
		}, "changed since the preview"},
		"bad id": {func(_ *viewAuthority, in *ViewDeleteInput, _ *string) { in.ViewID = "Urgent bugs" }, "names its view by ID"},
	} {
		s, a, _, scope := viewWriteTest(t)
		storedDeletable(t, a)
		in := ViewDeleteInput{ViewID: vwView, OperationID: "del-1", Confirm: true, ExpectedFilterAsOf: deleteAsOf}
		tc.mod(a, &in, &scope.Session)
		key := &fakeKey{}
		if _, e := s.ViewDelete(context.Background(), scope, in, key); e == nil || !strings.Contains(e.Error(), tc.want) || len(key.deletes) != 0 {
			t.Fatal(name, e)
		}
	}
}

// A view someone changed in Linear since ClawBridge wrote it isn't deleted;
// a refused read is final, an unreadable one free to retry.
func TestViewDeleteLeavesAChangedView(t *testing.T) {
	for name, tc := range map[string]struct {
		back  func(KeyView) (KeyView, error)
		check func(error) bool
	}{
		"renamed": {func(KeyView) (KeyView, error) { v := inLinear(); v.Name = "Mine now"; return v, nil }, func(e error) bool {
			var r refusedBeforeDispatchError
			return errors.As(e, &r) && strings.Contains(e.Error(), "name")
		}},
		"refiltered": {func(KeyView) (KeyView, error) { v := inLinear(); v.Filter = `{"priority":{"eq":2}}`; return v, nil }, func(e error) bool {
			var r refusedBeforeDispatchError
			return errors.As(e, &r) && strings.Contains(e.Error(), "filter")
		}},
		"refused": {func(KeyView) (KeyView, error) { return KeyView{}, fmt.Errorf("%w: input", ErrKeyRefused) }, func(e error) bool { var r refusedBeforeDispatchError; return errors.As(e, &r) }},
		"unread":  {func(KeyView) (KeyView, error) { return KeyView{}, errors.New("unavailable") }, func(e error) bool { var n notDispatchedError; return errors.As(e, &n) }},
	} {
		s, a, _, scope := viewWriteTest(t)
		storedDeletable(t, a)
		key := &fakeKey{back: tc.back}
		_, e := s.ViewDelete(context.Background(), scope, ViewDeleteInput{ViewID: vwView, OperationID: "del-1", Confirm: true, ExpectedFilterAsOf: deleteAsOf}, key)
		if !tc.check(e) || len(key.deletes) != 0 {
			t.Fatal(name, e, key.deletes)
		}
	}
}
