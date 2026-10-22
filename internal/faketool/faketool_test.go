package faketool

import (
	"bytes"
	"os/exec"
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
