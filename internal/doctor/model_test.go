package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

type modelExecutor struct {
	routeExecutor
	data map[string]any
	err  error
	// number decodes the data as a remote client does: json.Number when
	// set, else float64.
	number bool
}

func (m modelExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "dispatcher_model_doctor" {
		return bus.Result{}, errors.New("unexpected " + r.Operation)
	}
	if m.err != nil {
		return bus.Result{}, m.err
	}
	raw, _ := json.Marshal(m.data)
	d := json.NewDecoder(bytes.NewReader(raw))
	if m.number {
		d.UseNumber()
	}
	var out map[string]any
	if err := d.Decode(&out); err != nil {
		return bus.Result{}, err
	}
	return bus.Result{Source: "server", Data: out}, nil
}

// healthyModel is a project in model mode whose one owner's agent runs.
func healthyModel() map[string]any {
	return map[string]any{
		"seal_configured": true, "seal_previous": false, "model_on": true,
		"holders":  []any{map[string]any{"principal": "owner@example.com", "you": true, "key": "active", "config_revision": 3, "agent": "running", "last_seen_age": 8, "agent_status": "ok"}},
		"launcher": map[string]any{"tokens": 1, "reporting": true, "last_used_age": 20, "expires_in": 60 * 86400, "expiring": false},
		"capacity": map[string]any{"live_agents": 1, "max_agents": 2, "agent_memory": bus.ModelAgentMemory, "mem_available": 900 << 20},
	}
}

// M2: doctor shows a project owner the model mode: the seal key (and a
// previous one still set), each owner's key and agent with what went wrong,
// agentd's heartbeat and token expiry, and the agents against the cap with
// the memory headroom. A member, an old server, or a project without model
// setup sees nothing.
func TestDoctorShowsTheDispatcherModel(t *testing.T) {
	type want struct{ status, text string }
	for name, c := range map[string]struct {
		change func(m map[string]any)
		err    error
		want   map[string]want
	}{
		"refused": {err: codedError{"forbidden", "project owner permission required"}, want: map[string]want{}},
		"not set up": {change: func(m map[string]any) {
			m["model_on"], m["holders"] = false, []any{}
		}, want: map[string]want{}},
		"previous left, not set up": {change: func(m map[string]any) {
			m["model_on"], m["holders"], m["seal_previous"] = false, []any{}, true
		}, want: map[string]want{"dispatcher.model.seal": {Warn, "CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS is still set"}}},
		"healthy": {want: map[string]want{
			"dispatcher.model.seal":     {Pass, "no previous one is left"},
			"dispatcher.model.key":      {Pass, "yours: active (revision 3)"},
			"dispatcher.model.agent":    {Pass, "yours: running, last seen 8 s ago"},
			"dispatcher.model.launcher": {Pass, "agentd is reporting: it last called 20 s ago; its launcher token expires in 60 days"},
			"dispatcher.model.capacity": {Pass, "1 of at most 2 model agents"},
		}},
		"broken": {change: func(m map[string]any) {
			m["seal_configured"] = false
			h := m["holders"].([]any)[0].(map[string]any)
			h["agent_status"], h["launcher_report"], h["launcher_report_age"], h["credential_flag"] = "provider_auth", "agent_crashloop", 300, "second_redemption"
			h["test_failure"], h["test_revision"] = "quota", 4
			m["holders"] = append(m["holders"].([]any), map[string]any{"principal": "lead@example.com", "you": false, "key": "pending_test", "pending_revision": 2, "agent": "offline", "last_seen_age": nil})
			m["launcher"] = map[string]any{"tokens": 1, "reporting": false, "last_used_age": 7200, "expires_in": 5 * 86400, "expiring": true}
			m["capacity"] = map[string]any{"live_agents": 0, "max_agents": 2, "agent_memory": bus.ModelAgentMemory, "mem_available": 200 << 20}
		}, want: map[string]want{
			"dispatcher.model.seal":     {Fail, "no CLAWBRIDGE_MODEL_KEY_SEAL_KEY"},
			"dispatcher.model.key":      {Warn, "yours: the setup test of revision 4 failed (quota)"},
			"dispatcher.model.agent":    {Fail, "lead@example.com: never seen. yours: the provider refuses the key (provider_auth)"},
			"dispatcher.model.launcher": {Warn, "agentd is not reporting: it last called 2 hours ago. agentd's launcher token expires in 5 days"},
			"dispatcher.model.capacity": {Warn, "The 2 agents that may still start need up to 320 MiB, but the host has 200 MiB available"},
		}},
		"no tested key, no agent, at the cap": {change: func(m map[string]any) {
			m["holders"] = []any{map[string]any{"principal": "lead@example.com", "you": false, "key": "pending_test", "pending_revision": 1, "agent": "offline", "last_seen_age": 900}}
			m["launcher"] = map[string]any{"tokens": 0, "reporting": false, "last_used_age": nil, "expires_in": nil, "expiring": false}
			m["capacity"] = map[string]any{"live_agents": 2, "max_agents": 2, "agent_memory": bus.ModelAgentMemory, "mem_available": nil}
		}, want: map[string]want{
			"dispatcher.model.seal":     {Pass, ""},
			"dispatcher.model.key":      {Fail, "no owner of this project has a tested model key"},
			"dispatcher.model.agent":    {Warn, "lead@example.com: offline, last seen 15 minutes ago. No model agent has called"},
			"dispatcher.model.launcher": {Warn, "No live launcher token"},
			// At the cap, but no owner here has an active key waiting for an agent.
			"dispatcher.model.capacity": {Pass, "2 of at most 2 model agents"},
		}},
		"at the cap, this project's agent running": {change: func(m map[string]any) {
			m["capacity"] = map[string]any{"live_agents": 2, "max_agents": 2, "agent_memory": bus.ModelAgentMemory, "mem_available": 900 << 20}
		}, want: map[string]want{
			"dispatcher.model.seal": {Pass, ""}, "dispatcher.model.key": {Pass, ""}, "dispatcher.model.agent": {Pass, ""}, "dispatcher.model.launcher": {Pass, ""},
			"dispatcher.model.capacity": {Pass, "2 of at most 2 model agents are running on the server."},
		}},
		"at the cap, an active owner waiting": {change: func(m map[string]any) {
			h := m["holders"].([]any)[0].(map[string]any)
			h["agent"], h["last_seen_age"] = "offline", 600
			m["capacity"] = map[string]any{"live_agents": 2, "max_agents": 2, "agent_memory": bus.ModelAgentMemory, "mem_available": 900 << 20}
		}, want: map[string]want{
			"dispatcher.model.seal": {Pass, ""}, "dispatcher.model.key": {Pass, ""}, "dispatcher.model.agent": {Warn, "No model agent has called"}, "dispatcher.model.launcher": {Pass, ""},
			"dispatcher.model.capacity": {Warn, "At the cap, 1 owner(s) of this project with an active key have no agent running"},
		}},
		"a local store has no cap": {change: func(m map[string]any) {
			m["capacity"] = map[string]any{"live_agents": 0, "max_agents": 0, "agent_memory": bus.ModelAgentMemory, "mem_available": nil}
		}, want: map[string]want{
			"dispatcher.model.seal": {Pass, ""}, "dispatcher.model.key": {Pass, ""}, "dispatcher.model.agent": {Pass, ""}, "dispatcher.model.launcher": {Pass, ""},
		}},
	} {
		for _, number := range []bool{false, true} {
			data := healthyModel()
			if c.change != nil {
				c.change(data)
			}
			var checks []Check
			checkDispatcherModel(context.Background(), modelExecutor{data: data, err: c.err, number: number}, "w", "p", func(check Check) { checks = append(checks, check) })
			got := map[string]Check{}
			for _, check := range checks {
				got[check.ID] = check
			}
			if len(got) != len(checks) || len(got) != len(c.want) {
				t.Fatalf("%s: %+v", name, checks)
			}
			for id, w := range c.want {
				g, ok := got[id]
				if !ok || g.Status != w.status || !strings.Contains(g.Message, w.text) || g.Status != Pass && g.Action == "" {
					t.Fatalf("%s (json.Number %v) %s: %+v, want %v", name, number, id, g, w)
				}
			}
		}
	}
}
