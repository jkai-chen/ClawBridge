package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestL2CLIMalformedIntentAndHelpPrecedeConfigOrState(t *testing.T) {
	p := newProcessFixture(t)
	for _, tc := range []struct {
		name string
		args []string
		help bool
	}{
		{"help_create", []string{"linear", "create", "--help"}, true},
		{"help_update", []string{"linear", "update", "--help"}, true},
		{"help_labels", []string{"linear", "labels", "--help"}, true},
		{"help_members", []string{"linear", "members", "--help"}, true},
		{"help_relation", []string{"linear", "relation", "--help"}, true},
		{"parent_set_clear", []string{"linear", "update", l1IssueID, "--profile", l1ProfileID, "--operation-id", "bad", "--parent", l1StateID, "--clear-parent"}, false},
		{"assignee_set_clear", []string{"linear", "update", l1IssueID, "--profile", l1ProfileID, "--operation-id", "bad", "--assignee", l2LabelA, "--clear-assignee"}, false},
		{"project_set_clear", []string{"linear", "update", l1IssueID, "--profile", l1ProfileID, "--operation-id", "bad", "--linear-project", l1Project, "--clear-project"}, false},
		{"labels_set_clear", []string{"linear", "update", l1IssueID, "--profile", l1ProfileID, "--operation-id", "bad", "--label", l2LabelA, "--clear-labels"}, false},
		{"labels_duplicate", []string{"linear", "create", "--profile", l1ProfileID, "--operation-id", "bad", "--title", "synthetic", "--label", l2LabelA, "--label", l2LabelA}, false},
		{"update_team_move", []string{"linear", "update", l1IssueID, "--profile", l1ProfileID, "--operation-id", "bad", "--title", "synthetic", "--team", l1TeamB}, false},
		{"relation_self", []string{"linear", "relation", "add", l1IssueID, l1IssueID, "--profile", l1ProfileID, "--operation-id", "bad", "--type", "blocks"}, false},
		{"relation_unsupported", []string{"linear", "relation", "remove", l1IssueID, l1StateID, "--profile", l1ProfileID, "--operation-id", "bad", "--type", "duplicate"}, false},
		{"teamboard_and_project", []string{"linear", "link", "--profile", l1ProfileID, "--team", l1TeamA, "--default-team", l1TeamA, "--expected-revision", "0", "--team-board", "--linear-project", l1Project}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, "never-state")
			config := filepath.Join(home, "poison.toml")
			sentinel := []byte("NOT_VALID_TOML [ SYNTHETIC_CONFIG_SENTINEL_DO_NOT_ECHO")
			if err := os.WriteFile(config, sentinel, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, p.binary, tc.args...)
			cmd.Dir = home
			cmd.Env = []string{"HOME=" + home, "PATH=" + home, "CLAWBRIDGE_HOME=" + state, "CLAWBRIDGE_CONFIG=" + config, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home, "XDG_CACHE_HOME=" + home}
			output, err := cmd.CombinedOutput()
			if tc.help && err != nil {
				t.Fatalf("help depended on config/credentials: %v %s", err, output)
			}
			if !tc.help && err == nil {
				t.Fatal("malformed intent accepted")
			}
			if ctx.Err() != nil {
				t.Fatal("parse/help did not terminate promptly")
			}
			if strings.Contains(string(output), "SYNTHETIC_CONFIG_SENTINEL") || strings.Contains(string(output), "poison.toml") || strings.Contains(string(output), "parse config") {
				t.Fatal("command parsed/reflected config before handling arguments")
			}
			if tc.name == "help_create" || tc.name == "help_update" {
				if !strings.Contains(strings.ToLower(string(output)), "replacement") {
					t.Fatal("CLI label help omitted replacement semantics")
				}
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("help/invalid intent initialized app state", err)
			}
			if got, err := os.ReadFile(config); err != nil || string(got) != string(sentinel) {
				t.Fatal("config sentinel changed")
			}
			files, err := os.ReadDir(home)
			if err != nil || len(files) != 1 || files[0].Name() != "poison.toml" {
				t.Fatal("parse/help created profiles/db/cache/config artifacts")
			}
		})
	}
}
