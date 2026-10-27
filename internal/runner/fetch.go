package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// tokenPatterns are credential shapes an agent's branch must not carry:
// a token it could read and commit would leave with the next push.
var tokenPatterns = []struct {
	id string
	re *regexp.Regexp
}{
	{"github-token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})`)},
	{"aws-access-key", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"linear-api-key", regexp.MustCompile(`\blin_(api|oauth)_[A-Za-z0-9]{20,}`)},
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{"npmrc-auth-token", regexp.MustCompile(`_authToken\s*=\s*[^\s$]\S{8,}`)},
	{"google-refresh-token", regexp.MustCompile(`\b1//0[A-Za-z0-9_-]{30,}`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bauthorization:\s*bearer\s+[A-Za-z0-9._~+/=-]{20,}`)},
}

// Hit is one token-shaped match: the file and the pattern, never the text.
type Hit struct {
	File    string `json:"file"`
	Pattern string `json:"pattern"`
}

// ScanPatch finds token shapes in the lines a patch adds.
func ScanPatch(patch string) []Hit {
	var hits []Hit
	seen := map[Hit]bool{}
	file := ""
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			continue
		}
		if !strings.HasPrefix(line, "+") {
			continue
		}
		for _, p := range tokenPatterns {
			if h := (Hit{file, p.id}); p.re.MatchString(line) && !seen[h] {
				seen[h] = true
				hits = append(hits, h)
			}
		}
	}
	return hits
}

// Fetched is what runner fetch did.
type Fetched struct {
	Session string `json:"session"`
	Branch  string `json:"branch"`
	Commits int    `json:"commits"`
	Hits    []Hit  `json:"token_hits,omitempty"`
	Fetched bool   `json:"fetched"`
}

// checkClone refuses a clone the agent has made unsafe for the one git
// that runs in it from the host, the fetch's upload-pack (G9-7, G9-14).
// Its .git must be a plain directory, borrowing no other repository's
// objects (alternates, commondir), and its config, read by git's own
// parser without following includes and outside any repository, must set
// no hooks path, file-system monitor or include.
func (r *Runner) checkClone(clone string) error {
	gitDir := filepath.Join(clone, ".git")
	refuse := func(why string) error {
		return fmt.Errorf("the clone's .git %s, so nothing was fetched; inspect the clone's files (not with git), and remove the session", why)
	}
	if info, err := os.Lstat(gitDir); err != nil || !info.IsDir() {
		return refuse("is not a plain directory")
	}
	for _, f := range []string{"objects/info/alternates", "objects/info/http-alternates", "commondir"} {
		if _, err := os.Lstat(filepath.Join(gitDir, f)); err == nil {
			return refuse("borrows objects from elsewhere (" + f + ")")
		}
	}
	// Its objects and refs are its own directories, not links elsewhere
	// (R141-L3).
	for _, d := range []string{"objects", "refs"} {
		if info, err := os.Lstat(filepath.Join(gitDir, d)); err != nil || !info.IsDir() {
			return refuse("has an " + d + " entry that isn't a plain directory")
		}
	}
	config := filepath.Join(gitDir, "config")
	if info, err := os.Lstat(config); err != nil || !info.Mode().IsRegular() {
		return refuse("has no plain config file")
	}
	names, err := r.Git(r.StateDir, "config", "--file", config, "--no-includes", "--name-only", "--list")
	if err != nil {
		return refuse("has a config git can't read")
	}
	for _, name := range strings.Fields(names) {
		name = strings.ToLower(name)
		if name == "core.hookspath" || name == "core.fsmonitor" || strings.HasPrefix(name, "include.") || strings.HasPrefix(name, "includeif.") {
			return refuse("config sets " + name)
		}
	}
	return nil
}

// Fetch brings a launch's branch into the owner's repository. The one git
// that runs in the clone is the fetch's upload-pack, after checkClone, with
// every object checked. The branch first lands under a quarantine ref;
// everything it adds over its base is scanned for token shapes; only a
// clean branch becomes agent/<id>. A hit removes the quarantine ref and
// refuses, naming files and patterns only.
func (r *Runner) Fetch(session string) (Fetched, error) {
	all, err := r.Launches()
	if err != nil {
		return Fetched{}, err
	}
	var l *Launch
	for i := range all {
		if all[i].Session == session {
			l = &all[i]
		}
	}
	if l == nil {
		return Fetched{}, errors.New("no runner launch with that session")
	}
	out := Fetched{Session: session, Branch: l.Branch}
	if err = r.checkClone(l.Clone); err != nil {
		return out, err
	}
	quarantine := "refs/clawbridge-runner/" + session
	if _, err = r.Git(l.Repo, "-c", "transfer.fsckObjects=true", "fetch", "--quiet", "--no-tags", l.Clone, "+refs/heads/"+l.Branch+":"+quarantine); err != nil {
		return out, fmt.Errorf("git fetch from the clone: %w", err)
	}
	drop := func() { _, _ = r.Git(l.Repo, "update-ref", "-d", quarantine) }
	count, err := r.Git(l.Repo, "rev-list", "--count", l.BaseCommit+".."+quarantine)
	if err != nil {
		drop()
		return out, err
	}
	fmt.Sscan(count, &out.Commits)
	// Every commit's own additions, so a token added and later removed
	// is still caught: it would be in the pushed history. -m shows a
	// merge's own changes against each parent, so a token that only a
	// merge adds is caught too (G9-8).
	patch, err := r.Git(l.Repo, "log", "--no-ext-diff", "--no-textconv", "--text", "-p", "-m", "--format=", l.BaseCommit+".."+quarantine)
	if err != nil {
		drop()
		return out, err
	}
	if out.Hits = ScanPatch(patch); len(out.Hits) > 0 {
		drop()
		return out, errors.New("the agent's branch adds token-shaped text; nothing was fetched. Inspect the clone's files (not with git), and remove the session")
	}
	if _, err = r.Git(l.Repo, "branch", "-f", l.Branch, quarantine); err != nil {
		drop()
		return out, err
	}
	drop()
	out.Fetched = true
	return out, nil
}
