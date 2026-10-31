// Package uiflow holds the flows the terminal UI and other front ends
// share: the write gate, typed confirmations and the Linear link draft.
// It knows nothing about any one front end.
package uiflow

// WriteGate decides whether a front end may write.
type WriteGate struct {
	// Cloud is true when the front end talks to a cloud project.
	Cloud bool
	// Online is nil when connectivity is unknown.
	Online *bool
	// Source names where the data on screen came from.
	Source string
}

// Blocked is true when writes must be refused: a cloud project that is
// offline or shown from the offline mirror.
func (g WriteGate) Blocked() bool {
	if !g.Cloud {
		return false
	}
	if g.Online != nil && !*g.Online {
		return true
	}
	return g.Source == "offline-mirror"
}
