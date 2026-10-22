package linearboard

import (
	"context"
	"testing"
	"time"
)

// newProofHarness is an MCP-shaped Service: one kept connection and, when
// cached, read commands reusing a verified binding.
func newProofHarness(t *testing.T, cached bool) *poolHarness {
	t.Helper()
	h := newPoolHarness(t, true)
	if cached {
		h.s.CacheReadBindingProof(BindingProofTTL)
	}
	return h
}

// bindingReads counts the GetTeam and GetProject calls of binding verification.
func (h *poolHarness) bindingReads() (teams, projects int) {
	for _, c := range h.opened {
		teams += c.teams
		projects += c.projects
	}
	return teams, projects
}

var proofReads = []struct {
	operation string
	args      map[string]any
}{
	{"context", map[string]any{}},
	{"issue", map[string]any{"id": issueID}},
	{"issues", map[string]any{}},
	{"labels", map[string]any{"team_id": teamID}},
	{"members", map[string]any{"team_id": teamID}},
	{"issue", map[string]any{"id": missingIssueV2}},
}

func TestLinearV2MCPReadsReuseTheBindingProofForSixtySeconds(t *testing.T) {
	plain, cached := newProofHarness(t, false), newProofHarness(t, true)
	for n, c := range proofReads {
		_, wantErr := plain.run(t, c.operation, c.args)
		_, gotErr := cached.run(t, c.operation, c.args)
		if gotErr != wantErr {
			t.Fatalf("%d %s: cached err=%q, fresh err=%q", n, c.operation, gotErr, wantErr)
		}
	}
	// The binding has 2 teams and 2 projects. Fresh: verified by every read.
	if teams, projects := plain.bindingReads(); teams != 2*len(proofReads) || projects != 2*len(proofReads) {
		t.Fatalf("fresh binding reads teams=%d projects=%d", teams, projects)
	}
	// Cached: verified once for the five successful reads; the failed read
	// (the last) found the proof still cached.
	if teams, projects := cached.bindingReads(); teams != 2 || projects != 2 {
		t.Fatalf("cached binding reads teams=%d projects=%d", teams, projects)
	}
	// That failure dropped the proof, so the next read verifies again.
	cached.run(t, "issue", map[string]any{"id": issueID})
	if teams, _ := cached.bindingReads(); teams != 4 {
		t.Fatalf("binding reads after a failed read = %d", teams)
	}
	// So does a read after the TTL.
	cached.now = cached.now.Add(BindingProofTTL)
	cached.run(t, "issue", map[string]any{"id": issueID})
	if teams, _ := cached.bindingReads(); teams != 6 {
		t.Fatalf("binding reads after the TTL = %d", teams)
	}
}

func TestLinearV2MCPStaleBindingProofNeverLetsAWriteThrough(t *testing.T) {
	for _, op := range []string{"update", "relation_add"} {
		t.Run(op, func(t *testing.T) {
			write := map[string]any{"id": issueID, "title": "stale proof", "operation_id": "proof-" + op}
			if op == "relation_add" {
				write = map[string]any{"from_id": issueID, "to_id": parentA, "type": "blocks", "operation_id": "proof-" + op}
			}
			results := map[bool]string{}
			for _, cached := range []bool{false, true} {
				h := newProofHarness(t, cached)
				h.p.issues[parentA] = h.p.issues[issueID]
				if _, e := h.run(t, "issue", map[string]any{"id": issueID}); e != "" {
					t.Fatal(e)
				}
				// Linear moves the second team out of its project: the binding no
				// longer verifies, but a cached proof would still say it does.
				h.p.projects[projectB] = []string{}
				h.now = h.now.Add(time.Second)
				_, e := h.run(t, op, write)
				if e == "" || h.p.writes != 0 {
					t.Fatalf("cached=%v: write went through on a stale proof: writes=%d", cached, h.p.writes)
				}
				results[cached] = e
				// The refusal dropped the proof: reads verify again and refuse too.
				if _, e := h.run(t, "issue", map[string]any{"id": issueID}); e == "" {
					t.Fatalf("cached=%v: read used the proof after a refused write", cached)
				}
			}
			if results[true] != results[false] {
				t.Fatalf("refusal differs: cached %q, fresh %q", results[true], results[false])
			}
		})
	}
}

func TestLinearV2MCPBindingChangeMissesTheProofCache(t *testing.T) {
	h := newProofHarness(t, true)
	read := map[string]any{"id": issueID}
	h.run(t, "issue", read)
	h.a.b.Revision++
	h.p.projects[projectB] = []string{}
	if _, e := h.run(t, "issue", read); e == "" {
		t.Fatal("a new binding revision reused the old proof")
	}
	// Within the TTL and with an unchanged binding, a read does reuse the proof;
	// that up-to-sixty-second lag applies to reads only.
	h2 := newProofHarness(t, true)
	h2.run(t, "issue", read)
	h2.p.projects[projectB] = []string{}
	if _, e := h2.run(t, "issue", read); e != "" {
		t.Fatalf("read within the TTL: %s", e)
	}
	h2.now = h2.now.Add(BindingProofTTL)
	if _, e := h2.run(t, "issue", read); e == "" {
		t.Fatal("read after the TTL used the stale proof")
	}
}

func TestBindingProofCacheRejectsABackwardClockAndForgetsOnFailedVerify(t *testing.T) {
	h := newProofHarness(t, true)
	read := map[string]any{"id": issueID}
	h.run(t, "issue", read)
	h.now = h.now.Add(-time.Second)
	h.run(t, "issue", read)
	if teams, _ := h.bindingReads(); teams != 4 {
		t.Fatalf("a clock stepped back reused the proof: team reads = %d", teams)
	}

	// Called directly (no MCP error handling), a failed verification must
	// still forget the earlier proof, so returning to the old binding verifies
	// again instead of reusing it.
	h = newProofHarness(t, true)
	if _, e := h.s.IssueV2(context.Background(), h.scope, issueID); e != nil {
		t.Fatal(e)
	}
	h.p.projects[projectB] = []string{}
	h.a.b.Revision++
	if _, e := h.s.IssueV2(context.Background(), h.scope, issueID); e == nil {
		t.Fatal("broken binding verified")
	}
	h.a.b.Revision--
	if _, e := h.s.IssueV2(context.Background(), h.scope, issueID); e == nil {
		t.Fatal("the old binding reused a proof after a failed verification")
	}
}
