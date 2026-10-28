package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	vwView   = "b2b2b2b2-b2b2-4b2b-8b2b-b2b2b2b2b2b2"
	vwLabel  = "c3c3c3c3-c3c3-4c3c-8c3c-c3c3c3c3c3c3"
	vwMember = "d4d4d4d4-d4d4-4d4d-8d4d-d4d4d4d4d4d4"
	vwFar    = "e5e5e5e5-e5e5-4e5e-8e5e-e5e5e5e5e5e5"
)

// viewAuthority answers the stored-view ops and records each record call;
// everything else goes to the V2 fake authority.
type viewAuthority struct {
	*authorityV2
	stored   map[string]any
	records  []map[string]any
	recordOK bool
	onForget func(map[string]any)
}

func (a *viewAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "linear_view_get":
		if a.stored == nil || a.stored["view_id"] != r.Args["view_id"] {
			return bus.Result{}, errors.New("not_found")
		}
		return bus.Result{Source: "local-authority", Data: a.stored}, nil
	case "linear_view_forget":
		if a.onForget != nil {
			a.onForget(r.Args)
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"forgotten": true}}, nil
	case "linear_view_record":
		a.records = append(a.records, r.Args)
		if !a.recordOK {
			return bus.Result{}, errors.New("conflict: the task board binding changed since the preview")
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{}}, nil
	}
	return a.authorityV2.Execute(ctx, r)
}

// fakeKey plays the person's key client: it records every create and
// update, and reads back what back gives (the sent view by default).
type fakeKey struct {
	creates, updates, deletes []string
	err                       error
	back                      func(KeyView) (KeyView, error)
	last                      KeyView
}

func (k *fakeKey) DeleteView(_ context.Context, id string) error {
	k.deletes = append(k.deletes, id)
	return k.err
}

func (k *fakeKey) CreateView(_ context.Context, team, name string, filter json.RawMessage) (KeyView, error) {
	k.creates = append(k.creates, team+"|"+name+"|"+string(filter))
	if k.err != nil {
		return KeyView{}, k.err
	}
	k.last = KeyView{ID: vwView, Name: name, TeamID: team, Shared: true, Filter: string(filter), UpdatedAt: "2026-10-27T22:00:00.000Z"}
	return k.last, nil
}

func (k *fakeKey) UpdateView(_ context.Context, id, name string, filter json.RawMessage) (KeyView, error) {
	k.updates = append(k.updates, id+"|"+name+"|"+string(filter))
	if k.err != nil {
		return KeyView{}, k.err
	}
	k.last = KeyView{ID: id, Name: name, TeamID: teamID, Shared: true, Filter: string(filter), UpdatedAt: "2026-10-27T22:05:00.000Z"}
	return k.last, nil
}

func (k *fakeKey) View(context.Context, string) (KeyView, error) {
	if k.back != nil {
		return k.back(k.last)
	}
	return k.last, nil
}

func viewWriteTest(t *testing.T, views ...linearprovider.View) (*Service, *viewAuthority, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, _ := testV2(t)
	a.actor, a.name, a.role = "", "", "owner"
	va := &viewAuthority{authorityV2: a, recordOK: true}
	s.cfg.Executor = va
	p.labels = []linearprovider.Label{{ID: vwLabel, TeamID: teamID}}
	p.members = []linearprovider.Member{{ID: vwMember, TeamID: teamID}, {ID: "f6f6f6f6-f6f6-4f6f-8f6f-f6f6f6f6f6f6", TeamID: teamID}}
	pages := []linearprovider.ViewPage{{Page: linearprovider.Page[linearprovider.View]{Items: views, Complete: true}}}
	v := viewingV2{p, &pages, &[]string{}, nil}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return v, nil }
	return s, va, p, bus.Request{Workspace: "ws", Project: "prj", Args: map[string]any{}}
}

func createInput(filter string) ViewWriteInput {
	return ViewWriteInput{Action: "create", Team: teamID, Name: "Urgent bugs", Filter: []byte(filter), OperationID: "view-1"}
}

// A preview sends nothing and states the scope: the team's name and its
// member count, read through the person's MCP connection, plus the
// filter's agent_filter.
func TestViewWritePreviewStatesTheScope(t *testing.T) {
	s, a, _, scope := viewWriteTest(t)
	key := &fakeKey{}
	got, e := s.ViewWrite(context.Background(), scope, createInput(`{"or":[{"labels":{"some":{"id":{"eq":"`+vwLabel+`"}}}}]}`), key)
	if e != nil {
		t.Fatal(e)
	}
	if !got.Preview || got.Scope != "scope: team Team ("+teamID+"), a shared team view: visible to everyone in that team (2 members)" || got.AgentFilter != "unavailable" || !reflect.DeepEqual(got.Uses, []string{"or"}) || got.BindingRevision != 1 {
		t.Fatal(got)
	}
	if len(key.creates)+len(key.updates) != 0 || len(a.records) != 0 {
		t.Fatal("a preview sent or recorded")
	}
	if line := ScopeLine(teamID, "", 0, false); !strings.HasSuffix(line, "(members: unknown)") || !strings.Contains(line, "team "+teamID+",") {
		t.Fatal(line)
	}
}

// A confirmed create sends one create naming its team, reads it back,
// records it, and a replay sends nothing more.
func TestViewWriteCreateRecordsAfterTheReadBack(t *testing.T) {
	s, a, _, scope := viewWriteTest(t)
	key := &fakeKey{}
	in := createInput(`{"priority":{"eq":1},"labels":{"some":{"id":{"eq":"` + vwLabel + `"}}}}`)
	in = confirmIn(t, in, "")
	got, e := s.ViewWrite(context.Background(), scope, in, key)
	if e != nil {
		t.Fatal(e)
	}
	canonical := `{"labels":{"some":{"id":{"eq":"` + vwLabel + `"}}},"priority":{"eq":1}}`
	if got.Outcome != "accepted" || !got.Recorded || got.ViewID != vwView || got.FilterAsOf != "2026-10-27T22:00:00.000Z" || got.Preview {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(key.creates, []string{teamID + "|Urgent bugs|" + canonical}) {
		t.Fatal(key.creates)
	}
	rec := a.records[0]
	if rec["action"] != "create" || rec["view_id"] != vwView || rec["team_id"] != teamID || rec["filter"] != canonical || rec["filter_as_of"] != "2026-10-27T22:00:00.000Z" || rec["agent_filter"] != "available" || rec["expected_binding_revision"] != int64(1) || rec["expected_filter_as_of"] != nil {
		t.Fatal(rec)
	}
	again, e := s.ViewWrite(context.Background(), scope, in, key)
	if e != nil || again.Outcome != "accepted" || len(key.creates) != 1 || len(a.records) != 2 {
		t.Fatal("a replay sent again:", again, e, key.creates)
	}
}

// S2: a read-back on another team, or differing in any field, is
// uncertain: nothing is recorded, and a retry never sends again.
func TestViewWriteReadBackMismatchIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name string
		back func(KeyView) (KeyView, error)
		want []string
	}{
		{"team", func(v KeyView) (KeyView, error) { v.TeamID = otherTeamID; return v, nil }, []string{"team"}},
		{"no team", func(v KeyView) (KeyView, error) { v.TeamID = ""; return v, nil }, []string{"team"}},
		{"filter", func(v KeyView) (KeyView, error) { v.Filter = `{"priority":{"eq":2}}`; return v, nil }, []string{"filter"}},
		{"shared", func(v KeyView) (KeyView, error) { v.Shared = false; v.Name = "x"; return v, nil }, []string{"name", "shared"}},
		{"unread", func(KeyView) (KeyView, error) { return KeyView{}, errors.New("unavailable") }, []string{"read_back"}},
	} {
		s, a, _, scope := viewWriteTest(t)
		key := &fakeKey{back: tc.back}
		in := createInput(`{"priority":{"eq":1}}`)
		in = confirmIn(t, in, "")
		got, e := s.ViewWrite(context.Background(), scope, in, key)
		if e != nil || got.Outcome != "uncertain" || !reflect.DeepEqual(got.Mismatch, tc.want) || got.Recorded || len(a.records) != 0 {
			t.Fatal(tc.name, got, e)
		}
		if again, e := s.ViewWrite(context.Background(), scope, in, key); e != nil || again.Outcome != "uncertain" || len(key.creates) != 1 {
			t.Fatal(tc.name, "an uncertain write was sent again:", again, e)
		}
	}
}

// What the key reports decides the ledger: nothing sent can be retried;
// Linear's refusal is final for that operation ID; anything else is
// uncertain.
func TestViewWriteKeyFailures(t *testing.T) {
	for _, tc := range []struct {
		err   error
		check func(error) bool
	}{
		{fmt.Errorf("%w: agent", ErrKeyNotSent), func(e error) bool { var n notDispatchedError; return errors.As(e, &n) }},
		{fmt.Errorf("%w: input", ErrKeyRefused), func(e error) bool { var r refusedBeforeDispatchError; return errors.As(e, &r) }},
		{errors.New("unavailable"), IsUncertain},
	} {
		s, a, _, scope := viewWriteTest(t)
		key := &fakeKey{err: tc.err}
		in := createInput(`{"priority":{"eq":1}}`)
		in = confirmIn(t, in, "")
		if _, e := s.ViewWrite(context.Background(), scope, in, key); !tc.check(e) || len(a.records) != 0 {
			t.Fatal(tc.err, e)
		}
	}
}

// Refused before anything is read or sent: a session, a non-owner, a team
// outside the binding, a filter naming an outside ID, a taken name.
func TestViewWriteRefusals(t *testing.T) {
	try := func(name string, mod func(*Service, *viewAuthority, *bus.Request, *ViewWriteInput), want string, views ...linearprovider.View) {
		t.Helper()
		s, a, _, scope := viewWriteTest(t, views...)
		in := createInput(`{"priority":{"eq":1}}`)
		in = confirmIn(t, in, "")
		mod(s, a, &scope, &in)
		key := &fakeKey{}
		if _, e := s.ViewWrite(context.Background(), scope, in, key); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(name, e)
		}
		if len(key.creates)+len(key.updates) != 0 || len(a.records) != 0 {
			t.Fatal(name, "sent or recorded")
		}
	}
	try("session", func(_ *Service, _ *viewAuthority, r *bus.Request, _ *ViewWriteInput) { r.Session = "ses" }, "a session can't")
	try("lead", func(_ *Service, a *viewAuthority, _ *bus.Request, _ *ViewWriteInput) { a.role = "lead" }, "only the project owner")
	try("no team", func(_ *Service, _ *viewAuthority, _ *bus.Request, in *ViewWriteInput) { in.Team = "" }, "--team")
	try("unbound team", func(_ *Service, _ *viewAuthority, _ *bus.Request, in *ViewWriteInput) { in.Team = vwFar }, "not in this project's binding")
	try("outside ID", func(_ *Service, _ *viewAuthority, _ *bus.Request, in *ViewWriteInput) {
		in.Filter = []byte(`{"or":[{"creator":{"id":{"eq":"` + vwFar + `"}}}]}`)
	}, "filter.or[0].creator.id.eq names an ID outside")
	try("other team", func(_ *Service, _ *viewAuthority, _ *bus.Request, in *ViewWriteInput) {
		in.Filter = []byte(`{"team":{"id":{"eq":"` + otherTeamID + `"}}}`)
	}, "filter.team.id.eq names an ID outside")
	try("taken", func(*Service, *viewAuthority, *bus.Request, *ViewWriteInput) {}, "already has an issue view "+exampleView,
		linearprovider.View{ID: exampleView, Name: " urgent BUGS", Kind: "issue", TeamID: teamID})
	try("bad name", func(_ *Service, _ *viewAuthority, _ *bus.Request, in *ViewWriteInput) { in.Name = "a\u202eb" }, "view name")
}

// An update keeps the stored team, sends only the name and the filter,
// names what changes, and records against the row the preview read.
func TestViewWriteUpdate(t *testing.T) {
	s, a, _, scope := viewWriteTest(t, linearprovider.View{ID: vwView, Name: "Urgent bugs", Kind: "issue", TeamID: teamID})
	old := `{"priority":{"eq":1}}`
	a.stored = map[string]any{"view_id": vwView, "team_id": teamID, "name": "Urgent bugs", "filter": old, "filter_sha256": mustFilterSHA(t, old), "filter_as_of": "2026-10-27T21:00:00.000Z", "team_bound": true}
	key := &fakeKey{}
	in := ViewWriteInput{Action: "update", ViewID: vwView, Filter: []byte(`{"priority":{"in":[1,2]}}`), OperationID: "view-2"}
	got, e := s.ViewWrite(context.Background(), scope, in, key)
	if e != nil || !got.Preview || !reflect.DeepEqual(got.Changes, []string{"filter"}) || got.TeamID != teamID || got.ExpectedFilterAsOf != "2026-10-27T21:00:00.000Z" {
		t.Fatal(got, e)
	}
	in = confirmIn(t, in, "2026-10-27T21:00:00.000Z")
	key.last = KeyView{ID: vwView, Name: "Urgent bugs", TeamID: teamID, Shared: true, Filter: old, UpdatedAt: "2026-10-27T21:00:00.000Z"}
	if got, e = s.ViewWrite(context.Background(), scope, in, key); e != nil || got.Outcome != "accepted" || !got.Recorded {
		t.Fatal(got, e)
	}
	if !reflect.DeepEqual(key.updates, []string{vwView + `|Urgent bugs|{"priority":{"in":[1,2]}}`}) || len(key.creates) != 0 {
		t.Fatal(key.updates)
	}
	if rec := a.records[0]; rec["action"] != "update" || rec["expected_filter_as_of"] != "2026-10-27T21:00:00.000Z" || rec["filter_as_of"] != "2026-10-27T22:05:00.000Z" {
		t.Fatal(rec)
	}
	// --team is refused on an update, and a view whose team left the
	// binding isn't updated.
	in.OperationID, in.Team = "view-3", teamID
	if _, e = s.ViewWrite(context.Background(), scope, in, key); e == nil || !strings.Contains(e.Error(), "keeps its team") {
		t.Fatal(e)
	}
	in.Team = ""
	a.stored["team_bound"] = false
	if _, e = s.ViewWrite(context.Background(), scope, in, key); e == nil || !strings.Contains(e.Error(), "left the binding") || len(key.updates) != 1 {
		t.Fatal(e)
	}
}

// A view accepted in Linear but refused by the bus says why, and a retry
// with the same operation ID records it without sending again.
func TestViewWriteRecordRetry(t *testing.T) {
	s, a, _, scope := viewWriteTest(t)
	a.recordOK = false
	key := &fakeKey{}
	in := createInput(`{"priority":{"eq":1}}`)
	in = confirmIn(t, in, "")
	got, e := s.ViewWrite(context.Background(), scope, in, key)
	if e != nil || got.Outcome != "accepted" || got.Recorded || !strings.Contains(got.RecordError, "binding changed") {
		t.Fatal(got, e)
	}
	a.recordOK = true
	if got, e = s.ViewWrite(context.Background(), scope, in, key); e != nil || !got.Recorded || got.RecordError != "" || len(key.creates) != 1 {
		t.Fatal(got, e)
	}
}

// confirmIn is in confirmed with what its preview showed (M3): binding
// revision 1, its filter's digest and, for an update, asOf.
func confirmIn(t *testing.T, in ViewWriteInput, asOf string) ViewWriteInput {
	t.Helper()
	in.Confirm, in.ExpectedRevision, in.ExpectedFilterAsOf = true, 1, asOf
	if in.Filter != nil {
		in.ExpectedFilterSHA = mustFilterSHA(t, string(in.Filter))
	}
	return in
}

// M3: a confirm writes only what its preview showed; M2: an update never
// overwrites a change made in Linear since ClawBridge wrote the view.
func TestViewWriteConfirmHoldsThePreview(t *testing.T) {
	old := `{"priority":{"eq":1}}`
	setup := func() (*Service, *viewAuthority, bus.Request, *fakeKey, ViewWriteInput) {
		s, a, _, scope := viewWriteTest(t, linearprovider.View{ID: vwView, Name: "Urgent bugs", Kind: "issue", TeamID: teamID})
		a.stored = map[string]any{"view_id": vwView, "team_id": teamID, "name": "Urgent bugs", "filter": old, "filter_sha256": mustFilterSHA(t, old), "filter_as_of": "2026-10-27T21:00:00.000Z", "team_bound": true}
		key := &fakeKey{last: KeyView{ID: vwView, Name: "Urgent bugs", TeamID: teamID, Shared: true, Filter: old, UpdatedAt: "2026-10-27T21:00:00.000Z"}}
		in := confirmIn(t, ViewWriteInput{Action: "update", ViewID: vwView, Name: "  Urgent and high  ", Filter: []byte(`{"priority":{"in":[1,2]}}`), OperationID: "view-2"}, "2026-10-27T21:00:00.000Z")
		return s, a, scope, key, in
	}
	for name, mod := range map[string]func(*ViewWriteInput){
		"no revision": func(in *ViewWriteInput) { in.ExpectedRevision = -1 },
		"revision":    func(in *ViewWriteInput) { in.ExpectedRevision = 2 },
		"no digest":   func(in *ViewWriteInput) { in.ExpectedFilterSHA = "" },
		"digest":      func(in *ViewWriteInput) { in.ExpectedFilterSHA = mustFilterSHA(t, old) },
		"no as of":    func(in *ViewWriteInput) { in.ExpectedFilterAsOf = "" },
		"as of":       func(in *ViewWriteInput) { in.ExpectedFilterAsOf = "2026-10-27T20:00:00.000Z" },
	} {
		s, a, scope, key, in := setup()
		mod(&in)
		want := "changed since the preview"
		if strings.HasPrefix(name, "no ") {
			want = "repeat the command the preview printed"
		}
		if _, e := s.ViewWrite(context.Background(), scope, in, key); e == nil || !strings.Contains(e.Error(), want) || len(key.updates) != 0 || len(a.records) != 0 {
			t.Fatal(name, e)
		}
	}
	for name, mod := range map[string]func(*KeyView){
		"name":   func(v *KeyView) { v.Name = "Renamed in Linear" },
		"filter": func(v *KeyView) { v.Filter = `{"priority":{"eq":3}}` },
		"shared": func(v *KeyView) { v.Shared = false },
	} {
		s, a, scope, key, in := setup()
		mod(&key.last)
		_, e := s.ViewWrite(context.Background(), scope, in, key)
		var refused refusedBeforeDispatchError
		if !errors.As(e, &refused) || !strings.Contains(e.Error(), "changed in Linear") || !strings.Contains(e.Error(), name) || len(key.updates) != 0 || len(a.records) != 0 {
			t.Fatal(name, e)
		}
	}
	// A view Linear no longer has is refused for good; a read that failed
	// leaves the operation free to retry.
	for _, tc := range []struct {
		err   error
		check func(error) bool
	}{
		{fmt.Errorf("%w: gone", ErrKeyNotFound), func(e error) bool { var r refusedBeforeDispatchError; return errors.As(e, &r) }},
		{errors.New("unavailable"), func(e error) bool { var n notDispatchedError; return errors.As(e, &n) }},
	} {
		s, _, scope, key, in := setup()
		key.back = func(KeyView) (KeyView, error) { return KeyView{}, tc.err }
		if _, e := s.ViewWrite(context.Background(), scope, in, key); !tc.check(e) || len(key.updates) != 0 {
			t.Fatal(tc.err, e)
		}
	}
	// Unchanged in Linear: sent once, the name trimmed (L5).
	s, _, scope, key, in := setup()
	if got, e := s.ViewWrite(context.Background(), scope, in, key); e != nil || got.Outcome != "accepted" || !reflect.DeepEqual(key.updates, []string{vwView + `|Urgent and high|{"priority":{"in":[1,2]}}`}) {
		t.Fatal(got, e, key.updates)
	}
	// L6: a read-back of another view is uncertain.
	s, _, scope, key, _ = setup()
	key.back = func(v KeyView) (KeyView, error) { v.ID = exampleView; return v, nil }
	in = confirmIn(t, createInput(`{"priority":{"eq":1}}`), "")
	in.Name = "Other bugs"
	if got, e := s.ViewWrite(context.Background(), scope, in, key); e != nil || got.Outcome != "uncertain" || !reflect.DeepEqual(got.Mismatch, []string{"id"}) {
		t.Fatal(got, e)
	}
}

func mustFilterSHA(t *testing.T, text string) string {
	t.Helper()
	f, e := ParseViewFilter([]byte(text))
	if e != nil {
		t.Fatal(e)
	}
	return f.SHA256
}
