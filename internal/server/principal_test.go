package server

import (
	"testing"
	"time"

	"google.golang.org/api/idtoken"
)

// The server applies the authority's own principal rule, so a verified token
// is never accepted here and then refused as a principal by the bus.
func TestVerifiedIdentityUsesTheSharedPrincipalRule(t *testing.T) {
	now := time.Now()
	payload := func(email string) *idtoken.Payload {
		return &idtoken.Payload{Issuer: "https://accounts.google.com", Audience: "aud", Subject: "sub", Expires: now.Add(time.Hour).Unix(), Claims: map[string]any{"email": email, "email_verified": true}}
	}
	if id, err := verifiedIdentity(payload("Alice@Example.TEST"), "aud", now); err != nil || id.Principal != "alice@example.test" {
		t.Fatal("mixed-case verified email refused or not normalised", id, err)
	}
	for _, email := range []string{"<alice@example.test>", "Alice <alice@example.test>", "alice@[1.2.3.4"} {
		if _, err := verifiedIdentity(payload(email), "aud", now); err == nil {
			t.Errorf("%q accepted by the server but refused by the authority", email)
		}
	}
}
