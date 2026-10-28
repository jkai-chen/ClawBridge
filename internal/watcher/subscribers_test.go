package watcher

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

const (
	subTeam    = "00000000-0000-4000-8000-00000000aa01"
	subOther   = "00000000-0000-4000-8000-00000000aa02"
	subIssue   = "00000000-0000-4000-8000-00000000bb01"
	subPerson  = "00000000-0000-4000-8000-00000000cc01"
	subLeft    = "00000000-0000-4000-8000-00000000cc02"
	ownerEmail = "owner@example.test"
)

// subscribeStore answers the pass's two bus calls: the due rows, and each
// settle, recorded.
type subscribeStore struct {
	Store
	rows    []any
	settles []map[string]any
}

func (s *subscribeStore) ExecuteSystem(_ context.Context, _ string, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "subscribe_due":
		return bus.Result{Data: map[string]any{"rows": s.rows, "left": 0, "stopped": ""}}, nil
	case "subscribe_settle":
		s.settles = append(s.settles, r.Args)
		return bus.Result{Data: map[string]any{"state": r.Args["outcome"]}}, nil
	}
	return bus.Result{}, errors.New("unexpected " + r.Operation)
}

func subscribeRun(t *testing.T, f *fakeLinear, row map[string]any) (*SubscribePass, map[string]any) {
	t.Helper()
	store := &subscribeStore{rows: []any{row}}
	w := &Watcher{Store: store, App: fakeApp{f}}
	pass, err := w.subscribePass(context.Background(), "prj_1", map[string]any{"lease_token": "t", "generation": 1}, func() []string { return []string{subTeam} })
	if err != nil {
		t.Fatal(err)
	}
	if len(store.settles) != 1 {
		t.Fatal(store.settles)
	}
	return pass, store.settles[0]
}

func subscribeRow(people ...string) map[string]any {
	row := map[string]any{"issue_uuid": subIssue, "user_ids": []any{}, "charged": 1 + len(people) + 1}
	for _, p := range people {
		row["user_ids"] = append(row["user_ids"].([]any), p)
	}
	row["owner_email"] = ownerEmail
	return row
}

// The owner by email and each member by ID; a person no longer in the
// issue's team is left out (B5); the settle carries the calls made and
// the row's charge, fenced.
func TestSubscribePassSubscribesTheOwnerAndMembers(t *testing.T) {
	f := &fakeLinear{subscribeTeam: subTeam, subscribeMembers: []string{subPerson}}
	pass, settle := subscribeRun(t, f, subscribeRow(subPerson, subLeft))
	if strings.Join(f.subscribed, ",") != subIssue+" "+ownerEmail+","+subIssue+" "+subPerson {
		t.Fatal(f.subscribed)
	}
	if settle["outcome"] != "done" || settle["reason"] != "partial" || settle["calls"] != 3 || settle["charged"] != 4 || settle["lease_token"] != "t" || pass.Done != 1 {
		t.Fatalf("%+v", settle)
	}
	// Everyone added: no reason.
	f = &fakeLinear{subscribeTeam: subTeam, subscribeMembers: []string{subPerson}}
	if _, settle = subscribeRun(t, f, subscribeRow(subPerson)); settle["outcome"] != "done" || settle["reason"] != "" {
		t.Fatalf("%+v", settle)
	}
}

// B1: an issue outside the bound teams, or gone, is skipped after the one
// read; nobody is subscribed.
func TestSubscribePassSkipsAnIssueOutsideTheBinding(t *testing.T) {
	f := &fakeLinear{subscribeTeam: subOther}
	if _, settle := subscribeRun(t, f, subscribeRow()); settle["outcome"] != "skipped" || settle["reason"] != "not_bound" || settle["calls"] != 1 || len(f.subscribed) != 0 {
		t.Fatalf("%+v %v", settle, f.subscribed)
	}
	f = &fakeLinear{}
	if _, settle := subscribeRun(t, f, subscribeRow()); settle["outcome"] != "skipped" || settle["reason"] != "not_found" {
		t.Fatalf("%+v", settle)
	}
	// A read that fails is tried again.
	f = &fakeLinear{subscribeErr: errors.New("unavailable")}
	if pass, settle := subscribeRun(t, f, subscribeRow()); settle["outcome"] != "retry" || settle["reason"] != "error" || pass.Retry != 1 {
		t.Fatalf("%+v", settle)
	}
}

// Linear refusing the owner's email: the others are still subscribed and
// the bus remembers it; alone, the row waits. The email is never logged or
// settled (B4).
func TestSubscribePassOwnerNotInLinear(t *testing.T) {
	var logged bytes.Buffer
	prev := logf
	logf = log.New(&logged, "", 0).Printf
	t.Cleanup(func() { logf = prev })
	f := &fakeLinear{subscribeTeam: subTeam, subscribeMembers: []string{subPerson}, subscribeClass: map[string]string{ownerEmail: "not_found"}}
	pass, settle := subscribeRun(t, f, subscribeRow(subPerson))
	if settle["outcome"] != "done" || settle["reason"] != "owner_not_in_linear" || settle["owner_missing"] != true {
		t.Fatalf("%+v", settle)
	}
	logTick("prj_1", "tick", ProjectTick{Subscribers: pass})
	f = &fakeLinear{subscribeTeam: subTeam, subscribeClass: map[string]string{ownerEmail: "refused"}}
	if _, settle = subscribeRun(t, f, subscribeRow()); settle["outcome"] != "retry" || settle["reason"] != "owner_not_in_linear" || settle["owner_missing"] != true {
		t.Fatalf("%+v", settle)
	}
	for _, s := range []string{logged.String(), strings.Join(mapStrings(settle), " ")} {
		if strings.Contains(s, ownerEmail) {
			t.Fatal("the owner's email escaped:", s)
		}
	}
	if !strings.Contains(logged.String(), "subscribers: project prj_1 (tick): done 1") {
		t.Fatal(logged.String())
	}
}

// A token that may not subscribe people says so and waits; a subscribe that
// fails for everyone is tried again.
func TestSubscribePassNoScopeAndFailures(t *testing.T) {
	f := &fakeLinear{subscribeTeam: subTeam, subscribeMembers: []string{subPerson}, subscribeClass: map[string]string{ownerEmail: "scope", subPerson: "scope"}}
	if _, settle := subscribeRun(t, f, subscribeRow(subPerson)); settle["outcome"] != "retry" || settle["reason"] != "no_scope" {
		t.Fatalf("%+v", settle)
	}
	f = &fakeLinear{subscribeTeam: subTeam, subscribeMembers: []string{subPerson}, subscribeClass: map[string]string{ownerEmail: "unavailable", subPerson: "unavailable"}}
	if _, settle := subscribeRun(t, f, subscribeRow(subPerson)); settle["outcome"] != "retry" || settle["reason"] != "error" {
		t.Fatalf("%+v", settle)
	}
}

func mapStrings(m map[string]any) []string {
	out := []string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out = append(out, k+"="+s)
		}
	}
	return out
}
