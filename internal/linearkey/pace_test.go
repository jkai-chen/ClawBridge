package linearkey

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A key of its own: the pacer is per key in the process.
const pacedKey = "lin_api_" + "SyntheticPaced0123456789abcdefghijklmnop"

// Linear's rate limit on the key is its own error, saying nothing was sent
// and when to try again; the pacer then holds the next call unsent; a view
// change is paced as a write.
func TestKeyRateLimited(t *testing.T) {
	noAgent(t)
	calls := 0
	fakeLinear(t, func(*http.Request) *http.Response {
		calls++
		r := reply(429, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
		r.Header.Set("Retry-After", "900")
		return r
	})
	c, err := NewClient(pacedKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Viewer(context.Background())
	var limited RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter != "900" || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err = c.Viewer(context.Background()); !errors.As(err, &limited) || calls != 1 {
		t.Fatal("the held call was sent:", err, calls)
	}
	if got := rateLimited("soon; drop table"); got.(RateLimitedError).RetryAfter != "" {
		t.Fatal("a Retry-After that isn't seconds was kept:", got)
	}
}

// A RATELIMITED error in a 200 is rate-limited too.
func TestKeyRateLimitedInA200(t *testing.T) {
	noAgent(t)
	fakeLinear(t, func(*http.Request) *http.Response {
		return reply(200, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
	})
	c, _ := NewClient("lin_api_" + "SyntheticPaced2000123456789abcdefghijklm")
	if _, err := c.Viewer(context.Background()); !errors.As(err, new(RateLimitedError)) {
		t.Fatal(err)
	}
}

// A query Linear rate-limits is sent once more when its Retry-After fits a
// read's bound: the key's queries are paced as reads (review M1).
func TestKeyQueryIsARead(t *testing.T) {
	noAgent(t)
	calls := 0
	fakeLinear(t, func(*http.Request) *http.Response {
		calls++
		if calls == 1 {
			r := reply(429, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
			r.Header.Set("Retry-After", "1")
			return r
		}
		return reply(200, `{"data":{"viewer":{"id":"00000000-0000-4000-8000-000000000001"}}}`)
	})
	c, _ := NewClient("lin_api_" + "SyntheticPacedRead0123456789abcdefghijk")
	if _, err := c.Viewer(context.Background()); err != nil || calls != 2 {
		t.Fatal("a rate-limited query was not sent once more:", err, calls)
	}
}
