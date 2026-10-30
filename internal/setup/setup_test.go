package setup

import (
	"context"
	"testing"

	"clawbridge/internal/faketool"
)

func TestGCloudRunnerConsumesFullCommandExactlyOnce(t *testing.T) {
	fake := faketool.Install(t, t.TempDir(), "fake-gcloud", "args")
	runner := GCloudRunner{Path: fake}
	command := []string{"gcloud", "projects", "describe", "review-project", "--quiet"}
	output, err := runner.Run(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "projects\ndescribe\nreview-project\n--quiet\n" {
		t.Fatal("full command arguments were not passed exactly once")
	}
	if command[0] != "gcloud" || len(command) != 5 {
		t.Fatal("reviewed command was mutated")
	}
	for _, command := range [][]string{nil, {"gcloud"}, {"other", "projects"}, {"/tmp/gcloud", "projects"}} {
		if _, err := runner.Run(context.Background(), command); err == nil {
			t.Fatal("invalid executable token accepted")
		}
	}
}
