package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"clawbridge/internal/notify"
)

func TestNotificationCLIInvalidFlagsAndHelpHaveNoEffects(t *testing.T) {
	d := isolated(t)
	t.Setenv("CLAWBRIDGE_CONFIG", filepath.Join(d, "inaccessible"))
	old := newNotifyExecutor
	defer func() { newNotifyExecutor = old }()
	newNotifyExecutor = func(Config) (notify.Executor, func(), error) {
		t.Fatal("invalid options opened authority")
		return nil, nil, nil
	}
	for _, args := range [][]string{{"notify", "--help"}, {"notify", "--adapter", "json", "--state", "relative"}, {"notify", "--adapter", "codex", "--thread", "alias", "--state", filepath.Join(d, "state.json")}, {"notify", "--adapter", "json", "--thread=", "--state", filepath.Join(d, "state.json")}, {"notify", "--adapter", "json", "--interval", "0s", "--state", filepath.Join(d, "state.json")},
		// The command adapter takes its argv after --, and only it does.
		{"notify", "--adapter", "command", "--state", filepath.Join(d, "state.json")},
		{"notify", "--adapter", "command", "--state", filepath.Join(d, "state.json"), "--"},
		{"notify", "--adapter", "json", "--state", filepath.Join(d, "state.json"), "--", "tmux"},
		{"notify", "--adapter", "command", "--thread=x", "--state", filepath.Join(d, "state.json"), "--", "tmux"},
		{"notify", "--adapter", "command", "--state", "relative", "--", "tmux"}} {
		var out, diag bytes.Buffer
		err := Run(context.Background(), args, &out, &diag)
		if len(args) == 2 {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("invalid options accepted", args)
		}
	}
	if entries, err := os.ReadDir(d); err != nil || len(entries) != 0 {
		t.Fatal("invalid/help mutated state", entries, err)
	}
}
