package bus

import (
	"sort"

	"clawbridge/internal/agentenv"
)

// The agent marker is an accident guard, NOT a security boundary: the client
// reads its own environment (agentenv) and sends the name of the variable
// that says an agent runs it, and an agent can clear its environment. The
// server refuses a person-only operation that arrives with a marker, so an
// agent that runs one by mistake stops. A request with no marker is allowed:
// an older client sends none.

// personOnlyOps are the operations that are a person's decision, run in the
// person's own terminal. Each takes an agent_marker argument. The client
// sends the marker on every one of them (WithAgentMarker), and the store
// refuses a non-empty marker before the operation runs.
var personOnlyOps = map[string]bool{
	"status_closes_set":           true,
	"status_cancel_kinds_set":     true,
	"status_policy_set":           true,
	"project_lead_set":            true,
	"project_lead_record":         true,
	"team_roles_set":              true,
	"lane_pin_set":                true,
	"lane_pin_clear":              true,
	"project_kind_set":            true,
	"project_kind_record":         true,
	"project_flow_resume":         true,
	"intake_trust_add":            true,
	"intake_trust_remove":         true,
	"issue_subscribers_set":       true,
	"dispatcher_enable":           true,
	"dispatcher_disable":          true,
	"dispatcher_model_advise":     true,
	"dispatcher_model_tools":      true,
	"dispatcher_model_budget":     true,
	"dispatcher_model_setup":      true,
	"dispatcher_model_key_delete": true,
	"dispatcher_chat_ask":         true,
	"dispatcher_chat_leads":       true,
	"project_create_ack":          true,
	"issue_writes_ack":            true,
	"taskboard_set_v2":            true,
	"taskboard_clear_v2":          true,
	"taskboard_names_set":         true,
	"taskboard_lanes_set":         true,
}

// markerWhenPolicyChanges are person-only operations that refuse a marker
// only when they change who closes or who may cancel; a status policy's
// other fields stay an owner session's to edit. The operation checks it
// itself against the stored policy.
var markerWhenPolicyChanges = map[string]bool{"status_policy_set": true}

// PersonOnly reports whether op is a person-only operation.
func PersonOnly(op string) bool { return personOnlyOps[op] }

// PersonOnlyOps is the person-only operation names, sorted.
func PersonOnlyOps() []string {
	out := make([]string, 0, len(personOnlyOps))
	for op := range personOnlyOps {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// WithAgentMarker adds the client's agent marker (agentenv.Marker, "" for
// none) to a person-only operation's args, and returns them.
func WithAgentMarker(args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	args["agent_marker"] = agentenv.Marker()
	return args
}

// agentMarkerArg reads an operation's agent_marker: "" when missing or
// empty. A value that isn't a short string is invalid.
func agentMarkerArg(a map[string]any) (string, error) {
	switch v := a["agent_marker"].(type) {
	case nil:
		return "", nil
	case string:
		if len(v) > 64 {
			return "", invalid("agent_marker must be a short string")
		}
		return v, nil
	}
	return "", invalid("agent_marker must be a short string")
}

// refuseAgent is the refusal of a person-only operation that came with a
// marker. It names the variable, never its value, and only a known agent
// variable's name (agentenv.Display): the marker is the client's word.
func refuseAgent(marker string) error {
	return scoped("this is a person's decision; run it in your own terminal, not from an agent session (the agent marker "+agentenv.Display(marker)+" is set)", "forbidden")
}

// checkAgentMarker refuses a person-only operation sent with a non-empty
// marker, before the operation runs.
func checkAgentMarker(r Request) error {
	if !personOnlyOps[r.Operation] {
		return nil
	}
	marker, err := agentMarkerArg(r.Args)
	if err != nil {
		return err
	}
	if marker != "" && !markerWhenPolicyChanges[r.Operation] {
		return refuseAgent(marker)
	}
	return nil
}

// RefuseMarkedRequest is the refusal a client makes locally, before sending
// anything, for a person-only operation that carries a non-empty marker:
// the text the store would give, for an older server as for a newer one. It
// is nil for any other request, and for status_policy_set, which only the
// store can judge (it refuses a marker only when the policy changes who
// closes or who may cancel).
func RefuseMarkedRequest(op string, args map[string]any) error {
	if !personOnlyOps[op] || markerWhenPolicyChanges[op] {
		return nil
	}
	if marker, _ := args["agent_marker"].(string); marker != "" {
		return refuseAgent(marker)
	}
	return nil
}
