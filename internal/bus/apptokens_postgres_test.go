package bus

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// One live token per app and organisation, published by compare-and-swap;
// a loser is kept as discarded and still counts toward the mint limit.
func TestPostgresAppTokenPublishIsACompareAndSwap(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	app := newID("app")
	org := "b0000000-0000-4000-8000-0000000000b1"
	now := time.Now().UTC().Truncate(time.Second)
	sha := func(c string) string { return strings.Repeat(c, 64) }
	first, published, err := f.s.PublishAppToken(ctx, sealed(app, org, sha("1"), now), "")
	if err != nil || !published || first.TokenSHA256 != sha("1") {
		t.Fatal(first, published, err)
	}
	// A publish that does not replace the live token loses and gets it back.
	current, published, err := f.s.PublishAppToken(ctx, sealed(app, org, sha("2"), now), "")
	if err != nil || published || current.TokenSHA256 != sha("1") {
		t.Fatal("a second live token was published", current, published, err)
	}
	// Replacing the live one by its digest wins.
	current, published, err = f.s.PublishAppToken(ctx, sealed(app, org, sha("3"), now), sha("1"))
	if err != nil || !published || current.TokenSHA256 != sha("3") {
		t.Fatal(current, published, err)
	}
	live, found, err := f.s.LiveAppToken(ctx, app, "")
	if err != nil || !found || live.TokenSHA256 != sha("3") || live.LinearOrgID != org || live.Ciphertext != "c2VhbGVk" {
		t.Fatal("live token", live, found, err)
	}
	if n, err := f.s.AppTokensMinted(ctx, app, now.Add(-time.Hour)); err != nil || n != 3 {
		t.Fatal("mint count includes replaced and discarded tokens", n, err)
	}
	if err = f.s.RevokedAppToken(ctx, app, sha("2")); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.s.AppTokensMinted(ctx, app, now.Add(-time.Hour)); n != 2 {
		t.Fatal("a revoked token still counts", n)
	}
	if _, _, err = f.s.PublishAppToken(ctx, AppToken{AppClientID: app}, ""); err == nil {
		t.Fatal("an incomplete record was published")
	}
	// An app live in two organisations is refused without an organisation.
	other := "b0000000-0000-4000-8000-0000000000b2"
	f.s.PublishAppToken(ctx, sealed(app, other, sha("4"), now), "")
	if _, _, err = f.s.LiveAppToken(ctx, app, ""); err == nil {
		t.Fatal("two live organisations accepted")
	}
	if live, _, err = f.s.LiveAppToken(ctx, app, other); err != nil || live.TokenSHA256 != sha("4") {
		t.Fatal(live, err)
	}
}

// Two instances publishing at once: exactly one token goes live.
func TestPostgresAppTokenPublishRaceHasOneWinner(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	app := newID("app")
	org := "b0000000-0000-4000-8000-0000000000b1"
	now := time.Now().UTC()
	var wg sync.WaitGroup
	won := make([]bool, 4)
	errs := make([]error, 4)
	got := make([]AppToken, 4)
	for i := range won {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], won[i], errs[i] = f.s.PublishAppToken(ctx, sealed(app, org, strings.Repeat(string(rune('a'+i)), 64), now), "")
		}()
	}
	wg.Wait()
	// The app lock serialises them: every loser gets the winner's token,
	// never a constraint error.
	for i, err := range errs {
		if err != nil || got[i].TokenSHA256 != got[0].TokenSHA256 {
			t.Fatal("a racing publish failed or saw another token", i, err)
		}
	}
	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatal("live tokens published at once:", winners)
	}
}

// A publish waits for the app's advisory lock, which another instance's
// publish holds.
func TestPostgresAppTokenPublishTakesTheAppLock(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	app := newID("app")
	holder, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "linear-app:"+app); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := f.s.PublishAppToken(ctx, sealed(app, "b0000000-0000-4000-8000-0000000000b1", strings.Repeat("9", 64), time.Now().UTC()), "")
		done <- err
	}()
	select {
	case err := <-done:
		holder.Rollback()
		t.Fatal("published without the app lock", err)
	case <-time.After(300 * time.Millisecond):
	}
	holder.Rollback()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
