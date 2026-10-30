package linearapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ArchiveIssue sends the one fixed archive mutation for the webhook probe's
// own issue and answers a class, never Linear's text.
func TestArchiveIssueSendsTheFixedMutation(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	l := &subscribeLinear{linear: &linear{app: true}, answers: []string{
		`{"data":{"issueArchive":{"success":true}}}`,
		`{"data":{"issueArchive":null},"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INVALID_INPUT"}}]}`,
		`{"data":null,"errors":[{"message":"Forbidden","extensions":{"type":"forbidden"}}]}`,
	}}
	a, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: l}, Now: func() time.Time { return now }})
	ctx := context.Background()
	if class, err := a.ArchiveIssue(ctx, strings.ToUpper(subscribeIssueID)); err != nil || class != "archived" {
		t.Fatal(class, err)
	}
	var sent struct {
		Query     string
		Variables map[string]any
	}
	if err := json.Unmarshal([]byte(l.bodies[0]), &sent); err != nil || sent.Query != issueArchiveMutation || sent.Variables["id"] != subscribeIssueID || len(sent.Variables) != 1 {
		t.Fatal(l.bodies[0])
	}
	for _, want := range []string{"not_found", "scope"} {
		if class, err := a.ArchiveIssue(ctx, subscribeIssueID); err != nil || class != want {
			t.Fatal(want, class, err)
		}
	}
	var e *Error
	if _, err := a.ArchiveIssue(ctx, "not-a-uuid"); !errors.As(err, &e) || e.Code != "invalid" {
		t.Fatal(err)
	}
}
