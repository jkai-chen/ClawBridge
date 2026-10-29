package bus

import "fmt"

// The server's PROVEN gates, by name: each turns on a Linear write only
// after its live probe proves the app may make it (serve --*-proven or
// CLAWBRIDGE_*_PROVEN=1). The project owner's overview shows which are on,
// so doctor and the TUI can say why a write is refused as unproven. The
// list is fixed; nothing else of the server's settings is shown.

// ProvenGateNames are the gates in the order doctor and the TUI show them.
// A gate the server reads must be named here (server test
// TestEveryProvenGateIsShown).
var ProvenGateNames = []string{"project_flow", "issue_subscribers", "issue_move", "issue_family_move", "project_team_add", "project_status_domain"}

// provenGates lists every gate with whether it is on.
func (s *Store) provenGates() []map[string]any {
	on := map[string]bool{
		"project_flow":          s.projectFlowProven,
		"issue_subscribers":     s.issueSubscribersProven,
		"issue_move":            s.issueMoveProven,
		"issue_family_move":     s.issueFamilyMoveProven,
		"project_team_add":      s.projectTeamAddProven,
		"project_status_domain": s.projectStatusDomainProven,
	}
	gates := make([]map[string]any, 0, len(ProvenGateNames))
	for _, name := range ProvenGateNames {
		gates = append(gates, map[string]any{"name": name, "on": on[name]})
	}
	return gates
}

// ProvenGatesOnOff names the gates in an overview's proven_gates that are
// on and those that are off, from a local (typed) or remote (decoded)
// answer; ok is false when the overview has none (a member's, or an older
// server's).
func ProvenGatesOnOff(v any) (on, off []string, ok bool) {
	each := func(gate map[string]any) {
		if gate["on"] == true {
			on = append(on, fmt.Sprint(gate["name"]))
		} else {
			off = append(off, fmt.Sprint(gate["name"]))
		}
	}
	switch gates := v.(type) {
	case []map[string]any:
		for _, gate := range gates {
			each(gate)
		}
	case []any:
		for _, raw := range gates {
			gate, _ := raw.(map[string]any)
			each(gate)
		}
	default:
		return nil, nil, false
	}
	return on, off, true
}
