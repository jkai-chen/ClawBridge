package linearapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearprovider"
)

// PC-2: an app that may not mint never reaches the token endpoint, and
// uses the live token another instance published.
func TestNoMintUsesOnlyTheLiveToken(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	l := &linear{app: true}
	probe, err := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: store, HTTP: &http.Client{Transport: l}, Now: func() time.Time { return now }, NoMint: true})
	if err != nil {
		t.Fatal(err)
	}
	var e *Error
	if _, _, err = probe.Token(context.Background()); !errors.As(err, &e) || e.Code != "mint_disabled" || l.mints != 0 || l.viewers != 0 {
		t.Fatal("no live token must refuse without a request", err, l.mints, l.viewers)
	}
	if _, _, err = newApp(t, store, l, &now).Token(context.Background()); err != nil || l.mints != 1 {
		t.Fatal("server mint", err, l.mints)
	}
	token, id, err := probe.Token(context.Background())
	if err != nil || token != "lin_oauth_token_1" || id.AppUserID != appUser || l.mints != 1 {
		t.Fatal("live token not adopted", token, id, err, l.mints)
	}
	// An expiring token is not replaced either.
	now = now.Add(29 * 24 * time.Hour)
	probe2, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: store, HTTP: &http.Client{Transport: l}, Now: func() time.Time { return now }, NoMint: true})
	if _, _, err = probe2.Token(context.Background()); !errors.As(err, &e) || e.Code != "mint_disabled" || l.mints != 1 {
		t.Fatal("expiring token must not be re-minted", err, l.mints)
	}
	if _, _, err = probe.Remint(context.Background(), token); !errors.As(err, &e) || e.Code != "mint_disabled" || l.mints != 1 {
		t.Fatal("remint must not mint", err, l.mints)
	}
}

// PC-2: a scope set other than the released one is refused before any
// request, since minting it would revoke every token the app holds.
func TestChangedScopesAreRefusedBeforeAnyRequest(t *testing.T) {
	saved := Scopes
	t.Cleanup(func() { Scopes = saved })
	for _, scopes := range [][]string{append(append([]string{}, saved...), "write"), saved[:4], {"read", "write"}} {
		Scopes = scopes
		now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
		l := &linear{app: true}
		var e *Error
		if _, _, err := newApp(t, &memStore{live: map[string]int{}}, l, &now).Token(context.Background()); !errors.As(err, &e) || e.Code != "scopes_changed" || l.mints != 0 || l.viewers != 0 {
			t.Fatal(scopes, err, l.mints, l.viewers)
		}
	}
	Scopes = saved
	if strings.Join(Scopes, ",") != releasedScopes {
		t.Fatal("Scopes and releasedScopes differ")
	}
}

func TestProjectUpdateProbeClassifies(t *testing.T) {
	gqlError := func(message, kind string) string {
		raw, _ := json.Marshal(map[string]any{"data": nil, "errors": []any{map[string]any{"message": message, "extensions": map[string]any{"type": kind, "userPresentableMessage": message}}}})
		return string(raw)
	}
	for _, c := range []struct {
		status int
		body   string
		class  string
	}{
		{200, `{"data":{"projectUpdateCreate":{"success":true,"projectUpdate":{"id":"x"}}}}`, "created"},
		{200, `{"data":{"projectUpdateCreate":{"success":false,"projectUpdate":{"id":"x"}}}}`, "created"},
		{400, gqlError("Invalid scope: `write` required", "forbidden"), "scope"},
		{400, gqlError("Invalid scope: `write` required", "invalid input"), "scope"},
		{200, gqlError("You do not have permission to do this", "authorization error"), "scope"},
		{200, gqlError("Entity not found: Project", "invalid input"), "not_found"},
		{400, gqlError("Argument Validation Error", "invalid input"), "validation"},
		{401, `{}`, "token_rejected"},
		{502, `<html>bad gateway</html>`, "unavailable"},
		{200, `{"data":{"projectUpdateCreate":null}}`, "other"},
	} {
		got := classifyProbe(c.status, []byte(c.body))
		if got.Class != c.class || got.Status != c.status {
			t.Fatal(c.body, got)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "Entity") || strings.Contains(string(raw), "permission") || strings.Contains(string(raw), "write") {
			t.Fatal("message text leaked:", string(raw))
		}
	}
	// PC-3: only a created update's ID is printed, so the owner can delete it.
	const created = "e0000000-0000-4000-8000-0000000000e1"
	got := classifyProbe(200, []byte(`{"data":{"projectUpdateCreate":{"success":true,"projectUpdate":{"id":"`+created+`"}}}}`))
	if got.Class != "created" || got.CreatedID != created {
		t.Fatal(got)
	}
	if got = classifyProbe(200, []byte(`{"data":{"projectUpdateCreate":{"success":true,"projectUpdate":{"id":"not an id"}}}}`)); got.Class != "created" || got.CreatedID != "" {
		t.Fatal("a malformed ID was kept", got)
	}
	if got = classifyProbe(400, []byte(`{"data":{"projectUpdateCreate":{"success":false,"projectUpdate":null}},"errors":[{"message":"Entity not found: `+created+`","extensions":{"type":"invalid input"}}]}`)); got.Class != "not_found" || got.CreatedID != "" {
		t.Fatal("an ID was kept for another class", got)
	}
	got = classifyProbe(400, []byte(gqlError("x", "forbidden; DROP")))
	if len(got.Types) != 0 {
		t.Fatal("an unsafe type name is printed", got.Types)
	}
}

// mutationLinear answers the probe's mutation after the viewer.
type mutationLinear struct {
	*linear
	status int
	bodies []string
}

func (m *mutationLinear) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if r.URL.String() != graphqlURL || strings.Contains(string(body), "viewer") {
		return m.linear.RoundTrip(r)
	}
	m.bodies = append(m.bodies, string(body))
	return &http.Response{StatusCode: m.status, Body: io.NopCloser(strings.NewReader(`{"data":null,"errors":[{"message":"Entity not found: Project","extensions":{"type":"invalid input"}}]}`)), Header: http.Header{}}, nil
}

// PC-3: one fixed mutation, a random project, body "probe", onTrack; a
// refused token is reported and never replaced.
func TestProjectUpdateProbeSendsOneFixedMutation(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	m := &mutationLinear{linear: &linear{app: true}, status: 200}
	if _, _, err := newApp(t, store, m.linear, &now).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: store, HTTP: &http.Client{Transport: m}, Now: func() time.Time { return now }, NoMint: true})
	got, err := probe.ProbeProjectUpdateScope(context.Background())
	if err != nil || got.Class != "not_found" || len(m.bodies) != 1 {
		t.Fatal(got, err, m.bodies)
	}
	var sent struct {
		Query     string `json:"query"`
		Variables struct {
			Input map[string]any `json:"input"`
		} `json:"variables"`
	}
	_ = json.Unmarshal([]byte(m.bodies[0]), &sent)
	in := sent.Variables.Input
	if sent.Query != projectUpdateProbe || len(in) != 3 || in["body"] != "probe" || in["health"] != "onTrack" || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(in["projectId"].(string)) {
		t.Fatal("probe input", m.bodies[0])
	}
	m.status = 401
	if got, err = probe.ProbeProjectUpdateScope(context.Background()); err != nil || got.Class != "token_rejected" || len(m.bodies) != 2 || m.mints != 1 {
		t.Fatal("a refused token is reported once, never re-minted", got, err, len(m.bodies), m.mints)
	}
}

// shapeLinear answers the project read after the viewer.
type shapeLinear struct {
	*linear
	user string
}

func (s *shapeLinear) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if r.URL.String() != graphqlURL || strings.Contains(string(body), "viewer") {
		return s.linear.RoundTrip(r)
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"project": map[string]any{"targetDate": "2026-12-01", "startDate": nil, "createdAt": "2026-10-01T00:00:00Z", "health": "atRisk", "healthUpdatedAt": "2026-10-20T00:00:00Z", "lastUpdate": map[string]any{"createdAt": "2026-10-20T00:00:00Z", "health": "atRisk", "user": map[string]any{"id": s.user}}}}})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}, nil
}

func TestProjectShapeIsKindsOnly(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	s := &shapeLinear{linear: &linear{app: true}, user: appUser}
	if _, _, err := newApp(t, store, s.linear, &now).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: store, HTTP: &http.Client{Transport: s}, Now: func() time.Time { return now }, NoMint: true})
	if _, err := probe.ProbeProjectShape(context.Background(), "not-a-uuid"); err == nil {
		t.Fatal("an invalid project ID was sent")
	}
	const project = "c0000000-0000-4000-8000-0000000000c1"
	shape, err := probe.ProbeProjectShape(context.Background(), project)
	if err != nil || shape.LastUpdateByApp != "app" || shape.Fields["targetDate"] != "string" || shape.Fields["startDate"] != "null" || shape.Fields["lastUpdate.user"] != "object" {
		t.Fatal(shape, err)
	}
	raw, _ := json.Marshal(shape)
	for _, value := range []string{"2026", "atRisk", appUser} {
		if strings.Contains(string(raw), value) {
			t.Fatal("a value leaked:", string(raw))
		}
	}
	s.user = "d0000000-0000-4000-8000-0000000000d1"
	if shape, err = probe.ProbeProjectShape(context.Background(), project); err != nil || shape.LastUpdateByApp != "other" {
		t.Fatal(shape, err)
	}
}

func TestSummariseDocumentsOrder(t *testing.T) {
	ref := func(at string) linearprovider.DocRef { return linearprovider.DocRef{UpdatedAt: at} }
	for want, items := range map[string][]linearprovider.DocRef{
		"newest_first": {ref("2026-10-26T10:00:00Z"), ref("2026-10-26T09:00:00Z")},
		"oldest_first": {ref("2026-10-26T09:00:00Z"), ref("2026-10-26T10:00:00Z")},
		"unordered":    {ref("2026-10-26T09:00:00Z"), ref("2026-10-26T10:00:00Z"), ref("2026-10-26T08:00:00Z")},
		"too_few":      {ref("2026-10-26T09:00:00Z")},
	} {
		if got := SummariseDocuments(linearprovider.Page[linearprovider.DocRef]{Items: items}); got.Order != want {
			t.Fatal(want, got)
		}
	}
	got := SummariseDocuments(linearprovider.Page[linearprovider.DocRef]{Items: []linearprovider.DocRef{{IssueID: "x", ProjectID: "y"}, {InitiativeID: "z"}}})
	if got.Parents["issue"] != 1 || got.Parents["initiative"] != 1 || got.WithoutTime != 2 {
		t.Fatal(got)
	}
}

// p8Linear answers P8's project read after the viewer.
type p8Linear struct {
	*linear
	project map[string]any
	queries []string
}

func (s *p8Linear) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if r.URL.String() != graphqlURL || strings.Contains(string(body), "viewer") {
		return s.linear.RoundTrip(r)
	}
	s.queries = append(s.queries, string(body))
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"project": s.project}})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}, nil
}

// P8's precheck reads, as a query, whether the project is empty: no
// issue (archived included), member or lead.
func TestP8ProjectPrecheck(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	const project = "c0000000-0000-4000-8000-0000000000c1"
	none := map[string]any{"nodes": []any{}}
	one := map[string]any{"nodes": []any{map[string]any{"id": "c0000000-0000-4000-8000-0000000000c2"}}}
	s := &p8Linear{linear: &linear{app: true}}
	if _, _, err := newApp(t, store, s.linear, &now).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe, _ := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: store, HTTP: &http.Client{Transport: s}, Now: func() time.Time { return now }, NoMint: true})
	two := map[string]any{"nodes": []any{map[string]any{"id": "c0000000-0000-4000-8000-0000000000c2"}, map[string]any{"id": "c0000000-0000-4000-8000-0000000000c3"}}}
	for name, c := range map[string]struct {
		project map[string]any
		empty   bool
		allow   string
	}{
		"empty":   {map[string]any{"id": project, "lead": nil, "members": none, "issues": none}, true, ""},
		"issue":   {map[string]any{"id": project, "lead": nil, "members": none, "issues": one}, false, ""},
		"member":  {map[string]any{"id": project, "lead": nil, "members": one, "issues": none}, false, ""},
		"lead":    {map[string]any{"id": project, "lead": map[string]any{"id": appUser}, "members": none, "issues": none}, false, ""},
		"missing": {nil, false, ""},
		"other":   {map[string]any{"id": "c0000000-0000-4000-8000-0000000000c9", "lead": nil, "members": none, "issues": none}, false, ""},
		// --allow-member: exactly the one member allowed, by Linear ID.
		"member allowed":       {map[string]any{"id": project, "lead": nil, "members": one, "issues": none}, true, "c0000000-0000-4000-8000-0000000000c2"},
		"member allowed, case": {map[string]any{"id": project, "lead": nil, "members": one, "issues": none}, true, "C0000000-0000-4000-8000-0000000000C2"},
		"another member":       {map[string]any{"id": project, "lead": nil, "members": one, "issues": none}, false, "c0000000-0000-4000-8000-0000000000c3"},
		"two members":          {map[string]any{"id": project, "lead": nil, "members": two, "issues": none}, false, "c0000000-0000-4000-8000-0000000000c2"},
		"allowed, but issue":   {map[string]any{"id": project, "lead": nil, "members": one, "issues": one}, false, "c0000000-0000-4000-8000-0000000000c2"},
		"allowed, but lead":    {map[string]any{"id": project, "lead": map[string]any{"id": appUser}, "members": one, "issues": none}, false, "c0000000-0000-4000-8000-0000000000c2"},
		"allowed, no member":   {map[string]any{"id": project, "lead": nil, "members": none, "issues": none}, true, "c0000000-0000-4000-8000-0000000000c2"},
	} {
		s.project = c.project
		got, err := probe.ProbeP8Project(context.Background(), project)
		got.Allow(c.allow)
		if err != nil || got.Empty() != c.empty {
			t.Fatal(name, got, err)
		}
		// Only the one member allowed is reported as allowed.
		allowed := strings.HasPrefix(name, "member allowed") || name == "allowed, but issue" || name == "allowed, but lead"
		if got.MemberAllowed != allowed {
			t.Fatal(name, "member_allowed", got.MemberAllowed)
		}
	}
	for _, q := range s.queries {
		if strings.Contains(q, "mutation") || !strings.Contains(q, "includeArchived: true") || !strings.Contains(q, "members(first: 2)") {
			t.Fatal("the precheck is not a read of every issue:", q)
		}
	}
	if _, err := probe.ProbeP8Project(context.Background(), "not-a-uuid"); err == nil {
		t.Fatal("an invalid project ID was sent")
	}
}
