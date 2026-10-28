// Package playbook finds a project's coordinator playbook, the rules the
// project keeps for its coordinator session in its own repository. The
// coordinator follows it only as merged on the default branch, so this reads
// the default branch, and the working tree only to say an edit isn't merged.
package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Path is the playbook's place in its repository.
const Path = "docs/COORDINATOR.md"

// State is what the checkout here says about the playbook.
type State struct {
	// Root is the checkout's top directory; empty outside a git checkout.
	Root string
	// Ref is the default branch, as origin/HEAD names it (origin/main).
	Ref string
	// Merged: the default branch has the file.
	Merged bool
	// Local: the working tree has the file.
	Local bool
	// Err says why the default branch couldn't be read.
	Err error
}

// gitTimeout bounds each git call: a playbook check never holds up whoami.
const gitTimeout = 2 * time.Second

// git runs git in dir; tests replace it.
var git = func(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// Find reads the playbook's state in the checkout that holds dir.
func Find(ctx context.Context, dir string) State {
	root, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return State{}
	}
	s := State{Root: root}
	if info, e := os.Stat(filepath.Join(root, filepath.FromSlash(Path))); e == nil && info.Mode().IsRegular() {
		s.Local = true
	}
	ref, err := git(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil || ref == "" {
		s.Err = errors.New("the checkout has no origin/HEAD, so its default branch is unknown (git remote set-head origin --auto)")
		return s
	}
	s.Ref = ref
	_, err = git(ctx, root, "cat-file", "-e", ref+":"+Path)
	s.Merged = err == nil
	return s
}

// plainRef is a ref name safe to put in a command line as it is. The
// remote chooses origin/HEAD's target, and a ref name may hold shell
// characters ($, (, ;, a backtick), so any other name gets no command; nor
// does one starting with -, which git would read as an option.
var plainRef = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/-]*$`)

// View is the playbook's entry in whoami: where to read it, and whether
// it is merged.
func (s State) View() map[string]any {
	v := map[string]any{"path": Path, "merged": s.Merged, "local": s.Local}
	var notes []string
	if s.Err != nil {
		notes = append(notes, s.Err.Error())
	}
	if s.Ref != "" {
		v["ref"] = s.Ref
		if plainRef.MatchString(s.Ref) {
			v["read"] = "git show " + s.Ref + ":" + Path
		} else {
			notes = append(notes, "the default branch's name has characters a shell would run, so no command is given: read "+Path+" from that branch by hand")
		}
		notes = append(notes, "this reads your copy of "+s.Ref+": git fetch first to see what is merged now")
	}
	if len(notes) > 0 {
		v["note"] = strings.Join(notes, "; ")
	}
	return v
}

// Coordinator reports whether a whoami answer is the designated
// coordinator's: its project's coordinator_session is its session's ID.
func Coordinator(whoami any) bool {
	var w struct {
		Project *struct {
			Coordinator string `json:"coordinator_session"`
		} `json:"project"`
		Session *struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	b, err := json.Marshal(whoami)
	if err != nil || json.Unmarshal(b, &w) != nil || w.Project == nil || w.Session == nil {
		return false
	}
	return w.Project.Coordinator != "" && w.Project.Coordinator == w.Session.ID
}

// Annotate adds coordinator_playbook to a coordinator's whoami answer, read
// from the checkout that holds dir. Any other answer is returned unchanged.
func Annotate(ctx context.Context, whoami any, dir string) any {
	data, ok := whoami.(map[string]any)
	if !ok || !Coordinator(whoami) {
		return whoami
	}
	s := Find(ctx, dir)
	if s.Root == "" {
		data["coordinator_playbook"] = map[string]any{"path": Path, "note": "not in a git checkout: run from the project's repository to find its playbook"}
		return data
	}
	data["coordinator_playbook"] = s.View()
	return data
}
