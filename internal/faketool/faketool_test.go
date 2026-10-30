package faketool

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

func TestInstalledFakeActsAsTheTool(t *testing.T) {
	dir := t.TempDir()
	path := Install(t, dir, "fake tool", "args")
	out, err := exec.Command(path, "projects", "describe", "a b").Output()
	if err != nil || string(out) != "projects\ndescribe\na b\n" {
		t.Fatalf("%q %v", out, err)
	}
	t.Setenv(env, "exit:3")
	err = exec.Command(path).Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatal("exit status not reproduced", err)
	}
	var b bytes.Buffer
	if run("print:token", nil, &b) != 0 || b.String() != "token" || run("nonsense", nil, &b) == 0 || !strings.Contains(path, "fake tool") {
		t.Fatal("behaviours", b.String())
	}
}

func TestRecordBehavioursWriteTheArgumentsAndAnswerQueueHelp(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv")
	t.Setenv(LogEnv, log)
	path := Install(t, t.TempDir(), "codex", "codex-queue")
	out, err := exec.Command(path, "queue", "--help").Output()
	if err != nil || string(out) != "queue --thread --message\n" {
		t.Fatalf("%q %v", out, err)
	}
	if out, err = exec.Command(path, "queue", "a b", "$(x)").Output(); err != nil || !strings.Contains(string(out), "synthetic") {
		t.Fatalf("%q %v", out, err)
	}
	if b, err := os.ReadFile(log); err != nil || string(b) != "queue\x00a b\x00$(x)\x00" {
		t.Fatalf("%q", b)
	}
}

func TestAppendKeepsEveryRun(t *testing.T) {
	log := filepath.Join(t.TempDir(), "runs")
	t.Setenv(LogEnv, log)
	path := Install(t, t.TempDir(), "notifier", "append")
	for _, input := range []string{"first\n", "{\"n\":2}\n"} {
		cmd := exec.Command(path, "--text", "a b")
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = append(os.Environ(), "CLAWBRIDGE_NOTICE_TEXT=notice")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%q %v", out, err)
		}
	}
	runs, err := ReadRuns(log)
	if err != nil || len(runs) != 2 || runs[1].Stdin != "{\"n\":2}\n" || strings.Join(runs[0].Args, "|") != "--text|a b" || runs[0].NoticeText != "notice" {
		t.Fatalf("%+v %v", runs, err)
	}
}
