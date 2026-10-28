package linearapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// issueSubscribeMutation is the one board write the app sends as GraphQL
// rather than through Linear's MCP, which has no subscribe tool
// (live/ISSUE-SUBSCRIBERS-PLAN.md §4): it adds one subscriber to one issue
// and leaves the others. The owner goes by email, anyone else by ID.
const issueSubscribeMutation = `mutation Subscribe($id: String!, $email: String, $user: String) { issueSubscribe(id: $id, userEmail: $email, userId: $user) { success } }`

// SubscribeIssue subscribes one person to one issue as the app: email or
// userID, exactly one. Its answer is a class, never Linear's text, which
// may repeat the email: subscribed, scope, not_found (the issue or the
// person), refused, token_rejected or unavailable. A 401 re-mints the
// token once and sends again; adding a subscriber twice changes nothing.
func (a *App) SubscribeIssue(ctx context.Context, issueID, email, userID string) (string, error) {
	if !validUUID(issueID) || (email == "") == (userID == "") || (userID != "" && !validUUID(userID)) {
		return "", fail("invalid")
	}
	variables := map[string]any{"id": strings.ToLower(issueID)}
	if email != "" {
		variables["email"] = email
	} else {
		variables["user"] = strings.ToLower(userID)
	}
	body, err := json.Marshal(map[string]any{"query": issueSubscribeMutation, "variables": variables})
	if err != nil {
		return "", fail("invalid")
	}
	for attempt := 0; ; attempt++ {
		token, _, err := a.Token(ctx)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := a.cfg.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "unavailable", nil
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if _, _, err = a.Remint(ctx, token); err != nil {
				return "", err
			}
			continue
		}
		return classifySubscribe(res.StatusCode, raw), nil
	}
}

func classifySubscribe(status int, raw []byte) string {
	switch {
	case status == http.StatusUnauthorized:
		return "token_rejected"
	case status >= 500:
		return "unavailable"
	}
	var envelope struct {
		Data struct {
			IssueSubscribe *struct {
				Success bool `json:"success"`
			} `json:"issueSubscribe"`
		} `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return "unavailable"
	}
	if s := envelope.Data.IssueSubscribe; s != nil && s.Success && len(envelope.Errors) == 0 {
		return "subscribed"
	}
	words := []string{}
	for _, e := range envelope.Errors {
		words = append(words, strings.ToLower(e.Message))
		for _, k := range []string{"type", "code"} {
			if v, ok := e.Extensions[k].(string); ok {
				words = append(words, strings.ToLower(v))
			}
		}
	}
	text := strings.Join(words, " ")
	switch {
	case strings.Contains(text, "scope") || strings.Contains(text, "forbidden") || strings.Contains(text, "permission") || strings.Contains(text, "not authorized") || strings.Contains(text, "unauthorized"):
		return "scope"
	case strings.Contains(text, "not found") || strings.Contains(text, "does not exist") || strings.Contains(text, "entity not found"):
		return "not_found"
	}
	return "refused"
}
