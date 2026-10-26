package runner

import (
	"errors"
	"fmt"
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

// Fetch brings a launch's branch into the owner's repository, never
// running git inside the clone. The branch first lands under a quarantine
// ref; everything it adds over its base is scanned for token shapes; only
// a clean branch becomes agent/<id>. A hit removes the quarantine ref and
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
	quarantine := "refs/clawbridge-runner/" + session
	if _, err = r.Git(l.Repo, "fetch", "--quiet", "--no-tags", l.Clone, "+refs/heads/"+l.Branch+":"+quarantine); err != nil {
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
	// is still caught: it would be in the pushed history.
	patch, err := r.Git(l.Repo, "log", "--no-ext-diff", "--no-textconv", "--text", "-p", "--format=", l.BaseCommit+".."+quarantine)
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
