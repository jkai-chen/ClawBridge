package bus

import "testing"

// A decide's trust reason is corrected from the case's own domain matches:
// more than one project match is ambiguous (as the rules say), and a label
// match never counts as a project match, whatever flag it carries. The
// watcher cannot make two project domains match one issue, so the matches
// here are hand-built.
func TestModelDecideTrustReasonFromHandBuiltMatches(t *testing.T) {
	project := func(id string, trusted bool) map[string]any {
		return map[string]any{"domain": id, "by": "project", "trusted": trusted}
	}
	label := func(id string, trusted bool) map[string]any {
		return map[string]any{"domain": id, "by": "label", "trusted": trusted}
	}
	for _, tc := range []struct {
		name    string
		matches []any
		reason  string
		want    string
		guard   string
	}{
		{"two project matches, trusted", []any{project("a", true), project("b", true)}, "trusted", "ambiguous", "overridden"},
		{"two project matches, untrusted", []any{project("a", false), project("b", true)}, "untrusted", "ambiguous", "overridden"},
		{"one project match, trusted", []any{project("a", true)}, "trusted", "trusted", "permitted"},
		{"one project match, untrusted", []any{project("a", false), label("b", true)}, "untrusted", "untrusted", "permitted"},
		{"a trusted label match is no project match", []any{label("a", true)}, "trusted", "label_only", "overridden"},
		{"no match", []any{}, "untrusted", "no_domain", "overridden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newChatHarness(t)
			subject, revision := c.propose(budgetIssue)
			out, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_case_open", Args: map[string]any{
				"kind": "decide", "subject_id": subject, "subject_revision": revision, "policy_revision": 1,
				"permitted": []any{"wait", "escalate"}, "issue": map[string]any{"id": budgetIssue}, "domain_matches": tc.matches}})
			if err != nil || out.Data.(map[string]any)["opened"] != true {
				t.Fatal(out, err)
			}
			caseID, _ := out.Data.(map[string]any)["case_id"].(string)
			next, err := c.s.ModelNextCase(c.ctx, c.token, 0)
			if err != nil || next.Case == nil || next.Case["case_id"] != caseID {
				t.Fatal("the model's next case:", next, err)
			}
			res, err := c.s.ModelTool(c.ctx, c.token, caseID, "decide", map[string]any{"action": "wait", "reason_code": tc.reason, "explanation": "x"})
			result, _ := res.Result.(map[string]any)
			if err != nil || res.Error != "" || result["reason_code"] != tc.want || result["guardrail"] != tc.guard {
				t.Fatal(res, err)
			}
		})
	}
}
