package linearconn

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/linearpace"
)

// A profile's client is paced by that profile's one pacer.
func TestProfileClientIsPaced(t *testing.T) {
	m := testManager(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{}`), nil }))
	p, e := m.Import(context.Background(), "paced", Read, strings.NewReader("token-paced\n"))
	if e != nil {
		t.Fatal(e)
	}
	client, e := m.HTTPClient(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	paced, ok := client.Transport.(linearpace.Transport)
	if !ok || paced.Pacer != linearpace.For("profile:"+p.ID, linearpace.OAuthPerHour) {
		t.Fatalf("the profile's client is not paced by its pacer: %T", client.Transport)
	}
}
