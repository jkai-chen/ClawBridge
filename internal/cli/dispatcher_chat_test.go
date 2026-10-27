package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// D6-L1: what could drive a terminal is dropped from model text, and
// newlines and tabs kept.
func TestTerminalSafe(t *testing.T) {
	for in, want := range map[string]string{
		"It waited.\x1b]52;c;ZXZpbA==\x07 ok": "It waited.]52;c;ZXZpbA== ok",
		"a\u202eb\u2066c\u2069d\u202ae":       "abcde",
		"bell\x07del\x7fc1\u0085\u009b[2J":    "belldelc1[2J",
		"line one\n\tline two":                "line one\n\tline two",
		"Grüße — 日本":                          "Grüße — 日本",
	} {
		if got := terminalSafe(in); got != want {
			t.Fatalf("%q: %q, want %q", in, got, want)
		}
	}
}

// dispatcher chat needs model mode; chat show prints the asker's chat
// with its reply stripped, its state in words and where the question
// went; decisions reads the log; chat leads takes on or off.
func TestCLIDispatcherChat(t *testing.T) {
	dir := isolated(t)
	db := filepath.Join(dir, "bus.db")
	t.Setenv("CLAWBRIDGE_DB", db)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	rejectCLI(t, "dispatcher", "chat", "why?")
	rejectCLI(t, "dispatcher", "chat")
	rejectCLI(t, "dispatcher", "chat", "show")
	rejectCLI(t, "dispatcher", "chat", "leads", "maybe", "--expected-revision", "1")
	rejectCLI(t, "dispatcher", "chat", "leads", "on")
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"dispatcher", "decisions"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"data":[]`) {
		t.Fatal(e, out.String(), errs.String())
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var pid, owner string
	if err = raw.QueryRow(`SELECT p.id,w.owner_principal FROM projects p JOIN workspaces w ON w.id=p.workspace_id`).Scan(&pid, &owner); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,deadline,created_at,closed_at,outcome,outcome_body,asked_by,message) VALUES('case_1',?,'chat','message','msg_1','2026-01-01T00:02:00.000000000Z','2026-01-01T00:00:00.000000000Z','2026-01-01T00:01:00.000000000Z','reply',?,?,'{"text":"why\u202e?"}')`,
		pid, `{"tool":"reply","text":"It waited.\u001b]52;c;ZXZpbA==\u0007 \u202edone"}`, owner); err != nil {
		t.Fatal(err)
	}
	shown := invoke(t, "dispatcher", "chat", "show")
	if shown["case_id"] != "case_1" || shown["state"] != "replied" || shown["reply"] != "It waited.]52;c;ZXZpbA== done" || shown["question"] != "why?" || shown["status"] != "replied" || !strings.Contains(shown["note"].(string), "model provider") {
		t.Fatal(shown)
	}
	if _, err = raw.Exec(`UPDATE dispatcher_cases SET outcome='model_off',outcome_body=''`); err != nil {
		t.Fatal(err)
	}
	if shown = invoke(t, "dispatcher", "chat", "show", "case_1"); !strings.Contains(shown["status"].(string), "turned off") {
		t.Fatal("D6-L3:", shown)
	}
	rejectCLI(t, "dispatcher", "chat", "show", "case_2")
	var data struct{ Data []any }
	out.Reset()
	if e := Run(context.Background(), []string{"dispatcher", "decisions", "--since-hours", "2", "--limit", "3"}, &out, &errs); e != nil || json.Unmarshal(out.Bytes(), &data) != nil {
		t.Fatal(e, out.String())
	}
	rejectCLI(t, "dispatcher", "decisions", "--limit", "51")
}
