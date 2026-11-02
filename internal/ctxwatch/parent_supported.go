//go:build darwin || linux || windows

package ctxwatch

// ParentCheckSupported says whether ParentPID can answer on this platform.
const ParentCheckSupported = true
