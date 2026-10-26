package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

func TestLinearAppProbeArguments(t *testing.T) {
	t.Setenv("CLAWBRIDGE_TEST_EMPTY_DSN", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"linear-app-probe"}, "--confirm"},
		{[]string{"linear-app-probe", "extra", "--confirm"}, "usage"},
		{[]string{"linear-app-probe", "--confirm", "--project", "c0000000-0000-4000-8000-0000000000c1", "--database-url-env", "CLAWBRIDGE_TEST_EMPTY_DSN"}, "environment variable is empty"},
		{[]string{"principal-unbind", "a@example.test", "--project", "x", "--confirm"}, "not defined"},
	} {
		var out bytes.Buffer
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) || out.Len() != 0 {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

// noTokens is a sealed-token store with no live token; any write fails the
// test, since the probe never mints.
type noTokens struct{ t *testing.T }

func (n noTokens) RecordMintedAppToken(context.Context, string, string, time.Time, time.Time) error {
	n.t.Fatal("the probe recorded a mint")
	return nil
}
func (n noTokens) LiveAppToken(context.Context, string, string) (bus.AppToken, bool, error) {
	return bus.AppToken{}, false, nil
}
func (n noTokens) PublishAppToken(context.Context, bus.AppToken, string) (bus.AppToken, bool, error) {
	n.t.Fatal("the probe published a token")
	return bus.AppToken{}, false, nil
}
func (n noTokens) AppTokensMinted(context.Context, string, time.Time) (int, error) { return 0, nil }

// PC-2: the probe's app never mints. With no live token it stops at
// mint_disabled before any request.
func TestProbeAppNeverMints(t *testing.T) {
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID", "client-1")
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET", "s3cret-value-never-logged")
	t.Setenv("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	app, err := newProbeApp(noTokens{t})
	if err != nil {
		t.Fatal(err)
	}
	var coded *linearapp.Error
	if _, err = app.ProbeProjectUpdateScope(context.Background()); !errors.As(err, &coded) || coded.Code != "mint_disabled" {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = linearAppProbe(context.Background(), noTokens{t}, "", &out)
	if err == nil || strings.Contains(err.Error(), "s3cret") || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID", "")
	if _, err = newProbeApp(noTokens{t}); err == nil {
		t.Fatal("a partial app configuration was accepted")
	}
	for _, name := range []string{"CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET", "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"} {
		t.Setenv(name, "")
	}
	if _, err = newProbeApp(noTokens{t}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatal(err)
	}
}

type fakeProbeApp struct {
	tools   map[string][]string
	class   string
	project string
}

func (f *fakeProbeApp) Tools(context.Context) (map[string][]string, error) { return f.tools, nil }
func (f *fakeProbeApp) ProbeProjectShape(_ context.Context, project string) (linearapp.ProjectShape, error) {
	f.project = project
	return linearapp.ProjectShape{Fields: map[string]string{"health": "string"}, LastUpdateByApp: "other"}, nil
}
func (f *fakeProbeApp) ProbeProjectUpdateScope(context.Context) (linearapp.ScopeProbe, error) {
	return linearapp.ScopeProbe{Class: f.class, Status: 400, Types: []string{"forbidden"}}, nil
}

func TestLinearAppProbeReportsClassesAndNames(t *testing.T) {
	fake := &fakeProbeApp{tools: map[string][]string{"save_issue": {"id*"}, "save_project_update": {"body", "health", "projectId*"}, "list_issues": nil}, class: "scope"}
	saved := newProbeApp
	t.Cleanup(func() { newProbeApp = saved })
	newProbeApp = func(linearapp.Store) (probeApp, error) { return fake, nil }
	var out bytes.Buffer
	if err := linearAppProbe(context.Background(), nil, "", &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"names":["list_issues","save_issue","save_project_update"]`) || !strings.Contains(got, `"project_update_tools":{"save_project_update":["body","health","projectId*"]}`) || !strings.Contains(got, `"class":"scope"`) || strings.Contains(got, `"project"`) || fake.project != "" {
		t.Fatal(got)
	}
	out.Reset()
	if err := linearAppProbe(context.Background(), nil, "c0000000-0000-4000-8000-0000000000c1", &out); err != nil || !strings.Contains(out.String(), `"last_update_by":"other"`) || fake.project == "" {
		t.Fatal(err, out.String())
	}
	// PC-3: anything created is reported at once.
	fake.class = "created"
	out.Reset()
	if err := linearAppProbe(context.Background(), nil, "", &out); err == nil || !strings.Contains(err.Error(), "ALERT") || !strings.Contains(out.String(), `"class":"created"`) {
		t.Fatal(err, out.String())
	}
}
