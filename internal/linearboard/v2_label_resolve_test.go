package linearboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

// membershipV2 adds the native label-ID proof: a full catalog and a
// list_issues membership answer per label UUID.
type membershipV2 struct {
	*providerV2
	catalog  []linearprovider.LabelName
	member   map[string]bool
	bounded  bool
	catalogs *int
	checks   *int
	sent     *[]string
}

func (p membershipV2) LabelCatalog(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.LabelName], error) {
	*p.catalogs++
	return linearprovider.Page[linearprovider.LabelName]{Items: p.catalog, Complete: true}, nil
}
func (p membershipV2) IssueHasLabel(_ context.Context, _, _, _, label string) (bool, bool, error) {
	*p.checks++
	*p.sent = append(*p.sent, label)
	if p.bounded {
		return false, false, nil
	}
	return p.member[label], true, nil
}

func resolvingV2(t *testing.T, names []string, catalog []linearprovider.LabelName, member map[string]bool) (*Service, *providerV2, *membershipV2, func() WriteInputV2) {
	t.Helper()
	s, _, p, _ := testV2(t)
	p.caps.LabelMembershipProof = true
	issue := p.issues[issueID]
	issue.LabelsKnown, issue.LabelIDs, issue.LabelNames = false, nil, names
	p.issues[issueID] = issue
	catalogs, checks, sent := 0, 0, []string{}
	m := &membershipV2{providerV2: p, catalog: catalog, member: member, catalogs: &catalogs, checks: &checks, sent: &sent}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return *m, nil }
	clear := func() WriteInputV2 {
		return WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "clear-named", IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}}
	}
	return s, p, m, clear
}

func TestLinearV2NamedLabelsAreProvenBeforeReplacement(t *testing.T) {
	catalog := []linearprovider.LabelName{{ID: labelA, Name: "Bug"}, {ID: memberA, Name: "Ops"}}
	s, p, m, clear := resolvingV2(t, []string{"Bug"}, catalog, map[string]bool{labelA: true})
	_, _, _, scope := testV2(t)
	if _, e := s.WriteV2(context.Background(), scope, clear()); e != nil || p.writes != 1 || len(p.updated[0].Labels.Value) != 0 {
		t.Fatal("proven named label could not be cleared", e)
	}
	// One catalog walk and one membership check per pass; only UUIDs sent.
	if *m.catalogs != 2 || *m.checks != 2 || (*m.sent)[0] != labelA {
		t.Fatalf("proof cost catalogs=%d checks=%d sent=%v", *m.catalogs, *m.checks, *m.sent)
	}
	// Plain reads keep names unknown and cost nothing extra; the opt-in read proves IDs.
	plain, e := s.IssueV2(context.Background(), scope, issueID)
	if e != nil || plain.LabelsKnown || plain.LabelNames[0] != "Bug" || *m.catalogs != 2 {
		t.Fatal("plain read resolved labels", plain, e)
	}
	resolved, e := s.IssueV2ResolvedLabels(context.Background(), scope, issueID)
	if e != nil || !resolved.LabelsKnown || len(resolved.LabelIDs) != 1 || resolved.LabelIDs[0] != labelA {
		t.Fatal("opt-in read did not prove label IDs", resolved, e)
	}
}

func TestLinearV2UnprovenNamedLabelsAreRefused(t *testing.T) {
	bug := linearprovider.LabelName{ID: labelA, Name: "Bug"}
	cases := map[string]struct {
		names   []string
		catalog []linearprovider.LabelName
		member  map[string]bool
		bounded bool
		noCap   bool
	}{
		"ambiguous name":     {[]string{"Bug"}, []linearprovider.LabelName{bug, {ID: memberA, Name: "Bug"}}, map[string]bool{labelA: true, memberA: true}, false, false},
		"missing name":       {[]string{"Bug"}, []linearprovider.LabelName{{ID: memberA, Name: "Ops"}}, nil, false, false},
		"not a member":       {[]string{"Bug"}, []linearprovider.LabelName{bug}, map[string]bool{}, false, false},
		"bound exceeded":     {[]string{"Bug"}, []linearprovider.LabelName{bug}, map[string]bool{labelA: true}, true, false},
		"duplicate on issue": {[]string{"Bug", "Bug"}, []linearprovider.LabelName{bug}, map[string]bool{labelA: true}, false, false},
		"capability off":     {[]string{"Bug"}, []linearprovider.LabelName{bug}, map[string]bool{labelA: true}, false, true},
		"non-UUID candidate": {[]string{"Bug"}, []linearprovider.LabelName{{ID: "Bug", Name: "Bug"}}, map[string]bool{"Bug": true}, false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, p, m, clear := resolvingV2(t, tc.names, tc.catalog, tc.member)
			m.bounded = tc.bounded
			if tc.noCap {
				p.caps.LabelMembershipProof = false
			}
			_, _, _, scope := testV2(t)
			if _, e := s.WriteV2(context.Background(), scope, clear()); e == nil || !strings.Contains(e.Error(), "label context is unavailable") || p.writes != 0 {
				t.Fatalf("unproven label set replaced: %v", e)
			}
			for _, label := range *m.sent {
				if label != labelA && label != memberA {
					t.Fatal("non-UUID label filter sent", label)
				}
			}
		})
	}
}

// Native list rows name their labels. A filtered page is accepted only when
// the selected label's name is unique in the team's complete catalog and every
// names-only row carries it, so an ignored filter cannot pass other rows.
func TestLinearV2NamesOnlyRowsProveTheLabelFilterByUniqueName(t *testing.T) {
	named := func(id string, names ...string) linearprovider.IssueV2 {
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, TeamID: teamID, ProjectID: projectA}, ProjectKnown: true, LabelNames: names}
	}
	for name, tc := range map[string]struct {
		catalog []linearprovider.LabelName
		rows    []linearprovider.IssueV2
		proof   bool
		ok      bool
	}{
		"filtered page":             {[]linearprovider.LabelName{{ID: labelA, Name: "Bug"}, {ID: memberA, Name: "Ops"}}, []linearprovider.IssueV2{named(issueID, "Ops", "Bug"), named(parentA, "Bug")}, true, true},
		"filter ignored by Linear":  {[]linearprovider.LabelName{{ID: labelA, Name: "Bug"}, {ID: memberA, Name: "Ops"}}, []linearprovider.IssueV2{named(issueID, "Bug"), named(parentA, "Ops")}, true, false},
		"unlabelled names-only row": {[]linearprovider.LabelName{{ID: labelA, Name: "Bug"}}, []linearprovider.IssueV2{named(issueID)}, true, false},
		"duplicate name":            {[]linearprovider.LabelName{{ID: labelA, Name: "Bug"}, {ID: memberA, Name: "Bug"}}, []linearprovider.IssueV2{named(issueID, "Bug")}, true, false},
		"label absent from catalog": {[]linearprovider.LabelName{{ID: memberA, Name: "Bug"}}, []linearprovider.IssueV2{named(issueID, "Bug")}, true, false},
		"no membership capability":  {[]linearprovider.LabelName{{ID: labelA, Name: "Bug"}}, []linearprovider.IssueV2{named(issueID, "Bug")}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, p, m, _ := resolvingV2(t, nil, tc.catalog, nil)
			_, _, _, scope := testV2(t)
			p.caps.LabelMembershipProof = tc.proof
			p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}}
			p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: tc.rows, Complete: true}}
			page, e := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10, LabelID: labelA})
			if tc.ok != (e == nil) {
				t.Fatalf("accepted=%v want %v: %v", e == nil, tc.ok, e)
			}
			if tc.ok && (len(page.Items) != 2 || page.Items[0].LabelsKnown || *m.catalogs != 1 || *m.checks != 0) {
				t.Fatalf("filtered page overclaimed or walked the catalog per row: %+v catalogs=%d checks=%d", page.Items, *m.catalogs, *m.checks)
			}
		})
	}
}
