package runner

import (
	"os"
	"path/filepath"
	"strings"
)

// Collected is one recorded launch and what gc does with it.
type Collected struct {
	Session  string `json:"session"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	// Action is remove or keep; Reason says why.
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Removed bool   `json:"removed,omitempty"`
	Error   string `json:"error,omitempty"`
}

// GC reviews the runner's own recorded worktrees (RN-M4) and, with
// confirm, removes those that are finished: the branch is merged into its
// base, or it is a week old with no commits. It touches only worktrees the
// runner recorded, never forces a removal, and keeps anything with
// uncommitted changes.
func (r *Runner) GC(confirm bool) ([]Collected, error) {
	all, err := r.Launches()
	if err != nil {
		return nil, err
	}
	var out []Collected
	var keep []Launch
	for _, l := range all {
		c := Collected{Session: l.Session, Worktree: l.Worktree, Branch: l.Branch, Action: "keep"}
		_, missing := os.Stat(l.Worktree)
		switch {
		case missing != nil:
			c.Action, c.Reason = "remove", "the worktree is already gone"
		default:
			status, e := r.Git(l.Worktree, "status", "--porcelain")
			ahead, e2 := r.Git(l.Repo, "rev-list", "--count", l.Base+".."+l.Branch)
			// Merges here rebase, so a merged commit has a new ID: git
			// cherry matches it by its patch instead.
			cherry, e3 := r.Git(l.Repo, "cherry", l.Base, l.Branch)
			tip, e4 := r.Git(l.Repo, "rev-parse", "--verify", l.Branch+"^{commit}")
			switch {
			case e != nil || e2 != nil || e3 != nil || e4 != nil:
				c.Reason = "could not be read; left as is"
			case status != "":
				c.Reason = "has uncommitted changes"
			case ahead != "0" && allUpstream(cherry):
				c.Action, c.Reason = "remove", "merged into "+l.Base
			case ahead == "0" && tip != l.BaseCommit:
				// Its commits are in the base already (a fast-forward).
				c.Action, c.Reason = "remove", "merged into "+l.Base
			case ahead == "0" && r.now().Sub(l.CreatedAt) >= retainNoCommits:
				c.Action, c.Reason = "remove", "no commits in a week"
			case ahead == "0":
				c.Reason = "no commits yet"
			default:
				c.Reason = "has commits not in " + l.Base + "; waiting for review"
			}
		}
		if confirm && c.Action == "remove" {
			// A branch whose every commit is upstream, or that has none,
			// loses nothing when deleted; any other goes only if git agrees.
			c.Removed, c.Error = r.remove(l, missing != nil, missing == nil)
		}
		if !c.Removed || c.Error != "" {
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

// remove takes a finished launch away: its worktree (never forced), its
// branch, and its session files. proven says the branch holds nothing
// that is not upstream.
func (r *Runner) remove(l Launch, gone, proven bool) (bool, string) {
	if !gone {
		if _, err := r.Git(l.Repo, "worktree", "remove", l.Worktree); err != nil {
			return false, "git worktree remove: " + err.Error()
		}
	}
	_, _ = r.Git(l.Repo, "worktree", "prune")
	if proven {
		_, _ = r.Git(l.Repo, "branch", "-D", l.Branch)
	} else {
		_, _ = r.Git(l.Repo, "branch", "-d", l.Branch)
	}
	_ = os.RemoveAll(filepath.Join(r.StateDir, "sessions", l.Session))
	return true, ""
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
