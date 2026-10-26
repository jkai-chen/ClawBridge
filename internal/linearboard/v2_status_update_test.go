package linearboard

import (
	"context"
	"errors"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// A status update is refused before anything is sent for a mention, an
// unknown health or project, and an unreadable body.
func TestPostStatusUpdateRefusesBeforeSending(t *testing.T) {
	s := &Service{}
	project := "00000000-0000-4000-8000-000000000001"
	for name, c := range map[string]struct{ project, body, health, code string }{
		"mention":    {project, "ask @someone", "onTrack", "mention"},
		"health":     {project, "body", "great", "invalid"},
		"project":    {"not-a-uuid", "body", "onTrack", "invalid"},
		"control":    {project, "a\x1bb", "onTrack", "invalid"},
		"empty body": {project, "", "onTrack", "invalid"},
	} {
		_, err := s.PostStatusUpdateV2(context.Background(), bus.Request{}, c.project, c.body, c.health)
		outcome, code := StatusUpdateOutcome("", err)
		if outcome != "refused" || code != c.code {
			t.Fatal(name, outcome, code, err)
		}
	}
}

func TestStatusUpdateOutcome(t *testing.T) {
	id := "cccccccc-cccc-4ccc-8ccc-000000000001"
	for name, c := range map[string]struct {
		id      string
		err     error
		outcome string
		code    string
	}{
		"accepted":           {id, nil, "accepted", ""},
		"no readable id":     {"", nil, "uncertain", "receipt_unreadable"},
		"out of binding":     {"", refusedBeforeDispatch(outOfScopeError("x")), "refused", "out_of_binding"},
		"mention":            {"", refusedBeforeDispatch(errMention), "refused", "mention"},
		"capability":         {"", &linearprovider.Error{Kind: "capability_unavailable"}, "refused", "capability_unavailable"},
		"tool error":         {"", &linearprovider.Error{Kind: "provider_rejected"}, "refused", "provider_rejected"},
		"rate limited":       {"", &linearprovider.Error{Kind: "rate_limited", NotSent: true}, "refused", "rate_limited"},
		"unavailable, sent":  {"", &linearprovider.Error{Kind: "unavailable"}, "uncertain", "unavailable"},
		"unreadable answer":  {"", &linearprovider.Error{Kind: "invalid_response"}, "uncertain", "invalid_response"},
		"anything else":      {"", errors.New("x"), "uncertain", "failed"},
		"deadline, unproven": {"", context.DeadlineExceeded, "uncertain", "failed"},
	} {
		if outcome, code := StatusUpdateOutcome(c.id, c.err); outcome != c.outcome || code != c.code {
			t.Fatal(name, outcome, code)
		}
	}
}
