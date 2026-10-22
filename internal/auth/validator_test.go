package auth

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/api/idtoken"
)

// Token checks share one validator, which holds Google's certificate cache;
// a failed build is retried rather than remembered.
func TestIDTokenChecksShareOneValidator(t *testing.T) {
	original := newIDTokenValidator
	t.Cleanup(func() {
		newIDTokenValidator = original
		idTokenValidator.v = nil
	})
	idTokenValidator.v = nil
	builds := 0
	newIDTokenValidator = func() (*idtoken.Validator, error) {
		builds++
		if builds == 1 {
			return nil, errors.New("transient")
		}
		return original()
	}
	if _, err := verifyGoogleIDToken(context.Background(), "not-a-token", "audience"); err == nil {
		t.Fatal("a failed validator build verified a token")
	}
	for range 3 {
		if _, err := verifyGoogleIDToken(context.Background(), "not-a-token", "audience"); err == nil {
			t.Fatal("malformed token verified")
		}
	}
	if builds != 2 {
		t.Fatalf("validator built %d times, want one failed and one kept", builds)
	}
}
