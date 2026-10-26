package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Local: the workspace and first project are made by the same commands as
// setup --local and project create, and a new terminal finds them.
func TestStartLocal(t *testing.T) {
	isolated(t)
	clearRouteEnv(t)
	wizardHooks(t, "1\ndemo\nfirst\n")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	for _, want := range []string{startText, "Workspace demo and project first are ready.", "Optional setup", "clawbridge register me"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	invoke(t, "register", "eng", "--instance", "eng-i")
	if got := invoke(t, "sessions"); len(got["sessions"].([]any)) != 1 {
		t.Fatal(got)
	}
}

// Local with the defaults: my-team, and the directory's name as the project.
func TestStartLocalDefaults(t *testing.T) {
	dir := isolated(t)
	clearRouteEnv(t)
	project := filepath.Join(dir, "my-repo")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	wizardHooks(t, "1\n\n\n")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(out.String(), "Workspace my-team and project my-repo are ready.") {
		t.Fatal(out.String())
	}
}

func TestStartLocalRefusesABadProjectName(t *testing.T) {
	home := isolated(t)
	clearRouteEnv(t)
	wizardHooks(t, "1\ndemo\nMy Project\n")
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused name saved a profile")
	}
}

// Join hands over to the existing guided join, which reads its own answers.
func TestStartJoin(t *testing.T) {
	isolated(t)
	wizardHooks(t, "2\nhttps://service.test\nws\nprj\ndevice\nn\n")
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a project is one codebase", "Join an existing ClawBridge service", "Canceled; no credentials or configuration changed."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
}

// Own server shows the steps and their checks, and changes nothing.
func TestStartOwnServer(t *testing.T) {
	home := isolated(t)
	wizardHooks(t, "3\n")
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out, io.Discard); err != nil || !strings.HasSuffix(out.String(), ownServerText) {
		t.Fatal(err, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own server wrote state")
	}
}

func TestStartRefusesOtherAnswersAndFlags(t *testing.T) {
	for _, input := range []string{"4\n", "local\n", ""} {
		isolated(t)
		wizardHooks(t, input)
		if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil {
			t.Fatalf("%q accepted", input)
		}
	}
	// With complete answers, so only the flag check can refuse.
	for refusal, args := range map[string][]string{"accepts flags only": {"onboard", "extra"}, "accepts only --client-config": {"--workspace", "w", "onboard"}} {
		isolated(t)
		clearRouteEnv(t)
		wizardHooks(t, "1\ndemo\nfirst\n")
		if err := Run(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// Without a terminal onboard prints Start, as bare clawbridge does.
	wizardTerminal = func(io.Writer) error { return errors.New("not terminal") }
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil || out.String() != startText+startCommands {
		t.Fatal(err, out.String())
	}
}
