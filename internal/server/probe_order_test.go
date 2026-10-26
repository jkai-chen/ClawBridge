package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"clawbridge/internal/linearapp"
	"clawbridge/internal/watcher"
)

const orderProject = "c0000000-0000-4000-8000-0000000000c1"

// orderLinear answers the health read with first, and its last: variant
// with last; it records every query.
type orderLinear struct {
	first, last []map[string]any
	project     bool
	err         error
	queries     []string
}

func (o *orderLinear) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	o.queries = append(o.queries, query)
	if o.err != nil {
		return o.err
	}
	if variables["id"] != orderProject {
		return errors.New("wrong project")
	}
	nodes := o.first
	if strings.Contains(query, "projectUpdates(last: ") {
		nodes = o.last
	}
	var project any
	if o.project {
		project = map[string]any{"id": orderProject, "createdAt": "2026-01-01T00:00:00Z", "projectUpdates": map[string]any{"nodes": nodes}}
	}
	raw, _ := json.Marshal(map[string]any{"project": project})
	return json.Unmarshal(raw, out)
}

func update(id, at string) map[string]any {
	return map[string]any{"id": id, "createdAt": at, "user": map[string]any{"id": "secret-author"}, "body": "secret body"}
}

func runOrder(t *testing.T, o *orderLinear) (map[string]any, error) {
	t.Helper()
	old := newOrderApp
	t.Cleanup(func() { newOrderApp = old })
	newOrderApp = func(linearapp.Store) (orderApp, error) { return o, nil }
	var out bytes.Buffer
	err := linearAppHealthOrder(context.Background(), noTokens{t}, orderProject, &out)
	if strings.Contains(out.String(), "secret") {
		t.Fatal("printed more than IDs and times:", out.String())
	}
	if out.Len() == 0 {
		return nil, err
	}
	var got struct {
		HealthOrder map[string]any `json:"health_order"`
	}
	if e := json.Unmarshal(out.Bytes(), &got); e != nil {
		t.Fatal(e, out.String())
	}
	return got.HealthOrder, err
}

// The probe sends the judgement's own read unchanged, then the same read
// with last:, and nothing else.
func TestHealthOrderSendsTheJudgementsReadAndItsLastVariant(t *testing.T) {
	o := &orderLinear{project: true}
	if _, err := runOrder(t, o); err != nil {
		t.Fatal(err)
	}
	if len(o.queries) != 2 || o.queries[0] != watcher.HealthProjectQuery ||
		o.queries[1] != strings.Replace(watcher.HealthProjectQuery, "(first: 5,", "(last: 5,", 1) {
		t.Fatalf("%q", o.queries)
	}
	for _, q := range o.queries {
		if strings.Contains(strings.ToLower(q), "mutation") {
			t.Fatal("a write was sent:", q)
		}
	}
}

func TestHealthOrderSaysWhetherTheReadHoldsTheNewest(t *testing.T) {
	u := func(n int) map[string]any {
		return update(fmt.Sprintf("u-%02d", n), fmt.Sprintf("2026-10-%02dT10:00:00.5Z", n))
	}
	list := func(ns ...int) []map[string]any {
		out := []map[string]any{}
		for _, n := range ns {
			out = append(out, u(n))
		}
		return out
	}
	for name, k := range map[string]struct {
		first, last           []map[string]any
		order                 string
		distinct              float64
		hasNewest, conclusive bool
	}{
		"newest five, newest first": {list(9, 8, 7, 6, 5), list(5, 4, 3, 2, 1), "newest_first", 9, true, true},
		"newest five, oldest first": {list(5, 6, 7, 8, 9), list(1, 2, 3, 4, 5), "oldest_first", 9, true, true},
		"oldest five":               {list(1, 2, 3, 4, 5), list(5, 6, 7, 8, 9), "oldest_first", 9, false, true},
		"misses the newest":         {list(8, 7, 6, 5, 4), list(9, 3, 2, 1, 4), "newest_first", 9, false, true},
		"five, both the same":       {list(5, 4, 3, 2, 1), list(5, 4, 3, 2, 1), "newest_first", 5, true, false},
		"five, out of order":        {list(4, 3, 2, 1, 5), list(5, 4, 3, 2, 1), "mixed", 5, true, false},
		"one update":                {list(9), list(9), "single", 1, true, false},
		"no updates":                {nil, nil, "single", 0, true, false},
	} {
		got, err := runOrder(t, &orderLinear{project: true, first: k.first, last: k.last})
		if err != nil || got["first_order"] != k.order || got["distinct"] != k.distinct || got["window_has_newest"] != k.hasNewest ||
			got["conclusive"] != k.conclusive || got["agree"] != (k.hasNewest && k.conclusive) {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	got, _ := runOrder(t, &orderLinear{project: true, first: list(9, 8), last: list(8, 9)})
	if got["last_order"] != "oldest_first" || len(got["first"].([]any)) != 2 ||
		got["first"].([]any)[0].(map[string]any)["id"] != "u-09" || got["first"].([]any)[0].(map[string]any)["created_at"] != "2026-10-09T10:00:00.5Z" {
		t.Fatal(got)
	}
}

func TestHealthOrderRefusals(t *testing.T) {
	if _, err := runOrder(t, &orderLinear{}); err == nil || !strings.Contains(err.Error(), "cannot see the project") {
		t.Fatal(err)
	}
	o := &orderLinear{err: &linearapp.Error{Code: "linear_unavailable"}}
	if _, err := runOrder(t, o); err == nil || err.Error() != "linear-app-probe health_order_first: linear_unavailable" || len(o.queries) != 1 {
		t.Fatal(err, o.queries)
	}
	if _, err := runOrder(t, &orderLinear{project: true, first: []map[string]any{update("u-a", "yesterday")}}); err == nil || !strings.Contains(err.Error(), "no readable createdAt") {
		t.Fatal(err)
	}
	o = &orderLinear{project: true}
	old := newOrderApp
	defer func() { newOrderApp = old }()
	newOrderApp = func(linearapp.Store) (orderApp, error) { return o, nil }
	for _, project := range []string{"", "PRJ-1", orderProject + " "} {
		if err := linearAppHealthOrder(context.Background(), noTokens{t}, project, &bytes.Buffer{}); err == nil || len(o.queries) != 0 {
			t.Fatal(project, err)
		}
	}
}

// Like every probe, its app never mints: with no live token it stops at
// mint_disabled.
func TestHealthOrderNeverMints(t *testing.T) {
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID", "client-1")
	t.Setenv("CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET", "s3cret-value-never-logged")
	t.Setenv("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	var out bytes.Buffer
	err := linearAppHealthOrder(context.Background(), noTokens{t}, orderProject, &out)
	if err == nil || err.Error() != "linear-app-probe health_order_first: mint_disabled" || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
}

func TestHealthOrderArguments(t *testing.T) {
	t.Setenv("CLAWBRIDGE_TEST_EMPTY_DSN", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"linear-app-probe", "--health-order", "--confirm"}, "--health-order needs --project"},
		{[]string{"linear-app-probe", "--health-order", "--p8", "--project", orderProject, "--confirm"}, "not --p8"},
		{[]string{"linear-app-probe", "--health-order", "--project", orderProject}, "writes nothing; repeat with --confirm"},
		{[]string{"linear-app-probe", "--health-order", "--project", orderProject, "--confirm", "--database-url-env", "CLAWBRIDGE_TEST_EMPTY_DSN"}, "environment variable is empty"},
		{[]string{"principal-unbind", "a@example.test", "--health-order", "--confirm"}, "not defined"},
	} {
		var out bytes.Buffer
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) || out.Len() != 0 {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}
