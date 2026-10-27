package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func threadsEnv(t *testing.T, environ ...string) {
	t.Helper()
	old := closesEnv
	closesEnv = func() []string { return environ }
	t.Cleanup(func() { closesEnv = old })
}

// project threads and project thread pass the ops' answers through, with
// what could drive a terminal taken out of the bodies and names.
func TestCLIProjectThreads(t *testing.T) {
	isolated(t)
	threadsEnv(t, "HOME=/x")
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "a-one")
	invoke(t, "register", "b", "--instance", "b-one")
	sent := invoke(t, "--session", "a", "send", "b", "hello\u202e\x1b[2Jthere", "--instance", "a-one", "--operation-id", "one")
	id := sent["id"].(string)

	list := invoke(t, "project", "threads", "--limit", "5")
	rows := list["threads"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["thread_id"] != id || list["readable_from"] == nil {
		t.Fatal(list)
	}
	one := invoke(t, "project", "thread", id, "--limit", "10")
	body := one["messages"].([]any)[0].(map[string]any)["body"].(string)
	if strings.ContainsAny(body, "\u202e\x1b") || !strings.Contains(body, "hello") {
		t.Fatalf("%q", body)
	}
	// A default session, here from the environment, is left out: the read
	// is sessionless.
	t.Setenv("CLAWBRIDGE_SESSION", "a")
	t.Setenv("CLAWBRIDGE_INSTANCE", "a-one")
	invoke(t, "project", "threads")
	t.Setenv("CLAWBRIDGE_SESSION", "")
	t.Setenv("CLAWBRIDGE_INSTANCE", "")

	rejectCLI(t, "project", "thread")
	rejectCLI(t, "project", "threads", "extra")
	rejectCLI(t, "project", "thread", id, "--before", "x")
}

// An explicit session or an agent's marker is refused before anything is
// sent; the server refuses any session too.
func TestCLIProjectThreadsRefuseAgents(t *testing.T) {
	isolated(t)
	threadsEnv(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "a-one")
	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		err := Run(context.Background(), args, &out, &errOut)
		if err == nil || out.Len() != 0 {
			t.Fatalf("%v: %v %s", args, err, out.String())
		}
		return err.Error()
	}
	if msg := run("--session", "a", "project", "threads"); !strings.Contains(msg, "without --session") {
		t.Fatal(msg)
	}
	for _, marker := range []string{"CLAUDECODE=1", "CODEX_HOME=/x", "CLAWBRIDGE_SESSION=a", "CLAWBRIDGE_INSTANCE=i"} {
		threadsEnv(t, "HOME=/x", marker)
		name, _, _ := strings.Cut(marker, "=")
		for _, args := range [][]string{{"project", "threads"}, {"project", "thread", "msg_" + strings.Repeat("0", 32)}} {
			if msg := run(args...); !strings.Contains(msg, name+" says an agent runs this command") {
				t.Fatal(marker, msg)
			}
		}
	}
}
