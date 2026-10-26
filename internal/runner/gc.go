package runner

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Collected is one recorded launch and what gc does with it.
type Collected struct {
	Session string `json:"session"`
	Clone   string `json:"clone"`
	Branch  string `json:"branch"`
	// Action is remove or keep; Reason says why.
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Removed bool   `json:"removed,omitempty"`
	Error   string `json:"error,omitempty"`
}

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// GC reviews the runner's own recorded clones (RN-M4) and, with confirm,
// removes the finished ones. It never runs git inside a clone: the agent
// could have written its configuration and hooks, and they would run
// unsandboxed here. It reads the clone's branch from its files and judges
// the work in the owner's repository, after the owner fetched it:
//   - merged: the owner's copy of the branch is the clone's, and every
//     commit of it is in the base by its patch (rebase merges count);
//   - idle: the branch is still at the launch's base, a week on, and no
//     file in the clone changed in that week.
//
// Anything else is kept: commits not yet fetched or merged, recent
// changes, or a clone it cannot read.
func (r *Runner) GC(confirm bool) ([]Collected, error) {
	all, err := r.Launches()
	if err != nil {
		return nil, err
	}
	var out []Collected
	var keep []Launch
	for _, l := range all {
		c := Collected{Session: l.Session, Clone: l.Clone, Branch: l.Branch, Action: "keep"}
		proven := false
		if _, missing := os.Stat(l.Clone); missing != nil {
			c.Action, c.Reason = "remove", "the clone is already gone"
		} else {
			tip, readErr := cloneRef(l.Clone, l.Branch)
			fetched, fetchErr := r.Git(l.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+l.Branch)
			switch {
			case readErr != nil:
				c.Reason = "its branch could not be read; left as is"
			case tip == l.BaseCommit:
				changed, walkErr := changedSince(l.Clone, r.now().Add(-retainNoCommits))
				switch {
				case walkErr != nil:
					c.Reason = "its files could not be read; left as is"
				case r.now().Sub(l.CreatedAt) >= retainNoCommits && !changed:
					c.Action, c.Reason, proven = "remove", "no commits and no changes in a week", true
				default:
					c.Reason = "no commits yet"
				}
			case fetchErr != nil || fetched != tip:
				c.Reason = "has commits the owner has not fetched; fetch them with: clawbridge runner fetch " + l.Session
			default:
				cherry, e := r.Git(l.Repo, "cherry", l.Base, l.Branch)
				_, ancestor := r.Git(l.Repo, "merge-base", "--is-ancestor", l.Branch, l.Base)
				switch {
				case e != nil:
					c.Reason = "could not be compared with " + l.Base + "; left as is"
				case allUpstream(cherry) || ancestor == nil:
					c.Action, c.Reason, proven = "remove", "merged into "+l.Base, true
				default:
					c.Reason = "has commits not in " + l.Base + "; waiting for review"
				}
			}
		}
		if confirm && c.Action == "remove" {
			c.Removed, c.Error = r.remove(l, proven)
		}
		if !c.Removed {
			keep = append(keep, l)
		}
		out = append(out, c)
	}
	if confirm {
		if err = r.saveLaunches(keep); err != nil {
			return out, err
		}
	}
	return out, nil
}

// remove deletes a finished clone and its session files, and the owner's
// fetched copy of its branch when the work is proven merged or was never
// begun.
func (r *Runner) remove(l Launch, proven bool) (bool, string) {
	if err := os.RemoveAll(l.Clone); err != nil {
		return false, err.Error()
	}
	if l.Cache != "" {
		// The module cache is read-only on disk; make it removable.
		_ = filepath.WalkDir(l.Cache, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
		_ = os.RemoveAll(l.Cache)
	}
	_ = os.RemoveAll(filepath.Join(r.StateDir, "sessions", l.Session))
	// A self-test the owner ran by hand leaves its planted files behind.
	if home, err := r.home(); err == nil {
		dirs := []string{home}
		if gitDir, err := r.Git(l.Repo, "rev-parse", "--absolute-git-dir"); err == nil {
			dirs = append(dirs, gitDir)
		}
		for _, dir := range dirs {
			planted, _ := filepath.Glob(filepath.Join(dir, sentinelPrefix(l.Session)+"*"))
			removeAll(planted)
		}
	}
	if proven {
		if _, err := r.Git(l.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+l.Branch); err == nil {
			_, _ = r.Git(l.Repo, "branch", "-D", l.Branch)
		}
	}
	return true, ""
}

// cloneRef reads a branch's commit from the clone's files, without git.
func cloneRef(clone, branch string) (string, error) {
	gitDir := filepath.Join(clone, ".git")
	if raw, err := os.ReadFile(filepath.Join(gitDir, "refs", "heads", filepath.FromSlash(branch))); err == nil {
		id := strings.TrimSpace(string(raw))
		if objectID.MatchString(id) {
			return id, nil
		}
		return "", fs.ErrInvalid
	}
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch && objectID.MatchString(fields[0]) {
			return fields[0], nil
		}
	}
	return "", fs.ErrNotExist
}

// changedSince reports whether any file of the clone, outside its .git,
// was modified after since. Symbolic links are not followed.
func changedSince(clone string, since time.Time) (bool, error) {
	changed := false
	err := filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" && filepath.Dir(path) == clone {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(since) {
			changed = true
			return filepath.SkipAll
		}
		return nil
	})
	return changed, err
}

// allUpstream says every commit git cherry lists is already in the base.
func allUpstream(cherry string) bool {
	lines := strings.Split(strings.TrimSpace(cherry), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "- ") {
			return false
		}
	}
	return cherry != ""
}
