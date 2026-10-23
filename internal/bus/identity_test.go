package bus

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func identityStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func events(t *testing.T, s *Store, principal string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT action,subject_hash FROM principal_events WHERE principal=? ORDER BY changed_at,id`, principal)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, hash string
		if err = rows.Scan(&action, &hash); err != nil {
			t.Fatal(err)
		}
		out = append(out, action+":"+hash)
	}
	return out
}

func TestPrincipalBindsOnFirstSightAndRefusesAnotherAccount(t *testing.T) {
	s := identityStore(t)
	ctx := context.Background()
	alice := Identity{Principal: "alice@example.test", Issuer: GoogleIssuer, Subject: "sub-alice"}
	if err := s.bindSubject(ctx, alice); err != nil {
		t.Fatal("first sight did not bind", err)
	}
	if err := s.bindSubject(ctx, alice); err != nil {
		t.Fatal("bound account refused", err)
	}
	for name, other := range map[string]Identity{
		"another account": {Principal: alice.Principal, Issuer: GoogleIssuer, Subject: "sub-intruder"},
		"another issuer":  {Principal: alice.Principal, Issuer: "https://issuer.example.test", Subject: "sub-alice"},
	} {
		// A refused account retrying is audited once an hour, not per request.
		for range 3 {
			err := s.bindSubject(ctx, other)
			message, public := PublicMessage(err)
			var coded interface{ Code() string }
			if err == nil || !public || !strings.Contains(message, "not the one bound to alice@example.test") || !errors.As(err, &coded) || coded.Code() != "forbidden" {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
	var subject string
	if err := s.db.QueryRow(`SELECT subject FROM principal_subjects WHERE principal=?`, alice.Principal).Scan(&subject); err != nil || subject != "sub-alice" {
		t.Fatal("a refused account changed the binding", subject, err)
	}
	got := strings.Join(events(t, s, alice.Principal), ",")
	if strings.Count(got, "principal_bound:") != 1 || strings.Count(got, "principal_subject_mismatch:") != 2 || strings.Contains(got, "sub-intruder") || !strings.Contains(got, subjectHash(GoogleIssuer, "sub-intruder")) {
		t.Fatal("binding events wrong or raw subject recorded:", got)
	}
}

func TestConcurrentFirstSightBindsOneAccount(t *testing.T) {
	s := identityStore(t)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, subject := range []string{"sub-a", "sub-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.bindSubject(context.Background(), Identity{Principal: "bob@example.test", Issuer: GoogleIssuer, Subject: subject})
		}()
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) {
		t.Fatal("concurrent first sight bound both or neither account", results)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM principal_subjects`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal(rows, err)
	}
}

func TestOnlyTheServerOwnerUnbindsAndTheReleasedAccountStaysRefused(t *testing.T) {
	s := identityStore(t)
	ctx := context.Background()
	old := Identity{Principal: "carol@example.test", Issuer: GoogleIssuer, Subject: "sub-old"}
	fresh := Identity{Principal: old.Principal, Issuer: GoogleIssuer, Subject: "sub-new"}
	if err := s.bindSubject(ctx, old); err != nil {
		t.Fatal(err)
	}
	unbind := func(as, owner string, allowPrevious bool) (map[string]any, error) {
		scoped := *s
		scoped.principal, scoped.bootstrapOwner = as, owner
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := scoped.unbindPrincipal(ctx, &transaction{Tx: tx}, map[string]any{"principal": " Carol@Example.TEST ", "allow_previous": allowPrevious})
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		m, _ := out.(map[string]any)
		return m, err
	}
	refused := func(id Identity, want string) {
		t.Helper()
		message, public := PublicMessage(s.bindSubject(ctx, id))
		if !public || !strings.Contains(message, want) {
			t.Fatalf("%s accepted or refused wrongly: %q", id.Subject, message)
		}
	}
	for name, c := range map[string][2]string{"workspace owner": {"dave@example.test", "root@example.test"}, "local store": {"root@example.test", ""}} {
		if _, err := unbind(c[0], c[1], false); err == nil {
			t.Fatalf("%s unbound a principal", name)
		}
	}
	refused(fresh, "not the one bound")
	if out, err := unbind("root@example.test", "root@example.test", false); err != nil || out["unbound"] != true {
		t.Fatal("server owner could not unbind", out, err)
	}
	// The released account may still hold a valid token: it must not win
	// the race to bind again.
	refused(old, "released from carol@example.test")
	refused(old, "released from carol@example.test")
	var previous string
	if err := s.db.QueryRow(`SELECT previous_subject FROM principal_subjects WHERE principal=?`, old.Principal).Scan(&previous); err != nil || previous != "sub-old" {
		t.Fatal("previous subject not kept", previous, err)
	}
	// An unbind made by mistake: --allow-previous on the unbound principal
	// lets the released account bind again.
	if out, err := unbind("root@example.test", "root@example.test", true); err != nil || out["unbound"] != true {
		t.Fatal("allow-previous not recorded", out, err)
	}
	if out, _ := unbind("root@example.test", "root@example.test", true); out["unbound"] != false {
		t.Fatal("allow-previous recorded twice", out)
	}
	if err := s.bindSubject(ctx, old); err != nil {
		t.Fatal("allowed previous account could not bind", err)
	}
	if _, err := unbind("root@example.test", "root@example.test", false); err != nil {
		t.Fatal(err)
	}
	refused(old, "released from")
	if err := s.bindSubject(ctx, fresh); err != nil {
		t.Fatal("next sign-in after unbind did not bind", err)
	}
	refused(old, "not the one bound")
	got := strings.Join(events(t, s, old.Principal), ",")
	oldHash := subjectHash(GoogleIssuer, "sub-old")
	for _, want := range []string{"principal_unbound:" + oldHash, "principal_unbound_allow_previous:" + oldHash, "principal_bound:" + subjectHash(GoogleIssuer, "sub-new")} {
		if !strings.Contains(got, want) {
			t.Fatal("missing", want, "in", got)
		}
	}
	if n := strings.Count(got, "principal_previous_refused:"); n != 1 {
		t.Fatal("previous-account refusals not deduplicated within the hour:", n, got)
	}
}

func TestOperatorUnbindsWithoutASignedInPrincipal(t *testing.T) {
	s := identityStore(t)
	ctx := context.Background()
	owner := Identity{Principal: "root@example.test", Issuer: GoogleIssuer, Subject: "sub-root-old"}
	if err := s.bindSubject(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OperatorUnbindPrincipal(ctx, "not-an-email", false); err == nil {
		t.Fatal("operator unbind accepted a malformed principal")
	}
	out, err := s.OperatorUnbindPrincipal(ctx, " Root@Example.test", false)
	if err != nil || out["unbound"] != true {
		t.Fatal(out, err)
	}
	if err = s.bindSubject(ctx, Identity{Principal: owner.Principal, Issuer: GoogleIssuer, Subject: "sub-root-new"}); err != nil {
		t.Fatal("owner's new account did not bind after operator unbind", err)
	}
	var by string
	if err = s.db.QueryRow(`SELECT changed_by FROM principal_events WHERE principal=? AND action='principal_unbound'`, owner.Principal).Scan(&by); err != nil || by != OperatorPrincipal {
		t.Fatal("operator unbind not audited as the operator", by, err)
	}
}
