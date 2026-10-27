package tui

import "time"

// now is the model's clock; a Model built without NewModel uses the wall
// clock.
func (m Model) now() time.Time {
	if m.cfg.Now == nil {
		return time.Now()
	}
	return m.cfg.Now()
}
