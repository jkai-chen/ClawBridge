package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"clawbridge/internal/bus"
)

// A3 (live/STATUS-FLOW-DESIGN.md §10): who closes each bound Linear
// project's issues, shown at link time and set one project at a time.

// Closes values as the CLI and the bus spell them.
const (
	ClosesAgent = "agent"
	ClosesHuman = "human"
)

// MinimalPolicyConsequences is what creating the minimal status policy
// changes (§10.2), stated before the owner says yes.
const MinimalPolicyConsequences = "No status policy exists, so who closes is not enforced. Letting agents close creates the minimal policy {\"enabled\": true, \"closes\": {PROJECT: \"agent\"}} with no team targets: markers still move no issues; close protection turns on for every bound project, so completing an issue needs agent there and the hop rule applies; and an agent's write to a state of unknown type is refused."

// minimalPolicy is what the minimal status policy is, and what creating
// it changes: with project flow proven on the server it also lets project
// status follow the work (A4 §11.4), the same answer's other half.
func minimalPolicy(proven bool) string {
	if !proven {
		return MinimalPolicyConsequences + " Project status: not moved, since project flow is not yet proven on this server."
	}
	return strings.Replace(MinimalPolicyConsequences, `"closes": {PROJECT: "agent"}}`, `"closes": {PROJECT: "agent"}, "project_flow": true}`, 1) + " Project status: follows the work. Each bound project moves to started when its work starts. One whose issues agents close also moves to completed when they are all closed; for the others, the coordinator is told to move it."
}

// ClosesLine is one bound project's close rule.
type ClosesLine struct {
	LinearProject string `json:"linear_project"`
	Name          string `json:"name,omitempty"`
	// Closes is people (default), agents, or not enforced (no status policy).
	Closes string `json:"closes"`
	Bound  bool   `json:"bound"`
	// Set is the value this link sets, when it changes one.
	Set string `json:"set,omitempty"`
}

// ClosesView is closes show: the policy's revision and every project the
// binding or the policy names.
type ClosesView struct {
	Revision int64        `json:"revision"`
	Enforced bool         `json:"enforced"`
	Projects []ClosesLine `json:"projects"`
}

type closesPolicy struct {
	Revision int64             `json:"revision"`
	Enabled  bool              `json:"enabled"`
	Closes   map[string]string `json:"closes"`
	// FlowProven is false from a server older than project flow.
	FlowProven bool `json:"project_flow_proven"`
}

func (s *Service) closesPolicy(ctx context.Context, scope bus.Request) (closesPolicy, error) {
	data, e := s.StatusPolicyV2(ctx, scope)
	if e != nil {
		return closesPolicy{}, e
	}
	raw, _ := json.Marshal(data)
	var p closesPolicy
	if json.Unmarshal(raw, &p) != nil {
		return closesPolicy{}, errors.New("the status policy could not be read")
	}
	return p, nil
}

func closesWords(p closesPolicy, project string) string {
	switch {
	case p.Revision == 0 || !p.Enabled:
		return "not enforced (no status policy)"
	case p.Closes[project] == ClosesAgent:
		return "agents"
	}
	return "people (default)"
}

// ClosesV2 shows who closes each bound project, and the keys left for
// projects no longer bound, marked so.
func (s *Service) ClosesV2(ctx context.Context, scope bus.Request) (ClosesView, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return ClosesView{}, e
	}
	p, e := s.closesPolicy(ctx, scope)
	if e != nil {
		return ClosesView{}, e
	}
	out := ClosesView{Revision: p.Revision, Enforced: p.Revision > 0 && p.Enabled, Projects: []ClosesLine{}}
	seen := map[string]bool{}
	if b.Configured && b.Mode == "project_set" {
		for _, id := range SortedTeams(b.LinearProjectIDs) {
			seen[id] = true
			out.Projects = append(out.Projects, ClosesLine{LinearProject: id, Closes: closesWords(p, id), Bound: true})
		}
	}
	var stale []string
	for id := range p.Closes {
		if !seen[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		out.Projects = append(out.Projects, ClosesLine{LinearProject: id, Closes: closesWords(p, id) + " (not bound)"})
	}
	return out, nil
}

// SetClosesV2 sets who closes one bound project at the policy's revision.
// via is how the person confirmed it: cli, or cli-tty or tui for agent.
func (s *Service) SetClosesV2(ctx context.Context, scope bus.Request, project, closes string, expected int64, via string) (any, error) {
	scope.Operation = "status_closes_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["linear_project"] = strings.ToLower(project)
	scope.Args["closes"] = closes
	scope.Args["expected_revision"] = expected
	scope.Args["via"] = via
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil && strings.Contains(e.Error(), `unsupported operation "status_closes_set"`) {
		// §10.4: an older server; never write the whole policy for a lead.
		return nil, fmt.Errorf("this server predates closes set; the project owner sets who closes with clawbridge linear status-policy set, naming %s in the file's \"closes\"", project)
	}
	return r.Data, e
}

// ProjectNameV2 is a bound project's name, read from Linear, for a person
// to type before agents may close its issues.
func (s *Service) ProjectNameV2(ctx context.Context, scope bus.Request, project string) (string, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return "", e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return "", e
	}
	defer done()
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return "", e
	}
	for _, pr := range v.Projects {
		if strings.EqualFold(pr.ID, project) {
			return pr.Name, nil
		}
	}
	return "", fmt.Errorf("Linear project %s is not in this project's board", project)
}

// validateLinkCloses checks a link's --closes against its projects.
func validateLinkCloses(in LinkInputV2) error {
	if len(in.Closes) == 0 {
		return nil
	}
	if in.TeamBoard {
		return errors.New("a team board has no per-project close rule; --closes needs --linear-project")
	}
	for project, who := range in.Closes {
		if !contains(in.ProjectIDs, project) {
			return fmt.Errorf("--closes names Linear project %s, which is not one of this link's --linear-project; nothing was sent", project)
		}
		if who != ClosesAgent && who != ClosesHuman {
			return errors.New("--closes is UUID=agent or UUID=human")
		}
	}
	return nil
}

// closesChanges are the projects whose rule this link changes: agent where
// it isn't, human where it is agent. A human answer with no policy changes
// nothing.
func closesChanges(p closesPolicy, want map[string]string) []string {
	var out []string
	for project, who := range want {
		if (who == ClosesAgent) != (p.Closes[project] == ClosesAgent) {
			out = append(out, project)
		}
	}
	sort.Strings(out)
	return out
}

// linkClosesPreview is the per-project line of a link preview.
func linkClosesPreview(p closesPolicy, in LinkInputV2, preview *LinkPreviewV2) {
	if in.TeamBoard {
		return
	}
	names := map[string]string{}
	for _, pr := range preview.Projects {
		names[strings.ToLower(pr.ID)] = pr.Name
	}
	changes := closesChanges(p, in.Closes)
	for _, id := range SortedTeams(in.ProjectIDs) {
		line := ClosesLine{LinearProject: id, Name: names[id], Closes: closesWords(p, id), Bound: true}
		if contains(changes, id) {
			line.Set = in.Closes[id]
		}
		preview.Closes = append(preview.Closes, line)
		if line.Set == ClosesAgent && p.Revision == 0 {
			preview.MinimalPolicy = minimalPolicy(p.FlowProven) + " Confirm with --minimal-status-policy; only the project owner can create it."
		}
	}
}

// minimalPolicyTyped refuses the minimal policy where agents could no
// longer set states at all: a bound team whose status list has no types,
// read neither from the connection nor from a hosted server's Linear app.
func (s *Service) minimalPolicyTyped(ctx context.Context, scope bus.Request, teams []string) error {
	for _, team := range teams {
		states, e := s.StatesV2(ctx, scope, team)
		if e == nil && !typed(states) {
			states, e = s.appStates(ctx, scope, team)
		}
		if e != nil || !typed(states) {
			return fmt.Errorf("Linear did not report workflow state types for team %s, so the minimal status policy would stop agents setting states there; letting agents close needs a hosted server or a full status policy (clawbridge linear status-policy set)", team)
		}
	}
	return nil
}

// ClosesOutcome is what a confirmed link did about who closes.
type ClosesOutcome struct {
	LinearProject string `json:"linear_project"`
	Closes        string `json:"closes"`
	Outcome       string `json:"outcome"` // set, or failed
	Error         string `json:"error,omitempty"`
	Rerun         string `json:"rerun,omitempty"`
}

// applyLinkCloses sets who closes, after the binding, at the revision it
// just read. A failure leaves the project closed by people, the safe
// default, and says what to re-run.
func (s *Service) applyLinkCloses(ctx context.Context, scope bus.Request, in LinkInputV2, changes []string) []ClosesOutcome {
	var out []ClosesOutcome
	fail := func(project string, e error) {
		who := in.Closes[project]
		out = append(out, ClosesOutcome{LinearProject: project, Closes: who, Outcome: "failed", Error: e.Error(), Rerun: fmt.Sprintf("clawbridge linear closes set --linear-project %s --%s --expected-revision N (N from clawbridge linear closes show). Until then its issues are closed by people (the default).", project, who)})
	}
	var minimal []string
	proven := false
	for _, project := range changes {
		p, e := s.closesPolicy(ctx, scope)
		if e != nil {
			fail(project, e)
			continue
		}
		if p.Revision == 0 {
			proven = p.FlowProven
			if in.Closes[project] == ClosesAgent {
				minimal = append(minimal, project)
			}
			continue
		}
		via := ClosesViaCLI
		if in.Closes[project] == ClosesAgent {
			via = in.Via
		}
		if _, e = s.SetClosesV2(ctx, scope, project, in.Closes[project], p.Revision, via); e != nil {
			fail(project, e)
			continue
		}
		out = append(out, ClosesOutcome{LinearProject: project, Closes: in.Closes[project], Outcome: "set"})
	}
	if len(minimal) == 0 {
		return out
	}
	closes := map[string]any{}
	for _, project := range minimal {
		closes[project] = ClosesAgent
	}
	scope.Operation = "status_policy_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = int64(0)
	policy := map[string]any{"enabled": true, "teams": map[string]any{}, "closes": closes}
	if proven {
		policy["project_flow"] = true
	}
	scope.Args["status_policy"] = policy
	scope.Args["via"] = in.Via
	_, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil && strings.Contains(e.Error(), `unsupported argument "via"`) {
		delete(scope.Args, "via")
		_, e = s.cfg.Executor.Execute(ctx, scope)
	}
	if e != nil && strings.Contains(e.Error(), "owner permission required") {
		// §10.8 L1: the minimal policy is the owner's to create.
		e = errors.New(minimalOwnerOnly)
	}
	for _, project := range minimal {
		if e != nil {
			fail(project, e)
		} else {
			out = append(out, ClosesOutcome{LinearProject: project, Closes: ClosesAgent, Outcome: "set"})
		}
	}
	return out
}

// minimalOwnerOnly answers a lead who asks for the minimal policy.
const minimalOwnerOnly = "only the project owner can create the status policy who closes needs; ask them to run this link again with --minimal-status-policy"

// ConfirmAgentCloses has a person confirm, at a terminal, that agents
// will close the issues of each project, named by Linear ID → name.
type ConfirmAgentCloses func(names map[string]string) error

// confirmLoosened is the via a status policy write carries: cli-tty when
// closes lets agents close a project the stored policy doesn't and confirm
// accepted it, cli when nothing is loosened.
func (s *Service) confirmLoosened(ctx context.Context, scope bus.Request, closes map[string]any, confirm ConfirmAgentCloses) (string, error) {
	current, e := s.closesPolicy(ctx, scope)
	if e != nil {
		return "", e
	}
	names := map[string]string{}
	for project, who := range closes {
		if who != ClosesAgent || current.Closes[project] == ClosesAgent {
			continue
		}
		if confirm == nil {
			return "", errors.New("letting agents close issues is confirmed at a terminal, by typing each project's name; nothing was set")
		}
		if names[project], e = s.ProjectNameV2(ctx, scope, project); e != nil {
			return "", e
		}
	}
	if len(names) == 0 {
		return ClosesViaCLI, nil
	}
	if e = confirm(names); e != nil {
		return "", e
	}
	return ClosesViaTTY, nil
}

// Ways a person confirmed a closes change (the audit's via). It is the
// client's claim: the server records it and can't check it. cli-tty is a
// name typed at a terminal, tui one typed in the TUI's link review.
const (
	ClosesViaCLI = "cli"
	ClosesViaTTY = "cli-tty"
	ClosesViaTUI = "tui"
)
