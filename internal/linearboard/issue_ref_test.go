package linearboard

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// refProvider answers an identifier read with a chosen provider error, the way
// the native provider refuses a stale identifier or a missing issue.
type refProvider struct {
	*providerV2
	fail error
	// answer, when set, is returned for any identifier read.
	answer *linearprovider.IssueV2
}

func (p refProvider) IssueV2(ctx context.Context, id string) (linearprovider.IssueV2, error) {
	if p.answer != nil && !linearprovider.ValidID(id) {
		p.providerV2.reads++
		return *p.answer, nil
	}
	if p.fail != nil && !linearprovider.ValidID(id) {
		p.providerV2.reads++
		return linearprovider.IssueV2{}, p.fail
	}
	return p.providerV2.IssueV2(ctx, id)
}

// staleTo is the provider's refusal of a stale identifier: Linear returned the
// issue now named id, in team.
func staleTo(id, team string) error {
	return &linearprovider.Error{Kind: "scope_mismatch", Returned: &linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, Identifier: id, TeamID: team, ProjectID: projectA}, ProjectKnown: true}}
}

func refTest(t *testing.T, fail error) (*Service, *providerV2, func(string) (string, string, error)) {
	t.Helper()
	s, _, p, scope := testV2(t)
	i := p.issues[issueID]
	i.Identifier = "ENG-88"
	p.issues[issueID] = i
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		return refProvider{providerV2: p, fail: fail}, nil
	}
	return s, p, func(ref string) (string, string, error) { return s.IssueRef(context.Background(), scope, ref) }
}

func TestIssueRefUUIDPassesWithNoLinearCall(t *testing.T) {
	_, p, ref := refTest(t, nil)
	id, identifier, e := ref(issueID)
	if e != nil || id != issueID || identifier != "" || p.tools != 0 || p.reads != 0 {
		t.Fatal(id, identifier, e, p.tools, p.reads)
	}
}

func TestIssueRefResolvesAnIdentifierToItsUUID(t *testing.T) {
	_, p, ref := refTest(t, nil)
	id, identifier, e := ref("ENG-88")
	if e != nil || id != issueID || identifier != "ENG-88" || p.reads != 1 {
		t.Fatal(id, identifier, e, p.reads)
	}
}

func TestIssueRefRefusesAStaleIdentifierNamingTheCurrentOne(t *testing.T) {
	_, _, ref := refTest(t, staleTo("OPS-5", teamID))
	if _, _, e := ref("ENG-88"); e == nil || e.Error() != "ENG-88 now resolves to OPS-5; use that identifier or the UUID" {
		t.Fatal(e)
	}
	// A provider that does not say what it resolved to still refuses.
	_, _, ref = refTest(t, &linearprovider.Error{Kind: "scope_mismatch"})
	if _, _, e := ref("ENG-88"); e == nil || !strings.Contains(e.Error(), "no longer names") {
		t.Fatal(e)
	}
	// An identifier Linear does not know is refused.
	_, _, ref = refTest(t, nil)
	if _, _, e := ref("ENG-99"); e == nil {
		t.Fatal("an unknown identifier resolved")
	}
}

func TestIssueRefNotFound(t *testing.T) {
	for _, code := range []string{"NOT_FOUND", "ENTITY_NOT_FOUND"} {
		_, _, ref := refTest(t, &linearprovider.Error{Kind: "provider_rejected", LinearCode: code})
		if _, _, e := ref("ENG-88"); e == nil || e.Error() != "no issue ENG-88 in this binding" || !IsOutOfScope(e) {
			t.Fatal(code, e)
		}
	}
}

func TestIssueRefOtherRejectionsPassThrough(t *testing.T) {
	for _, code := range []string{"FORBIDDEN", "INVALID_INPUT", ""} {
		rejected := &linearprovider.Error{Kind: "provider_rejected", LinearCode: code}
		_, _, ref := refTest(t, rejected)
		if _, _, e := ref("ENG-88"); e == nil || e != error(rejected) || strings.Contains(e.Error(), "no issue") {
			t.Fatal(code, e)
		}
	}
}

// A stale identifier whose issue moved to a team the binding does not hold is
// refused as a UUID read of that issue is, and never names the new identifier.
func TestIssueRefStaleIntoAnUnboundTeamNamesNothing(t *testing.T) {
	const unbound = "99999999-aaaa-4aaa-8aaa-999999999999"
	moved := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, Identifier: "OPS-5", TeamID: unbound}, ProjectKnown: true}
	for name, fail := range map[string]error{
		"provider refusal": staleTo("OPS-5", unbound),
		"returned issue":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, p, scope := testV2(t)
			i := p.issues[issueID]
			i.Identifier = "ENG-88"
			p.issues[issueID] = i
			p.issues[parentA] = moved
			var answer *linearprovider.IssueV2
			if fail == nil {
				// The provider hands back the moved issue for ENG-88 without refusing.
				answer = &moved
			}
			s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
				return refProvider{providerV2: p, fail: fail, answer: answer}, nil
			}
			_, eUUID := s.IssueV2(context.Background(), scope, parentA)
			_, _, e := s.IssueRef(context.Background(), scope, "ENG-88")
			if eUUID == nil || e == nil || e.Error() != eUUID.Error() || strings.Contains(e.Error(), "OPS-5") {
				t.Fatalf("%v (uuid: %v)", e, eUUID)
			}
		})
	}
}

func TestIssueRefRefusesAnInvalidReference(t *testing.T) {
	_, p, ref := refTest(t, nil)
	for _, bad := range []string{"", "eng-88", "ENG88", "ENG-0", "not an id", "ENG-88; drop", "00000000-0000-4000-8000-00000000000G"} {
		if _, _, e := ref(bad); e == nil || e.Error() != "issue requires its UUID or an identifier like ENG-88" {
			t.Fatalf("%q: %v", bad, e)
		}
	}
	if p.tools != 0 {
		t.Fatal("an invalid reference reached Linear")
	}
}

func TestIssueRefOutsideTheBindingIsRefusedLikeItsUUID(t *testing.T) {
	s, _, p, scope := testV2(t)
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, Identifier: "OPS-5", TeamID: "99999999-aaaa-4aaa-8aaa-999999999999"}, ProjectKnown: true}
	_, _, eIdent := s.IssueRef(context.Background(), scope, "OPS-5")
	_, eUUID := s.IssueV2(context.Background(), scope, parentA)
	if eIdent == nil || eUUID == nil || eIdent.Error() != eUUID.Error() {
		t.Fatalf("identifier: %v; uuid: %v", eIdent, eUUID)
	}
}

func TestIssueV2AndCommentsAcceptAnIdentifier(t *testing.T) {
	s, p, read, _ := commentingTestV2(t)
	scope := bus.Request{Workspace: "ws", Project: "prj", Session: "ses", Args: map[string]any{"instance": "secret-instance"}}
	i := p.issues[issueID]
	i.Identifier = "ENG-88"
	p.issues[issueID] = i
	issue, e := s.IssueV2(context.Background(), scope, "ENG-88")
	if e != nil || issue.ID != issueID || issue.Identifier != "ENG-88" {
		t.Fatal(issue, e)
	}
	out, e := s.IssueV2WithComments(context.Background(), scope, "ENG-88", CommentsInputV2{})
	if e != nil || out.ID != issueID || len(*read) != 1 || !strings.HasPrefix((*read)[0], issueID+"|") {
		t.Fatal(out.ID, e, *read)
	}
}

// countedComments is a provider with comment reads that counts the issue reads
// and the binding verification reads.
type countedComments struct {
	commentingV2
	n *readCounts
}

func (p countedComments) IssueV2(ctx context.Context, id string) (linearprovider.IssueV2, error) {
	p.n.issues++
	return p.commentingV2.IssueV2(ctx, id)
}
func (p countedComments) GetTeam(ctx context.Context, id string) (linearprovider.Team, error) {
	p.n.teams++
	return p.commentingV2.GetTeam(ctx, id)
}
func (p countedComments) GetProject(ctx context.Context, id string) (linearprovider.Project, error) {
	p.n.projects++
	return p.commentingV2.GetProject(ctx, id)
}

// An identifier read costs what a UUID read costs: one connection, one binding
// verification and one issue read, for the plain read and the comments read.
func TestIssueReadByIdentifierCostsWhatAUUIDDoes(t *testing.T) {
	read := func(ref string, comments bool) (readCounts, int, string) {
		s, _, p, scope := testV2(t)
		i := p.issues[issueID]
		i.Identifier = "ENG-88"
		p.issues[issueID] = i
		var n readCounts
		opens := 0
		listed, since, deadlines := []string{}, []string{}, []time.Time{}
		s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
			opens++
			return countedComments{commentingV2{providerV2: p, read: &listed, since: &since, deadlines: &deadlines}, &n}, nil
		}
		var e error
		var got string
		if comments {
			var out IssueCommentsV2
			out, e = s.IssueV2WithComments(context.Background(), scope, ref, CommentsInputV2{})
			got = out.ID
		} else {
			var out linearprovider.IssueV2
			out, e = s.IssueV2(context.Background(), scope, ref)
			got = out.ID
		}
		if e != nil || got != issueID {
			t.Fatal(ref, comments, got, e)
		}
		if comments && (len(listed) != 1 || listed[0][:len(issueID)] != issueID) {
			t.Fatal("comments not read by the UUID:", listed)
		}
		return n, opens, got
	}
	for _, comments := range []bool{false, true} {
		byUUID, opensUUID, _ := read(issueID, comments)
		byIdentifier, opensIdentifier, _ := read("ENG-88", comments)
		if byUUID.issues != 1 || byUUID.teams != 2 || byUUID.projects != 2 || opensUUID != 1 {
			t.Fatalf("comments=%v UUID read %+v opens=%d", comments, byUUID, opensUUID)
		}
		if byIdentifier != byUUID || opensIdentifier != opensUUID {
			t.Fatalf("comments=%v identifier read %+v opens=%d, UUID read %+v opens=%d", comments, byIdentifier, opensIdentifier, byUUID, opensUUID)
		}
	}
}
