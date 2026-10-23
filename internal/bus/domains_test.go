package bus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDomainGateAdmitsOnlyListedWorkspaceDomains(t *testing.T) {
	for name, c := range map[string]struct {
		list, principal, hd string
		ok                  bool
	}{
		"empty list is off":          {`[]`, "a@example.com", "", true},
		"listed domain":              {`["example.com","corp.example"]`, "a@corp.example", "corp.example", true},
		"other domain":               {`["example.com"]`, "a@other.example", "other.example", false},
		"consumer account":           {`["example.com"]`, "a@gmail.example", "", false},
		"null list admits no one":    {`null`, "a@example.com", "example.com", false},
		"empty text admits no one":   {``, "a@example.com", "example.com", false},
		"unreadable list":            {`not json`, "a@example.com", "example.com", false},
		"listed empty value":         {`[""]`, "a@gmail.example", "", false},
		"member service account":     {`["example.com"]`, "bot@proj.iam.gserviceaccount.com", "", true},
		"service account look-alike": {`["example.com"]`, "bot@iam.gserviceaccount.com.example", "", false},
		"compute service account":    {`["example.com"]`, "1-compute@developer.gserviceaccount.com", "", true},
		"app engine service account": {`["example.com"]`, "proj@appspot.gserviceaccount.com", "", true},
		"damaged list refuses SA":    {`null`, "bot@proj.iam.gserviceaccount.com", "", false},
		"unreadable list refuses SA": {`not json`, "bot@proj.iam.gserviceaccount.com", "", false},
	} {
		s := &Store{principal: c.principal, hostedDomain: c.hd}
		err := s.domainGate(c.list)
		var coded interface{ Code() string }
		if (err == nil) != c.ok || (err != nil && (!errors.As(err, &coded) || coded.Code() != "forbidden")) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The request path itself accepts the domains array: on a local store the
// operation is refused only because it needs the hosted authority.
func TestWorkspaceSetDomainsArgumentsPassValidationThroughExecute(t *testing.T) {
	s := identityStore(t)
	ctx := context.Background()
	if _, err := s.Execute(ctx, Request{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}); err != nil {
		t.Fatal(err)
	}
	for _, domains := range []any{[]any{"example.com"}, []string{"example.com"}, []any{}} {
		_, err := s.Execute(ctx, Request{Operation: "workspace_set_domains", Workspace: "w", Args: map[string]any{"domains": domains}})
		if err == nil || err.Error() != "operation requires hosted authority" {
			t.Fatalf("%v: %v", domains, err)
		}
	}
	if _, err := s.Execute(ctx, Request{Operation: "workspace_set_domains", Workspace: "w", Args: map[string]any{"domains": "example.com"}}); err == nil || !strings.Contains(err.Error(), "array") {
		t.Fatal("a non-array domains argument passed validation", err)
	}
}

func TestOnlyTheOwnerSetsDomainsAndMustKeepTheirOwn(t *testing.T) {
	s := identityStore(t)
	ctx := context.Background()
	if _, err := s.Execute(ctx, Request{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}); err != nil {
		t.Fatal(err)
	}
	set := func(as, hd string, domains any) (any, error) {
		scoped := *s
		scoped.principal, scoped.hostedDomain = as, hd
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		w, err := one(ctx, &transaction{Tx: tx}, `SELECT * FROM workspaces WHERE name='w'`)
		if err != nil {
			t.Fatal(err)
		}
		out, err := scoped.setWorkspaceDomains(ctx, &transaction{Tx: tx}, w, map[string]any{"domains": domains})
		if err == nil {
			err = tx.Commit()
		}
		return out, err
	}
	owner := s.principal
	tooMany := []any{"example.com"}
	for i := range maxAllowedDomains {
		tooMany = append(tooMany, fmt.Sprintf("d%d.example", i))
	}
	for name, c := range map[string]struct {
		as, hd  string
		domains any
	}{
		"not the owner":      {"someone@example.com", "example.com", []any{"example.com"}},
		"without own domain": {owner, "example.com", []any{"other.example"}},
		"consumer owner":     {owner, "", []any{"example.com"}},
		"not a domain":       {owner, "example.com", []any{"example.com", "not a domain"}},
		"not strings":        {owner, "example.com", []any{1}},
		"21 valid domains":   {owner, "example.com", tooMany},
	} {
		if _, err := set(c.as, c.hd, c.domains); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := set(owner, "example.com", tooMany[:maxAllowedDomains]); err != nil {
		t.Fatal("20 domains refused", err)
	}
	out, err := set(owner, "example.com", []any{" Example.COM. ", "corp.example", "example.com"})
	if err != nil || !reflect.DeepEqual(out.(map[string]any)["allowed_domains"], []string{"example.com", "corp.example"}) {
		t.Fatal("owner could not set a list holding their own domain", out, err)
	}
	if out, err = set(owner, "", []any{}); err != nil || len(out.(map[string]any)["allowed_domains"].([]string)) != 0 {
		t.Fatal("owner could not clear the list", out, err)
	}
	var audited int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE action='allowed_domains_set'`).Scan(&audited); err != nil || audited != 3 {
		t.Fatal("domain changes not audited", audited, err)
	}
	if out, err := s.OperatorClearWorkspaceDomains(ctx, "w"); err != nil || len(out["allowed_domains"].([]string)) != 0 {
		t.Fatal("operator could not clear the list", out, err)
	}
	if _, err := s.OperatorClearWorkspaceDomains(ctx, "absent"); err == nil {
		t.Fatal("operator cleared an absent workspace")
	}
}
