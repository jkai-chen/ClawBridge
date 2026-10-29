//go:build !darwin && !linux && !windows

package ctxwatch

// ParentPID is unknown here, so the queue hooks never act.
func ParentPID(int) (int, bool) { return 0, false }
