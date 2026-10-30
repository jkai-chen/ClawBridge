package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// scopeFixture is a local checkout with workspace w and project p, both
// saved in this machine's config file, as setup and project create leave
// them. It returns the config file's and the checkout file's paths.
func scopeFixture(t *testing.T) (string, string) {
	t.Helper()
	isolated(t)
	git(t, "init", "-q")
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	home, err := filepath.Abs(filepath.Join("state", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return home, filepath.Join(checkoutRoot(context.Background()), ".clawbridge.toml")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func wantSources(t *testing.T, ctx context.Context, flags Config, want map[string]string) Config {
	t.Helper()
	c, _, err := resolveConfig(ctx, flags)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sources) != len(want) {
		t.Fatalf("sources %v, want %v", c.Sources, want)
	}
	for k, v := range want {
		if c.Sources[k] != v {
			t.Fatalf("source of %s = %q, want %q (all: %v)", k, c.Sources[k], v, c.Sources)
		}
	}
	return c
}

func TestConfigSourcesNameTheLayerThatSetEachScopeField(t *testing.T) {
	home, repo := scopeFixture(t)
	ctx := context.Background()
	fromHome, fromRepo := "config file "+home, ".clawbridge.toml at "+repo
	wantSources(t, ctx, Config{}, map[string]string{"workspace": fromHome, "project": fromHome})

	// The checkout's file beats the config file.
	writeFile(t, repo, "project = 'p'\nsession = 's1'\n")
	wantSources(t, ctx, Config{}, map[string]string{"workspace": fromHome, "project": fromRepo, "session": fromRepo})

	// The environment beats the checkout, each variable by its own name.
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	t.Setenv("CLAWBRIDGE_INSTANCE", "i-one")
	wantSources(t, ctx, Config{}, map[string]string{"workspace": fromHome, "project": "env CLAWBRIDGE_PROJECT", "session": fromRepo, "instance": "env CLAWBRIDGE_INSTANCE"})

	// A flag beats the environment.
	c := wantSources(t, ctx, Config{Project: "flagged", Session: "s2"}, map[string]string{"workspace": fromHome, "project": "flag", "session": "flag", "instance": "env CLAWBRIDGE_INSTANCE"})
	if c.Project != "flagged" || c.Session != "s2" {
		t.Fatalf("flags lost: %+v", c)
	}

	// The guided route clears the environment, so never names it.
	guided := context.WithValue(ctx, guidedRouteKey{}, true)
	wantSources(t, guided, Config{}, map[string]string{"workspace": fromHome, "project": fromRepo, "session": fromRepo})

	// An explicit config file replaces the home file and the checkout's.
	t.Setenv("CLAWBRIDGE_PROJECT", "")
	t.Setenv("CLAWBRIDGE_INSTANCE", "")
	writeFile(t, "other.toml", "workspace = 'w'\nproject = 'p'\n")
	other, _ := filepath.Abs("other.toml")
	wantSources(t, ctx, Config{ClientConfig: "other.toml"}, map[string]string{"workspace": "config file " + other, "project": "config file " + other})
	t.Setenv("CLAWBRIDGE_CONFIG", "other.toml")
	wantSources(t, ctx, Config{}, map[string]string{"workspace": "config file " + other, "project": "config file " + other})

	// overlay never copies the sources.
	if got := overlay(Config{}, Config{Project: "p", Sources: map[string]string{"project": "flag"}}); got.Sources != nil {
		t.Fatalf("overlay copied sources: %v", got.Sources)
	}
}

func TestOutOfScopeRefusalNamesTheScopeAndItsSource(t *testing.T) {
	home, repo := scopeFixture(t)
	// An unbound project binds no team, so a view list for any team is
	// refused out of scope before any Linear call.
	args := []string{"view", "list", "--team", "11111111-2222-4333-8444-555555555555", "--profile", "prof-1"}
	const refusal = "view team is outside the selected task-board scope"
	const another = "; pass --workspace and --project, or set CLAWBRIDGE_WORKSPACE and CLAWBRIDGE_PROJECT, to use another"
	cases := []struct {
		name  string
		setup func()
		flags Config
		want  string
	}{
		{"config file", func() {}, Config{}, refusal + "; this used workspace w and project p (project from config file " + home + ")" + another},
		{"checkout file", func() { writeFile(t, repo, "project = 'p'\n") }, Config{}, refusal + "; this used workspace w and project p (project from .clawbridge.toml at " + repo + ")" + another},
		{"environment", func() { t.Setenv("CLAWBRIDGE_PROJECT", "p") }, Config{}, refusal + "; this used workspace w and project p (project from env CLAWBRIDGE_PROJECT)" + another},
		// The person chose the project: no advice.
		{"flag", func() {}, Config{Project: "p"}, refusal + "; this used workspace w and project p"},
	}
	for _, tc := range cases {
		tc.setup()
		err := runLinear(context.Background(), tc.flags, args, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: got %v\nwant %s", tc.name, err, tc.want)
		}
		if !linearboard.IsOutOfScope(err) {
			t.Fatalf("%s: the wrapped refusal is no longer out of scope", tc.name)
		}
	}
	// A missing project fails before the wrap; the unit test below checks
	// that explainOutOfScope leaves other errors as they were.
	if err := runLinear(context.Background(), Config{Project: "missing"}, args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "this used") {
		t.Fatalf("a failure that isn't out of scope was explained: %v", err)
	}
}

func TestExplainOutOfScopeLeavesOtherErrorsAndAnUnknownSource(t *testing.T) {
	other := errors.New("x")
	if got := explainOutOfScope(other, Config{Sources: map[string]string{"project": "env CLAWBRIDGE_PROJECT"}}, bus.Request{}); got != other {
		t.Fatalf("an error that isn't out of scope was changed: %v", got)
	}
	// A project with no recorded source keeps the advice, without an
	// empty "(project from )".
	scopeFixture(t)
	args := []string{"view", "list", "--team", "11111111-2222-4333-8444-555555555555", "--profile", "prof-1"}
	outside := errors.Unwrap(runLinear(context.Background(), Config{Project: "p"}, args, &bytes.Buffer{}, &bytes.Buffer{}))
	if !linearboard.IsOutOfScope(outside) || outside.Error() != "view team is outside the selected task-board scope" {
		t.Fatalf("no bare out-of-scope refusal: %v", outside)
	}
	got := explainOutOfScope(outside, Config{Workspace: "w", Project: "p"}, bus.Request{})
	want := outside.Error() + "; this used workspace w and project p; pass --workspace and --project, or set CLAWBRIDGE_WORKSPACE and CLAWBRIDGE_PROJECT, to use another"
	if got.Error() != want || !linearboard.IsOutOfScope(got) {
		t.Fatalf("got %v\nwant %s", got, want)
	}
}

func TestLinearContextCarriesTheScopeWithoutTheInstance(t *testing.T) {
	c := Config{Workspace: "w", Project: "p", Session: "s1", Instance: "secret-instance",
		Sources: map[string]string{"workspace": "config file /home/x/config.toml", "project": "env CLAWBRIDGE_PROJECT", "session": "flag", "instance": "flag"}}
	raw, err := json.Marshal(linearboard.ContextV2{Scope: linearScopeView(c, bus.Request{Workspace: "ws_id", Project: "prj_example"})})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Scope map[string]map[string]string `json:"scope"`
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"workspace": {"value": "w", "from": "config file /home/x/config.toml"},
		"project":   {"value": "p", "from": "env CLAWBRIDGE_PROJECT"},
		"session":   {"value": "s1", "from": "flag"},
		"instance":  {"from": "flag"},
	}
	if len(got.Scope) != len(want) {
		t.Fatalf("scope %v", got.Scope)
	}
	for k, v := range want {
		if len(got.Scope[k]) != len(v) || got.Scope[k]["value"] != v["value"] || got.Scope[k]["from"] != v["from"] {
			t.Fatalf("scope %s = %v, want %v", k, got.Scope[k], v)
		}
	}
	if strings.Contains(string(raw), "secret-instance") {
		t.Fatal("context printed the instance")
	}
	// Names left empty fall back to the resolved IDs; an unset field is
	// left out.
	view := linearScopeView(Config{Sources: map[string]string{"workspace": "flag", "project": "flag"}}, bus.Request{Workspace: "ws_id", Project: "prj_example"})
	if len(view) != 2 || view["workspace"].Value != "ws_id" || view["project"].Value != "prj_example" {
		t.Fatalf("view %v", view)
	}
	// The MCP tool's context leaves the scope out.
	if raw, _ = json.Marshal(linearboard.ContextV2{}); strings.Contains(string(raw), "scope") {
		t.Fatalf("an empty scope was printed: %s", raw)
	}
}

func TestLinearBindingPrintsItsScopeToStderr(t *testing.T) {
	home, _ := scopeFixture(t)
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "s1", "--instance", "i-one")
	ctx := context.Background()
	flags := Config{Session: "s1", Instance: "i-one"}

	// The binding JSON as linear binding printed it before.
	executor, closeExecutor, scope, err := linearExecutor(ctx, flags)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := linearDirectory()
	binding, err := newLinearService(directory, "", newLinearManager(directory), executor, scope.Args["linear_authority"].(string)).BindingV2(ctx, scope)
	closeExecutor()
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err = emit(ctx, &want, "linear_binding", bus.Result{Source: "linear", Data: binding}); err != nil {
		t.Fatal(err)
	}

	wantErr := "scope: workspace w (from config file " + home + ")\n" +
		"scope: project p (from env CLAWBRIDGE_PROJECT)\n" +
		"scope: session s1 (from flag)\n" +
		"scope: instance set (from flag)\n"
	for _, args := range [][]string{{"binding"}, {"binding", "--profile", "prof-1"}} {
		var stdout, stderr bytes.Buffer
		if err = runLinear(ctx, flags, args, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if stdout.String() != want.String() {
			t.Fatalf("%v: stdout changed:\n%s\nwant:\n%s", args, stdout.String(), want.String())
		}
		if stderr.String() != wantErr {
			t.Fatalf("%v: stderr:\n%s\nwant:\n%s", args, stderr.String(), wantErr)
		}
		if strings.Contains(stdout.String()+stderr.String(), "i-one") {
			t.Fatalf("%v: printed the instance", args)
		}
	}
	err = runLinear(ctx, flags, []string{"binding", "--profile", "bad profile"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "invalid personal profile ID" {
		t.Fatalf("an invalid profile ID was accepted: %v", err)
	}
}
