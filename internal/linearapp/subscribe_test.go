package linearapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	subscribeIssueID = "00000000-0000-4000-8000-0000000000e1"
	subscribeUserID  = "00000000-0000-4000-8000-0000000000d1"
)

// subscribeLinear answers the subscribe mutation with answers in order,
// after the viewer.
type subscribeLinear struct {
	*linear
	answers []string
	bodies  []string
}

func (s *subscribeLinear) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != graphqlURL {
		return s.linear.RoundTrip(r)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if strings.Contains(string(body), "viewer") {
		return s.linear.RoundTrip(r)
	}
	s.bodies = append(s.bodies, string(body))
	status, answer := 200, `{"data":{"issueSubscribe":{"success":true}}}`
	if len(s.answers) > 0 {
		answer, s.answers = s.answers[0], s.answers[1:]
	}
	if answer == "401" {
		status, answer = 401, `{}`
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(answer)), Header: http.Header{}}, nil
}

// SubscribeIssue sends the one fixed mutation, the owner by email or a
// person by ID, and answers a class: never Linear's text, which may repeat
// the email (ISSUE-SUBSCRIBERS-PLAN B4).
func TestSubscribeIssueSendsTheFixedMutation(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	l := &subscribeLinear{linear: &linear{app: true}}
	a, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: l}, Now: func() time.Time { return now }})
	ctx := context.Background()
	if class, err := a.SubscribeIssue(ctx, strings.ToUpper(subscribeIssueID), "owner@example.test", ""); err != nil || class != "subscribed" {
		t.Fatal(class, err)
	}
	var sent struct {
		Query     string
		Variables map[string]any
	}
	if err := json.Unmarshal([]byte(l.bodies[0]), &sent); err != nil || sent.Query != issueSubscribeMutation || sent.Variables["id"] != subscribeIssueID || sent.Variables["email"] != "owner@example.test" || sent.Variables["user"] != nil {
		t.Fatal(l.bodies[0])
	}
	if class, _ := a.SubscribeIssue(ctx, subscribeIssueID, "", subscribeUserID); class != "subscribed" || !strings.Contains(l.bodies[1], `"user":"`+subscribeUserID+`"`) {
		t.Fatal(class, l.bodies[1])
	}
	for _, bad := range [][3]string{{"not-a-uuid", "a@b.test", ""}, {subscribeIssueID, "", ""}, {subscribeIssueID, "a@b.test", subscribeUserID}, {subscribeIssueID, "", "x"}} {
		var e *Error
		if _, err := a.SubscribeIssue(ctx, bad[0], bad[1], bad[2]); !errors.As(err, &e) || e.Code != "invalid" {
			t.Fatal("not refused:", bad, err)
		}
	}
	if len(l.bodies) != 2 {
		t.Fatal("a refused call reached Linear")
	}
	for answer, want := range map[string]string{
		`{"errors":[{"message":"User owner@example.test not found","extensions":{"type":"invalid input"}}]}`: "not_found",
		`{"errors":[{"message":"Invalid scope: write required"}]}`:                                           "scope",
		`{"errors":[{"message":"Something else about owner@example.test"}]}`:                                 "refused",
		`{"data":{"issueSubscribe":{"success":false}}}`:                                                      "refused",
		`not json`: "unavailable",
	} {
		l.answers = []string{answer}
		class, err := a.SubscribeIssue(ctx, subscribeIssueID, "owner@example.test", "")
		if err != nil || class != want || strings.Contains(class, "@") {
			t.Fatal(answer, class, err)
		}
	}
	// A 401 re-mints once and sends again.
	now = now.Add(2 * time.Minute)
	l.answers = []string{"401"}
	before := len(l.bodies)
	if class, err := a.SubscribeIssue(ctx, subscribeIssueID, "", subscribeUserID); err != nil || class != "subscribed" || len(l.bodies) != before+2 {
		t.Fatal(class, err, len(l.bodies)-before)
	}
}
