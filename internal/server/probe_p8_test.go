package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

const (
	p8Project = "e0000000-0000-4000-8000-0000000000e1"
	p8Update  = "e0000000-0000-4000-8000-0000000000f1"
)

// fakeP8 scripts the app's answers and records the calls P8 made.
type fakeP8 struct {
	project   linearapp.P8Project
	preflight error
	saveID    string
	saveErr   error
	reads     [][]linearprovider.StatusUpdateRef
	readErrs  []error
	deleteErr error
	readsDone int
	calls     []string
	closed    bool
}

func (f *fakeP8) ProbeP8Project(context.Context, string) (linearapp.P8Project, error) {
	f.calls = append(f.calls, "precheck")
	return f.project, nil
}
func (f *fakeP8) StatusUpdater(context.Context) (statusUpdater, error) { return f, nil }
func (f *fakeP8) StatusUpdateSchemas() map[string]any {
	return map[string]any{"save_status_update": map[string]any{"required": []string{"type"}}}
}
func (f *fakeP8) StatusUpdatePreflight(string) error {
	f.calls = append(f.calls, "preflight")
	return f.preflight
}
func (f *fakeP8) SaveStatusUpdate(_ context.Context, project, body, health string) (string, error) {
	if project != p8Project || body != p8Body || health != "onTrack" {
		panic("unexpected save input")
	}
	f.calls = append(f.calls, "save")
	return f.saveID, f.saveErr
}
func (f *fakeP8) StatusUpdates(_ context.Context, _ string, marker string) ([]linearprovider.StatusUpdateRef, error) {
	if !strings.Contains(p8Body, marker) {
		panic("the marker is not in the body")
	}
	i := f.readsDone
	f.readsDone++
	f.calls = append(f.calls, "read")
	var rows []linearprovider.StatusUpdateRef
	var err error
	if i < len(f.reads) {
		rows = f.reads[i]
	}
	if i < len(f.readErrs) {
		err = f.readErrs[i]
	}
	return rows, err
}
func (f *fakeP8) DeleteStatusUpdate(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete:"+id)
	return f.deleteErr
}
func (f *fakeP8) Close() error { f.closed = true; return nil }

func runP8(t *testing.T, f *fakeP8, dry bool) (map[string]any, string, error) {
	t.Helper()
	saved := newP8App
	t.Cleanup(func() { newP8App = saved })
	newP8App = func(linearapp.Store) (p8App, error) { return f, nil }
	var out bytes.Buffer
	err := linearAppP8(context.Background(), nil, p8Project, dry, &out)
	var got map[string]map[string]any
	if json.Unmarshal(out.Bytes(), &got) != nil {
		t.Fatal("output is not JSON:", out.String())
	}
	if strings.Contains(out.String(), p8Body) {
		t.Fatal("the probe printed the body")
	}
	return got["p8"], strings.Join(f.calls, ","), err
}

var emptyProject = linearapp.P8Project{Found: true}

func marked(id string) []linearprovider.StatusUpdateRef {
	return []linearprovider.StatusUpdateRef{{ID: id, Health: "onTrack", Marked: true}}
}

// The whole round trip: saved, read back, deleted, gone.
func TestP8PostsReadsDeletesAndConfirms(t *testing.T) {
	f := &fakeP8{project: emptyProject, saveID: p8Update, reads: [][]linearprovider.StatusUpdateRef{marked(p8Update), {}}}
	out, calls, err := runP8(t, f, false)
	if err != nil || calls != "precheck,preflight,save,read,delete:"+p8Update+",read" || !f.closed {
		t.Fatal(err, calls)
	}
	read := out["read"].(map[string]any)
	if out["save"] != "ok" || read["found"] != float64(1) || read["health"] != "onTrack" || out["update_id"] != p8Update || out["delete"] != "ok" || out["read_after_delete"].(map[string]any)["present"] != false {
		t.Fatal(out)
	}
}

// Nothing is sent to a project with issues, members or a lead, or when
// the tools could not also list and delete what they create; a dry run
// stops before the save.
func TestP8RefusesBeforeSending(t *testing.T) {
	for name, p := range map[string]linearapp.P8Project{"missing": {}, "issues": {Found: true, Issues: true}, "members": {Found: true, Members: true}, "lead": {Found: true, Lead: true}} {
		_, calls, err := runP8(t, &fakeP8{project: p}, false)
		if err == nil || calls != "precheck" {
			t.Fatal(name, err, calls)
		}
	}
	out, calls, err := runP8(t, &fakeP8{project: emptyProject, preflight: &linearprovider.Error{Kind: "capability_unavailable"}}, false)
	if err == nil || calls != "precheck,preflight" || out["preflight"] != "capability_unavailable" {
		t.Fatal(out, calls, err)
	}
	out, calls, err = runP8(t, &fakeP8{project: emptyProject}, true)
	if err != nil || calls != "precheck,preflight" || out["dry_run"] != true || out["schemas"] == nil {
		t.Fatal(out, calls, err)
	}
}

// A refused save that left nothing is the answer, with no delete.
func TestP8RefusedSaveIsTheAnswer(t *testing.T) {
	f := &fakeP8{project: emptyProject, saveErr: &linearprovider.Error{Kind: "tool_error", LinearCode: "FORBIDDEN"}, reads: [][]linearprovider.StatusUpdateRef{{}}}
	out, calls, err := runP8(t, f, false)
	if err != nil || calls != "precheck,preflight,save,read" || out["save"] != "tool_error:FORBIDDEN" {
		t.Fatal(out, calls, err)
	}
}

// A save whose answer was lost is found by its marker and deleted.
func TestP8LostAnswerIsFoundAndDeleted(t *testing.T) {
	f := &fakeP8{project: emptyProject, saveErr: &linearprovider.Error{Kind: "uncertain"}, reads: [][]linearprovider.StatusUpdateRef{marked(p8Update), {}}}
	out, calls, err := runP8(t, f, false)
	if err != nil || calls != "precheck,preflight,save,read,delete:"+p8Update+",read" || out["read"].(map[string]any)["id_from"] != "read" {
		t.Fatal(out, calls, err)
	}
}

// Every way an update could be left behind is an ALERT naming it.
func TestP8AlertsWhenAnUpdateMayRemain(t *testing.T) {
	for name, f := range map[string]*fakeP8{
		"delete failed":       {project: emptyProject, saveID: p8Update, reads: [][]linearprovider.StatusUpdateRef{marked(p8Update), marked(p8Update)}, deleteErr: &linearprovider.Error{Kind: "tool_error"}},
		"still present":       {project: emptyProject, saveID: p8Update, reads: [][]linearprovider.StatusUpdateRef{marked(p8Update), marked(p8Update)}},
		"unconfirmed":         {project: emptyProject, saveID: p8Update, reads: [][]linearprovider.StatusUpdateRef{marked(p8Update)}, readErrs: []error{nil, errors.New("down")}},
		"unreadable and lost": {project: emptyProject, saveErr: &linearprovider.Error{Kind: "uncertain"}, readErrs: []error{errors.New("down")}},
		"several marked":      {project: emptyProject, saveErr: &linearprovider.Error{Kind: "uncertain"}, reads: [][]linearprovider.StatusUpdateRef{append(marked(p8Update), marked("e0000000-0000-4000-8000-0000000000f2")...)}},
	} {
		_, calls, err := runP8(t, f, false)
		if err == nil || !strings.HasPrefix(err.Error(), "ALERT") {
			t.Fatal(name, err, calls)
		}
		if strings.Contains(calls, "save") && name != "unreadable and lost" && name != "several marked" && !strings.Contains(err.Error(), p8Update) {
			t.Fatal(name, "the ALERT does not name the update:", err)
		}
	}
}

func TestP8Arguments(t *testing.T) {
	for _, args := range [][]string{{"linear-app-probe", "--confirm", "--p8"}, {"linear-app-probe", "--confirm", "--dry-run"}, {"linear-app-probe", "--p8", "--project", p8Project}} {
		var out bytes.Buffer
		if err := RunOperator(context.Background(), args, &out); err == nil || out.Len() != 0 {
			t.Fatal(args, err)
		}
	}
}
