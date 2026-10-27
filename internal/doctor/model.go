package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
)

// checkDispatcherModel shows a project owner the dispatcher's model mode
// (DISPATCHER-AGENT-DESIGN §11.5): the server's seal keys, each owner's key
// and agent, agentd's heartbeat and token expiry, and the live agents
// against the cap with the host's memory headroom. It reads only, and only
// for a Linear user, as the task board's checks do. Nothing
// is shown to a member who is not an owner, nor by a server without it,
// nor while no owner has set the model up (except a previous seal key
// still set).
func checkDispatcherModel(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	r, err := executor.Execute(ctx, bus.Request{Operation: "dispatcher_model_doctor", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	if err != nil || m == nil {
		return
	}
	holders, _ := m["holders"].([]any)
	modelOn := m["model_on"] == true
	inUse := modelOn || len(holders) > 0
	if !inUse && m["seal_previous"] != true {
		return
	}
	seal := Check{ID: "dispatcher.model.seal", Title: "Model key seal key", Status: Pass, Message: "The server's model key seal key is set, and no previous one is left."}
	switch {
	case m["seal_configured"] != true:
		seal.Status, seal.Message = Fail, "Model setup is unavailable: the server has no CLAWBRIDGE_MODEL_KEY_SEAL_KEY, so no stored key opens and the rules decide."
		seal.Action = "The server operator sets CLAWBRIDGE_MODEL_KEY_SEAL_KEY (docs/operations/deployment.md) and restarts the server."
	case m["seal_previous"] == true:
		seal.Status, seal.Message = Warn, "CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS is still set: a rotation of the seal key is not finished."
		seal.Action = "The server operator runs `clawbridge operator model-seal-key rotate`, then `--confirm`; once it reports that none needs the previous key, they remove it and restart the server."
	}
	add(seal)
	if !inUse {
		return
	}
	waiting := checkModelHolders(m, holders, modelOn, add)
	checkModelProjects(holders, add)
	checkModelLauncher(m, add)
	checkModelCapacity(m, waiting, add)
}

// mintsExpected is the agent credentials a day doctor expects for one
// owner: the daily renewal, and a restart.
const mintsExpected = 3

// checkModelHolders reports each owner's key, and the agent that holds it.
// It returns how many owners with an active key have no running agent.
func checkModelHolders(m map[string]any, holders []any, modelOn bool, add func(Check)) int {
	key := Check{ID: "dispatcher.model.key", Title: "Model keys", Status: Pass}
	agent := Check{ID: "dispatcher.model.agent", Title: "Model agents", Status: Pass}
	var keys, agents, keyProblems, agentProblems []string
	active, running, waiting := 0, 0, 0
	for _, raw := range holders {
		h, _ := raw.(map[string]any)
		who := text(h["principal"])
		if h["you"] == true {
			who = "yours"
		}
		switch {
		case h["key"] == "active":
			active++
			keys = append(keys, fmt.Sprintf("%s: active (revision %d)", who, number(h["config_revision"])))
		default:
			keys = append(keys, fmt.Sprintf("%s: setup test pending (revision %d)", who, number(h["pending_revision"])))
		}
		if f := text(h["test_failure"]); f != "" {
			keyProblems = append(keyProblems, fmt.Sprintf("%s: the setup test of revision %d failed (%s)", who, number(h["test_revision"]), f))
		}
		if h["key"] == "active" && h["agent"] != "running" {
			waiting++
		}
		if h["agent"] == "running" {
			running++
			agents = append(agents, fmt.Sprintf("%s: running, last seen %s ago", who, age(h["last_seen_age"])))
		} else if h["last_seen_age"] != nil {
			agents = append(agents, fmt.Sprintf("%s: offline, last seen %s ago", who, age(h["last_seen_age"])))
		} else {
			agents = append(agents, who+": never seen")
		}
		if h["agent_status"] == "provider_auth" {
			agent.Status = Fail
			agentProblems = append(agentProblems, who+": the provider refuses the key (provider_auth), so the key stopped working")
			agent.Action = "That owner runs model setup again with a working key."
		}
		if c := text(h["launcher_report"]); c != "" {
			agent.Status = Fail
			agentProblems = append(agentProblems, fmt.Sprintf("%s: agentd reported %s %s ago", who, c, age(h["launcher_report_age"])))
			if agent.Action == "" {
				agent.Action = "The server operator reads agentd's log for that owner's agent container."
			}
		}
		switch h["credential_flag"] {
		case "second_redemption":
			agentProblems = append(agentProblems, who+": an agent credential was presented a second time, so someone else may have held it (refused)")
		case "wrong_binding":
			agentProblems = append(agentProblems, who+": an agent presented a wrong binding secret (refused)")
		}
		// One mint a day is the credential's renewal; more is agentd
		// restarting the agent, or someone else holding the launcher token.
		if n := number(h["mints_24h"]); n > mintsExpected {
			agentProblems = append(agentProblems, fmt.Sprintf("%s: its agent was given a new credential %d times in 24 hours, so it was restarted that often", who, n))
			if agent.Status == Pass {
				agent.Status = Warn
				agent.Action = "The server operator reads agentd's log; if agentd didn't restart it, someone else holds the launcher token: revoke it (operator launcher-token revoke ID) and create a new one."
			}
		}
		if h["credential_flag"] != nil && agent.Status == Pass {
			agent.Status = Warn
			agent.Action = "The server operator checks who can reach agentd and its containers; the next credential agentd mints revokes the flagged one."
		}
	}
	key.Message = "Model keys: " + strings.Join(keys, "; ") + "."
	if len(holders) == 0 {
		key.Message = "No owner of this project has a model key."
	}
	switch {
	case modelOn && active == 0:
		key.Status = Fail
		key.Message = "Model mode is on, but no owner of this project has a tested model key: the rules decide every claim."
		key.Action = "A project owner runs model setup."
	case len(keyProblems) > 0:
		key.Status = Warn
		key.Message += " " + strings.Join(keyProblems, "; ") + "; the active key, if any, keeps running."
		key.Action = "That owner runs model setup again once the cause is fixed."
	}
	add(key)
	agent.Message = "Model agents: " + strings.Join(agents, "; ") + "."
	if len(holders) == 0 {
		agent.Message = "No owner of this project has a model key, so no agent runs."
	}
	if len(agentProblems) > 0 {
		agent.Message += " " + strings.Join(agentProblems, "; ") + "."
	}
	if modelOn && running == 0 && agent.Status == Pass {
		agent.Status = Warn
		agent.Message += " No model agent has called in the last 5 minutes, so the rules decide claims."
		agent.Action = "The server operator checks that agentd runs (see the launcher check)."
	}
	add(agent)
	return waiting
}

// checkModelProjects warns when an owner of this project has the model on
// in more projects than one agent serves: the first ones by project ID are
// served, the rest are not. It is shown only then.
func checkModelProjects(holders []any, add func(Check)) {
	var over []string
	for _, raw := range holders {
		h, _ := raw.(map[string]any)
		if h["model_projects"] == nil {
			continue
		}
		who := text(h["principal"])
		if h["you"] == true {
			who = "yours"
		}
		served := "this project is among them"
		if h["project_served"] != true {
			served = "this project is NOT among them, so the model does not serve it"
		}
		over = append(over, fmt.Sprintf("%s: the model is on in %d projects, and the agent serves the first %d by project ID; %s", who, number(h["model_projects"]), number(h["model_projects_cap"]), served))
	}
	if len(over) == 0 {
		return
	}
	add(Check{ID: "dispatcher.model.projects", Title: "Model projects per owner", Status: Warn,
		Message: strings.Join(over, "; ") + ".",
		Action:  "That owner turns the model off (dispatcher disable, or dispatcher model advise off) in the projects that need it least."})
}

// checkModelLauncher reports agentd's heartbeat (its launcher token's last
// use) and that token's expiry.
func checkModelLauncher(m map[string]any, add func(Check)) {
	l, _ := m["launcher"].(map[string]any)
	check := Check{ID: "dispatcher.model.launcher", Title: "Agent launcher (agentd)", Status: Pass}
	const create = "The server operator creates the next token with `clawbridge operator launcher-token create --out PATH`, gives it to agentd, then revokes the old one with `launcher-token revoke ID`."
	switch {
	case number(l["tokens"]) == 0:
		check.Status, check.Message, check.Action = Warn, "No live launcher token: agentd cannot start model agents.", create
	case l["last_used_age"] == nil:
		check.Status, check.Message = Warn, "agentd has never called: no model agent is started."
		check.Action = "The server operator starts agentd with the launcher token."
	case l["reporting"] != true:
		check.Status, check.Message = Warn, fmt.Sprintf("agentd is not reporting: it last called %s ago.", age(l["last_used_age"]))
		check.Action = "The server operator checks that agentd runs, and its log."
	default:
		check.Message = fmt.Sprintf("agentd is reporting: it last called %s ago; its launcher token expires in %s.", age(l["last_used_age"]), age(l["expires_in"]))
	}
	if l["expiring"] == true {
		check.Status = Warn
		check.Message += fmt.Sprintf(" agentd's launcher token expires in %s; after that agentd is refused and no agent starts.", age(l["expires_in"]))
		check.Action = create
	}
	add(check)
}

// checkModelCapacity reports the server's live agents against the cap. At
// the cap, the steady state of a full server, it warns only when an owner
// of this project with an active key has no agent running (waiting), which
// the cap may be why. It warns when the slots left could need more memory
// than the host has.
func checkModelCapacity(m map[string]any, waiting int, add func(Check)) {
	c, _ := m["capacity"].(map[string]any)
	limit := number(c["max_agents"])
	if limit == 0 {
		return
	}
	live, per := number(c["live_agents"]), number(c["agent_memory"])
	check := Check{ID: "dispatcher.model.capacity", Title: "Model agent capacity", Status: Pass}
	check.Message = fmt.Sprintf("%d of at most %d model agents are running on the server.", live, limit)
	if live >= limit && waiting > 0 {
		check.Status = Warn
		check.Message += fmt.Sprintf(" At the cap, %d owner(s) of this project with an active key have no agent running, and a new one is refused (agent_cap).", waiting)
		check.Action = "The server operator raises CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS if the host has the memory."
	}
	if c["mem_available"] == nil {
		check.Message += " The host's available memory is unknown."
	} else if need, have := (limit-live)*per, number(c["mem_available"]); live < limit && need > have {
		check.Status = Warn
		check.Message += fmt.Sprintf(" The %d agents that may still start need up to %d MiB, but the host has %d MiB available.", limit-live, need>>20, have>>20)
		check.Action = "The server operator lowers CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS, or gives the host more memory."
	}
	add(check)
}

// number reads a count from a local result's typed value or a remote
// one's decoded JSON.
func number(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

// age renders seconds as the largest whole unit.
func age(v any) string {
	s := number(v)
	switch {
	case s >= 2*86400:
		return fmt.Sprintf("%d days", s/86400)
	case s >= 2*3600:
		return fmt.Sprintf("%d hours", s/3600)
	case s >= 120:
		return fmt.Sprintf("%d minutes", s/60)
	}
	return fmt.Sprintf("%d s", s)
}
