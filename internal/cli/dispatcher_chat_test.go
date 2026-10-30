package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// #121 M2: the log's explanations are model output, printed stripped.
	if _, err = raw.Exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at) VALUES('dec_1',?,'proposal','asg_1','model','{}','uncertain',?,'permitted','wait',1,?)`,
		pid, "It waited.\x1b]52;c;ZXZpbA==\x07 \u202edone\u0085", time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if e := Run(context.Background(), []string{"dispatcher", "decisions"}, &out, &errs); e != nil || !strings.Contains(out.String(), "It waited.]52;c;ZXZpbA== done") || strings.ContainsAny(out.String(), "\x1b\x07\u202e\u0085") || strings.Contains(out.String(), `\u001b`) || strings.Contains(out.String(), `\u0007`) {
		t.Fatal("D6-L1 in decisions:", e, out.String())
	}
	var data struct{ Data []any }
	out.Reset()
	if e := Run(context.Background(), []string{"dispatcher", "decisions", "--since-hours", "2", "--limit", "3"}, &out, &errs); e != nil || json.Unmarshal(out.Bytes(), &data) != nil {
		t.Fatal(e, out.String())
	}
	rejectCLI(t, "dispatcher", "decisions", "--limit", "51")
}

// dispatcher model advise takes on or off and the revision; tools set takes
// known tools only; escalations prints the model's note stripped of
// terminal controls (D6-L1).
func TestCLIDispatcherAdvise(t *testing.T) {
	dir := isolated(t)
	db := filepath.Join(dir, "bus.db")
	t.Setenv("CLAWBRIDGE_DB", db)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	rejectCLI(t, "dispatcher", "model", "advise", "maybe", "--expected-revision", "1")
	rejectCLI(t, "dispatcher", "model", "advise", "on")
	rejectCLI(t, "dispatcher", "model", "tools", "set", "ask_session")
	rejectCLI(t, "dispatcher", "model", "tools", "set", "ask_session", "route_notice", "shell", "--expected-revision", "1")
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var pid string
	if err = raw.QueryRow(`SELECT id FROM projects`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'rules','{"domains":[]}','2026-01-01T00:00:00Z','x')`,
		`INSERT INTO dispatcher_escalations(id,project_id,subject_type,subject_id,subject_revision,reason_code,recipients,state,delivered,created_at,updated_at,recommendation) VALUES('esc_1',?,'proposal','asg_1','1','untrusted','[]','open',0,'2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z','{"action":"decline","reason_code":"untrusted","explanation":"It idles.\u001b]52;c;ZXZpbA==\u0007 \u202edone","case_id":"case_1"}')`,
	} {
		if _, err = raw.Exec(q, pid); err != nil {
			t.Fatal(err)
		}
	}
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"dispatcher", "model", "tools", "set", "ask_session", "--expected-revision", "1"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"preview":true`) || !strings.Contains(out.String(), `"opt_in_tools":["ask_session"]`) {
		t.Fatal(e, out.String(), errs.String())
	}
	// --confirm reaches the server and makes the change: the revision moves.
	out.Reset()
	if e := Run(context.Background(), []string{"dispatcher", "model", "tools", "set", "ask_session", "--expected-revision", "1", "--confirm"}, &out, &errs); e != nil || strings.Contains(out.String(), `"preview":true`) || !strings.Contains(out.String(), `"revision":2`) {
		t.Fatal("tools set --confirm:", e, out.String(), errs.String())
	}
	out.Reset()
	// Advice confirmed without a model setup is refused by the server: the
	// confirm got there (a preview would have answered instead).
	if e := Run(context.Background(), []string{"dispatcher", "model", "advise", "on", "--expected-revision", "2", "--confirm"}, &out, &errs); e == nil || !strings.Contains(e.Error(), "model setup") || strings.Contains(out.String(), `"preview":true`) {
		t.Fatal("advise --confirm:", e, out.String(), errs.String())
	}
	out.Reset()
	if e := Run(context.Background(), []string{"dispatcher", "escalations", "--all"}, &out, &errs); e != nil || !strings.Contains(out.String(), `It idles.]52;c;ZXZpbA== done`) || strings.Contains(out.String(), `\u001b`) || strings.Contains(out.String(), "\u202e") {
		t.Fatal(e, out.String(), errs.String())
	}
}
