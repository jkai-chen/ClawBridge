package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// M3: the stream URL names its scope once each and never carries a secret.
func TestStreamScope(t *testing.T) {
	cases := map[string]string{
		"workspace=w&project=p&session=s&mode=consume":            "",
		"workspace=w&project=p&session=s&mode=observe":            "",
		"workspace=w&project=p&session=s&mode=consume&instance=i": "must not carry",
		"workspace=w&project=p&session=s&mode=consume&ticket=t":   "must not carry",
		"workspace=w&project=p&session=s&mode=consume&x=1":        "unsupported",
		"workspace=w&project=p&session=s&session=t&mode=consume":  "once",
		"workspace=w&project=p&mode=consume":                      "required",
		"workspace=w&project=p&session=s&mode=tail":               "mode",
	}
	for query, want := range cases {
		_, err := streamScope(httptest.NewRequest(http.MethodGet, StreamPath+"?"+query, nil))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Error(query, err)
		}
	}
}

// A burst of wakes coalesces into one; a takeover replaces the session's
// other consume streams only, and observe streams never.
func TestStreamHub(t *testing.T) {
	h := NewStreamHub()
	old, observer, other := h.subscribe("p:s", "c1"), h.subscribe("p:s", ""), h.subscribe("p:t", "c9")
	for range 5 {
		h.notified("cb_delivery", "p:s")
	}
	if len(old.wake) != 1 || len(observer.wake) != 1 || len(other.wake) != 0 {
		t.Fatal(len(old.wake), len(observer.wake), len(other.wake))
	}
	h.notified("cb_consumer", "p:s:c2")
	h.notified("cb_consumer", "p:s:c2")
	select {
	case <-old.replaced:
	default:
		t.Fatal("the old consumer stayed")
	}
	for _, sub := range []*streamSub{observer, other} {
		select {
		case <-sub.replaced:
			t.Fatal("replaced", sub.key, sub.consumer)
		default:
		}
	}
	h.unsubscribe(old)
	h.unsubscribe(observer)
	h.WakeAll()
	if len(other.wake) != 1 || len(h.subs["p:s"]) != 0 {
		t.Fatal(len(other.wake), h.subs)
	}
}

// Without a hub there is no stream: a client reads the 404 as an older
// server and keeps polling.
func TestStreamOffIsNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandlerWithOptions(nil, nil, HandlerOptions{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, StreamPath+"?workspace=w&project=p&session=s&mode=consume", nil))
	if recorder.Code != 404 {
		t.Fatal(recorder.Code)
	}
}
