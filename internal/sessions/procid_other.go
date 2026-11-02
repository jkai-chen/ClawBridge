//go:build !linux && !darwin && !windows

package sessions

// platformLookup knows nothing here, so no identity is ever Alive and no
// link is ever swept.
func platformLookup(int) (int64, procState) { return 0, procUnknown }
